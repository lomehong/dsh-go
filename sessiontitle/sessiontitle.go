// Package sessiontitle ports @deepseek-ai/dsh-session-title's service half:
// provider registration, deterministic fallback materialization, automatic
// title scheduling over the store's post-commit event feed, and explicit user
// renames. The deterministic fold surface (normalize, fallback, collect,
// fold) already lives in sessionquery; this package composes it into a live
// service. Deviations from the TypeScript original are recorded on the types
// they affect.
package sessiontitle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/sessionquery"
)

// Automatic-mode names a provider may declare.
const (
	// AutomaticFirstPrompt schedules one generation for a root session's
	// first eligible human message.
	AutomaticFirstPrompt = "first-prompt"
	// AutomaticAllPrompts schedules a generation after every eligible human
	// message.
	AutomaticAllPrompts = "all-prompts"
)

// ErrInvalid rejects an explicit user title whose text normalizes to empty —
// the one Rename failure that blames the input. Liveness and disposal
// failures stay plain errors.
var ErrInvalid = errors.New("sessiontitle: session title must contain visible characters")

// Provider generates title candidates from a session's eligible human
// messages. A provider is a plain interface value: the TypeScript contract's
// `automatic` discriminator becomes a method.
type Provider interface {
	// ID is the stable provider identifier recorded in accepted titles.
	ID() string
	// Automatic is one of AutomaticFirstPrompt or AutomaticAllPrompts.
	Automatic() string
	// Generate produces one candidate; a non-nil error abandons the
	// generation attempt (contained to a logged warning).
	Generate(request ProviderRequest) (ProviderResult, error)
}

// ProviderRequest is the closed input set of one Generate call.
type ProviderRequest struct {
	// Session is the live session being titled.
	Session *session.Session
	// Messages are the eligible human messages through the scheduled seq.
	Messages []sessionquery.SessionTitleUserMessage
	// Route is the exact main-request route that triggered the generation,
	// when known.
	Route *sessionquery.SessionTitleModelProvenance
	// Signal cancels the generation.
	Signal context.Context
}

// ProviderResult is one candidate title. MessageSeqs must reference the
// eligible messages the title derives from (empty means the provider
// expressed no provenance); Model carries the auxiliary-route provenance.
type ProviderResult struct {
	Title       string
	MessageSeqs []int64
	Model       *sessionquery.SessionTitleModelProvenance
}

// ProviderFunc adapts function values to the Provider interface.
type ProviderFunc struct {
	// IDFunc returns the stable provider identifier.
	IDFunc func() string
	// AutomaticFunc returns one of the Automatic* mode names.
	AutomaticFunc func() string
	// GenerateFunc performs one candidate generation.
	GenerateFunc func(request ProviderRequest) (ProviderResult, error)
}

// ID returns the stable provider identifier.
func (f ProviderFunc) ID() string { return f.IDFunc() }

// Automatic returns the declared automatic mode.
func (f ProviderFunc) Automatic() string { return f.AutomaticFunc() }

// Generate produces one candidate.
func (f ProviderFunc) Generate(request ProviderRequest) (ProviderResult, error) {
	return f.GenerateFunc(request)
}

// Config mirrors the official service config. All values are required
// positive integers and validated fail-loud at construction.
type Config struct {
	// FallbackMaxWords is the maximum whitespace-delimited words in the
	// built-in fallback.
	FallbackMaxWords int
	// FallbackMaxBytes is the maximum UTF-8 bytes in the built-in fallback.
	FallbackMaxBytes int
	// MaxTitleBytes is the maximum UTF-8 bytes in any accepted title.
	MaxTitleBytes int
}

type registration struct {
	provider Provider
	closing  bool
}

type pendingWork struct {
	// reg is the registration identity the work was scheduled under;
	// comparing pointers (not interface values) keeps ProviderFunc
	// implementations comparable.
	reg        *registration
	revision   uint64
	throughSeq int64
}

type workState struct {
	revision uint64
	pending  *pendingWork
	cancel   context.CancelFunc
	// pipe serializes one session's title pipeline: the fallback
	// materialization and the provider generation append under the same
	// lock, so a stale fallback can never land after a provider title
	// (the official port memoizes the fallback promise and awaits it
	// before generation, which yields the same ordering guarantee).
	pipe sync.Mutex
}

