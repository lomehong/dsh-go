// Package subprocess ports @deepseek-ai/dsh-subprocess (+ subprocess-local):
// the vocabulary and local implementation for fully-specified child-process
// spawns — Node-shaped per-stream stdio modes, bounded collected output with
// spill recovery, raw piped streams, and tree-scoped termination.
//
// Command defaulting, shell semantics, protocol framing, and presentation
// belong to consumers (the bash executor seam, fs-search); this seam applies
// no defaults: every disposition, limit, and directory is explicit, so the
// caller's own config decides them.
package subprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// DshEnvPrefix is the namespace prefix reserved for DeepSeek Harness-managed
// child environment facts. The scrubbed parent base drops every ambient
// entry under this prefix.
const DshEnvPrefix = "DSH_"

// maxTimerDelayMs bounds one grace period (Node's single-timer ceiling).
const maxTimerDelayMs = 1<<31 - 1

// CollectedOutput is one captured stream: the (possibly truncated) text plus
// recovery info. Text is the TAIL of the stream when truncated.
type CollectedOutput struct {
	// Text is the collected text — the tail when Truncated.
	Text string
	// Truncated reports whether bytes were dropped from Text.
	Truncated bool
	// SpillPath holds the complete stream when truncated and available.
	SpillPath string
}

// StdinMode is the stdin disposition.
type StdinMode interface {
	stdinMode()
}

// StdinIgnore leaves fd 0 on the null device.
type StdinIgnore struct{}

// StdinPipe exposes Handle.Stdin for the caller's ongoing protocol writes.
type StdinPipe struct{}

// StdinData writes the bytes and closes (the batch shape).
type StdinData string

func (StdinIgnore) stdinMode() {}
func (StdinPipe) stdinMode()   {}
func (StdinData) stdinMode()   {}

// OutputMode is the stdout/stderr disposition.
type OutputMode interface {
	outputMode()
}

// OutputPipe exposes the raw stream for the caller's protocol decoding.
type OutputPipe struct{}

// OutputInherit passes the parent's descriptor through.
type OutputInherit struct{}

// OutputCollect buffers boundedly with offset-based reads. SpillMaxBytes > 0
// enables a full-stream spill file: while the stream stays within the cap it
// remains fully recoverable; a larger stream discards its now-incomplete
// spill. SpillMaxBytes == 0 keeps only the in-memory tail — the
// diagnostic-tail shape (a language server's stderr).
type OutputCollect struct {
	// MaxBytes is the in-memory cap; overflow keeps the TAIL.
	MaxBytes int
	// SpillMaxBytes is the whole-stream byte cap; zero disables spilling.
	SpillMaxBytes int
}

func (OutputPipe) outputMode()    {}
func (OutputInherit) outputMode() {}
func (OutputCollect) outputMode() {}

// Stdio carries the per-stream dispositions, all explicit — this seam
// applies no defaults.
type Stdio struct {
	Stdin  StdinMode
	Stdout OutputMode
	Stderr OutputMode
}

// SpawnSpec is a fully-specified spawn request. This seam applies no
// defaults: every disposition, limit, and directory is explicit, so the
// caller's own config — not a hidden subprocess-service default — decides
// them (the shell request/spec split is the owning template).
type SpawnSpec struct {
	// Argv is the executable and arguments; Argv[0] is the program. Never
	// shell-interpreted here.
	Argv []string
	// Cwd is the working directory for the child.
	Cwd string
	// Stdio carries the per-stream dispositions.
	Stdio Stdio
	// GraceMs is the positive grace period in milliseconds for the
	// Terminate escalation and for draining still-open collected pipes
	// after the process exits (an inherited descriptor held by a surviving
	// descendant cannot hold the outcome open indefinitely).
	GraceMs int
	// Env carries explicit environment entries merged onto the scrubbed
	// parent base (see ScrubbedParentEnv), with no namespace validation.
	// A non-nil value is a deliberate caller opt-in, so a forwarded
	// credential-shaped entry or current DSH_* fact survives the scrub; a
	// nil value is a tombstone that removes an ordinary ambient entry from
	// the child.
	Env map[string]*string
}

// ErrAborted classifies a spawn cancelled before start by its context.
var ErrAborted = errors.New("subprocess: spawn aborted before start")

// Outcome carries the exit facts of one closed process — Node's
// close-event vocabulary. It deliberately carries NO timeout or
// cancellation classification (the caller reads the signal it owns to
// classify causes) and NO output: collected streams stay readable through
// the handle after settlement, so batch and streaming callers share one
// access path.
type Outcome struct {
	// ExitCode is the exit code; -1 when the process died from a signal.
	ExitCode int
	// Signal is the terminating signal name (e.g. "SIGTERM"); empty on a
	// normal exit.
	Signal string
}

// OutputRead is one incremental OutputReader.ReadFrom read.
type OutputRead struct {
	// Text is the stream text from the requested offset (the whole
	// retained tail when lossy).
	Text string
	// NextOffset is the whole-stream offset to resume from on the next read.
	NextOffset int
	// Lossy is true when the requested offset slid out of the in-memory
	// tail window.
	Lossy bool
	// SpillPath is the full-stream spill file, when one was created and
	// remains intact.
	SpillPath string
}

// OutputReader is cursor-free incremental access to one collected output
// stream. Offsets are whole-stream byte coordinates owned by the caller, so
// independent readers cannot consume one another's output; ReadFrom(0)
// after settlement is the batch result (Lossy then means the in-memory tail
// lost its head — the CollectedOutput.Truncated fact).
type OutputReader interface {
	ReadFrom(fromByte int) OutputRead
}

// Handle is a live child process rooted in its own process tree. Collected
// output remains readable after exit; piped streams belong to the caller.
//
// Termination is tree-scoped everywhere: POSIX signals the detached process
// group (falling back to the direct child when the group is gone), Windows
// terminates the tree via `taskkill /T`, so helper processes cannot outlive
// the handle unnoticed.
type Handle interface {
	// Pid is the process id (tree root); -1 when the spawn itself failed.
	Pid() int
	// Stdin is the child's stdin, present iff spawned with StdinPipe.
	Stdin() io.WriteCloser
	// Stdout is the child's raw stdout, present iff spawned with OutputPipe.
	Stdout() io.ReadCloser
	// Stderr is the child's raw stderr, present iff spawned with OutputPipe.
	Stderr() io.ReadCloser
	// CollectedStdout is present iff stdout spawned as OutputCollect.
	CollectedStdout() OutputReader
	// CollectedStderr is present iff stderr spawned as OutputCollect.
	CollectedStderr() OutputReader
	// Done resolves at process close with exit facts; the error is
	// non-nil only for spawn-level failures.
	Done() <-chan struct{}
	// Outcome returns the settled exit facts (blocking until Done); the
	// error is non-nil only for spawn-level failures.
	Outcome() (Outcome, error)
	// Terminate begins the SIGTERM → GraceMs → SIGKILL escalation on the
	// process tree (Windows force-terminates immediately) — the seam's
	// only consumer-facing termination verb. Idempotent, a no-op once the
	// tree is gone (the pid may be reused), and also triggered by the
	// spawn context's cancellation.
	Terminate()
	// WaitForExit waits until the process tree has exited — the tree, not
	// just the direct child, so a still-running helper is observable
	// before teardown returns. It returns false when ctx ends first.
	WaitForExit(ctx context.Context) bool
}

// Compile-time format guards for the disposition vocabulary.
var (
	_ = fmt.Sprintf
	_ = errors.Is
)
