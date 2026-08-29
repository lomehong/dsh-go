// Package sessionquery re-implements @deepseek-ai/dsh-session-query
// (official tag dsh-v0.1.2-alpha.1): the unified live-preferred query
// service over session logs — exact reads, filters, traces, and detached
// projections, with full-text search left to a mounted backend.
//
// Go adaptation: cordis context injection becomes construction-time seams
// (a live-session registry, an optional *persistence.Coordinator, optional
// projection seams); AbortSignal becomes context.Context with
// SESSION_QUERY_ABORTED on a done context. Returned events are detached
// value copies; event payloads are immutable raw JSON owned by the log, so
// sharing their bytes preserves the no-live-state contract.
package sessionquery

import "fmt"

// SESSION_QUERY_READ_WINDOW_MAX is the default maximum before/after
// raw-event window accepted by ReadEvent.
const SESSION_QUERY_READ_WINDOW_MAX = 50

// SESSION_QUERY_DEFAULT_PERSISTED_INSPECT_CONCURRENCY is the default
// maximum concurrent persisted-log inspections in one batch read.
const SESSION_QUERY_DEFAULT_PERSISTED_INSPECT_CONCURRENCY = 4

// Config is backend-independent configuration inherited by every
// session-query implementation. Nil fields take the defaults above.
type Config struct {
	// ReadWindowMax is the maximum accepted raw read context on either
	// side. Must be a non-negative integer.
	ReadWindowMax *int
	// PersistedInspectConcurrency is the maximum concurrent persisted-log
	// inspections in one batch read. Must be a positive safe integer.
	PersistedInspectConcurrency *int
}

// SessionQueryErrorCode is one member of the stable machine-routable
// failure taxonomy for session reads, traces, and search.
type SessionQueryErrorCode = string

// The closed SessionQueryErrorCode taxonomy.
const (
	CodeAborted           SessionQueryErrorCode = "SESSION_QUERY_ABORTED"
	CodeCorruptSession    SessionQueryErrorCode = "SESSION_QUERY_CORRUPT_SESSION"
	CodeEventNotFound     SessionQueryErrorCode = "SESSION_QUERY_EVENT_NOT_FOUND"
	CodeIndexFailed       SessionQueryErrorCode = "SESSION_QUERY_INDEX_FAILED"
	CodeInvalidConfig     SessionQueryErrorCode = "SESSION_QUERY_INVALID_CONFIG"
	CodeInvalidCursor     SessionQueryErrorCode = "SESSION_QUERY_INVALID_CURSOR"
	CodeInvalidFilter     SessionQueryErrorCode = "SESSION_QUERY_INVALID_FILTER"
	CodeInvalidLimit      SessionQueryErrorCode = "SESSION_QUERY_INVALID_LIMIT"
	CodeInvalidQuery      SessionQueryErrorCode = "SESSION_QUERY_INVALID_QUERY"
	CodeInvalidLineage    SessionQueryErrorCode = "SESSION_QUERY_INVALID_LINEAGE"
	CodeInvalidSurface    SessionQueryErrorCode = "SESSION_QUERY_INVALID_SURFACE"
	CodeInvalidWindow     SessionQueryErrorCode = "SESSION_QUERY_INVALID_WINDOW"
	CodePersistenceFailed SessionQueryErrorCode = "SESSION_QUERY_PERSISTENCE_FAILED"
	CodeSearchDisabled    SessionQueryErrorCode = "SESSION_QUERY_SEARCH_DISABLED"
	CodeSessionNotFound   SessionQueryErrorCode = "SESSION_QUERY_SESSION_NOT_FOUND"
	CodeStaleCursor       SessionQueryErrorCode = "SESSION_QUERY_STALE_CURSOR"
	CodeSourceConflict    SessionQueryErrorCode = "SESSION_QUERY_SOURCE_CONFLICT"
)

// SessionQueryError is the typed session-query failure whose Code is one
// closed taxonomy member.
type SessionQueryError struct {
	Message string
	Code    SessionQueryErrorCode
	Cause   error
}

// Error renders the failure message.
func (e *SessionQueryError) Error() string { return e.Message }

// Unwrap exposes the wrapped cause for errors.Is/As.
func (e *SessionQueryError) Unwrap() error { return e.Cause }

func queryError(code SessionQueryErrorCode, format string, args ...any) *SessionQueryError {
	return &SessionQueryError{Message: "session-query: " + fmt.Sprintf(format, args...), Code: code}
}

func queryErrorCause(code SessionQueryErrorCode, cause error, format string, args ...any) *SessionQueryError {
	return &SessionQueryError{Message: "session-query: " + fmt.Sprintf(format, args...), Code: code, Cause: cause}
}
