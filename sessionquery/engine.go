package sessionquery

import (
	"context"

	session "dshgo/session"
	"dshgo/session/persistence"
)

// SearchBackend is the abstract full-text surface: the only part of the
// query service owned by a mounted backend (the released implementation is
// session-query-sqlite). Ranking, reconciliation, cursor generations, and
// query execution are backend-owned.
type SearchBackend interface {
	// SearchSessions searches the live-preferred logical corpus and groups
	// by session.
	SearchSessions(ctx context.Context, request SessionSearchRequest) (*SessionSearchPage[SessionSearchHit], error)
	// SearchEvents searches events within one live-preferred logical
	// session.
	SearchEvents(ctx context.Context, request SessionEventSearchRequest) (*SessionEventSearchPage, error)
}

// Engine is the unified live-preferred session query service. Exact reads,
// filters, and traces are backend-independent concrete behavior; a nil
// backend leaves full-text search disabled.
type Engine struct {
	readWindowMax int
	corpus        *SessionCorpus
	observations  *SessionObservationReader
	search        SearchBackend
}

// NewEngine validates the configuration and builds the engine over the
// given seams. A nil Sessions or corpus panics at use; construction fails
// loud on invalid configuration.
func NewEngine(sessions Sessions, persistenceCoordinator *persistence.Coordinator, projections ProjectionSource, backend SearchBackend, config *Config) (*Engine, error) {
	readWindowMax := SESSION_QUERY_READ_WINDOW_MAX
	if config != nil && config.ReadWindowMax != nil {
		readWindowMax = *config.ReadWindowMax
	}
	if readWindowMax < 0 {
		return nil, queryError(CodeInvalidConfig, "readWindowMax must be a non-negative integer")
	}
	persistedInspectConcurrency := SESSION_QUERY_DEFAULT_PERSISTED_INSPECT_CONCURRENCY
	if config != nil && config.PersistedInspectConcurrency != nil {
		persistedInspectConcurrency = *config.PersistedInspectConcurrency
	}
	if persistedInspectConcurrency < 1 {
		return nil, queryError(CodeInvalidConfig, "persistedInspectConcurrency must be a positive safe integer")
	}
	return &Engine{
		readWindowMax: readWindowMax,
		corpus:        NewSessionCorpus(sessions, persistenceCoordinator, persistedInspectConcurrency),
		observations:  NewSessionObservationReader(sessions, persistenceCoordinator, projections),
		search:        backend,
	}, nil
}

// ReadWindowMax reports the configured maximum read context.
func (e *Engine) ReadWindowMax() int { return e.readWindowMax }

// ObserveSession observes one exact live or prepared Session without a
// persistence listing preflight. The caller owns the observation lease.
func (e *Engine) ObserveSession(ctx context.Context, sessionID session.SessionID, options SessionObservationOptions) (*SessionObservation, error) {
	return e.observations.Read(ctx, sessionID, options)
}

// SearchSessions searches the live-preferred logical corpus and groups by
// session.
func (e *Engine) SearchSessions(ctx context.Context, request SessionSearchRequest) (*SessionSearchPage[SessionSearchHit], error) {
	if e.search == nil {
		return nil, queryError(CodeSearchDisabled, "full-text search requires a mounted backend")
	}
	return e.search.SearchSessions(ctx, request)
}

// SearchEvents searches events within one live-preferred logical session.
func (e *Engine) SearchEvents(ctx context.Context, request SessionEventSearchRequest) (*SessionEventSearchPage, error) {
	if e.search == nil {
		return nil, queryError(CodeSearchDisabled, "full-text search requires a mounted backend")
	}
	return e.search.SearchEvents(ctx, request)
}

// ListSessions lists the complete logical corpus using live-preferred
// records: deterministic newest-first cloned session records.
func (e *Engine) ListSessions(ctx context.Context) ([]SessionRecord, error) {
	return e.corpus.ListSessions(ctx)
}