// Service is the live title service. It takes the store's post-commit event
// sink for its working lifetime (the sink is single-slot; see the deviation
// note on NewService).
type Service struct {
	store  *session.Store
	logger cordis.Logger
	config Config

	mu           sync.Mutex
	closed       bool
	registration *registration
	work         map[session.SessionID]*workState

	lifetime context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewService validates the config and attaches the service to the store's
// event feed.
//
// Deviation (sink exclusivity): the official service subscribes as one
// listener among many; the Go store's event sink is single-slot, so the
// service OWNS the slot until Dispose releases it. A composition that needs
// its own event tap must multiplex through the service or wrap the store.
func NewService(store *session.Store, config Config, logger cordis.Logger) (*Service, error) {
	if store == nil {
		return nil, errors.New("sessiontitle: store is required")
	}
	if config.FallbackMaxWords <= 0 {
		return nil, errors.New("sessiontitle: FallbackMaxWords must be a positive integer")
	}
	if config.FallbackMaxBytes <= 0 {
		return nil, errors.New("sessiontitle: FallbackMaxBytes must be a positive integer")
	}
	if config.MaxTitleBytes <= 0 {
		return nil, errors.New("sessiontitle: MaxTitleBytes must be a positive integer")
	}
	if config.FallbackMaxBytes > config.MaxTitleBytes {
		return nil, errors.New("sessiontitle: FallbackMaxBytes must not exceed MaxTitleBytes")
	}
	sessionquery.RegisterEvents()
	lifetime, cancel := context.WithCancel(context.Background())
	s := &Service{
		store:    store,
		logger:   logger,
		config:   config,
		work:     map[session.SessionID]*workState{},
		lifetime: lifetime,
		cancel:   cancel,
	}
	store.OnEvent(s.onEvent)
	store.OnDisposed(s.onDisposed)
	return s, nil
}

// Dispose stops the service: in-flight generation is aborted, pending work is
// dropped, the store's event sink is released, and tracked goroutines are
// awaited. Idempotent.
func (s *Service) Dispose() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.registration = nil
	for _, state := range s.work {
		if state.cancel != nil {
			state.cancel()
		}
	}
	s.work = map[session.SessionID]*workState{}
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
	s.store.OnEvent(nil)
	s.store.OnDisposed(nil)
}

// RegisterProvider installs the (single-slot) title provider and returns its
// closer. Registering again supersedes the previous registration — the
// previous provider's in-flight work is abandoned by the closing flag and
// revision guards. Deviation: the official register asserts a single
// registration for the service lifetime; the Go face returns an explicit
// closer so a replacement is a first-class composition move.
func (s *Service) RegisterProvider(provider Provider) (func(), error) {
	if provider == nil {
		return nil, errors.New("sessiontitle: provider is required")
	}
	switch provider.Automatic() {
	case AutomaticFirstPrompt, AutomaticAllPrompts:
	default:
		return nil, fmt.Errorf("sessiontitle: provider %q declares unknown automatic mode %q", provider.ID(), provider.Automatic())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("sessiontitle: service is disposed")
	}
	if s.registration != nil {
		s.registration.closing = true
	}
	mine := &registration{provider: provider}
	s.registration = mine
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Identity is the registration slot pointer, never the provider
		// interface value: ProviderFunc values are uncomparable.
		if s.registration == mine {
			s.registration.closing = true
			s.registration = nil
		}
	}, nil
}

// Get folds the latest title snapshot from the session's log.
func (s *Service) Get(sess *session.Session) *sessionquery.SessionTitleSnapshot {
	return sessionquery.FoldSessionTitle(sess.Events())
}

// Rename accepts an explicit user title. The rename pins the title: in-flight
// automatic generation is superseded and later user messages schedule none.
// An empty-after-normalization title fails with ErrInvalid.
func (s *Service) Rename(sess *session.Session, title string) (*sessionquery.SessionTitleSnapshot, error) {
	if err := s.assertServiceActive(); err != nil {
		return nil, err
	}
	if s.store.Get(sess.ID()) != sess {
		return nil, fmt.Errorf("sessiontitle: session %q is not live in this store", sess.ID())
	}
	normalized := sessionquery.NormalizeSessionTitle(title, s.config.MaxTitleBytes)
	if normalized == "" {
		return nil, ErrInvalid
	}
	state := s.stateFor(sess.ID())
	// Supersede first: the in-flight generation aborts and releases the
	// session's pipeline lock, so the pinned user title lands after any
	// straggler and no provider append can interleave past the revision
	// guard.
	s.supersede(state)
	state.pipe.Lock()
	defer state.pipe.Unlock()
	if _, err := sess.Append("session/title", sessionquery.SessionTitleEventData{
		Title:       normalized,
		MessageSeqs: []int64{},
		Source:      sessionquery.SessionTitleSource{Kind: sessionquery.TitleSourceUser},
	}, nil); err != nil {
		return nil, err
	}
	snapshot := s.Get(sess)
	if snapshot == nil {
		return nil, errors.New("sessiontitle: renamed title failed to fold")
	}
	return snapshot, nil
}

