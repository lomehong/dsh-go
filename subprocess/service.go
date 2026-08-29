package subprocess

import "context"

// Runtime is the abstract subprocess service: execution-world fully
// specified managed process trees with raw or collected stdio. Command
// defaulting, shell semantics, deadlines, protocol framing, terminal
// readiness, and presentation belong to consumers.
//
// Implementations must honor these semantics:
//   - Executable paths belong to one execution world shared with the
//     mounted filesystem provider.
//   - Spawn returns immediately with a live handle; Done resolves at
//     process close with exit facts and the error is non-nil only for
//     spawn-level failures.
//   - Collect-mode readers are offset-based and non-consuming, so
//     independent readers never consume one another's output; lossy reads
//     report truncation and the spill file holding the complete stream
//     when one exists. Piped streams are handed to the caller raw and
//     never buffered here.
//   - Handle.Terminate (and the spawn context's cancellation) escalates
//     SIGTERM → grace → SIGKILL — the only termination verb — tree-scoped
//     on every platform.
//
// The official terminal-process primitive (pty allocation, foreground-group
// inspection, win32 process inspectors) is NOT part of the Go seam yet; the
// local piped implementation covers the batch and streaming consumers
// (fs-search, the bash executor seam). Documented deferral, not a silent gap.
type Runtime interface {
	Spawn(ctx context.Context, spec SpawnSpec) (Handle, error)
}

// Compile-time assertion: the local implementation satisfies the face.
var _ Runtime = Local{}

// Local is the local implementation (dsh-subprocess-local): every spawn is
// an isolated detached process tree rooted in the caller-supplied cwd.
type Local struct{}

// NewLocal returns the local subprocess runtime.
func NewLocal() Local { return Local{} }

// Spawn starts one process tree with the spec's dispositions.
func (Local) Spawn(ctx context.Context, spec SpawnSpec) (Handle, error) {
	return Spawn(ctx, spec)
}
