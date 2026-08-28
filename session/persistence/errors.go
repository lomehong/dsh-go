// Stable failures exposed by the session-persistence service. Port of
// errors.ts plus the coordinator's corruption and format-refusal vocabulary.
package persistence

import (
	"fmt"

	"dshgo/session"
)

// Location is a backend-resolved, per-session local artifact location. The
// path is an absolute target path and can name an artifact that has not
// materialized yet. Consumers must treat it as a location hint, never as an
// authorization token.
type Location struct {
	// Path is the absolute artifact path.
	Path string `json:"path"`
}

// SessionLocation aliases Location for call-site readability.
type SessionLocation = Location

// NotFoundError is the requested Session identity has no materialized
// durable log.
type NotFoundError struct {
	SessionID session.SessionID
}

// Error renders the stable not-found message.
func (e *NotFoundError) Error() string {
	return fmt.Sprintf("session %q not found", e.SessionID)
}

// CorruptionError is durable session contents failed validation after a
// successful backend read.
type CorruptionError struct {
	Message string
	Cause   error
}

// Error renders the corruption context.
func (e *CorruptionError) Error() string { return e.Message }

// Unwrap exposes the original validation failure.
func (e *CorruptionError) Unwrap() error { return e.Cause }

// FormatUnsupportedError is the stored log is intact but this runtime
// cannot faithfully interpret it: the header carries an unsupported format
// version, or an event's type is unknown to this build. Distinct from
// CorruptionError — nothing is damaged; the raw log remains readable at
// Location when the backend keeps one artifact per session.
type FormatUnsupportedError struct {
	Message string
	// Location is the backend's artifact location, when one exists.
	Location *Location
}

// Error renders the stable refusal reason.
func (e *FormatUnsupportedError) Error() string { return e.Message }

// SessionFormatVersionRefusal renders direction-aware refusal text for a
// stored session whose format version this build does not read. Shared by
// the coordinator's load-time check and by backends that must refuse BEFORE
// decoding version-dependent structure: the user must see "upgrade the
// harness", never "corrupt".
func SessionFormatVersionRefusal(id string, version int64) string {
	if version > session.SESSION_FORMAT_VERSION {
		return fmt.Sprintf("session %q uses log format v%d, but this harness reads only v%d: the log was written by a newer harness — upgrade the harness to open it", id, version, session.SESSION_FORMAT_VERSION)
	}
	return fmt.Sprintf("session %q uses log format v%d, older than the supported v%d, and this build no longer reads that era of the format", id, version, session.SESSION_FORMAT_VERSION)
}