// Refresh retries the registered provider, or materializes the built-in
// fallback when no provider is registered. A standing user title is the
// unpin: refresh re-derives over it.
func (s *Service) Refresh(sess *session.Session, signal context.Context) (*sessionquery.SessionTitleSnapshot, error) {
	if err := s.assertServiceActive(); err != nil {
		return nil, err
	}
	if s.store.Get(sess.ID()) != sess {
		return nil, fmt.Errorf("sessiontitle: session %q is not live in this store", sess.ID())
	}
	if err := s.ensureFallback(sess); err != nil {
		return nil, err
	}
	s.mu.Lock()
	reg := s.registration
	state := s.stateForLocked(sess.ID())
	revision := state.revision
	s.mu.Unlock()
	if reg == nil || reg.closing {
		return s.Get(sess), nil
	}
	return s.runProvider(sess, state, revision, reg, nil, signal)
}

func (s *Service) assertServiceActive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sessiontitle: service is disposed")
	}
	return nil
}

func (s *Service) stateFor(id session.SessionID) *workState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateForLocked(id)
}

// stateForLocked requires s.mu held.
func (s *Service) stateForLocked(id session.SessionID) *workState {
	state := s.work[id]
	if state == nil {
		state = &workState{}
		s.work[id] = state
	}
	return state
}

// supersede bumps the revision, dropping pending work and cancelling any
// active generation for the session.
func (s *Service) supersede(state *workState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state.revision++
	state.pending = nil
	if state.cancel != nil {
		state.cancel()
		state.cancel = nil
	}
}

func (s *Service) onEvent(sess *session.Session, event session.Event) {
	switch event.Type {
	case session.EventUserMessage:
		s.onUserMessage(sess, event)
	case session.EventRequestHeader:
		s.onRequestHeader(sess, event)
	default:
	}
}

func (s *Service) onDisposed(sess *session.Session) {
	s.mu.Lock()
	state := s.work[sess.ID()]
	delete(s.work, sess.ID())
	s.mu.Unlock()
	if state != nil && state.cancel != nil {
		state.cancel()
	}
}

func (s *Service) onUserMessage(sess *session.Session, event session.Event) {
	if err := s.assertServiceActive(); err != nil {
		return
	}
	eligible := sessionquery.CollectSessionTitleMessages([]session.Event{event}, nil)
	if len(eligible) == 0 {
		return
	}
	current := s.Get(sess)
	if current != nil && current.Source.Kind == sessionquery.TitleSourceUser {
		// A user rename pins the title: no automatic revision may override it.
		return
	}
	s.mu.Lock()
	reg := s.registration
	open := reg != nil && !reg.closing
	var shouldSchedule bool
	if open {
		messages := sessionquery.CollectSessionTitleMessages(sess.Events(), &event.Seq)
		shouldSchedule = reg.provider.Automatic() == AutomaticAllPrompts ||
			(sess.Header().ParentSession == "" && len(messages) == 1 && current == nil)
		if shouldSchedule {
			state := s.stateForLocked(sess.ID())
			state.revision++
			state.pending = &pendingWork{reg: reg, revision: state.revision, throughSeq: event.Seq}
		}
	}
	s.mu.Unlock()
	// The fallback update runs off the commit path: appending inside the
	// store's post-commit feed would recurse into the feed.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.ensureFallback(sess); err != nil {
			if s.assertServiceActive() == nil {
				s.logger.Warn(fmt.Sprintf("session %q: fallback title update failed: %v", sess.ID(), err))
			}
		}
	}()
}

