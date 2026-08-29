package subprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type handle struct {
	pid int

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	collectedStdout *outputCollector
	collectedStderr *outputCollector

	// parentWriteEnds are the parent's copies of the child-bound write
	// ends, closed right after start so EOF is observable once the child
	// tree lets go.
	parentWriteEnds []*os.File
	// collectPipes are the harness-owned collect-mode read ends,
	// force-closed at the drain boundary. A pipe-mode stream belongs to
	// the caller and is never force-closed here.
	collectPipes  []*os.File
	pumpDone      []chan struct{}
	collectors    []*outputCollector
	parentEndsMu  sync.Mutex
	closeAtStart  bool
	parentsClosed bool

	cmd     *exec.Cmd
	graceMs int

	mu               sync.Mutex
	settled          bool
	outcome          Outcome
	spawnErr         error
	done             chan struct{}
	terminateStarted bool
	graceTimer       *time.Timer
	treeExitObserved bool

	processExited atomic.Bool
}

func (h *handle) Pid() int              { return h.pid }
func (h *handle) Stdin() io.WriteCloser { return h.stdin }
func (h *handle) Stdout() io.ReadCloser { return h.stdout }
func (h *handle) Stderr() io.ReadCloser { return h.stderr }

func (h *handle) CollectedStdout() OutputReader {
	if h.collectedStdout == nil {
		return nil
	}
	return collectedReader{h.collectedStdout}
}

func (h *handle) CollectedStderr() OutputReader {
	if h.collectedStderr == nil {
		return nil
	}
	return collectedReader{h.collectedStderr}
}

func (h *handle) Done() <-chan struct{} { return h.done }

// collectedReader adapts the concrete collector to the public face.
type collectedReader struct{ c *outputCollector }

func (r collectedReader) ReadFrom(fromByte int) OutputRead { return r.c.readFrom(fromByte) }

// Outcome blocks until settlement and returns the exit facts; the error is
// non-nil only for spawn-level failures.
func (h *handle) Outcome() (Outcome, error) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outcome, h.spawnErr
}

// Terminate begins the SIGTERM → GraceMs → SIGKILL escalation on the
// process tree (Windows force-terminates immediately) — the seam's only
// consumer-facing termination verb. Idempotent, a no-op once the tree is
// gone (the pid may be reused), and also triggered by the spawn context's
// cancellation.
func (h *handle) Terminate() {
	h.mu.Lock()
	if h.treeExitObserved || h.terminateStarted {
		h.mu.Unlock()
		return
	}
	h.terminateStarted = true
	// Observe from the first termination tier onward, even when inherited
	// pipes delay Done and no consumer has begun its own teardown wait.
	h.treeExitObservedLocked()
	grace := time.Duration(h.graceMs) * time.Millisecond
	h.graceTimer = time.AfterFunc(grace, func() { h.kill("SIGKILL") })
	h.mu.Unlock()
	h.kill("SIGTERM")
}

// treeExitObservedLocked runs (or joins) the single whole-tree exit
// observer; the first confirmed absence is a permanent no-more-signals
// boundary that cancels a pending escalation before this process-group id
// can be reused.
func (h *handle) treeExitObservedLocked() bool {
	if h.treeExitObserved {
		return true
	}
	if !h.treeAlive() {
		h.treeExitObserved = true
		if h.graceTimer != nil {
			h.graceTimer.Stop()
			h.graceTimer = nil
		}
	}
	return h.treeExitObserved
}

// treeAlive reports whether the detached tree's root (or POSIX group) is
// still alive.
func (h *handle) treeAlive() bool {
	if h.treeExitObserved {
		return false
	}
	if h.pid <= 0 {
		return false
	}
	if isWindows() {
		// Windows has no group-liveness probe; the direct child's exit is
		// the observable boundary (taskkill /T already took the tree).
		return !h.processExited.Load()
	}
	// A group containing only unreaped zombies still answers kill(0), but
	// it can execute no work and cannot be signalled into quiescence.
	return processGroupAlive(h.pid)
}

// kill is the escalation's tier primitive (not on the Handle face). It
// guards on TREE liveness, not outcome settlement: a TERM-trapping helper
// can outlive the settled direct child and must stay signalable, while a
// fully-dead tree (possible pid reuse) must not be re-signalled by a later
// tier.
func (h *handle) kill(signalName string) {
	h.mu.Lock()
	observed := h.treeExitObservedLocked()
	h.mu.Unlock()
	if observed {
		return
	}
	if !h.treeAlive() {
		return
	}
	defaultSignalTree(h.pid, signalName, h.processExited.Load())
}

