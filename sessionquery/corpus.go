package sessionquery

import (
	"context"
	"errors"
	"sort"
	"sync"

	"dshgo/session"
	"dshgo/session/persistence"
)

// Sessions is the live-session registry seam (official ctx.sessions).
type Sessions interface {
	// Get resolves one live session by id.
	Get(id session.SessionID) (*session.Session, bool)
	// List enumerates live sessions.
	List() []*session.Session
}

// StoreSessions adapts *session.Store to the Sessions seam.
type StoreSessions struct {
	Store *session.Store
}

// Get resolves one live session by id.
func (s StoreSessions) Get(id session.SessionID) (*session.Session, bool) {
	sess := s.Store.Get(id)
	return sess, sess != nil
}

// List enumerates live sessions.
func (s StoreSessions) List() []*session.Session {
	ids := s.Store.List()
	out := make([]*session.Session, 0, len(ids))
	for _, id := range ids {
		if sess := s.Store.Get(id); sess != nil {
			out = append(out, sess)
		}
	}
	return out
}

// LogicalSession is the detached source selected for one exact read.
type LogicalSession struct {
	// Header is the cloned source header.
	Header session.SessionHeader
	// Events is the cloned raw event log.
	Events []session.Event
}

// LogicalSessionSource is the borrowed source visible only during one
// synchronous batch projection; callers must clone retained output.
type LogicalSessionSource struct {
	// Header is selected with Events.
	Header session.SessionHeader
	// Events is the raw event log, valid only for the projection call.
	Events []session.Event
}

// ProjectionResult is one source-projection result in a batch
// logical-corpus observation.
type ProjectionResult[T any] struct {
	// SessionID is the projected id.
	SessionID session.SessionID
	// Fulfilled reports a successful projection; false isolates this
	// session's failure in Reason.
	Fulfilled bool
	// Value is the projected value, set when Fulfilled.
	Value T
	// Reason is the per-session failure, set when !Fulfilled.
	Reason error
}

// SessionCorpus resolves a live-preferred corpus against the persistence
// coordinator mounted at construction (nil = no persistence backend).
type SessionCorpus struct {
	sessions    Sessions
	persistence *persistence.Coordinator
	concurrency int
}

// NewSessionCorpus resolves a corpus against the given registry and
// optional persistence coordinator.
func NewSessionCorpus(sessions Sessions, persistence *persistence.Coordinator, persistedInspectConcurrency int) *SessionCorpus {
	return &SessionCorpus{
		sessions:    sessions,
		persistence: persistence,
		concurrency: persistedInspectConcurrency,
	}
}

// ListSessions lists the complete logical corpus with live precedence and
// cloned headers, in deterministic newest-first order.
func (c *SessionCorpus) ListSessions(ctx context.Context) ([]SessionRecord, error) {
	if err := ctxEnsureLive(ctx); err != nil {
		return nil, err
	}
	var persisted []session.SessionHeader
	if c.persistence != nil {
		listed, err := listPersisted(c.persistence)
		if err != nil {
			return nil, err
		}
		persisted = listed
	}
	records := make(map[session.SessionID]SessionRecord, len(persisted))
	for _, header := range persisted {
		records[header.ID] = SessionRecord{Header: header, Live: false, Persisted: true}
	}
	for _, live := range c.sessions.List() {
		header := live.Header()
		durable, ok := records[header.ID]
		if ok {
			if err := AssertSessionHeadersCompatible(header, durable.Header); err != nil {
				return nil, err
			}
		}
		records[header.ID] = SessionRecord{Header: header, Live: true, Persisted: ok && durable.Persisted}
	}
	out := make([]SessionRecord, 0, len(records))
	for _, record := range records {
		out = append(out, record)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Header.CreatedAt != out[j].Header.CreatedAt {
			return out[i].Header.CreatedAt > out[j].Header.CreatedAt
		}
		// Official ordering is localeCompare; the Go corpus compares
		// byte-wise (recorded divergence).
		return out[i].Header.ID < out[j].Header.ID
	})
	return out, nil
}