func (s *Service) onRequestHeader(sess *session.Session, event session.Event) {
	if err := s.assertServiceActive(); err != nil {
		return
	}
	s.mu.Lock()
	state := s.work[sess.ID()]
	s.mu.Unlock()
	if state == nil || state.pending == nil || state.pending.throughSeq >= event.Seq {
		return
	}
	var header struct {
		Header struct {
			Config struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"config"`
		} `json:"header"`
	}
	if err := json.Unmarshal(event.Data, &header); err != nil {
		return
	}
	route := &sessionquery.SessionTitleModelProvenance{
		Provider: header.Header.Config.Provider,
		Model:    header.Header.Config.Model,
	}
	s.startPending(sess, state, state.pending, route)
}

func (s *Service) startPending(sess *session.Session, state *workState, pending *pendingWork, route *sessionquery.SessionTitleModelProvenance) {
	s.mu.Lock()
	state.pending = nil
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.mu.Lock()
		reg := s.registration
		current := reg != nil && reg == pending.reg && !reg.closing &&
			s.work[sess.ID()] == state && state.revision == pending.revision
		s.mu.Unlock()
		if !current {
			return
		}
		ctx, cancel := context.WithCancel(s.lifetime)
		s.mu.Lock()
		state.cancel = cancel
		s.mu.Unlock()
		if _, err := s.runProvider(sess, state, pending.revision, reg, route, ctx); err != nil {
			if ctx.Err() == nil && s.assertServiceActive() == nil {
				s.logger.Warn(fmt.Sprintf("session %q: automatic title generation failed: %v", sess.ID(), err))
			}
		}
		s.mu.Lock()
		if state.cancel != nil {
			state.cancel = nil
		}
		s.mu.Unlock()
		cancel()
	}()
}

// runProvider executes and accepts one current provider generation. The
// session's pipeline lock is held across fallback, generation, and append so
// ordering is total (see workState.pipe).
func (s *Service) runProvider(sess *session.Session, state *workState, revision uint64, reg *registration, route *sessionquery.SessionTitleModelProvenance, ctx context.Context) (*sessionquery.SessionTitleSnapshot, error) {
	state.pipe.Lock()
	defer state.pipe.Unlock()
	if err := s.assertCurrent(sess, reg, revision); err != nil {
		return nil, err
	}
	if err := s.ensureFallbackLocked(sess, state); err != nil {
		return nil, err
	}
	if err := s.assertCurrent(sess, reg, revision); err != nil {
		return nil, err
	}
	messages := sessionquery.CollectSessionTitleMessages(sess.Events(), nil)
	result, err := reg.provider.Generate(ProviderRequest{
		Session:  sess,
		Messages: messages,
		Route:    route,
		Signal:   ctx,
	})
	if err != nil {
		return nil, err
	}
	if err := s.assertCurrent(sess, reg, revision); err != nil {
		return nil, err
	}
	title := sessionquery.NormalizeSessionTitle(result.Title, s.config.MaxTitleBytes)
	if title == "" {
		return nil, errors.New("provider returned an empty title")
	}
	seqs := result.MessageSeqs
	if seqs == nil {
		seqs = []int64{}
	}
	if _, err := sess.Append("session/title", sessionquery.SessionTitleEventData{
		Title:       title,
		MessageSeqs: seqs,
		Source: sessionquery.SessionTitleSource{
			Kind:     sessionquery.TitleSourceProvider,
			Provider: reg.provider.ID(),
			Model:    result.Model,
		},
	}, nil); err != nil {
		return nil, err
	}
	return s.Get(sess), nil
}

func (s *Service) assertCurrent(sess *session.Session, reg *registration, revision uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.work[sess.ID()]
	if s.closed || s.registration != reg || reg.closing || state == nil || state.revision != revision {
		return errors.New("sessiontitle: generation superseded")
	}
	return nil
}

// ensureFallback materializes the built-in fallback title when the session
// has no accepted title yet. Callers holding state.pipe must use
// ensureFallbackLocked; the standalone form serializes itself.
func (s *Service) ensureFallback(sess *session.Session) error {
	state := s.stateFor(sess.ID())
	state.pipe.Lock()
	defer state.pipe.Unlock()
	return s.ensureFallbackLocked(sess, state)
}

func (s *Service) ensureFallbackLocked(sess *session.Session, state *workState) error {
	if err := s.assertServiceActive(); err != nil {
		return err
	}
	if s.Get(sess) != nil {
		return nil
	}
	messages := sessionquery.CollectSessionTitleMessages(sess.Events(), nil)
	if len(messages) == 0 {
		return nil
	}
	title := sessionquery.FallbackSessionTitle(messages[0].Text, s.config.FallbackMaxWords, s.config.FallbackMaxBytes)
	if title == "" {
		return nil
	}
	if s.store.Get(sess.ID()) != sess {
		return fmt.Errorf("sessiontitle: session %q is not live in this store", sess.ID())
	}
	if s.Get(sess) != nil {
		return nil
	}
	_, err := sess.Append("session/title", sessionquery.SessionTitleEventData{
		Title:       title,
		MessageSeqs: []int64{messages[0].Seq},
		Source:      sessionquery.SessionTitleSource{Kind: sessionquery.TitleSourceFallback},
	}, nil)
	return err
}
