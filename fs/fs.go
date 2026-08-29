// Package fs is the filesystem Service Definition vocabulary for one
// execution world (official @deepseek-ai/dsh-fs): the opaque target/version
// identities, the metadata stat returns, the write-intent and outcome shapes,
// the literal-edit request/outcome, and the typed error taxonomy. Backends
// own stable target identity, path processing and file URIs, containment,
// text reads, decoding, binary rejection, and atomic mutations. Read windows
// and observed-state policy stay in consumer and policy plugins; EditText
// remains on the service so version check, literal match, and rewrite share
// one critical section.
package fs

import (
	"context"
	"fmt"
)

// TargetKey is the opaque key for stale guards and target lookup (official
// FsTargetKey). The local backend uses a realpath-like string; a remote
// backend might use a workspace URI or file id. Consumers MUST NOT parse it
// or assume it is a local absolute path.
type TargetKey string

// Version is the opaque file-version token — the freshness token a write or
// edit guards against (official FsVersion). The local backend derives it from
// high-resolution stat identity and freshness fields; a remote backend might
// use a revision id. The policy layer records it for stale checks; consumers
// may display related metadata but MUST NOT interpret this token.
type Version string

// Target is a path resolved by a backend into a stable identity. Resolve
// produces this; every other operation takes it.
type Target struct {
	// Key is the opaque key for stale guards and target lookup.
	Key TargetKey `json:"targetKey"`
	// DisplayPath is the path for model/UI-facing output. May be a local
	// absolute path, workspace-relative path, or remote URI depending on
	// the backend.
	DisplayPath string `json:"displayPath"`
}

// Target types for Info and DirEntry observations.
const (
	TypeFile      = "file"
	TypeDirectory = "directory"
	TypeSymlink   = "symlink"
	TypeOther     = "other"
)

// Info is metadata about a target — what Stat returns. Lets the policy layer
// reject directories/special files before reading and choose ReadText versus
// streaming from Size without probing by failure. Version is the freshness
// token. A nil *Info from Stat means the target is absent.
type Info struct {
	// Version is the opaque freshness token of the target right now.
	Version Version `json:"version"`
	// Type is whether the target is a regular file, a directory, or
	// something else.
	Type string `json:"type"`
	// Size is the byte size of a regular file, when the backend can
	// report it.
	Size *int64 `json:"size,omitempty"`
}

// PathInfo is metadata about a path without following the final path
// component when it is a symbolic link. Unlike Info, this path-level probe
// can report TypeSymlink so consumers with trust-boundary rules can reject
// repository-owned links before resolving a target.
type PathInfo struct {
	// Version is the opaque freshness token of the path entry right now.
	Version Version `json:"version"`
	// Type is whether the path entry is a regular file, directory,
	// symlink, or other.
	Type string `json:"type"`
	// Size is the byte size of the path entry, when the backend can
	// report it.
	Size *int64 `json:"size,omitempty"`
}

// Observation is one authoritative observation of a target. A present
// observation carries the version used by guarded replacement; an absent
// observation authorizes only a guarded create, never an edit.
type Observation struct {
	// Present discriminates the observation kind.
	Present bool `json:"kind,omitempty"`
	// Version is the observed freshness token when Present.
	Version Version `json:"version,omitempty"`
}

// ObservationPresent builds one positive observation.
func ObservationPresent(version Version) Observation {
	return Observation{Present: true, Version: version}
}

// ObservationAbsent builds one confirmed absence.
func ObservationAbsent() Observation {
	return Observation{}
}

// DirEntry is one direct child returned by ListDir. Listing returns metadata
// and resolved targets only; it must not read file contents.
type DirEntry struct {
	// Name is the basename of the child inside the listed directory.
	Name string `json:"name"`
	// Type is whether the child is a regular file, a directory, or
	// something else.
	Type string `json:"type"`
	// Target is the resolved child target for follow-up operations.
	Target Target `json:"target"`
	// Version is the opaque freshness token when the backend can report
	// metadata cheaply.
	Version Version `json:"version,omitempty"`
	// Size is the byte size of a regular file, when the backend can
	// report it.
	Size *int64 `json:"size,omitempty"`
}

// WriteIntent kinds.
const (
	// IntentCreateIfAbsent rejects an existing target with
	// CodeNotObserved.
	IntentCreateIfAbsent = "createIfAbsent"
	// IntentReplaceIfVersion rejects absence or mismatch with
	// CodeStaleVersion.
	IntentReplaceIfVersion = "replaceIfVersion"
)

