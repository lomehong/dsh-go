// Package shelllocal ports @deepseek-ai/dsh-bash-local and
// @deepseek-ai/dsh-pwsh-local: local shell executors over the subprocess
// seam. Public commands run as `bash -c` or through a resolved PowerShell
// executable in a managed process tree spawned through ctx.subprocess
// (in Go, the injected subprocess.Runtime). This executor owns command
// defaulting, deadlines and cause classification, the model-friendly
// terminal environment, and the model-facing stdout/stderr merge for
// background reads. Execution policy belongs in the pre-execute gate or a
// sandboxing executor (not ported: the confining subclasses compose the
// ctx.sandbox.confine boundary, which the Go composition mounts later).
package shelllocal

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dshgo/shell"
	"dshgo/subprocess"
)

// EnvOverrides are the model-friendly environment overrides: disable
// colors, pagers, and interactive terminal features that would garble tool
// output (the same set Codex hardcodes; Claude Code achieves it via
// TERM=dumb). Shell-tool policy — merged first into the spawn's explicit
// env, so a trusted caller's own entry still wins; the subprocess service
// applies its credential scrub independently.
var EnvOverrides = map[string]string{
	"NO_COLOR":  "1",
	"TERM":      "dumb",
	"PAGER":     "cat",
	"GIT_PAGER": "cat",
}

// Config defaults (the official schemastery defaults).
const (
	// DefaultTimeoutMs is the default foreground timeout.
	DefaultTimeoutMs = 120_000
	// DefaultMaxTimeoutMs is the upper bound for per-call timeout overrides.
	DefaultMaxTimeoutMs = 600_000
	// DefaultMaxOutputBytes is the per-stream in-memory output cap.
	DefaultMaxOutputBytes = 64_000
	// DefaultMaxSpillBytes is the per-stream spill-file cap; larger
	// streams retain only their in-memory tail.
	DefaultMaxSpillBytes = 64 * 1024 * 1024
	// DefaultGraceMs is the default SIGTERM→SIGKILL grace period
	// (matches OpenCode's 3s).
	DefaultGraceMs = 3_000
)

// Config carries the executor's budgets and defaults. Cwd and PwshPath may
// be empty (cwd falls back to the process working directory; pwshPath to
// candidate probing).
type Config struct {
	// Cwd is the default working directory for commands.
	Cwd string
	// TimeoutMs is the default foreground timeout in milliseconds.
	TimeoutMs int
	// MaxTimeoutMs is the upper bound for per-call timeout overrides.
	MaxTimeoutMs int
	// MaxOutputBytes is the per-stream in-memory output cap; overflow
	// spills to a temp file.
	MaxOutputBytes int
	// MaxSpillBytes is the per-stream spill-file cap.
	MaxSpillBytes int
	// GraceMs is the grace period for kill escalation and inherited pipes.
	GraceMs int
	// PwshPath is an explicit pwsh executable (pwsh executor only),
	// trusted as-is.
	PwshPath string
}

// DefaultConfig returns the official defaults.
func DefaultConfig() Config {
	return Config{
		TimeoutMs:      DefaultTimeoutMs,
		MaxTimeoutMs:   DefaultMaxTimeoutMs,
		MaxOutputBytes: DefaultMaxOutputBytes,
		MaxSpillBytes:  DefaultMaxSpillBytes,
		GraceMs:        DefaultGraceMs,
	}
}

func assertPositiveFinite(name string, value int) error {
	if value <= 0 {
		return fmt.Errorf("shell-local: %s must be a positive finite number", name)
	}
	return nil
}

