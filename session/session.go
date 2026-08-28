// The event-sourced session: an append-only log with its surface view,
// derived message history, and request-header fold. Port of the Session
// class in packages/core/session/src/index.ts.
package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dshgo/llm"
)

// Session is an event-sourced session: an append-only log of Events. Create
// live instances through a Store; NewDetached builds replay/fork copies.
// Seeding with an existing event log replays or forks a session.
type Session struct {
	log     []Event
	surface *SurfaceManager

	// Detached validated creation metadata, kept out of the event log — a
	// storage concern, not replayable conversation state.
	header       SessionHeader
	firstLiveSeq int64

	mu        sync.Mutex
	appending bool
	entry     *storeEntry

	// Incremental folds, each advanced once per unseen event.
	headerFold        *EpochHeader
	headerFoldSeq     int
	contextFold       *RequestContext
	contextFoldSeq    int
	derived           []llm.Message
	derivedNodes      int
	derivedGeneration int64
	eventsSnapshot    []Event
}

// NewDetached creates a detached session by validating and snapshotting
// borrowed seed events and storage metadata. A nil header synthesizes the
// minimal one (stamped with SESSION_FORMAT_VERSION).
func NewDetached(id SessionID, seed []Event, header *SessionHeader) (*Session, error) {
	return newSession(id, seed, header, false)
}

// NewRestored restores a detached session by taking ownership of fresh
// persistence values; the header must be present and validated.
func NewRestored(id SessionID, seed []Event, header SessionHeader) (*Session, error) {
	return newSession(id, seed, &header, true)
}

func newSession(id SessionID, seed []Event, header *SessionHeader, restore bool) (*Session, error) {
	s := &Session{}
	if restore {
		if err := validateSessionHeader(id, *header); err != nil {
			return nil, err
		}
		s.header = *header
	} else if header != nil {
		if err := validateSessionHeader(id, *header); err != nil {
			return nil, err
		}
		s.header = *header
	} else {
		s.header = SessionHeader{Version: SESSION_FORMAT_VERSION, ID: id, CreatedAt: time.Now().UnixMilli()}
	}

	s.surface = NewSurfaceManager(&s.log)
	for index, source := range seed {
		if err := validateSeedEvent(source, index); err != nil {
			return nil, err
		}
		if source.Seq != int64(index) {
			return nil, fmt.Errorf("seed event at index %d has seq %d (expected %d); seed must be contiguous from 0", index, source.Seq, index)
		}
		if err := s.surface.ValidateNext(source); err != nil {
			return nil, fmt.Errorf("invalid seed event at index %d: %w", index, err)
		}
		s.log = append(s.log, source)
	}
	s.firstLiveSeq = int64(len(s.log))
	// The marker is appended here so it is already in `events` when a
	// backend captures the creation seed: no load-time write. A seed already
	// ending in one is not re-marked, so reopening an untouched session does
	// not grow its log per open.
	if len(s.log) > 0 && s.log[len(s.log)-1].Type != EventEndSeed {
		if _, err := s.Append(EventEndSeed, map[string]any{}, nil); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ID returns the session identity from the header's single copy.
func (s *Session) ID() SessionID { return s.header.ID }

// Header returns the immutable creation metadata.
func (s *Session) Header() SessionHeader { return s.header }

// FirstLiveSeq is the first seq appended in this process: the constructor
// seed length (0 without one). Smaller seqs entered through construction and
// were never published on the event feed.
func (s *Session) FirstLiveSeq() int64 { return s.firstLiveSeq }

// Surface returns the ordered surface view over the log.
func (s *Session) Surface() *SurfaceManager { return s.surface }

// Events returns a snapshot copy of the append-only log; the copy is reused
// until the next append.
func (s *Session) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventsSnapshot == nil {
		s.eventsSnapshot = append([]Event(nil), s.log...)
	}
	return s.eventsSnapshot
}

// Seq is the next event's sequence number — always the log length (the
// seq = log.length contiguity contract).
func (s *Session) Seq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.log))
}

