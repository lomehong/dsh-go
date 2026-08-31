// The in-memory session store (`ctx.sessions` in the official tree).
// Persistence is intentionally not implemented here — persistence plugins
// subscribe to the event feed and flush on demand.
package session

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Fork rejection codes (SessionForkError).
const (
	ForkSessionNotFound = "SESSION_NOT_FOUND"
	ForkSessionNotLive  = "SESSION_NOT_LIVE"
	ForkSessionExists   = "SESSION_ALREADY_EXISTS"
	ForkInvalidBoundary = "INVALID_BOUNDARY"
	ForkOpenTurn        = "OPEN_TURN"
)

// ForkError is the typed error for session fork rejections.
type ForkError struct {
	Message string
	Code    string
}

func (e *ForkError) Error() string { return e.Message }

// CreateOptions attach creation metadata to a new live session.
type CreateOptions struct {
	// Seed populates the session with a copy of those events (replay/fork).
	Seed []Event
	// HeaderMetadata attaches validated fields of the immutable header; the
	// store fills version/id/createdAt.
	HeaderMetadata SessionHeader
	// DelegationDepth and Origin are subagent-child facts.
	DelegationDepth *int64
	Origin          string
}

// storeEntry is all mutable lifecycle state for one exact store entry.
type storeEntry struct {
	id      SessionID
	session *Session

	store *Store

	mu              sync.Mutex
	announced       bool
	detachRequested bool
	detached        bool
	detach          func()

	eventMu   sync.Mutex
	eventSink func(Event)
}

func (e *storeEntry) eventListeners() []func(Event) {
	e.eventMu.Lock()
	defer e.eventMu.Unlock()
	if e.eventSink == nil {
		return nil
	}
	fn := e.eventSink
	return []func(Event){fn}
}

func (e *storeEntry) warnf(format string, args ...any) {
	e.store.warnf(format, args...)
}

func (e *storeEntry) appendCommitted(event Event) {
	e.store.onEventCommit(e.session, event)
}

// Store is the in-memory session store: creation, publication, and the
// append feed. Persistence stays a plugin concern.
type Store struct {
	mu    sync.Mutex
	seq   int
	items map[SessionID]*storeEntry

	logger         Logger
	created        []func(*Session) error
	disposed       []func(*Session)
	flush          []func(*Session) error
	eventSink      func(*Session, Event)
	flushAfterWait sync.WaitGroup
}

// Logger is the minimal logging face the store needs (satisfied by
// cordis.Logger implementations).
type Logger interface {
	Warn(message string)
}

// NewStore builds an empty store; a nil logger discards records.
func NewStore(logger Logger) *Store {
	if logger == nil {
		logger = discardLogger{}
	}
	return &Store{items: map[SessionID]*storeEntry{}, logger: logger}
}

type discardLogger struct{}

func (discardLogger) Warn(string) {}

func (st *Store) warnf(format string, args ...any) {
	st.logger.Warn(fmt.Sprintf(format, args...))
}

// OnCreated registers a creation announcement. A synchronous error vetoes
// and rolls back the creation with a paired disposal.
func (st *Store) OnCreated(fn func(*Session) error) {
	st.mu.Lock()
	st.created = append(st.created, fn)
	st.mu.Unlock()
}

// OnDisposed registers a disposal observer; failures are logged and
// contained.
func (st *Store) OnDisposed(fn func(*Session)) {
	st.mu.Lock()
	st.disposed = append(st.disposed, fn)
	st.mu.Unlock()
}

// OnEvent registers the post-commit append feed. The callback runs after
// the event entered the log; failures are logged and contained.
func (st *Store) OnEvent(fn func(*Session, Event)) {
	st.mu.Lock()
	st.eventSink = fn
	st.mu.Unlock()
}

// OnFlush registers a flush participant. Flush runs participants in
// parallel and reports the first failure only after every participant
// settled.
func (st *Store) OnFlush(fn func(*Session) error) {
	st.mu.Lock()
	st.flush = append(st.flush, fn)
	st.mu.Unlock()
}

