// Package spill ports @deepseek-ai/dsh-spill: the spill-storage capability
// seam. An abstract service defining WHAT a spill backend does — persist a
// tool's oversized text and return a model-facing locator plus retrieval
// guidance — without saying HOW. Implementations implement Store and are
// handed to the spill policy (the Go seam is an explicit constructor
// argument, not a context service lookup).
//
// The seam is deliberately minimal: SaveText and nothing else. It owns NO
// retention policy (that is dshgo/outputretention), NO tool-result
// replacement (that is dshgo/spillpolicy), and NO retrieval or search API.
// The backend supplies the locator and retrieval hint appropriate for its
// storage substrate.
package spill

import "context"

// SessionID is the owning session's id, mirroring dsh-session's branded id.
// The spill seam treats it as an opaque string so the storage layer stays
// decoupled from the session package.
type SessionID = string

// SpillLocator is the opaque model-facing handle for one spilled artifact. A
// local backend may use a filesystem path; a remote or database backend may
// use a URI or key. Consumers render it with SpillRef.RetrievalHint, but do
// not parse it.
type SpillLocator = string

// SpillOwner is the save-time storage namespace for a spilled artifact. The
// session id lets a backend group storage under the producing session, but
// the returned SpillLocator is the model-facing handle. Forked sessions
// inherit locators already present in the seeded log; those artifacts are not
// copied or re-owned, and spills produced after the fork use the child
// session id.
type SpillOwner struct {
	SessionID SessionID
}

// SpillSource is the tool and call that produced one spilled artifact —
// recorded by the backend for a readable filename and inspection. Not
// interpreted for access control; purely descriptive.
type SpillSource struct {
	// ToolName is the tool whose result was spilled (e.g. web_fetch).
	ToolName string
	// CallID is the model-issued call id the result belongs to.
	CallID string
	// Label is a short human label for the artifact (e.g. result).
	Label string
}

// SaveTextSpill is one request to persist text to a spill artifact.
type SaveTextSpill struct {
	// Owner scopes the storage by the producing session.
	Owner SpillOwner
	// Source names the producing tool and call.
	Source SpillSource
	// SuggestedName is a caller-suggested base name (e.g. web_fetch.txt).
	// The backend sanitizes it to a single safe path segment before use — it
	// is a hint, never a path.
	SuggestedName string
	// Content is the full text to persist (UTF-8).
	Content string
}

// SpillRef is a saved spill artifact: its locator, byte length, and
// backend-specific retrieval guidance.
type SpillRef struct {
	// Locator is the opaque model-facing handle.
	Locator SpillLocator
	// Bytes is the exact stored byte length.
	Bytes int
	// RetrievalHint is the model-facing guidance for reading the artifact.
	RetrievalHint string
}

// Store is the abstract spill-storage service. SaveText persists
// input.Content verbatim and returns an opaque locator, exact byte length,
// and model-facing retrieval guidance.
//
// Semantics every implementation must honor:
//   - Storage is scoped by the request's owner session; the backend chooses a
//     private (not world-readable) location and a collision-free name derived
//     from — never equal to — the caller's SuggestedName.
//   - SaveText REJECTS on a real storage failure (permissions, no space,
//     backend unavailable); the caller decides how to degrade (the spill
//     policy treats a rejection as best-effort and keeps the inline result).
type Store interface {
	SaveText(ctx context.Context, input SaveTextSpill) (SpillRef, error)
}