// Load resolves one logical source, preferring a detached live snapshot. A
// known live target never consults persistence, so an optional backend's
// failure cannot make current in-memory history unreadable.
func (c *SessionCorpus) Load(ctx context.Context, sessionID session.SessionID) (LogicalSession, error) {
	if err := ctxEnsureLive(ctx); err != nil {
		return LogicalSession{}, err
	}
	if live, ok := c.sessions.Get(sessionID); ok {
		return snapshotLive(live), nil
	}
	if c.persistence == nil {
		return LogicalSession{}, sessionNotFound(sessionID)
	}
	listed, err := listPersisted(c.persistence)
	if err != nil {
		return LogicalSession{}, err
	}
	var listedHeader *session.SessionHeader
	for _, header := range listed {
		if header.ID == sessionID {
			headerCopy := header
			listedHeader = &headerCopy
			break
		}
	}
	if listedHeader == nil {
		return LogicalSession{}, sessionNotFound(sessionID)
	}
	loaded, err := inspectPersisted(c.persistence, sessionID)
	if err != nil {
		return LogicalSession{}, err
	}
	if err := ctxEnsureLive(ctx); err != nil {
		return LogicalSession{}, err
	}
	if attached, ok := c.sessions.Get(sessionID); ok {
		return snapshotLive(attached), nil
	}
	if err := AssertSessionHeadersCompatible(loaded.Meta, *listedHeader); err != nil {
		return LogicalSession{}, err
	}
	return LogicalSession{Header: loaded.Meta, Events: loaded.Events}, nil
}

// ProjectMany projects unique logical sources immediately from one
// persistence listing. The synchronous projector runs before a persisted
// worker claims its next id; full logs are borrowed only for that call.
// Results preserve first-occurrence input order; per-id failures stay
// isolated while context cancellation rejects the complete operation.
func ProjectMany[T any](ctx context.Context, corpus *SessionCorpus, sessionIDs []session.SessionID, project func(LogicalSessionSource) (T, error)) ([]ProjectionResult[T], error) {
	ids := uniqueIDs(sessionIDs)
	if err := ctxEnsureLive(ctx); err != nil {
		return nil, err
	}
	resolved := make(map[session.SessionID]ProjectionResult[T], len(ids))
	var unresolved []session.SessionID
	for _, id := range ids {
		if live, ok := corpus.sessions.Get(id); ok {
			resolved[id] = projectSource(ctx, id, LogicalSessionSource{Header: live.Header(), Events: live.Events()}, project)
			continue
		}
		unresolved = append(unresolved, id)
	}
	if len(unresolved) == 0 {
		return orderedResults(ids, resolved), nil
	}
	if corpus.persistence == nil {
		for _, id := range unresolved {
			resolved[id] = ProjectionResult[T]{SessionID: id, Fulfilled: false, Reason: sessionNotFound(id)}
		}
		return orderedResults(ids, resolved), nil
	}
	persisted, err := listPersisted(corpus.persistence)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, queryErrorCause(CodeAborted, ctxErr, "session observation was aborted")
		}
		for _, id := range unresolved {
			resolved[id] = ProjectionResult[T]{SessionID: id, Fulfilled: false, Reason: err}
		}
		return orderedResults(ids, resolved), nil
	}
	persistedByID := make(map[session.SessionID]session.SessionHeader, len(persisted))
	for _, header := range persisted {
		persistedByID[header.ID] = header
	}
	var cursorMu sync.Mutex
	cursor := 0
	settled := make([]ProjectionResult[T], len(unresolved))
	workerErr := error(nil)
	worker := func() {
		for {
			if err := ctx.Err(); err != nil {
				workerErr = err
				return
			}
			cursorMu.Lock()
			index := cursor
			cursor++
			cursorMu.Unlock()
			if index >= len(unresolved) {
				return
			}
			// Each worker writes its own slot; the map merge happens after
			// the barrier.
			settled[index] = resolvePersisted(ctx, corpus, persistedByID, unresolved[index], project)
		}
	}
	workerCount := corpus.concurrency
	if workerCount > len(unresolved) {
		workerCount = len(unresolved)
	}
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker()
		}()
	}
	wg.Wait()
	if workerErr != nil {
		return nil, queryErrorCause(CodeAborted, workerErr, "session observation was aborted")
	}
	if err := ctxEnsureLive(ctx); err != nil {
		return nil, err
	}
	for index, result := range settled {
		resolved[unresolved[index]] = result
	}
	return orderedResults(ids, resolved), nil
}

