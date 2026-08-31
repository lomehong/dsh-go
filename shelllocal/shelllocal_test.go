package shelllocal

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dshgo/shell"
	"dshgo/subprocess"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.TimeoutMs = 5000
	cfg.MaxTimeoutMs = 8000
	cfg.MaxOutputBytes = 4000
	cfg.GraceMs = 500
	return cfg
}

func TestConfigValidationFailLoud(t *testing.T) {
	sub := subprocess.NewLocal()
	base := testConfig()
	for _, breakage := range []struct {
		name    string
		breaker func(*Config)
		field   string
	}{
		{"timeoutMs", func(c *Config) { c.TimeoutMs = 0 }, "timeoutMs"},
		{"maxTimeoutMs", func(c *Config) { c.MaxTimeoutMs = -1 }, "maxTimeoutMs"},
		{"maxOutputBytes", func(c *Config) { c.MaxOutputBytes = 0 }, "maxOutputBytes"},
		{"maxSpillBytes", func(c *Config) { c.MaxSpillBytes = -5 }, "maxSpillBytes"},
		{"graceMs", func(c *Config) { c.GraceMs = 0 }, "graceMs"},
	} {
		cfg := base
		breakage.breaker(&cfg)
		if _, err := NewBash(sub, cfg); err == nil || !strings.Contains(err.Error(), breakage.field) {
			t.Fatalf("%s: %v", breakage.name, err)
		}
	}
}