func assertServiceableConfig(name string, config Config) error {
	for _, check := range []struct {
		field string
		value int
	}{
		{"timeoutMs", config.TimeoutMs},
		{"maxTimeoutMs", config.MaxTimeoutMs},
		{"maxOutputBytes", config.MaxOutputBytes},
		{"maxSpillBytes", config.MaxSpillBytes},
		{"graceMs", config.GraceMs},
	} {
		if err := assertPositiveFinite(check.field, check.value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// EncodingPreamble forces UTF-8 in both directions so PowerShell output
// survives code-page assumptions (the pwsh twin of bash's default UTF-8).
const EncodingPreamble = "[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [System.Text.UTF8Encoding]::new($false); "

// pwshCandidate is one probed pwsh install location.
type pwshCandidate struct {
	path string
	kind string // "env" for environment-derived, "wellknown", "path"
}

// CandidatePwshPaths returns well-known Windows PowerShell install
// locations plus PATH entries, newest first. Explicitly parameterized so
// resolution is a pure function of its inputs on every platform.
func CandidatePwshPaths(programFiles, systemRoot, pathEnv string) []string {
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	candidates := []string{filepath.Join(programFiles, "PowerShell", "7", "pwsh.exe")}
	for _, entry := range strings.Split(pathEnv, ";") {
		trimmed := strings.Trim(strings.TrimSpace(entry), `"`)
		if trimmed == "" {
			continue
		}
		candidates = append(candidates, filepath.Join(trimmed, "pwsh.exe"))
	}
	// Windows PowerShell 5.1 remains the last-resort fallback on legacy hosts.
	candidates = append(candidates, filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"))
	return candidates
}

// candidateExists probes with lstat semantics (open the entry itself, never
// follow reparse points), so it sees the Store app execution alias where a
// following stat hits the target's ACL. A real directory never matches.
func candidateExists(candidate string) bool {
	info, err := os.Lstat(candidate)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0
}

// ResolvePwshPath resolves the pwsh executable this executor spawns: an
// explicit configured path (trusted as-is), else on Windows the first
// existing well-known location (PowerShell 7 install, a PATH entry such as
// the Microsoft Store install, then Windows PowerShell 5.1), else `pwsh`
// for PATH resolution.
func ResolvePwshPath(configured string) string {
	if configured != "" {
		return configured
	}
	if runtime.GOOS == "windows" {
		for _, candidate := range CandidatePwshPaths(os.Getenv("ProgramFiles"), os.Getenv("SystemRoot"), os.Getenv("PATH")) {
			if candidateExists(candidate) {
				return candidate
			}
		}
	}
	return "pwsh"
}

// Executor is a local shell executor over the subprocess seam (the
// bash-local/pwsh-local pair; the shell flavor is fixed at construction).
type Executor struct {
	sub  subprocess.Runtime
	cfg  Config
	name string // "bash-local" | "pwsh-local"

	resolvedPwsh string
}

// NewBash builds the local bash executor. Public commands run as
// `bash -c <command>` (bash resolves from PATH at spawn time, exactly like
// the official implementation).
func NewBash(sub subprocess.Runtime, cfg Config) (*Executor, error) {
	if err := assertServiceableConfig("bash-local", cfg); err != nil {
		return nil, err
	}
	return &Executor{sub: sub, cfg: cfg, name: "bash-local"}, nil
}

// NewPwsh builds the local PowerShell executor; pwshPath is resolved once
// at construction (probing the filesystem is the one derived fact).
func NewPwsh(sub subprocess.Runtime, cfg Config) (*Executor, error) {
	if err := assertServiceableConfig("pwsh-local", cfg); err != nil {
		return nil, err
	}
	return &Executor{
		sub:          sub,
		cfg:          cfg,
		name:         "pwsh-local",
		resolvedPwsh: ResolvePwshPath(cfg.PwshPath),
	}, nil
}

// Name identifies the executor flavor in diagnostics.
func (e *Executor) Name() string { return e.name }

// PwshPath is the pwsh executable every command runs through.
func (e *Executor) PwshPath() string { return e.resolvedPwsh }

// SandboxMode is empty: this executor never confines (the seam contract —
// a sandboxing subclass stamps its default instead).
func (e *Executor) SandboxMode() string { return "" }

// Resolve fills workdir from config.cwd (else the process working
// directory) and timeoutMs from config.timeoutMs, capped at
// config.maxTimeoutMs. The tool layer calls this before Run/Start, so
// those methods receive explicit values and never re-default.
func (e *Executor) Resolve(request shell.ShellExecRequest) shell.ShellExecSpec {
	if request.TimeoutMs < 0 {
		panic(fmt.Sprintf("%s: request.timeoutMs must not be negative", e.name))
	}
	timeoutMs := request.TimeoutMs
	if timeoutMs == 0 {
		timeoutMs = e.cfg.TimeoutMs
	}
	if timeoutMs > e.cfg.MaxTimeoutMs {
		timeoutMs = e.cfg.MaxTimeoutMs
	}
	stdoutMaxBytes := request.StdoutMaxBytes
	if stdoutMaxBytes == 0 {
		stdoutMaxBytes = e.cfg.MaxOutputBytes
	}
	if stdoutMaxBytes <= 0 {
		panic(fmt.Sprintf("%s: request.stdoutMaxBytes must be a positive finite number", e.name))
	}
	workdir := request.Workdir
	if workdir == "" {
		workdir = e.cfg.Cwd
	}
	if workdir == "" {
		workdir = processCwd()
	}
	return shell.ShellExecSpec{
		Command:        request.Command,
		Workdir:        workdir,
		TimeoutMs:      timeoutMs,
		StdoutMaxBytes: stdoutMaxBytes,
		Signal:         request.Signal,
		Stdin:          request.Stdin,
		Env:            request.Env,
		DshEnv:         request.DshEnv,
		SandboxPolicy:  request.SandboxPolicy,
	}
}

// argv maps a resolved spec onto the executor's shell invocation.
func (e *Executor) argv(spec shell.ShellExecSpec) []string {
	if e.name == "pwsh-local" {
		return []string{
			e.resolvedPwsh, "-NoLogo", "-NoProfile", "-NonInteractive",
			"-Command", EncodingPreamble + spec.Command,
		}
	}
	return []string{"bash", "-c", spec.Command}
}

// spawnSpec layers the one explicit env map for the seam so the trusted
// dshEnv snapshot beats both the caller's env and the terminal overrides;
// the subprocess service merges the whole map after its ambient scrub.
func (e *Executor) spawnSpec(spec shell.ShellExecSpec, argv []string, stdoutMaxBytes int, signal context.Context) subprocess.SpawnSpec {
	stdinMode := subprocess.StdinMode(subprocess.StdinIgnore{})
	if spec.Stdin != "" {
		stdinMode = subprocess.StdinData(spec.Stdin)
	}
	env := map[string]*string{}
	for key, value := range EnvOverrides {
		v := value
		env[key] = &v
	}
	for key, value := range spec.Env {
		v := value
		env[key] = &v
	}
	for key, value := range spec.DshEnv {
		v := value
		env[key] = &v
	}
	return subprocess.SpawnSpec{
		Argv: argv,
		Cwd:  spec.Workdir,
		Stdio: subprocess.Stdio{
			Stdin:  stdinMode,
			Stdout: subprocess.OutputCollect{MaxBytes: stdoutMaxBytes, SpillMaxBytes: e.cfg.MaxSpillBytes},
			Stderr: subprocess.OutputCollect{MaxBytes: e.cfg.MaxOutputBytes, SpillMaxBytes: e.cfg.MaxSpillBytes},
		},
		GraceMs: e.cfg.GraceMs,
		Env:     env,
		// signal rides the spawn context (the seam terminates on its
		// cancellation).
	}
}

// runCause implements the single first-cause classification between the
// executor's own timeout and the caller's cancellation: the first arm to
// observe completion records itself; a timeout and an abort racing before
// process close report the single first-abort cause, not both.
type runCause struct {
	cause atomic.Int32 // 0 none, 1 timeout, 2 abort
}

const (
	causeNone    = 0
	causeTimeout = 1
	causeAbort   = 2
)

func (c *runCause) timeout() {
	c.cause.CompareAndSwap(causeNone, causeTimeout)
}

func (c *runCause) abort() {
	c.cause.CompareAndSwap(causeNone, causeAbort)
}

// Run executes a command in the foreground. It rejects only for
// infrastructure failures: nonzero exits, timeout kills, and abort kills
// resolve with a descriptive result.
func (e *Executor) Run(spec shell.ShellExecSpec) (shell.ShellRunResult, error) {
	return e.runArgv(spec, e.argv(spec))
}

func (e *Executor) runArgv(spec shell.ShellExecSpec, argv []string) (shell.ShellRunResult, error) {
	parent := spec.Signal
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	defer cancel()

	cause := &runCause{}
	timer := time.AfterFunc(time.Duration(spec.TimeoutMs)*time.Millisecond, func() {
		cause.timeout()
		cancel()
	})
	defer timer.Stop()
	abortWatch := make(chan struct{})
	go func() {
		select {
		case <-parent.Done():
			cause.abort()
		case <-abortWatch:
		}
	}()
	defer close(abortWatch)

	handle, err := e.sub.Spawn(runCtx, e.spawnSpec(spec, argv, spec.StdoutMaxBytes, runCtx))
	if err != nil {
		// Abort-before-start is an infrastructure failure on the run
		// path (official run() rejects for it too).
		return shell.ShellRunResult{}, err
	}
	outcome, err := handle.Outcome()
	if err != nil {
		return shell.ShellRunResult{}, err
	}
	timedOut := cause.cause.Load() == causeTimeout
	aborted := cause.cause.Load() == causeAbort && !timedOut
	return shell.ShellRunResult{
		ExitCode:  outcome.ExitCode,
		Signal:    outcome.Signal,
		TimedOut:  timedOut,
		Aborted:   aborted,
		TimeoutMs: spec.TimeoutMs,
		Stdout:    finalOutput(handle.CollectedStdout()),
		Stderr:    finalOutput(handle.CollectedStderr()),
	}, nil
}

// finalOutput projects a settled collect-mode reader into the final
// CollectedOutput shape.
func finalOutput(reader subprocess.OutputReader) subprocess.CollectedOutput {
	if reader == nil {
		return subprocess.CollectedOutput{}
	}
	read := reader.ReadFrom(0)
	return subprocess.CollectedOutput{Text: read.Text, Truncated: read.Lossy, SpillPath: read.SpillPath}
}

// Start spawns a background process and returns immediately; no timeout
// applies (callers stop it through the spec's cancellation signal).
func (e *Executor) Start(spec shell.ShellExecSpec) (shell.ShellProcess, error) {
	return e.startArgv(spec, e.argv(spec))
}

func (e *Executor) startArgv(spec shell.ShellExecSpec, argv []string) (shell.ShellProcess, error) {
	parent := spec.Signal
	if parent == nil {
		parent = context.Background()
	}
	handle, spawnErr := e.sub.Spawn(parent, e.spawnSpec(spec, argv, e.cfg.MaxOutputBytes, parent))
	proc := &backgroundProcess{handle: handle, doneCh: make(chan struct{}), status: shell.ProcessRunning}
	if spawnErr != nil {
		// A spawn failure produces no process output, so the subprocess
		// service has nothing to buffer; the note is delivered exactly
		// once through the read path.
		proc.spawnFailureNote = fmt.Sprintf("spawn failed: %v", spawnErr)
		proc.status = shell.ProcessKilled
		close(proc.doneCh)
		return proc, nil
	}
	go func() {
		outcome, _ := handle.Outcome()
		proc.mu.Lock()
		// Any signal termination is killed, including a command
		// signaling itself.
		if proc.status == shell.ProcessRunning {
			if (spec.Signal != nil && spec.Signal.Err() != nil) || outcome.Signal != "" {
				proc.status = shell.ProcessKilled
			} else {
				proc.status = shell.ProcessCompleted
			}
		}
		proc.exitCode = outcome.ExitCode
		proc.signal = outcome.Signal
		proc.mu.Unlock()
		close(proc.doneCh)
	}()
	return proc, nil
}

// backgroundProcess is the live background handle; buffered output remains
// readable after exit.
type backgroundProcess struct {
	handle subprocess.Handle
	doneCh chan struct{}

	mu     sync.Mutex
	status shell.ShellProcessStatus

	stdoutOffset int
	stderrOffset int
	// consumeOnce mirrors the official exactly-once spawn-failure note.
	spawnFailureNote string
	consumed         bool

	exitCode int
	signal   string
}

func (p *backgroundProcess) Status() shell.ShellProcessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *backgroundProcess) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

func (p *backgroundProcess) Signal() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.signal
}

func (p *backgroundProcess) Done() <-chan struct{} { return p.doneCh }

func (p *backgroundProcess) Sandbox() *shell.ShellSandboxInfo { return nil }

// ReadOutput reads output produced since the previous read (consuming —
// consecutive reads never re-deliver). Reads that lost data flag lossy and
// point at the spill files.
func (p *backgroundProcess) ReadOutput() shell.ShellProcessRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out, errRead subprocess.OutputRead
	if p.handle != nil {
		if reader := p.handle.CollectedStdout(); reader != nil {
			out = reader.ReadFrom(p.stdoutOffset)
			p.stdoutOffset = out.NextOffset
		}
		if reader := p.handle.CollectedStderr(); reader != nil {
			errRead = reader.ReadFrom(p.stderrOffset)
			p.stderrOffset = errRead.NextOffset
		}
	}
	// A failed spawn never produced process output, so the note and real
	// stderr text are mutually exclusive.
	errText := errRead.Text
	if errText == "" && p.spawnFailureNote != "" && !p.consumed {
		errText = p.spawnFailureNote
		p.consumed = true
	}
	// Single newline between sections: stdout chunks usually end with one
	// already; add it only when missing.
	separator := ""
	if out.Text != "" && !strings.HasSuffix(out.Text, "\n") {
		separator = "\n"
	}
	delta := out.Text
	if errText != "" {
		delta = out.Text + separator + "[stderr]\n" + errText
	}
	return shell.ShellProcessRead{
		Delta:           delta,
		Lossy:           out.Lossy || errRead.Lossy,
		StdoutSpillPath: out.SpillPath,
		StderrSpillPath: errRead.SpillPath,
	}
}

// processCwd mirrors the official process.cwd() fallback.
func processCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}

// lookPath resolves a binary from PATH (test seam).
func lookPath(name string) (string, error) {
	return exec.LookPath(name)
}
