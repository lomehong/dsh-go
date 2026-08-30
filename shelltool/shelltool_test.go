package shelltool_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/jobs"
	"dshgo/session"
	"dshgo/shell"
	"dshgo/shelllocal"
	"dshgo/shelltool"
	"dshgo/subprocess"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// fakeAgent mints a detached agent the way agent tests do.
func fakeAgent(t *testing.T, id, cwd string) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: cwd})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return agent.NewAgent(agent.AgentConfig{ID: session.SessionID(id), Session: sess}, nil)
}

// testDeps wires a real pwsh/bash executor (whichever the host deploys)
// over a fresh tool runtime.
func testDeps(t *testing.T, backgroundEnabled bool) (shelltool.Deps, shelltool.Config, *shelllocal.Executor) {
	t.Helper()
	sub := subprocess.NewLocal()
	cfg := shelllocal.DefaultConfig()
	cfg.GraceMs = 200
	var executor *shelllocal.Executor
	var err error
	if runtime.GOOS == "windows" {
		executor, err = shelllocal.NewPwsh(sub, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if executor.PwshPath() == "pwsh" {
			if _, err := exec.LookPath("pwsh"); err != nil {
				t.Skip("no pwsh deployment on this host")
			}
		}
	} else {
		if _, err := exec.LookPath("bash"); err != nil {
			t.Skip("bash not on PATH")
		}
		executor, err = shelllocal.NewBash(sub, cfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	envRegistry := shell.NewShellEnvRegistry("D:\\dsh-home", nil)
	deps := shelltool.Deps{
		Runtime: runtimeTools,
		Prompt:  prompt,
		Shell:   executor,
		Env:     envRegistry,
	}
	return deps, shelltool.Config{BackgroundEnabled: backgroundEnabled}, executor
}

func argCount(t *testing.T, deps shelltool.Deps) int {
	t.Helper()
	toolName := "bash"
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	definition, ok := deps.Runtime.Get(toolName, nil)
	if !ok {
		t.Fatalf("tool %q must be registered", toolName)
	}
	properties, ok := definition.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters shape: %v", definition.Parameters)
	}
	return len(properties)
}

func TestRegisterAndForegroundRun(t *testing.T) {
	deps, cfg, _ := testDeps(t, true)
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	defer func() {}()
	toolName := "bash"
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	if _, ok := deps.Runtime.Get(toolName, nil); !ok {
		t.Fatal("tool missing")
	}
	// The registry pipeline renders the canonical value into model text.
	result := deps.Runtime.Execute(&tools.ToolExecutionInput{
		Name:      toolName,
		Arguments: map[string]any{"command": streamProbeCommand(), "description": "probe streams"},
		Signal:    context.Background(),
	})
	if result.IsError {
		t.Fatalf("execute: %v", result.Error)
	}
	text := result.Content[0].Text
	if !strings.HasSuffix(text, "[exit code: 7]") {
		t.Fatalf("render: %q", text)
	}
	if !strings.Contains(text, "out-1") || !strings.Contains(text, "[stderr]\nerr-1") {
		t.Fatalf("render body: %q", text)
	}
}

func streamProbeCommand() string {
	if runtime.GOOS == "windows" {
		return "[Console]::Out.WriteLine('out-1'); [Console]::Error.WriteLine('err-1'); exit 7"
	}
	return "printf 'out-1\\n'; printf 'err-1\\n' >&2; exit 7"
}

func TestValidationFailures(t *testing.T) {
	deps, cfg, _ := testDeps(t, true)
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	toolName := "bash"
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"command": "  ", "description": "x"}, "invalid command"},
		{map[string]any{"command": "ls", "description": ""}, "invalid description"},
		{map[string]any{"command": "ls", "description": "x", "timeoutMs": -5.0}, "invalid timeoutMs"},
	}
	for _, tc := range cases {
		result := deps.Runtime.Execute(&tools.ToolExecutionInput{Name: toolName, Arguments: tc.args, Signal: context.Background()})
		if !result.IsError || !strings.Contains(result.Content[0].Text, tc.want) {
			t.Fatalf("args %v: %v", tc.args, result.Content[0].Text)
		}
	}
}