// WriteIntent is the guarded write intent. A nil *WriteIntent passed to
// WriteText means unconditional create-or-overwrite, not a third union arm.
type WriteIntent struct {
	// Kind discriminates the guard.
	Kind string `json:"kind"`
	// Version is the expected freshness token for replaceIfVersion.
	Version Version `json:"version,omitempty"`
}

// WriteOutcome is the outcome of a full-file write.
type WriteOutcome struct {
	// Operation is whether the write created a new file or replaced an
	// existing one.
	Operation string `json:"operation"`
	// Version is the opaque version of the file after the write.
	Version Version `json:"version"`
	// Before is the file's content BEFORE the write, or nil when the file
	// did not exist (a create) or the backend declined a contextual basis
	// (for example, a binary/non-UTF-8 prior file or either overwrite side
	// reaching its exclusive limit). LF-normalized storage text (the diff
	// basis), never a diff — a consumer computes the result-time
	// contextual diff from Before/After when Before is present, else falls
	// back to a whole-file diff.
	Before *string `json:"before"`
	// After is the file's content AFTER the write, LF-normalized to share
	// Before's diff basis.
	After string `json:"after"`
}

// EditRequest is a literal-replacement edit request.
type EditRequest struct {
	// OldString is the literal non-empty text to replace. Must match
	// exactly (after line-ending normalization).
	OldString string `json:"oldString"`
	// NewString is the literal replacement text. An empty string deletes
	// the matched text.
	NewString string `json:"newString"`
	// ReplaceAll replaces every match instead of requiring exactly one.
	ReplaceAll bool `json:"replaceAll"`
}

// EditOutcome is the outcome of a literal edit.
type EditOutcome struct {
	// Version is the opaque version of the file after the edit.
	Version Version `json:"version"`
	// Before is the file's content BEFORE the edit. Raw storage text
	// (LF-normalized by the backend), never a diff — a consumer computes
	// the result-time contextual diff (the applied hunk with context) from
	// Before/After.
	Before string `json:"before"`
	// After is the file's content AFTER the edit.
	After string `json:"after"`
}

// Stable, machine-routable codes for filesystem failures. Carried on Error;
// the tool registry exposes {name, code} on isError results so retry,
// permission, and UI layers can branch without parsing messages.
const (
	CodeNotFound       = "FS_NOT_FOUND"
	CodeNotDirectory   = "FS_NOT_DIRECTORY"
	CodeNotText        = "FS_NOT_TEXT"
	CodeNotRegularFile = "FS_NOT_REGULAR_FILE"
	CodeTooLarge       = "FS_TOO_LARGE"
	CodePermissionDnd  = "FS_PERMISSION_DENIED"
	CodeSandboxDenied  = "FS_SANDBOX_DENIED"
	CodeIOError        = "FS_IO_ERROR"
	CodeStaleVersion   = "FS_STALE_VERSION"
	CodeNotObserved    = "FS_NOT_OBSERVED"
	CodeAmbiguousEdit  = "FS_AMBIGUOUS_EDIT"
	CodeEditNotFound   = "FS_EDIT_NOT_FOUND"
	CodeAborted        = "FS_ABORTED"
)

// Error is the typed filesystem error: a stable Code plus the message and
// chained cause. The fs package owns this vocabulary so backends and the
// policy layer raise the same codes instead of each inventing message
// strings.
type Error struct {
	Code   string
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %v", e.Detail, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s (%s)", e.Detail, e.Code)
}

func (e *Error) Unwrap() error { return e.Cause }

// NewError builds one typed filesystem error.
func NewError(code string, detail string, cause error) *Error {
	return &Error{Code: code, Detail: detail, Cause: cause}
}

// SandboxExecutionPolicy is the per-call fencing policy a sandboxing backend
// runs one mutation under (official dsh-sandbox's shape, hosted in this
// package until the shell/sandbox capability round ports it).
type SandboxExecutionPolicy struct {
	// Mode is the effective sandbox mode for the call (the
	// permissionpresets mode vocabulary).
	Mode string
	// WorkspaceRoot is the writable root when Mode fences writes to a
	// workspace.
	WorkspaceRoot string
}

// Cordis event names (official declare-module events). Go adaptation: the
// waterfall decisions ride ctx.On(event, WaterfallHandler) and the
// observation record rides a plain emit; no registry entry is required.
const (
	// EventWriteIntent is the single-slot decision for the next
	// WriteText: the first listener returning an intent owns the decision
	// rather than composing with peers.
	EventWriteIntent = "fs/write-intent"
	// EventEditIntent is the single-slot decision for the next EditText:
	// the first returned guard wins.
	EventEditIntent = "fs/edit-intent"
	// EventObserved records an authoritative positive or negative
	// observation; listeners must be synchronous recorders.
	EventObserved = "fs/observed"
)