// WaitForExit waits until the process tree has exited — the tree, not just
// the direct child, so a still-running helper is observable before
// teardown returns. It returns false when ctx ends first.
func (h *handle) WaitForExit(ctx context.Context) bool {
	for {
		h.mu.Lock()
		observed := h.treeExitObservedLocked()
		h.mu.Unlock()
		if observed {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Spawn starts one isolated detached process tree with the spec's
// per-stream stdio dispositions. Runtime exits resolve Done with an
// Outcome; only spawn failures carry a non-nil error. The context is the
// abort signal: its cancellation triggers Terminate on the process tree
// (the caller owns deadlines and cause classification; this seam only
// reacts to the abort).
func Spawn(ctx context.Context, spec SpawnSpec) (Handle, error) {
	if len(spec.Argv) == 0 || strings.TrimSpace(spec.Argv[0]) == "" {
		return nil, errors.New(`subprocess: spawn spec requires a non-empty argv (argv[0] is the program)`)
	}
	if spec.Cwd == "" {
		return nil, errors.New(`subprocess: spawn spec requires an explicit cwd (this seam applies no defaults)`)
	}
	if spec.GraceMs <= 0 {
		return nil, errors.New(`subprocess: spawn spec requires a positive graceMs (this seam applies no defaults)`)
	}
	if spec.GraceMs > maxTimerDelayMs {
		return nil, fmt.Errorf("subprocess: graceMs %d exceeds one timer delay ceiling", spec.GraceMs)
	}
	if spec.Stdio.Stdin == nil || spec.Stdio.Stdout == nil || spec.Stdio.Stderr == nil {
		return nil, errors.New(`subprocess: stdio dispositions must all be explicit (this seam applies no defaults)`)
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %v", ErrAborted, ctx.Err())
	}

	h := &handle{
		pid:     -1,
		graceMs: spec.GraceMs,
		done:    make(chan struct{}),
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Cwd
	cmd.Env = envSlice(ChildEnv(spec.Env))
	if err := configureDetached(cmd); err != nil {
		return nil, fmt.Errorf("subprocess: detach: %w", err)
	}
	h.cmd = cmd

	var batchStdin io.WriteCloser
	switch mode := spec.Stdio.Stdin.(type) {
	case StdinIgnore:
		cmd.Stdin = nil // /dev/null on POSIX, an empty handle on Windows
	case StdinPipe:
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("subprocess: stdin pipe: %w", err)
		}
		h.stdin = pipe
	case StdinData:
		pipe, err := cmd.StdinPipe()
		if err != nil {
			return nil, fmt.Errorf("subprocess: stdin pipe: %w", err)
		}
		batchStdin = pipe
	default:
		return nil, fmt.Errorf("subprocess: unsupported stdin disposition %T", mode)
	}

	rawStdout, err := wireOutput(cmd, spec.Stdio.Stdout, "stdout", h)
	if err != nil {
		return nil, err
	}
	h.stdout = rawStdout
	rawStderr, err := wireOutput(cmd, spec.Stdio.Stderr, "stderr", h)
	if err != nil {
		return nil, err
	}
	h.stderr = rawStderr

	if err := cmd.Start(); err != nil {
		h.closeParentWriteEnds()
		h.closeCollectPipes()
		return nil, fmt.Errorf("subprocess: spawn %q: %w", spec.Argv[0], err)
	}
	h.closeParentWriteEnds()
	h.pid = cmd.Process.Pid

	if batchStdin != nil {
		// Batch stdin is written and closed up front; process exit and
		// captured output remain authoritative, so write errors (EPIPE)
		// are best-effort.
		payload, _ := spec.Stdio.Stdin.(StdinData)
		_, _ = io.WriteString(batchStdin, string(payload))
		_ = batchStdin.Close()
	}

	if ctx != nil {
		// The escalation reacts to abort; Done removes the hook.
		stop := context.AfterFunc(ctx, h.Terminate)
		go func() {
			<-h.done
			stop()
		}()
	}

	// Process-exit observation: with file-based stdio wiring cmd.Wait
	// returns at process exit (no cmd-owned copying goroutines), which is
	// the exit event. The drain boundary then bounds still-open collected
	// pipes by the same grace that governs kills — a surviving descendant
	// that inherited a pipe must not hold the outcome open indefinitely.
	waitDone := make(chan struct{})
	go func() {
		waitErr := cmd.Wait()
		h.processExited.Store(true)
		h.settle(waitErr)
		close(waitDone)
	}()
	go h.awaitPumpsThenSeal(waitDone)
	return h, nil
}

// wireOutput attaches one output disposition, returning the raw pipe for
// pipe mode (caller-owned).
func wireOutput(cmd *exec.Cmd, mode OutputMode, label string, h *handle) (io.ReadCloser, error) {
	switch m := mode.(type) {
	case OutputInherit:
		if label == "stdout" {
			cmd.Stdout = os.Stdout
		} else {
			cmd.Stderr = os.Stderr
		}
		return nil, nil
	case OutputPipe:
		read, write, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("subprocess: %s pipe: %w", label, err)
		}
		setCmdOutput(cmd, label, write)
		h.trackParentWriteEnd(write)
		if label == "stdout" {
			h.stdout = read
		} else {
			h.stderr = read
		}
		return read, nil
	case OutputCollect:
		if m.MaxBytes <= 0 {
			return nil, fmt.Errorf("subprocess: %s collect requires a positive maxBytes", label)
		}
		read, write, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("subprocess: %s pipe: %w", label, err)
		}
		setCmdOutput(cmd, label, write)
		h.trackParentWriteEnd(write)
		collector := newOutputCollector(m.MaxBytes, m.SpillMaxBytes, label, "")
		h.trackCollector(read, collector)
		pumpDone := make(chan struct{})
		h.trackPump(pumpDone)
		go pumpCollector(read, collector, pumpDone)
		return nil, nil
	default:
		return nil, fmt.Errorf("subprocess: unsupported %s disposition %T", label, mode)
	}
}

func setCmdOutput(cmd *exec.Cmd, label string, file *os.File) {
	if label == "stdout" {
		cmd.Stdout = file
	} else {
		cmd.Stderr = file
	}
}

// pumpCollector ingests one collect stream until EOF or the pipe is
// force-closed at the drain boundary.
func pumpCollector(read *os.File, collector *outputCollector, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 64*1024)
	for {
		n, err := read.Read(buf)
		if n > 0 {
			collector.push(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// trackParentWriteEnd registers a parent-held write end for closing after
// start.
func (h *handle) trackParentWriteEnd(file *os.File) {
	h.parentEndsMu.Lock()
	defer h.parentEndsMu.Unlock()
	if h.closeAtStart {
		_ = file.Close()
		return
	}
	h.parentWriteEnds = append(h.parentWriteEnds, file)
}

func (h *handle) trackCollector(read *os.File, collector *outputCollector) {
	h.parentEndsMu.Lock()
	defer h.parentEndsMu.Unlock()
	h.collectPipes = append(h.collectPipes, read)
	h.collectors = append(h.collectors, collector)
	if collector.label == "stdout" {
		h.collectedStdout = collector
	} else {
		h.collectedStderr = collector
	}
}

func (h *handle) trackPump(done chan struct{}) {
	h.parentEndsMu.Lock()
	defer h.parentEndsMu.Unlock()
	h.pumpDone = append(h.pumpDone, done)
}

// closeParentWriteEnds closes the parent's copies of the child-bound write
// ends after start.
func (h *handle) closeParentWriteEnds() {
	h.parentEndsMu.Lock()
	defer h.parentEndsMu.Unlock()
	for _, file := range h.parentWriteEnds {
		_ = file.Close()
	}
	h.closeAtStart = true
	h.parentWriteEnds = nil
}

func (h *handle) closeCollectPipes() {
	for _, pipe := range h.collectPipes {
		_ = pipe.Close()
	}
}

func (h *handle) sealCollectors() {
	for _, collector := range h.collectors {
		collector.seal()
	}
}

// awaitPumpsThenSeal enforces the drain boundary: after the process exits,
// the same bounded grace that governs kills bounds the close wait for
// still-open collected pipes. Only harness-collected pipes are
// force-closed; a pipe-mode stream belongs to the caller and closes with
// the child tree.
func (h *handle) awaitPumpsThenSeal(waitDone chan struct{}) {
	<-waitDone
	drain := time.NewTimer(time.Duration(h.graceMs) * time.Millisecond)
	defer drain.Stop()
	for _, done := range h.pumpDone {
		select {
		case <-done:
		case <-drain.C:
			h.closeCollectPipes()
			<-done
		}
	}
	h.sealCollectors()
}

// settle resolves Done exactly once; a spawn failure rejects.
func (h *handle) settle(waitErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.settled {
		return
	}
	h.settled = true
	if waitErr != nil && h.cmd.ProcessState == nil {
		// No meaningful close outcome follows a spawn failure.
		h.spawnErr = waitErr
	} else {
		outcome := Outcome{ExitCode: -1}
		if h.cmd.ProcessState != nil {
			outcome.ExitCode = h.cmd.ProcessState.ExitCode()
			outcome.Signal = exitSignal(h.cmd.ProcessState)
		}
		h.outcome = outcome
	}
	close(h.done)
}

// envSlice flattens an environment map deterministically.
func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}