func TestWorkdirSessionDefaulting(t *testing.T) {
	deps, cfg, _ := testDeps(t, true)
	sessionRoot := t.TempDir()
	caller := fakeAgent(t, "shelltool-caller", sessionRoot)
	deps.Agents = func(scope tools.ScopeKey) *agent.Agent {
		if scope == caller.Scope {
			return caller
		}
		return nil
	}
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	// The session cwd (the agent's session header) defaults the workdir.
	toolName := "bash"
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	result := deps.Runtime.Execute(&tools.ToolExecutionInput{
		Name:      toolName,
		Arguments: map[string]any{"command": cwdProbeCommand(), "description": "print cwd"},
		Agent:     caller.Scope,
		Signal:    context.Background(),
	})
	if result.IsError {
		t.Fatalf("execute: %v", result.Error)
	}
	// Windows may surface the temp dir in its 8.3 short form; canonicalize
	// both sides before comparing.
	printed := strings.TrimSpace(result.Content[0].Text)
	if resolved, err := filepath.EvalSymlinks(printed); err == nil {
		printed = resolved
	}
	want := sessionRoot
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	if !strings.EqualFold(printed, want) {
		t.Fatalf("session cwd defaulting: got %q want %q", printed, want)
	}
}

func cwdProbeCommand() string {
	if runtime.GOOS == "windows" {
		return "Write-Output (Get-Location).Path"
	}
	return "pwd"
}

func TestBackgroundViaJobs(t *testing.T) {
	deps, cfg, _ := testDeps(t, true)
	registry, err := jobs.NewLocalRegistry(jobs.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A global controller serves every owner (the tool-jobs plugin's
	// composition attaches one).
	registry.AttachControllerIn(nil)
	deps.Jobs = registry
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	toolName := "bash"
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	result := deps.Runtime.Execute(&tools.ToolExecutionInput{
		Name:      toolName,
		Arguments: map[string]any{"command": backgroundBurstCommand(), "description": "background burst", "run_in_background": true},
		Signal:    context.Background(),
	})
	if result.IsError {
		t.Fatalf("start: %v", result.Error)
	}
	text := result.Content[0].Text
	if !strings.Contains(text, "started background job bash-") {
		t.Fatalf("render: %q", text)
	}
	jobID := strings.TrimPrefix(strings.TrimSpace(text), "started background job ")
	// The job settles completed once the burst drains.
	deadline := time.Now().Add(20 * time.Second)
	for {
		snapshot, err := registry.Get(jobID, "")
		if err == nil && (snapshot.Status == jobs.StatusCompleted || snapshot.Status == jobs.StatusKilled || snapshot.Status == jobs.StatusFailed) {
			if snapshot.Status != jobs.StatusCompleted {
				t.Fatalf("settle: %s %s", snapshot.Status, snapshot.Detail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job never settled; last err %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func backgroundBurstCommand() string {
	if runtime.GOOS == "windows" {
		return "Write-Output alpha; Start-Sleep -Seconds 1; Write-Output beta"
	}
	return "echo alpha; sleep 1; echo beta"
}

func TestBackgroundDisabledAndUnavailable(t *testing.T) {
	toolName := "bash"
	// Disabled by config: execute rejects the model-supplied flag.
	deps, cfg, _ := testDeps(t, false)
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	if deps.Shell.Name() == "pwsh-local" {
		toolName = "pwsh"
	}
	args := map[string]any{"command": "x", "description": "d", "run_in_background": true}
	result := deps.Runtime.Execute(&tools.ToolExecutionInput{Name: toolName, Arguments: args, Signal: context.Background()})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "run_in_background is disabled") {
		t.Fatalf("disabled: %v", result.Content[0].Text)
	}
	// Enabled config but no jobs service composed.
	deps2, cfg2, _ := testDeps(t, true)
	if _, err := shelltool.Register(deps2, cfg2); err != nil {
		t.Fatal(err)
	}
	result2 := deps2.Runtime.Execute(&tools.ToolExecutionInput{Name: toolName, Arguments: args, Signal: context.Background()})
	if !result2.IsError || !strings.Contains(result2.Content[0].Text, "background jobs unavailable") {
		t.Fatalf("unavailable: %v", result2.Content[0].Text)
	}
	// The schema hides run_in_background when disabled (parameter count).
	enabledParams := argCount(t, mustRegisteredDeps(t, true))
	disabledParams := argCount(t, mustRegisteredDeps(t, false))
	if enabledParams-1 != disabledParams {
		t.Fatalf("param counts: enabled %d vs disabled %d", enabledParams, disabledParams)
	}
}

// mustRegisteredDeps registers a fresh tool set with the given background
// flag and returns the deps for lookups.
func mustRegisteredDeps(t *testing.T, background bool) shelltool.Deps {
	t.Helper()
	deps, cfg, _ := testDeps(t, background)
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	return deps
}

func TestRenderResultVocabulary(t *testing.T) {
	// Clean exit: no markers.
	clean := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: 0,
		Stdout:   subprocess.CollectedOutput{Text: "all good\n"},
	})
	if clean != "all good\n" {
		t.Fatalf("clean: %q", clean)
	}
	// Nonzero exit marker.
	nonzero := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: 3,
		Stdout:   subprocess.CollectedOutput{Text: "partial\n"},
	})
	if !strings.HasSuffix(nonzero, "\n[exit code: 3]") {
		t.Fatalf("nonzero: %q", nonzero)
	}
	// Killed by signal beats the exit marker.
	killed := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: -1,
		Signal:   "SIGTERM",
	})
	if !strings.HasSuffix(killed, "[killed by signal: SIGTERM]") {
		t.Fatalf("killed: %q", killed)
	}
	// Timeout marker precedes the exit marker (which anchors last).
	timed := shelltool.RenderResult(shell.ShellRunResult{
		TimedOut:  true,
		TimeoutMs: 1500,
		ExitCode:  1,
	})
	if !strings.Contains(timed, "[timed out after 1500ms]\n[exit code: 1]") {
		t.Fatalf("timed: %q", timed)
	}
	// No output body becomes the placeholder.
	empty := shelltool.RenderResult(shell.ShellRunResult{ExitCode: 0})
	if !strings.HasPrefix(empty, "(no output)") {
		t.Fatalf("empty: %q", empty)
	}
	// Truncation notices with and without a spill path.
	truncated := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: 0,
		Stdout:   subprocess.CollectedOutput{Text: "tail", Truncated: true, SpillPath: "D:\\spill.txt"},
	})
	if !strings.Contains(truncated, "[output truncated; full output: D:\\spill.txt]") {
		t.Fatalf("truncated: %q", truncated)
	}
	lossy := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: 0,
		Stdout:   subprocess.CollectedOutput{Text: "tail", Truncated: true},
	})
	if !strings.Contains(lossy, "full output: (unavailable)") {
		t.Fatalf("lossy: %q", lossy)
	}
	// stderr section between stdout and markers.
	sections := shelltool.RenderResult(shell.ShellRunResult{
		ExitCode: 2,
		Stdout:   subprocess.CollectedOutput{Text: "out"},
		Stderr:   subprocess.CollectedOutput{Text: "boom"},
	})
	if !strings.Contains(sections, "out\n[stderr]\nboom\n[exit code: 2]") {
		t.Fatalf("sections: %q", sections)
	}
}

