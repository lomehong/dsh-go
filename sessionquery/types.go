package sessionquery

import (
	session "dshgo/session"
)

// SessionEventSurface classifies an event's placement in the folded session
// surface: current model context, replaced context, or raw-log-only.
const (
	SurfaceCurrent  = "current"
	SurfaceShadowed = "shadowed"
	SurfaceLogOnly  = "log-only"
)

// SessionAvailability values understood by logical-session filters.
const (
	AvailabilityLive      = "live"
	AvailabilityPersisted = "persisted"
)

// SessionRecord is lightweight identity and source availability for one
// logical session.
type SessionRecord struct {
	// Header is the cloned session header selected from the live-preferred
	// corpus.
	Header session.SessionHeader
	// Live reports whether the id currently exists in the live registry.
	Live bool
	// Persisted reports whether the active persistence backend currently
	// materializes the id.
	Persisted bool
}

// SessionSurfaceSnapshot is one atomic live-preferred observation of a
// session's current model surface.
type SessionSurfaceSnapshot struct {
	// Session is the cloned header selected from the same corpus observation
	// as Events.
	Session session.SessionHeader
	// CapturedThroughSeq is the highest raw-log seq included in the
	// observation, nil for an empty log.
	CapturedThroughSeq *int64
	// Events are the cloned current surface events in model-history order.
	Events []session.Event
}

// SessionLogSnapshot is one validated detached observation of a logical
// session's complete raw log.
type SessionLogSnapshot struct {
	// Session is the cloned header selected from the same observation as
	// Events.
	Session session.SessionHeader
	// Events are the cloned contiguous raw events after replay validation.
	Events []session.Event
}

// SessionEventRecord is lightweight metadata for one event within a logical
// session.
type SessionEventRecord struct {
	// SessionID owns the event.
	SessionID session.SessionID
	// Seq is the monotonic event seq within the session.
	Seq int64
	// Type is the session-event discriminant.
	Type string
	// Time is the event timestamp in Unix epoch milliseconds.
	Time int64
	// Surface is the event placement in the folded session surface.
	Surface string
}

// SessionLineageNode is one recursive descendant in a session-lineage trace.
type SessionLineageNode struct {
	// Session is the detached logical-corpus record for this descendant.
	Session SessionRecord
	// Descendants are the direct children, each carrying its own recursive
	// descendants.
	Descendants []SessionLineageNode
}

// SessionLineageTrace is one session's known ancestry and descendants. With
// Complete set, Root is the top of the complete parent chain; otherwise
// UnresolvedParentID names the first parent id absent from the logical
// corpus.
type SessionLineageTrace struct {
	// Target is the detached record for the traced session.
	Target SessionRecord
	// Ancestors are the known parents from the immediate parent outward.
	Ancestors []SessionRecord
	// Descendants are the complete known descendant trees rooted at the
	// target's direct children.
	Descendants []SessionLineageNode
	// Complete reports whether the complete parent chain is present in the
	// logical corpus.
	Complete bool
	// Root is the detached record at the top of the complete lineage.
	Root *SessionRecord
	// UnresolvedParentID is the first parent id not present in the logical
	// corpus.
	UnresolvedParentID session.SessionID
}

// SessionEventTraceRequest targets one event by session id and seq.
type SessionEventTraceRequest struct {
	// SessionID owns the target event.
	SessionID session.SessionID
	// Seq is the target event seq.
	Seq int64
}

// SessionEventTrace carries one event's direct positional replacements and
// relationships to cited source events.
type SessionEventTrace struct {
	// Target is the lightweight target record.
	Target SessionEventRecord
	// ReplacedBy is the immediate positional replacement event, set only
	// when the target was shadowed.
	ReplacedBy *int64
	// ReplacementChain lists positional replacers from the immediate
	// replacement to the final replacement.
	ReplacementChain []int64
	// ReplacedEventSeqs lists surface nodes directly removed when the target
	// itself performed a replacement.
	ReplacedEventSeqs []int64
	// SourceEventSeqs lists earlier events cited directly as sources, in
	// their recorded order.
	SourceEventSeqs []int64
	// DerivedEventSeqs lists later events that directly cite the target as a
	// source, in log order.
	DerivedEventSeqs []int64
}

// SessionEventTraceObservation binds event relationships to the same
// session-header observation.
type SessionEventTraceObservation struct {
	SessionEventTrace
	// Session is the cloned header selected with the event log used for the
	// trace.
	Session session.SessionHeader
}

// SessionEventReadRequest asks for one event plus raw neighboring log
// context.
type SessionEventReadRequest struct {
	// SessionID owns the target event.
	SessionID session.SessionID
	// Seq is the target event seq.
	Seq int64
	// Before is the number of preceding raw events to include.
	Before *int
	// After is the number of following raw events to include.
	After *int
}