func resolvePersisted[T any](ctx context.Context, corpus *SessionCorpus, persistedByID map[session.SessionID]session.SessionHeader, sessionID session.SessionID, project func(LogicalSessionSource) (T, error)) ProjectionResult[T] {
	listed, wasListed := persistedByID[sessionID]
	if !wasListed {
		if attached, ok := corpus.sessions.Get(sessionID); ok {
			return projectSource(ctx, sessionID, LogicalSessionSource{Header: attached.Header(), Events: attached.Events()}, project)
		}
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: sessionNotFound(sessionID)}
	}
	loaded, err := inspectPersisted(corpus.persistence, sessionID)
	if err != nil {
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: err}
	}
	if err := ctx.Err(); err != nil {
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: err}
	}
	if attached, ok := corpus.sessions.Get(sessionID); ok {
		return projectSource(ctx, sessionID, LogicalSessionSource{Header: attached.Header(), Events: attached.Events()}, project)
	}
	if err := AssertSessionHeadersCompatible(loaded.Meta, listed); err != nil {
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: err}
	}
	return projectSource(ctx, sessionID, LogicalSessionSource{Header: loaded.Meta, Events: loaded.Events}, project)
}

func projectSource[T any](ctx context.Context, sessionID session.SessionID, source LogicalSessionSource, project func(LogicalSessionSource) (T, error)) ProjectionResult[T] {
	if err := ctx.Err(); err != nil {
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: err}
	}
	value, err := project(source)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: ctxErr}
		}
		return ProjectionResult[T]{SessionID: sessionID, Fulfilled: false, Reason: err}
	}
	return ProjectionResult[T]{SessionID: sessionID, Fulfilled: true, Value: value}
}

func orderedResults[T any](ids []session.SessionID, resolved map[session.SessionID]ProjectionResult[T]) []ProjectionResult[T] {
	out := make([]ProjectionResult[T], 0, len(ids))
	for _, id := range ids {
		out = append(out, resolved[id])
	}
	return out
}

func uniqueIDs(ids []session.SessionID) []session.SessionID {
	seen := make(map[session.SessionID]bool, len(ids))
	out := make([]session.SessionID, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func listPersisted(coordinator *persistence.Coordinator) ([]session.SessionHeader, error) {
	listed, err := coordinator.List()
	if err != nil {
		return nil, queryErrorCause(CodePersistenceFailed, err, "session persistence listing failed: %v", err)
	}
	return listed, nil
}

func inspectPersisted(coordinator *persistence.Coordinator, sessionID session.SessionID) (persistence.Inspection, error) {
	loaded, err := coordinator.Inspect(sessionID)
	if err != nil {
		corrupt := &persistence.CorruptionError{}
		if errors.As(err, &corrupt) {
			return persistence.Inspection{}, queryErrorCause(CodeCorruptSession, err, "stored session %q is corrupt: %v", sessionID, err)
		}
		return persistence.Inspection{}, queryErrorCause(CodePersistenceFailed, err, "failed to inspect session %q: %v", sessionID, err)
	}
	return loaded, nil
}

func snapshotLive(live *session.Session) LogicalSession {
	events := live.Events()
	return LogicalSession{Header: live.Header(), Events: append([]session.Event(nil), events...)}
}

func sessionNotFound(sessionID session.SessionID) error {
	return queryError(CodeSessionNotFound, "session %q not found", sessionID)
}

func ctxEnsureLive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return queryErrorCause(CodeAborted, err, "session observation was aborted")
	}
	return nil
}
