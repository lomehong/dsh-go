// Package shell ports @deepseek-ai/dsh-shell (+ dsh-shell-env): the
// execution contract for the bash/pwsh executor seam — foreground commands
// and background process handles. Job ids, ownership, polling, and notices
// belong to the jobs seam; this package exposes only process handles. The
// managed-environment and captured-output vocabulary is owned by the
// subprocess seam and re-exported here so shell consumers keep one import
// root.
package shell

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"dshgo/fs"
	"dshgo/subprocess"
)

// DshEnvPrefix re-exports the managed-environment namespace prefix.
const DshEnvPrefix = subprocess.DshEnvPrefix

// ShellSandboxInfo carries sandbox facts for one run, present iff a
// sandboxing executor handled it. Facts are reported independently of
// process exit status so callers can distinguish command failures from
// policy denials and runner failures.
type ShellSandboxInfo struct {
	// Mode is the mode the command actually ran under.
	Mode string
	// Denied reports whether the sandbox denied a file operation.
	Denied bool
	// Enforcement describes how completely the selected runner enforced
	// the requested mode (empty when the runner does not report it).
	Enforcement string
	// RunnerFailed reports whether the sandbox runner failed before the
	// command could run.
	RunnerFailed bool
}

// ShellExecRequest is a caller's execution REQUEST: workdir and timeoutMs
// are optional and filled by ShellExecutor.Resolve from the
// implementation's config. This is the model-/plugin-facing shape; pass it
// to Resolve to obtain a fully-resolved ShellExecSpec.
type ShellExecRequest struct {
	// Command is the shell command text.
	Command string
	// Workdir is the working directory override (default:
	// implementation-configured).
	Workdir string
	// TimeoutMs is the timeout override in milliseconds
	// (implementations cap it).
	TimeoutMs int
	// StdoutMaxBytes is the foreground stdout capture budget in bytes.
	// Absent (zero) uses the executor's default output cap. Trusted
	// in-process consumers use this when they must parse complete stdout
	// up to their own bounded limit; the model-facing bash tool does not
	// expose it as a parameter.
	StdoutMaxBytes int
	// Signal aborts the command — implementations kill it when the
	// context fires.
	Signal context.Context
	// Stdin carries bytes to write to the command's stdin, then close.
	// Empty leaves stdin closed/empty (the default for model-driven tool
	// calls). Set by in-process plugins (the hooks bridges); the
	// model-facing bash tool does not expose it as a parameter (a model
	// that needs stdin uses shell syntax like a heredoc or a pipe).
	Stdin string
	// Env carries ordinary environment entries for the command, merged
	// after the credential scrub. Managed facts belong in DshEnv, which
	// merges after this map, so an entry here can never displace one.
	Env map[string]string
	// DshEnv carries harness-owned DSH_* variables for this execution.
	// Executors discard ambient DSH_* entries before merging this
	// snapshot last, so an unavailable current fact cannot inherit a
	// stale value from the harness process and a caller Env entry cannot
	// displace a managed one.
	DshEnv map[string]string
	// SandboxPolicy is the fully resolved per-call sandbox policy;
	// sandboxing executors default it.
	SandboxPolicy fs.SandboxExecutionPolicy
}

// ShellExecSpec is a resolved execution spec. ShellExecutor.Resolve fills
// and caps the required fields; ShellExecutor.Start ignores TimeoutMs
// because background processes have no executor timeout.
type ShellExecSpec struct {
	Command        string
	Workdir        string
	TimeoutMs      int
	StdoutMaxBytes int
	Signal         context.Context
	Stdin          string
	Env            map[string]string
	DshEnv         map[string]string
	SandboxPolicy  fs.SandboxExecutionPolicy
}

// ShellRunResult is the outcome of one completed (or killed) foreground
// run. Run rejects only for infrastructure failures: nonzero exits, timeout
// kills, and abort kills resolve with a descriptive result.
type ShellRunResult struct {
	// ExitCode is the exit code; -1 when the process died from a signal.
	ExitCode int
	// Signal is the terminating signal name; empty on a normal exit.
	Signal string
	// TimedOut is true when the executor's own timeout was the FIRST
	// cause to cut the command short. Mutually exclusive with Aborted:
	// one fused deadline drives both the timeout and the caller's
	// cancellation, so a timeout and an abort racing before process close
	// report the single first-abort cause, not both.
	TimedOut bool
	// Aborted is true when the caller's Signal was the FIRST cause to
	// kill the command (and it was not the executor's own timeout).
	Aborted bool
	// TimeoutMs is the effective timeout applied to this run (after
	// defaulting/capping).
	TimeoutMs int
	// Stdout and Stderr are the captured streams.
	Stdout subprocess.CollectedOutput
	Stderr subprocess.CollectedOutput
	// Sandbox carries sandbox execution facts, absent for an unsandboxed
	// executor.
	Sandbox *ShellSandboxInfo
}

// ShellProcessStatus is the lifecycle of a background process.
type ShellProcessStatus string

// Background process lifecycle states (settled exactly once).
const (
	ProcessRunning   ShellProcessStatus = "running"
	ProcessCompleted ShellProcessStatus = "completed"
	ProcessKilled    ShellProcessStatus = "killed"
)