// SessionEventWindow is a full target event plus a bounded raw-log window.
type SessionEventWindow struct {
	// Session is the cloned header for the live-preferred source read.
	Session session.SessionHeader
	// Target is the full cloned target event.
	Target session.Event
	// Events are the full cloned events from StartSeq through EndSeq.
	Events []session.Event
	// StartSeq is the first seq included in Events.
	StartSeq int64
	// EndSeq is the last seq included in Events.
	EndSeq int64
}

// SessionTitleObservation is the latest folded title bound to the same
// session-header observation.
type SessionTitleObservation struct {
	// Session is the cloned header selected with the event log used for the
	// title fold.
	Session session.SessionHeader
	// Title is the latest title snapshot, nil when the observed log has no
	// title.
	Title *SessionTitleSnapshot
}

// SessionTitleObservationResult is one ordered result from a batch title
// observation.
type SessionTitleObservationResult struct {
	// SessionID is the requested session id.
	SessionID session.SessionID
	// Fulfilled reports an atomic header/title observation; false isolates
	// the operational failure for this session in Reason.
	Fulfilled bool
	// Value is the successful observation, set when Fulfilled.
	Value *SessionTitleObservation
	// Reason is the original failure from source resolution or title
	// folding, set when !Fulfilled.
	Reason error
}

// SessionResultRange is an inclusive numeric interval used by time and
// sequence filters. Nil bounds are open.
type SessionResultRange struct {
	// From is the inclusive lower bound.
	From *float64
	// To is the inclusive upper bound.
	To *float64
}

// SessionResultFilter is one logical-session predicate. A filter array is
// ANDed; Values within one clause are ORed. Kind selects the active fields:
// id/cwd/parent use Values (cwd and parent allow nil entries), created-at
// uses From/To, availability uses AvailabilityValues.
type SessionResultFilter struct {
	Kind string
	// Values holds id/availability values, or cwd/parent values.
	Values []string
	// NullableValues holds cwd/parent values where nil matches an absent
	// field.
	NullableValues []*string
	// From/To bound the created-at range.
	From, To *float64
}

// SessionEventResultFilter is one event predicate. Kind selects the active
// fields: seq/time use From/To, type/surface use Values, text uses Text.
// Text is a literal, case-insensitive, whitespace-flexible semantic-text
// scan.
type SessionEventResultFilter struct {
	Kind string
	// Values holds type or surface values.
	Values []string
	// Text is the literal text scanned for by the text clause.
	Text string
	// From/To bound the seq or time range.
	From, To *float64
}

// SessionEventMetadataFilter is an event predicate a full-text provider can
// apply before relevance ranking — SessionEventResultFilter without text.
type SessionEventMetadataFilter = SessionEventResultFilter

// SessionEventSearchDocument is a searchable semantic document derived from
// one session event.
type SessionEventSearchDocument struct {
	SessionEventRecord
	// Text is the first-party semantic text used by scan filters and
	// full-text indexes.
	Text string
}

// SessionSearchCursor is the provider-owned opaque continuation token
// returned by session search.
type SessionSearchCursor = string

// SessionSearchPage is one cursor-paginated result page.
type SessionSearchPage[T any] struct {
	// Items are the results for this page in contract-defined order.
	Items []T
	// NextCursor is the opaque continuation cursor, nil on the final page.
	NextCursor *SessionSearchCursor
}

// SessionEventSearchHit is one event full-text search hit with a bounded
// plain-text excerpt.
type SessionEventSearchHit struct {
	SessionEventRecord
	// Snippet is the plain text excerpt selected around the match.
	Snippet string
}

// SessionEventSearchPage binds event-search results to the indexed
// target-session observation.
type SessionEventSearchPage struct {
	// Items are the matching event hits from one indexed generation.
	Items []SessionEventSearchHit
	// NextCursor is the opaque continuation cursor, nil on the final page.
	NextCursor *SessionSearchCursor
	// Session is the cloned target header from the same indexed generation
	// as Items.
	Session session.SessionHeader
}

// SessionSearchHit is one grouped cross-session hit, ranked by its
// strongest matching event.
type SessionSearchHit struct {
	SessionRecord
	// BestMatch is the strongest matching event for this session.
	BestMatch SessionEventSearchHit
}

// SessionSearchRequest is a cross-session full-text search request. The
// query is interpreted as data, never executable search syntax.
type SessionSearchRequest struct {
	// Query is the full-text query.
	Query string
	// SessionFilters are logical-session predicates applied before event
	// ranking.
	SessionFilters []SessionResultFilter
	// EventFilters are event predicates applied before event ranking.
	EventFilters []SessionEventMetadataFilter
	// Limit is the maximum sessions in this page.
	Limit *int
	// Cursor continues a previous identical normalized request.
	Cursor SessionSearchCursor
}

// SessionEventSearchRequest is a within-session full-text search request.
type SessionEventSearchRequest struct {
	// SessionID is the live-preferred logical session to search.
	SessionID session.SessionID
	// Query is the full-text query.
	Query string
	// EventFilters are event predicates applied before ranking.
	EventFilters []SessionEventMetadataFilter
	// Limit is the maximum events in this page.
	Limit *int
	// Cursor continues a previous identical normalized request.
	Cursor SessionSearchCursor
}