// onEventCommit feeds the sink the exact event that just committed. The
// committed event is passed in rather than re-read from the log: the feed
// runs outside the session lock, so a concurrent append landing between
// commit and feed would otherwise lose one event and duplicate the next.
func (st *Store) onEventCommit(session *Session, event Event) {
	st.mu.Lock()
	sink := st.eventSink
	st.mu.Unlock()
	if sink == nil {
		return
	}
	// Only live (announced) sessions feed the store stream; seed replay in
	// a detached session has no entry.
	st.mu.Lock()
	entry := st.items[session.ID()]
	st.mu.Unlock()
	if entry == nil {
		return
	}
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				st.warnf("session %q: session/event listener panicked: %v", session.ID(), rec)
			}
		}()
		sink(session, event)
	}()
}

// Create prepares, enters, and announces a session owned by the calling
// fiber. A creation announcement that vetoes rolls back with a paired
// disposal.
func (st *Store) Create(id SessionID, options CreateOptions) (*Session, error) {
	header := options.HeaderMetadata
	header.ID = id
	header.Origin = options.Origin
	header.DelegationDepth = options.DelegationDepth
	session, err := NewDetached(id, options.Seed, &header)
	if err != nil {
		return nil, err
	}
	detach, err := st.Enter(session)
	if err != nil {
		return nil, err
	}
	if err := st.Announce(session); err != nil {
		detach()
		return nil, err
	}
	return session, nil
}

// Enter binds a prepared session to the store and returns its detach
// disposer. The binding installs the session/event feed; publication starts
// at Announce.
func (st *Store) Enter(session *Session) (func(), error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	id := session.ID()
	if _, exists := st.items[id]; exists {
		return nil, &ForkError{Message: fmt.Sprintf("session %q already exists", id), Code: ForkSessionExists}
	}
	entry := &storeEntry{id: id, session: session, store: st}
	st.items[id] = entry
	session.mu.Lock()
	session.entry = entry
	session.mu.Unlock()
	detached := false
	entry.detach = func() {
		st.mu.Lock()
		if detached {
			st.mu.Unlock()
			return
		}
		detached = true
		entry.mu.Lock()
		wasAnnounced := entry.announced
		entry.detached = true
		delete(st.items, id)
		entry.mu.Unlock()
		session.mu.Lock()
		session.entry = nil
		session.mu.Unlock()
		st.mu.Unlock()
		if wasAnnounced {
			st.dispatchDisposed(session)
		}
	}
	return entry.detach, nil
}

// Announce runs the creation announcement for an entered session. A
// synchronous veto error rolls the announcement back; the caller pairs it
// with the detach disposer (Create does).
func (st *Store) Announce(session *Session) error {
	st.mu.Lock()
	entry := st.items[session.ID()]
	callbacks := append([]func(*Session) error(nil), st.created...)
	st.mu.Unlock()
	if entry == nil {
		return fmt.Errorf("session %q is not entered in the store", session.ID())
	}
	entry.mu.Lock()
	if entry.announced {
		entry.mu.Unlock()
		return fmt.Errorf("session %q is already announced", session.ID())
	}
	entry.announced = true
	entry.mu.Unlock()

	for _, callback := range callbacks {
		if err := runVeto(callback, session, st); err != nil {
			entry.mu.Lock()
			entry.announced = false
			entry.mu.Unlock()
			return err
		}
	}
	return nil
}

func runVeto(callback func(*Session) error, session *Session, st *Store) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("creation announcement panicked: %v", rec)
		}
	}()
	return callback(session)
}

func (st *Store) dispatchDisposed(session *Session) {
	st.mu.Lock()
	callbacks := make([]func(*Session), len(st.disposed))
	copy(callbacks, st.disposed)
	st.mu.Unlock()
	for _, callback := range callbacks {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					st.warnf("session %q: session/disposed listener panicked: %v", session.ID(), rec)
				}
			}()
			callback(session)
		}()
	}
}