// Append adds one typed event to the log and notifies the store-owned
// observers after the event enters the log. The hot path never blocks on
// I/O — persistence buffers asynchronously. Once the event is in the log
// the append is committed: observer failures are logged and contained per
// listener. The intent is REQUIRED for surface events (every
// message-producing event must declare how it joins the surface) and
// forbidden for non-surface types like turn/start or assistant/chunk.
func (s *Session) Append(eventType string, data any, intent *SurfaceIntent) (Event, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("session event %q carries non-JSON-serializable data: %w", eventType, err)
	}
	if !json.Valid(encoded) {
		return Event{}, fmt.Errorf("session event %q carries non-JSON-serializable data", eventType)
	}
	if err := assertSupportedRequestHeader(eventType, encoded); err != nil {
		return Event{}, err
	}

	s.mu.Lock()
	if s.appending {
		s.mu.Unlock()
		return Event{}, fmt.Errorf("session append cannot reenter while another append is being published")
	}
	event := Event{Type: eventType, Seq: int64(len(s.log)), Time: time.Now().UnixMilli(), Data: json.RawMessage(encoded)}
	if intent != nil {
		event.SurfaceOp = &intent.SurfaceOp
		event.SourceEventSeqs = intent.SourceEventSeqs
	}
	if err := assertMessageEventShape(event); err != nil {
		s.mu.Unlock()
		return Event{}, err
	}
	if err := s.surface.ValidateNext(event); err != nil {
		s.mu.Unlock()
		return Event{}, err
	}

	// Publish with the same containment the store installs: the listener
	// snapshot resolves before the log push, callbacks run after it — and
	// after the session lock is released, so observers may read the session
	// — and an observer failure never fails the committed append.
	var callbacks []func(Event)
	s.appending = true
	if s.entry != nil {
		callbacks = s.entry.eventListeners()
	}
	s.log = append(s.log, event)
	s.eventsSnapshot = nil
	s.appending = false
	entry := s.entry
	s.mu.Unlock()

	for _, callback := range callbacks {
		s.runContainedObserver(callback, event)
	}
	if entry != nil {
		entry.appendCommitted()
	}
	return event, nil
}

// runContainedObserver invokes one observe-only listener with containment.
func (s *Session) runContainedObserver(callback func(Event), event Event) {
	defer func() {
		if rec := recover(); rec != nil {
			if s.entry != nil {
				s.entry.warnf("session %q: session/event listener panicked: %v", s.ID(), rec)
			}
		}
	}()
	callback(event)
}

// RequestHeader is the EpochHeader in force after the log's last header
// event — the header the NEXT request is compared against — or nil before
// the first request/header snapshot. Each header event folds once, so a
// per-step read costs O(new events).
func (s *Session) RequestHeader() *EpochHeader {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.headerFoldSeq < len(s.log) {
		s.headerFold = FoldRequestHeader(s.log[s.headerFoldSeq:], s.headerFold)
		s.headerFoldSeq = len(s.log)
	}
	return s.headerFold
}

// RequestContext returns the latest resolved route metadata, or nil before
// the first request/context event.
func (s *Session) RequestContext() *RequestContext {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contextFoldSeq < len(s.log) {
		for _, event := range s.log[s.contextFoldSeq:] {
			if event.Type != EventRequestCtx {
				continue
			}
			var decoded RequestContext
			if err := json.Unmarshal(event.Data, &decoded); err == nil {
				s.contextFold = &decoded
			}
		}
		s.contextFoldSeq = len(s.log)
	}
	return s.contextFold
}

// DeriveMessages folds the projection rules over the live surface: the
// single source of derived history. Cached — each surface node projects
// exactly once when first seen, and a surface rewrite (a replace) rebuilds.
// The returned slice is a fresh snapshot per call.
func (s *Session) DeriveMessages() []llm.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	generation := s.surface.ReplaceGeneration()
	if generation != s.derivedGeneration {
		s.derived = nil
		s.derivedNodes = 0
		s.derivedGeneration = generation
	}
	nodes := s.surface.Nodes()
	for _, seq := range nodes[s.derivedNodes:] {
		if seq < 0 || seq >= int64(len(s.log)) {
			continue
		}
		if msg := DeriveEventMessage(s.log[seq]); msg != nil {
			s.derived = append(s.derived, *msg)
		}
	}
	s.derivedNodes = len(nodes)
	return append([]llm.Message(nil), s.derived...)
}

// DeriveEventMessage projects one event into the LLM message it derives to,
// or nil when it produces none — a non-surface event, or an empty-content
// assistant/message (which exists only to host usage).
func DeriveEventMessage(event Event) *llm.Message {
	switch event.Type {
	case EventUserMessage:
		message, err := DecodeUserMessage(event)
		if err != nil {
			return nil
		}
		return &message
	case EventAssistantMsg:
		data, err := DecodeAssistantMessage(event)
		if err != nil || len(data.Message.Content) == 0 {
			return nil
		}
		return &data.Message
	case EventToolResult:
		data, err := DecodeToolResult(event)
		if err != nil {
			return nil
		}
		return &data.Message
	default:
		return nil
	}
}
