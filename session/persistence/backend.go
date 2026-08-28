// The storage contract between the persistence coordinator and a concrete
// backend: the minimal set of durable primitives the orchestration calls. A
// backend implements these (over files, rows, an object store, …); the
// coordinator supplies everything else (buffering, serialization, cursors,
// adoption, crash repair sequencing, dispose quiescence). Port of the
// coordinator's contract types.
package persistence

import "dshgo/session"

// StoredPrefix is a stored session's header, valid contiguous event prefix,
// source-qualified revision, and optional opaque torn-tail marker. The
// revision identifies the exact detached prefix. The coordinator only
// checks marker presence and returns its value to CommitRepair; each
// backend owns the marker type.
type StoredPrefix struct {
	Meta   session.SessionHeader
	Events []session.Event
	// Revision is the revision observed for exactly this detached prefix.
	Revision Revision
	// TornMarker is present iff there is a torn tail to truncate; opaque to
	// the coordinator.
	TornMarker any
}

// StoredSuffix is a stored session's header plus the events at or past a
// requested seq — the return shape of the optional seek-capable
// SuffixReader hook. Non-mutating reads carry no torn marker: there is
// nothing to repair.
type StoredSuffix struct {
	Meta   session.SessionHeader
	Events []session.Event
}

// Snapshot is one lightweight immutable source identity: detached metadata
// plus the opaque source-qualified revision of the stored log.
type Snapshot struct {
	Header   session.SessionHeader `json:"header"`
	Revision Revision              `json:"revision"`
}

// Backend is the storage contract the coordinator drives. Torn markers are
// fully opaque here; backends own their type.
type Backend interface {
	// Name is the human-readable backend name, used in dispose-failure
	// aggregates.
	Name() string
	// LoadStored reads a stored prefix by id, scanning every backend
	// storage scope. It returns nil if no stored artifact exists. Returned
	// metadata must identify id before repair or state publication. The
	// torn marker is present iff there is a torn tail to truncate. Every
	// header and event graph must be fresh, mutually unaliased, and
	// unretained by the backend, because preparation freezes and publishes
	// them in place. The returned revision must identify exactly those
	// values and use the same representation as ReadStoredRevision.
	LoadStored(id session.SessionID) (*StoredPrefix, error)
	// ReadStoredRevision reads the current source-qualified revision for
	// one stored session without loading its event log. It returns an
	// empty revision when the identity is absent.
	ReadStoredRevision(id session.SessionID) (Revision, error)
	// CommitRepair makes a crash repair durable: truncate the torn tail
	// (iff tornMarker != nil) and append closers (iff any). NOT required to
	// be atomic — a file backend may truncate-then-append in two durable
	// steps. Used by load (truncate + synthetic closers) and by
	// live-adoption (truncate only, closers = nil). The stored log's
	// revision afterwards must equal ReadStoredRevision's fresh read.
	CommitRepair(meta session.SessionHeader, tornMarker any, closers []session.Event) error
	// AppendBatch durably appends one contiguous batch. When materialized
	// is false the artifact does not exist yet and is created with the
	// header. Only backends run this; the coordinator guarantees contiguity
	// against its cursor.
	AppendBatch(meta session.SessionHeader, events []session.Event, materialized bool) error
	// List enumerates materialized sessions: one header per session.
	List() ([]session.SessionHeader, error)
	// ListSnapshots lists materialized sessions with cheap per-log change
	// tokens. Repeated observations of an unchanged log return the same
	// revision; a successful LoadStored repair changes the next listed
	// revision.
	ListSnapshots() ([]Snapshot, error)
	// Close releases backend resources after the coordinator reaches
	// quiescence.
	Close() error
}

// SuffixReader is the optional seek-capable suffix read behind the
// service's ReadFrom: return the header plus the stored events with
// seq >= fromSeq without reading the whole log. Backends whose medium can
// address events by seq implement this so ReadFrom scales with the suffix;
// sequential backends omit it and the coordinator re-reads the prefix.
type SuffixReader interface {
	ReadStoredFrom(id session.SessionID, fromSeq int64) (*StoredSuffix, error)
}

// ArtifactLocator is the optional raw-artifact hook: format refusals point
// at the raw log when the backend keeps one artifact per session.
type ArtifactLocator interface {
	Locate(meta session.SessionHeader) *Location
}

// RawArtifacts is the optional verbatim raw-artifact export hook.
type RawArtifacts interface {
	ReadStoredRaw(id session.SessionID) (RawArtifact, error)
}

// RawArtifact is a backend's own raw artifact text for one session,
// verbatim.
type RawArtifact struct {
	// Meta is the session header parsed from the artifact's own first line.
	Meta session.SessionHeader `json:"meta"`
	// Filename is the artifact's base filename on disk, without any
	// physical encoding suffix.
	Filename string `json:"filename"`
	// Content is the artifact's full text content, decoded from the
	// backend's physical encoding.
	Content string `json:"content"`
}