// ReadSession reads and replay-validates one complete logical session log
// without making it live.
func (e *Engine) ReadSession(ctx context.Context, sessionID session.SessionID) (SessionLogSnapshot, error) {
	loaded, err := e.corpus.Load(ctx, sessionID)
	if err != nil {
		return SessionLogSnapshot{}, err
	}
	if _, err := session.NewRestored(sessionID, loaded.Events, loaded.Header); err != nil {
		return SessionLogSnapshot{}, err
	}
	return SessionLogSnapshot{
		Session: loaded.Header,
		Events:  loaded.Events,
	}, nil
}

// FilterSessions filters the complete logical corpus with
// provider-independent predicates: matching cloned records in
// deterministic newest-first order.
func (e *Engine) FilterSessions(ctx context.Context, filters []SessionResultFilter) ([]SessionRecord, error) {
	owned, err := MaterializeSessionResultFilters(filters)
	if err != nil {
		return nil, err
	}
	records, err := e.corpus.ListSessions(ctx)
	if err != nil {
		return nil, err
	}
	return FilterSessionResults(records, owned)
}

// ReadTitle folds the latest log-backed title from one live-preferred
// logical session: the latest title snapshot, or nil when the log has no
// title event.
func (e *Engine) ReadTitle(ctx context.Context, sessionID session.SessionID) (*SessionTitleSnapshot, error) {
	observation, err := e.ReadTitleSnapshot(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return observation.Title, nil
}

// ReadTitleSnapshot folds the latest title and returns its source header
// from one corpus observation.
func (e *Engine) ReadTitleSnapshot(ctx context.Context, sessionID session.SessionID) (SessionTitleObservation, error) {
	results, err := e.ReadTitleSnapshots(ctx, []session.SessionID{sessionID})
	if err != nil {
		return SessionTitleObservation{}, err
	}
	result := results[0]
	if !result.Fulfilled {
		return SessionTitleObservation{}, result.Reason
	}
	return *result.Value, nil
}

// ReadTitleSnapshots folds titles for unique sessions from one cancellable
// corpus observation. Results preserve first-occurrence input order;
// operational failures stay isolated per session while cancellation
// rejects the complete operation.
func (e *Engine) ReadTitleSnapshots(ctx context.Context, sessionIDs []session.SessionID) ([]SessionTitleObservationResult, error) {
	projected, err := ProjectMany(ctx, e.corpus, sessionIDs, func(source LogicalSessionSource) (SessionTitleObservation, error) {
		title := FoldSessionTitle(source.Events)
		return SessionTitleObservation{Session: source.Header, Title: title}, nil
	})
	if err != nil {
		return nil, err
	}
	results := make([]SessionTitleObservationResult, 0, len(projected))
	for _, result := range projected {
		if result.Fulfilled {
			value := result.Value
			results = append(results, SessionTitleObservationResult{SessionID: result.SessionID, Fulfilled: true, Value: &value})
			continue
		}
		results = append(results, SessionTitleObservationResult{SessionID: result.SessionID, Fulfilled: false, Reason: result.Reason})
	}
	return results, nil
}

// ListEvents lists lightweight raw-log event records for one logical
// session in ascending seq order.
func (e *Engine) ListEvents(ctx context.Context, sessionID session.SessionID) ([]SessionEventRecord, error) {
	loaded, err := e.corpus.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return EventRecords(sessionID, loaded.Events)
}

// FilterEvents scans first-party semantic event documents with
// provider-independent filters: matching semantic documents in ascending
// seq order.
func (e *Engine) FilterEvents(ctx context.Context, sessionID session.SessionID, filters []SessionEventResultFilter) ([]SessionEventSearchDocument, error) {
	owned, err := MaterializeSessionEventResultFilters(filters)
	if err != nil {
		return nil, err
	}
	loaded, err := e.corpus.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	documents, err := BuildSessionEventSearchDocuments(sessionID, loaded.Events)
	if err != nil {
		return nil, err
	}
	return FilterSessionEventDocuments(documents, owned)
}

// ReadSurface reads one session's complete current model surface from one
// corpus observation: cloned header, current surface, and the last
// sequence number included in the raw-log capture.
func (e *Engine) ReadSurface(ctx context.Context, sessionID session.SessionID) (SessionSurfaceSnapshot, error) {
	loaded, err := e.corpus.Load(ctx, sessionID)
	if err != nil {
		return SessionSurfaceSnapshot{}, err
	}
	events, err := CurrentSurfaceEvents(sessionID, loaded.Events)
	if err != nil {
		return SessionSurfaceSnapshot{}, err
	}
	var capturedThrough *int64
	if len(loaded.Events) > 0 {
		seq := loaded.Events[len(loaded.Events)-1].Seq
		capturedThrough = &seq
	}
	return SessionSurfaceSnapshot{
		Session:            loaded.Header,
		CapturedThroughSeq: capturedThrough,
		Events:             events,
	}, nil
}

// TraceSession traces known ancestry and descendants from one corpus
// observation: a complete lineage or the first parent that could not be
// resolved.
func (e *Engine) TraceSession(ctx context.Context, sessionID session.SessionID) (SessionLineageTrace, error) {
	records, err := e.corpus.ListSessions(ctx)
	if err != nil {
		return SessionLineageTrace{}, err
	}
	if err := ctxEnsureLive(ctx); err != nil {
		return SessionLineageTrace{}, err
	}
	return TraceSession(records, sessionID)
}

// TraceEvent traces one event's direct positional replacements and cited
// source events: source header, direct links, and the target's positional
// replacement chain.
func (e *Engine) TraceEvent(ctx context.Context, request SessionEventTraceRequest) (SessionEventTraceObservation, error) {
	loaded, err := e.corpus.Load(ctx, request.SessionID)
	if err != nil {
		return SessionEventTraceObservation{}, err
	}
	if err := ctxEnsureLive(ctx); err != nil {
		return SessionEventTraceObservation{}, err
	}
	trace, err := TraceEvent(request.SessionID, loaded.Events, request.Seq)
	if err != nil {
		return SessionEventTraceObservation{}, err
	}
	return SessionEventTraceObservation{SessionEventTrace: trace, Session: loaded.Header}, nil
}

// ReadEvent reads one full event plus a bounded raw-log context window of
// cloned target and neighboring events.
func (e *Engine) ReadEvent(ctx context.Context, request SessionEventReadRequest) (SessionEventWindow, error) {
	before, err := e.readWindow("before", request.Before)
	if err != nil {
		return SessionEventWindow{}, err
	}
	after, err := e.readWindow("after", request.After)
	if err != nil {
		return SessionEventWindow{}, err
	}
	loaded, err := e.corpus.Load(ctx, request.SessionID)
	if err != nil {
		return SessionEventWindow{}, err
	}
	if err := ctxEnsureLive(ctx); err != nil {
		return SessionEventWindow{}, err
	}
	seq := request.Seq
	if seq < 0 || seq >= int64(len(loaded.Events)) || loaded.Events[seq].Seq != seq {
		return SessionEventWindow{}, queryError(CodeEventNotFound, "session %q has no event at seq %d", request.SessionID, seq)
	}
	target := loaded.Events[seq]
	startSeq := seq - int64(before)
	if startSeq < 0 {
		startSeq = 0
	}
	endSeq := seq + int64(after)
	if endSeq > int64(len(loaded.Events))-1 {
		endSeq = int64(len(loaded.Events)) - 1
	}
	return SessionEventWindow{
		Session:  loaded.Header,
		Target:   target,
		Events:   append([]session.Event(nil), loaded.Events[startSeq:endSeq+1]...),
		StartSeq: startSeq,
		EndSeq:   endSeq,
	}, nil
}

func (e *Engine) readWindow(name string, value *int) (int, error) {
	if value == nil {
		return 0, nil
	}
	if *value < 0 || *value > e.readWindowMax {
		return 0, queryError(CodeInvalidWindow, "%s must be an integer between 0 and %d", name, e.readWindowMax)
	}
	return *value, nil
}