// ShellProcessRead is one incremental ShellProcess.ReadOutput read.
type ShellProcessRead struct {
	// Delta is the output produced since the previous read (stderr in a
	// marked section).
	Delta string
	// Lossy is true when truncation dropped unread bytes the delta
	// cannot include.
	Lossy bool
	// StdoutSpillPath is the full stdout spill file, when stdout
	// truncation occurred and a safe path is available.
	StdoutSpillPath string
	// StderrSpillPath is the full stderr spill file, when stderr
	// truncation occurred and a safe path is available.
	StderrSpillPath string
}

// ShellProcess is a background process handle returned by
// ShellExecutor.Start. It is the only access path; buffered output remains
// readable after exit. Composition teardown (the subprocess service's
// disposal) kills running processes and awaits Done; an executor-only
// reload leaves them running.
type ShellProcess interface {
	// Status is the process lifecycle state (settled exactly once).
	Status() ShellProcessStatus
	// ExitCode is the exit code once finished (-1 = killed by signal /
	// still running).
	ExitCode() int
	// Signal is the terminating signal name, when signal-killed.
	Signal() string
	// Done resolves when the underlying process closes (never errors — a
	// spawn failure settles as ProcessKilled with the error on stderr).
	Done() <-chan struct{}
	// Sandbox is stamped once a confined process settles.
	Sandbox() *ShellSandboxInfo
	// ReadOutput reads output produced since the previous read
	// (consuming — consecutive reads never re-deliver). Reads that lost
	// data flag Lossy and point at the spill files.
	ReadOutput() ShellProcessRead
}

// ShellExecutor is the abstract bash execution service. Exactly one
// provider of the shell service is composed per context; loading a second
// fails loud on the duplicate service registration.
//
// Implementations must honor these semantics:
//   - Run rejects only for infrastructure failures. Nonzero exits, timeout
//     kills, and abort kills resolve with a ShellRunResult.
//   - Start returns immediately; no timeout applies to background
//     processes. Done settles at process close and never errors; spawn
//     failures settle as ProcessKilled with the error on stderr.
//   - ShellProcess.ReadOutput is incremental: consecutive reads never
//     repeat output. Lossy reads report truncation and available spill
//     files.
//   - A still-running background process is stopped and awaited when its
//     owning composition tears down. With the subprocess seam that
//     boundary is ctx.subprocess disposal, so a background process
//     survives an executor-only reload.
type ShellExecutor interface {
	// SandboxMode is the sandbox mode this executor applies by default,
	// or empty when it does not sandbox commands.
	SandboxMode() string
	// Resolve applies implementation-owned defaults and caps to a
	// request before execution: omitted fields get this implementation's
	// defaults, capped fields are clamped.
	Resolve(request ShellExecRequest) ShellExecSpec
	// Run executes a command in the foreground and resolves when it
	// finishes. It takes a resolved spec from Resolve, never a raw
	// request. Nonzero exits, timeout kills, and abort kills resolve
	// with a descriptive result rather than an error.
	Run(spec ShellExecSpec) (ShellRunResult, error)
	// Start spawns a background process and returns immediately.
	Start(spec ShellExecSpec) (ShellProcess, error)
}

// exitMarkerRe and signalMarkerRe match the exit-status markers the shell
// tools' renderers append. Requiring a leading newline and the end of the
// string keeps ordinary output that merely ends with marker-like text from
// matching unless the final line is indistinguishable from a real marker.
var (
	exitMarkerRe   = regexp.MustCompile(`\n\[exit code: (\d+)\]$`)
	signalMarkerRe = regexp.MustCompile(`\n\[killed by signal: ([^\]\n]+)\]$`)
)

// ParsedExitStatus is the exit status recovered from a rendered result,
// with the output body that status was split off from.
type ParsedExitStatus struct {
	// Body is the marker-free output body. The consumed marker is removed
	// because a terminal presentation shows the exit status as its own
	// pill: leaving the marker in the output would render the exit twice.
	// Other markers (timeout, sandbox denial) carry facts no pill shows,
	// so they stay in the body.
	Body string
	// ExitCode is set for a non-zero exit marker (-1 otherwise).
	ExitCode int
	// Signal is set for a killed marker (empty otherwise).
	Signal string
}

// ParseExitStatus splits a rendered shell-tool result string into its
// output body and the structured exit status — the inverse of the
// `[exit code: N]` / `[killed by signal: X]` markers the shell tools'
// renderers append. A killed marker yields Signal; otherwise a non-zero
// marker yields ExitCode; absent both means a clean exit 0.
//
// Replay only retains the rendered content text, not the original
// ShellRunResult, so terminal presentation must recover the exit pill
// here.
func ParseExitStatus(text string) ParsedExitStatus {
	if match := signalMarkerRe.FindStringSubmatch(text); match != nil {
		return ParsedExitStatus{Body: text[:strings.LastIndex(text, "\n")], Signal: match[1]}
	}
	if match := exitMarkerRe.FindStringSubmatch(text); match != nil {
		code, _ := strconv.Atoi(match[1])
		return ParsedExitStatus{Body: text[:strings.LastIndex(text, "\n")], ExitCode: code}
	}
	return ParsedExitStatus{Body: text, ExitCode: 0}
}