// Get returns the live session for one id, or nil.
func (st *Store) Get(id SessionID) *Session {
	st.mu.Lock()
	defer st.mu.Unlock()
	entry := st.items[id]
	if entry == nil {
		return nil
	}
	return entry.session
}

// Fork validates a fork of one live session's prefix and returns the seed
// event prefix for a child session (the child's constructor appends the
// end-seed marker). The source must be the store's live instance, the child
// id must be free, the boundary must be a contiguous existing seq, and the
// selected prefix must not end inside an open turn.
func (st *Store) Fork(source *Session, childID SessionID, boundary int64) ([]Event, error) {
	st.mu.Lock()
	entry := st.items[source.ID()]
	childTaken := st.items[childID] != nil
	st.mu.Unlock()
	if entry == nil {
		return nil, &ForkError{Message: fmt.Sprintf("session %q not found", source.ID()), Code: ForkSessionNotFound}
	}
	if entry.session != source {
		return nil, &ForkError{Message: fmt.Sprintf("session %q is not the live store instance", source.ID()), Code: ForkSessionNotLive}
	}
	if childTaken {
		return nil, &ForkError{Message: fmt.Sprintf("session %q already exists", childID), Code: ForkSessionExists}
	}
	events := source.Events()
	if boundary < 0 || boundary >= int64(len(events)) {
		return nil, &ForkError{Message: fmt.Sprintf("fork boundary %d is not a contiguous existing seq of %q", boundary, source.ID()), Code: ForkInvalidBoundary}
	}
	// The selected prefix ends inside an open turn when the last turn
	// boundary within it is a turn/start.
	for i := boundary; i >= 0; i-- {
		if events[i].Type == EventTurnStart {
			var start TurnStartData
			_ = json.Unmarshal(events[i].Data, &start)
			return nil, &ForkError{
				Message: fmt.Sprintf("fork boundary %d of %q ends inside open turn %d", boundary, source.ID(), start.Turn),
				Code:    ForkOpenTurn,
			}
		}
		if events[i].Type == EventTurnEnd {
			break
		}
	}
	prefix := make([]Event, boundary+1)
	for i := range prefix {
		prefix[i] = DeepCopyEvent(events[i])
	}
	return prefix, nil
}

// List returns every live session id in insertion order.
func (st *Store) List() []SessionID {
	st.mu.Lock()
	defer st.mu.Unlock()
	ids := make([]SessionID, 0, len(st.items))
	for id := range st.items {
		ids = append(ids, id)
	}
	return ids
}

// FlushResult reports one flush round.
type FlushResult struct {
	// Participated is true when any live session ran at least one flush
	// participant.
	Participated bool
	// Error is the first participant failure, reported only after every
	// participant settled.
	Error error
}

// Flush runs every registered flush participant over every live session in
// parallel (allSettled semantics) and detaches every session whose flush
// reported clean.
func (st *Store) Flush() FlushResult {
	st.mu.Lock()
	sessions := make([]*Session, 0, len(st.items))
	for _, entry := range st.items {
		if entry.session.FirstLiveSeq() >= int64(len(entry.session.Events())) {
			continue // nothing live to flush
		}
		sessions = append(sessions, entry.session)
	}
	participants := append([]func(*Session) error(nil), st.flush...)
	st.mu.Unlock()

	var (
		mu      sync.Mutex
		first   error
		anyRan  bool
		cleaned []*Session
	)
	var wg sync.WaitGroup
	for _, session := range sessions {
		session := session
		for _, participant := range participants {
			wg.Add(1)
			go func(participant func(*Session) error) {
				defer wg.Done()
				err := func() (err error) {
					defer func() {
						if rec := recover(); rec != nil {
							err = fmt.Errorf("flush participant panicked: %v", rec)
						}
					}()
					return participant(session)
				}()
				mu.Lock()
				defer mu.Unlock()
				anyRan = true
				if err != nil && first == nil {
					first = err
					return
				}
				if err == nil {
					cleaned = append(cleaned, session)
				}
			}(participant)
		}
	}
	wg.Wait()
	return FlushResult{Participated: anyRan, Error: first}
}