// FileSystem is the abstract filesystem provider for one execution world
// (official ctx.fs). Targets must preserve identity across aliases; reads
// expose regular UTF-8 text or typed errors, listings are stable and
// content-free, and mutations are atomic. Optional guards add stale
// protection without changing the unguarded provider contract.
//
// The cordis service name is "fs".
type FileSystem interface {
	// SandboxMode is the sandbox mode this backend enforces on mutations
	// BY DEFAULT, or "" when it does not confine at all — the capability
	// fact the tool layer reads to advertise the escalation fields
	// honestly. The bare local backend reports ""; a sandboxing backend
	// overrides it with the deployment default. A session override may
	// make the effective mode narrower or wider, so strict escalation
	// widening is checked per call rather than encoded in this
	// default-relative fact.
	SandboxMode() string

	// Resolve resolves a model/plugin-supplied path into a stable Target.
	// Relative paths resolve against cwd. The same file yields the same
	// Target.Key.
	Resolve(ctx context.Context, path string, cwd string) (Target, error)

	// ProcessPath returns the canonical absolute path a subprocess in
	// this filesystem's execution world can open. The path is
	// deliberately separate from Target.Key: consumers may pass this
	// value to another OS capability, but must continue treating the
	// target key as opaque.
	ProcessPath(target Target) string

	// ProcessPathFromHostPath maps an absolute path from the harness host
	// into this filesystem's execution world when both paths identify the
	// same file. Returns "" when this execution world cannot read that
	// host file. The base provider exposes no mapping; host-backed or
	// explicitly shared backends override it.
	ProcessPathFromHostPath(hostPath string) string

	// FileURL returns the canonical file: URI for a target in this
	// filesystem's execution world. Backends own URI encoding because the
	// host platform may differ from the execution platform.
	FileURL(target Target) string

	// Contains tests canonical containment without exposing or parsing
	// backend target keys. Both targets must come from this provider.
	Contains(parent Target, child Target) bool

	// Stat returns target metadata, or nil when the target does not
	// exist. Metadata only, never content.
	Stat(ctx context.Context, target Target) (*Info, error)

	// Lstat returns path metadata without following the final path
	// component when it is a symbolic link. This is intentionally
	// path-shaped, not target-shaped: Resolve follows symlinks to produce
	// the stable identity used by normal reads and writes, while Lstat
	// lets a consumer reject the path itself before that follow happens.
	// Relative paths resolve against cwd. A nil result means the path is
	// absent.
	Lstat(ctx context.Context, path string, cwd string) (*PathInfo, error)

	// ReadText reads the whole regular text file as a single decoded
	// string.
	ReadText(ctx context.Context, target Target) (string, error)

	// StreamText streams the whole regular text file as decoded text
	// chunks (same text semantics as ReadText, for large files). The
	// backend owns cross-chunk UTF-8 decoding and binary rejection so the
	// policy layer never touches raw bytes. The returned pull function
	// yields chunks until ok is false; it is invalid after the context
	// cancels.
	StreamText(ctx context.Context, target Target) (func() (string, bool), error)

	// ReadBytes reads the whole regular file as raw bytes with no
	// decoding or binary rejection. The bound lives at this seam so a
	// backend can never buffer an unbounded file: a target known or
	// discovered to exceed maxBytes fails with CodeTooLarge instead of
	// returning a truncated result.
	ReadBytes(ctx context.Context, target Target, maxBytes int64) ([]byte, error)

	// ListDir lists direct children of a directory in stable name order.
	// Returns resolved child targets plus cheap metadata only; never
	// reads file contents.
	ListDir(ctx context.Context, target Target) ([]DirEntry, error)

	// WriteText atomically creates or replaces UTF-8 text. A non-nil
	// expected guards intent and staleness; nil allows unconditional
	// overwrite. A non-nil sandboxPolicy is the per-call mode and
	// workspace root this write runs under; a sandboxing backend fences
	// the write by it, the bare backend ignores it.
	WriteText(ctx context.Context, target Target, content string, expected *WriteIntent, sandboxPolicy *SandboxExecutionPolicy) (WriteOutcome, error)

	// EditText atomically edits literal text. When supplied, the version
	// guard is checked before matching so stale content reports
	// CodeStaleVersion; nil edits the current content without a freshness
	// precondition. A non-nil sandboxPolicy fences the edit the same way
	// WriteText's does.
	EditText(ctx context.Context, target Target, edit EditRequest, expected *Version, sandboxPolicy *SandboxExecutionPolicy) (EditOutcome, error)
}

// ServiceName is the cordis service key the provider publishes under
// (official ctx.fs).
const ServiceName = "fs"