func TestResolveDefaultingAndCapping(t *testing.T) {
	sub := subprocess.NewLocal()
	cfg := testConfig()
	cfg.Cwd = "D:\\explicit-cwd"
	executor, err := NewBash(sub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Absent fields get the configured defaults; the timeout override is
	// clamped at maxTimeoutMs.
	spec := executor.Resolve(shell.ShellExecRequest{Command: "x"})
	if spec.Workdir != `D:\explicit-cwd` || spec.TimeoutMs != 5000 || spec.StdoutMaxBytes != 4000 {
		t.Fatalf("defaults: %+v", spec)
	}
	capped := executor.Resolve(shell.ShellExecRequest{Command: "x", TimeoutMs: 99_999})
	if capped.TimeoutMs != 8000 {
		t.Fatalf("cap: %d", capped.TimeoutMs)
	}
	// stdoutMaxBytes passes through for trusted in-process consumers.
	trusted := executor.Resolve(shell.ShellExecRequest{Command: "x", StdoutMaxBytes: 1 << 20})
	if trusted.StdoutMaxBytes != 1<<20 {
		t.Fatalf("trusted cap: %d", trusted.StdoutMaxBytes)
	}
	// Workdir override wins.
	over := executor.Resolve(shell.ShellExecRequest{Command: "x", Workdir: "D:\\elsewhere"})
	if over.Workdir != `D:\elsewhere` {
		t.Fatalf("workdir: %q", over.Workdir)
	}
	// An executor without a configured cwd falls back to the process cwd.
	noCwd, err := NewBash(sub, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	fallback := noCwd.Resolve(shell.ShellExecRequest{Command: "x"})
	if fallback.Workdir != processCwd() {
		t.Fatalf("fallback cwd: %q", fallback.Workdir)
	}
}

func TestArgvShapes(t *testing.T) {
	sub := subprocess.NewLocal()
	bash, _ := NewBash(sub, testConfig())
	spec := bash.Resolve(shell.ShellExecRequest{Command: "echo hi"})
	got := bash.argv(spec)
	if len(got) != 3 || got[0] != "bash" || got[1] != "-c" || got[2] != "echo hi" {
		t.Fatalf("bash argv: %v", got)
	}

	pwshCfg := testConfig()
	pwshCfg.PwshPath = `D:\pwsh\pwsh.exe`
	pwsh, err := NewPwsh(sub, pwshCfg)
	if err != nil {
		t.Fatal(err)
	}
	if pwsh.PwshPath() != `D:\pwsh\pwsh.exe` {
		t.Fatalf("configured pwsh path: %q", pwsh.PwshPath())
	}
	pspec := pwsh.Resolve(shell.ShellExecRequest{Command: "Get-ChildItem"})
	pgot := pwsh.argv(pspec)
	if len(pgot) != 6 || pgot[0] != `D:\pwsh\pwsh.exe` {
		t.Fatalf("pwsh argv: %v", pgot)
	}
	if pgot[1] != "-NoLogo" || pgot[2] != "-NoProfile" || pgot[3] != "-NonInteractive" || pgot[4] != "-Command" {
		t.Fatalf("pwsh flags: %v", pgot)
	}
	if !strings.HasPrefix(pgot[5], EncodingPreamble) || !strings.HasSuffix(pgot[5], "Get-ChildItem") {
		t.Fatalf("pwsh command preamble: %q", pgot[5])
	}
}

func TestSpawnSpecEnvLayering(t *testing.T) {
	sub := subprocess.NewLocal()
	bash, _ := NewBash(sub, testConfig())
	spec := bash.Resolve(shell.ShellExecRequest{
		Command: "x",
		Stdin:   "payload",
		Env:     map[string]string{"NO_COLOR": "0", "CALLER": "yes"},
		DshEnv:  map[string]string{"DSH_SESSION_ID": "s-1"},
	})
	sp := bash.spawnSpec(spec, bash.argv(spec), 1000, nil)
	// dshEnv beats the caller's env beats the terminal overrides.
	if got := *sp.Env["NO_COLOR"]; got != "0" {
		t.Fatalf("NO_COLOR: %q", got)
	}
	if got := *sp.Env["CALLER"]; got != "yes" {
		t.Fatalf("CALLER: %q", got)
	}
	if got := *sp.Env["DSH_SESSION_ID"]; got != "s-1" {
		t.Fatalf("DSH_SESSION_ID: %q", got)
	}
	if got := *sp.Env["TERM"]; got != "dumb" {
		t.Fatalf("TERM: %q", got)
	}
	// Stdin data rides through; absent stdin is ignore.
	if _, isData := sp.Stdio.Stdin.(subprocess.StdinData); !isData {
		t.Fatal("stdin must be StdinData when present")
	}
	noStdin := bash.spawnSpec(bash.Resolve(shell.ShellExecRequest{Command: "x"}), []string{"bash"}, 100, nil)
	if _, isIgnore := noStdin.Stdio.Stdin.(subprocess.StdinIgnore); !isIgnore {
		t.Fatal("absent stdin must be StdinIgnore")
	}
}

func TestResolvePwshPathProbing(t *testing.T) {
	// Explicit configuration is trusted as-is.
	if got := ResolvePwshPath(`D:\x\pwsh.exe`); got != `D:\x\pwsh.exe` {
		t.Fatalf("configured: %q", got)
	}
	// Candidate order: PS7 well-known first, then PATH entries (each
	// entry's whole-body quotes stripped), then the 5.1 last resort.
	paths := CandidatePwshPaths(`C:\PF`, `C:\Windows`, `C:\Store Apps;"D:\quoted dir"`)
	if paths[0] != `C:\PF\PowerShell\7\pwsh.exe` {
		t.Fatalf("first candidate: %q", paths[0])
	}
	if len(paths) != 4 || paths[1] != `C:\Store Apps\pwsh.exe` || paths[2] != `D:\quoted dir\pwsh.exe` || paths[3] != `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe` {
		t.Fatalf("candidates: %v", paths)
	}
	// A real file matches; a directory never does.
	dir := t.TempDir()
	if candidateExists(dir) {
		t.Fatal("directory must not match")
	}
	file := filepath.Join(dir, "pwsh.exe")
	if err := os.WriteFile(file, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !candidateExists(file) {
		t.Fatal("regular file must match")
	}
}

// realFlavor picks the shell flavor the host actually deploys: bash is the
// POSIX shell (the official profile disables tool-bash on win32, where the
// PATH "bash" is the WSL stub anyway); pwsh is the win32 shell.
type realFlavor struct {
	executor  *Executor
	streamCmd string
	sleepCmd  string
	burstCmd  string
}

func newRealExecutor(t *testing.T) *realFlavor {
	t.Helper()
	sub := subprocess.NewLocal()
	realConfig := func() Config {
		cfg := testConfig()
		// Load-tolerant deadlines for real-process spawns: the full suite
		// runs ~110 packages concurrently and Windows spawn+teardown can
		// exceed the 5s shared default, flaking completion tests. The
		// dedicated timeout tests override explicitly.
		cfg.TimeoutMs = 30000
		cfg.MaxTimeoutMs = 60000
		return cfg
	}
	if look, err := lookPath("bash"); err == nil && runtime.GOOS != "windows" {
		_ = look
		bash, err := NewBash(sub, realConfig())
		if err != nil {
			t.Fatal(err)
		}
		return &realFlavor{
			executor:  bash,
			streamCmd: "printf 'out-1\\n'; printf 'err-1\\n' >&2; exit 7",
			sleepCmd:  "sleep 30",
			burstCmd:  "echo alpha; sleep 1; echo beta; echo boom >&2",
		}
	}
	// win32: probe the pwsh deployment (PS7 store/MSI install, 5.1 fallback).
	cfg := realConfig()
	cfg.GraceMs = 200
	pwsh, err := NewPwsh(sub, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if pwsh.PwshPath() == "pwsh" {
		if _, err := lookPath("pwsh"); err != nil {
			t.Skip("no pwsh deployment on this host; shell integration needs the win32 shell")
		}
	}
	return &realFlavor{
		executor:  pwsh,
		streamCmd: "[Console]::Out.WriteLine('out-1'); [Console]::Error.WriteLine('err-1'); exit 7",
		sleepCmd:  "Start-Sleep -Seconds 30",
		burstCmd:  "Write-Output alpha; Start-Sleep -Seconds 1; Write-Output beta; [Console]::Error.WriteLine('boom')",
	}
}

// TestRunReal spawns a real shell end-to-end on the deployed flavor.
func TestRunReal(t *testing.T) {
	flavor := newRealExecutor(t)
	spec := flavor.executor.Resolve(shell.ShellExecRequest{Command: flavor.streamCmd})
	result, err := flavor.executor.Run(spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.ExitCode != 7 || result.Signal != "" || result.TimedOut || result.Aborted {
		t.Fatalf("outcome: %+v", result)
	}
	if !strings.Contains(result.Stdout.Text, "out-1") || !strings.Contains(result.Stderr.Text, "err-1") {
		t.Fatalf("streams: %q / %q", result.Stdout.Text, result.Stderr.Text)
	}
}

// TestTimeoutClassificationReal: only the executor's own timeout counts as
// timedOut (single first-cause classification).
func TestTimeoutClassificationReal(t *testing.T) {
	flavor := newRealExecutor(t)
	cfg := flavor.executor.cfg
	cfg.TimeoutMs = 400
	cfg.GraceMs = 100
	var executor *Executor
	if flavor.executor.name == "pwsh-local" {
		executor, _ = NewPwsh(flavor.executor.sub, cfg)
	} else {
		executor, _ = NewBash(flavor.executor.sub, cfg)
	}
	spec := executor.Resolve(shell.ShellExecRequest{Command: flavor.sleepCmd})
	started := time.Now()
	result, err := executor.Run(spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.TimedOut || result.Aborted {
		t.Fatalf("classification: %+v", result)
	}
	// A timeout kill is tree-terminated: the exact exit fact is platform
	// dependent (POSIX signal, Windows force-terminate), but the cause
	// classification is not.
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("timeout kill took too long: %v", elapsed)
	}
}

// TestAbortClassificationReal: the caller's cancellation is the first
// cause and reports as aborted, never timedOut.
func TestAbortClassificationReal(t *testing.T) {
	flavor := newRealExecutor(t)
	cfg := flavor.executor.cfg
	cfg.GraceMs = 100
	var executor *Executor
	if flavor.executor.name == "pwsh-local" {
		executor, _ = NewPwsh(flavor.executor.sub, cfg)
	} else {
		executor, _ = NewBash(flavor.executor.sub, cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	spec := executor.Resolve(shell.ShellExecRequest{Command: flavor.sleepCmd, Signal: ctx})
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	result, err := executor.Run(spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !result.Aborted || result.TimedOut {
		t.Fatalf("classification: %+v", result)
	}
}

// TestBackgroundIncrementalReadsReal: consecutive reads never re-deliver;
// stderr arrives in a marked section; status settles exactly once.
func TestBackgroundIncrementalReadsReal(t *testing.T) {
	flavor := newRealExecutor(t)
	spec := flavor.executor.Resolve(shell.ShellExecRequest{Command: flavor.burstCmd})
	proc, err := flavor.executor.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	<-proc.Done()
	if proc.Status() != shell.ProcessCompleted || proc.ExitCode() != 0 {
		t.Fatalf("settle: %s %d %q", proc.Status(), proc.ExitCode(), proc.Signal())
	}
	var whole string
	for {
		read := proc.ReadOutput()
		if read.Delta == "" {
			break
		}
		whole += read.Delta
	}
	if !strings.Contains(whole, "alpha") || !strings.Contains(whole, "beta") {
		t.Fatalf("stdout deltas: %q", whole)
	}
	if !strings.Contains(whole, "[stderr]\nboom") {
		t.Fatalf("stderr section: %q", whole)
	}
	// A read after exhaustion returns an empty delta, never a re-delivery.
	if again := proc.ReadOutput(); again.Delta != "" {
		t.Fatalf("re-delivery: %q", again.Delta)
	}
}

// TestBackgroundKillClassificationReal: cancelling the spec's signal
// settles a still-running process as killed.
func TestBackgroundKillClassificationReal(t *testing.T) {
	flavor := newRealExecutor(t)
	ctx, cancel := context.WithCancel(context.Background())
	spec := flavor.executor.Resolve(shell.ShellExecRequest{Command: flavor.sleepCmd, Signal: ctx})
	proc, err := flavor.executor.Start(spec)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case <-proc.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("done never settled after cancellation")
	}
	if proc.Status() != shell.ProcessKilled {
		t.Fatalf("status: %s", proc.Status())
	}
}

func TestSpawnFailureSettlesKilledThroughReadPath(t *testing.T) {
	// A nonexistent executable fails at spawn; the handle reports killed
	// and the read path surfaces the note exactly once.
	cfg := testConfig()
	executor := &Executor{sub: subprocess.NewLocal(), cfg: cfg, name: "bash-local"}
	spec := shell.ShellExecSpec{
		Command:        "x",
		Workdir:        t.TempDir(),
		TimeoutMs:      cfg.TimeoutMs,
		StdoutMaxBytes: cfg.MaxOutputBytes,
	}
	proc, err := executor.startArgv(spec, []string{`D:\definitely-not-a-real-binary-xyz`})
	if err != nil {
		t.Fatalf("start must settle, not error: %v", err)
	}
	<-proc.Done()
	if proc.Status() != shell.ProcessKilled {
		t.Fatalf("status: %s", proc.Status())
	}
	read := proc.ReadOutput()
	if !strings.Contains(read.Delta, "spawn failed") {
		t.Fatalf("note: %q", read.Delta)
	}
	if second := proc.ReadOutput(); strings.Contains(second.Delta, "spawn failed") {
		t.Fatalf("note delivered twice: %q", second.Delta)
	}
}