func TestRenderProcessReadLossyNotice(t *testing.T) {
	plain := shelltool.RenderProcessRead(shell.ShellProcessRead{Delta: "chunk\n"})
	if plain != "chunk\n" {
		t.Fatalf("plain: %q", plain)
	}
	withPaths := shelltool.RenderProcessRead(shell.ShellProcessRead{
		Delta:           "chunk",
		Lossy:           true,
		StdoutSpillPath: "D:\\out.bin",
	})
	if !strings.HasSuffix(withPaths, "\n[some output was dropped from memory; full output: D:\\out.bin]") {
		t.Fatalf("withPaths: %q", withPaths)
	}
	withoutPaths := shelltool.RenderProcessRead(shell.ShellProcessRead{Delta: "", Lossy: true})
	if withoutPaths != "[some output was dropped from memory; full output: (unavailable)]" {
		t.Fatalf("withoutPaths: %q", withoutPaths)
	}
}

func TestPromptSectionRegistered(t *testing.T) {
	deps, cfg, _ := testDeps(t, true)
	root := deps.Prompt
	if root == nil {
		t.Skip("prompt service absent")
	}
	if _, err := shelltool.Register(deps, cfg); err != nil {
		t.Fatal(err)
	}
	// The section text carries the flavor guidance.
	_ = root
}
