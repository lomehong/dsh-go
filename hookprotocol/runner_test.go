package hookprotocol

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testShellInvocation resolves through the platform shell so the same hook
// command line runs on both Windows and Unix.
func testShellInvocation(command string) (string, []string) {
	if os.PathSeparator == '\\' {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}

// withSeamShell swaps the invocation seam for the platform shell and
// restores the prior value when the test ends.
func withSeamShell(t *testing.T) {
	t.Helper()
	previous := resolveInvocation
	resolveInvocation = testShellInvocation
	t.Cleanup(func() { resolveInvocation = previous })
}

func fixedClock() func() int64 {
	now := int64(1_000)
	return func() int64 {
		now += 5
		return now
	}
}

func TestRunHookEchoesPayloadOnStdin(t *testing.T) {
	withSeamShell(t)
	// `sort` reads stdin and echoes a single line unchanged; it takes no
	// quoted arguments, which keeps the Windows argv escaping out of the
	// way.
	command := "sort"
	if os.PathSeparator != '\\' {
		command = "cat"
	}
	result := RunHook(CommandHook{Command: command}, RunHookOptions{
		Payload:           map[string]any{"session_id": "s1", "hook_event_name": "PreToolUse"},
		Signal:            context.Background(),
		TrailingNewline:   true,
		DefaultTimeoutMs:  10_000,
		ExpectedEventName: "PreToolUse",
	}, fixedClock())
	if result.Output.ExitCode == nil || *result.Output.ExitCode != 0 {
		t.Fatalf("exit = %+v (err %q out %q)", result.Output.ExitCode, result.Output.Stderr, result.Output.Stdout)
	}
	if !strings.Contains(result.Output.Stdout, "session_id") || !strings.Contains(result.Output.Stdout, "s1") {
		t.Fatalf("stdout = %q, want the stdin payload echoed (stderr %q)", result.Output.Stdout, result.Output.Stderr)
	}
}

func TestRunHookBlockingExitCarriesStderr(t *testing.T) {
	withSeamShell(t)
	command := "echo hook-said-no 1>&2 & exit 2"
	if os.PathSeparator != '\\' {
		command = "echo hook-said-no 1>&2; exit 2"
	}
	result := RunHook(CommandHook{Command: command}, RunHookOptions{
		Payload:          map[string]any{},
		Signal:           context.Background(),
		DefaultTimeoutMs: 10_000,
	}, fixedClock())
	if result.Output.Decision != DecisionBlock {
		t.Fatalf("decision = %q, want block", result.Output.Decision)
	}
	if result.Output.Reason == nil || !strings.Contains(*result.Output.Reason, "hook-said-no") {
		t.Fatalf("reason = %+v, want stderr", result.Output.Reason)
	}
	if result.Output.ExitCode == nil || *result.Output.ExitCode != 2 {
		t.Fatalf("exit = %+v, want 2", result.Output.ExitCode)
	}
}

func TestRunHookStructuredStdout(t *testing.T) {
	// The structured JSON is written to a temp file the hook prints, so
	// the command line carries no quotes (Go's Windows argv escaping would
	// otherwise hand cmd literal backslash-quotes).
	payloadFile := filepath.Join(t.TempDir(), "payload.json")
	stdout := `{"hookSpecificOutput":{"hookEventName":"Stop","permissionDecision":"deny","permissionDecisionReason":"no stop"}}`
	if err := os.WriteFile(payloadFile, []byte(stdout), 0o644); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	previous := resolveInvocation
	if os.PathSeparator == '\\' {
		resolveInvocation = func(string) (string, []string) { return "cmd", []string{"/c", "type", payloadFile} }
	} else {
		resolveInvocation = func(string) (string, []string) { return "sh", []string{"-c", "cat " + payloadFile} }
	}
	t.Cleanup(func() { resolveInvocation = previous })
	result := RunHook(CommandHook{Command: "print-payload"}, RunHookOptions{
		Payload:           map[string]any{},
		Signal:            context.Background(),
		DefaultTimeoutMs:  10_000,
		ExpectedEventName: "Stop",
	}, fixedClock())
	if result.Output.Decision != DecisionDeny {
		t.Fatalf("decision = %q (stdout %q)", result.Output.Decision, result.Output.Stdout)
	}
	if result.Output.Reason == nil || *result.Output.Reason != "no stop" {
		t.Fatalf("reason = %+v", result.Output.Reason)
	}
	if result.Output.HookEventName != "Stop" {
		t.Fatalf("hookEventName = %q", result.Output.HookEventName)
	}
}

func TestRunHookEnvAndWorkdir(t *testing.T) {
	withSeamShell(t)
	dir := t.TempDir()
	command := "echo %DSH_HOOK_PROBE% %CD%"
	if os.PathSeparator != '\\' {
		command = "echo $DSH_HOOK_PROBE $PWD"
	}
	result := RunHook(CommandHook{Command: command}, RunHookOptions{
		Payload:          map[string]any{},
		Env:              map[string]string{"DSH_HOOK_PROBE": "probe-value"},
		CWD:              dir,
		Signal:           context.Background(),
		DefaultTimeoutMs: 10_000,
	}, fixedClock())
	if !strings.Contains(result.Output.Stdout, "probe-value") {
		t.Fatalf("stdout = %q, want the extra env var", result.Output.Stdout)
	}
	if !strings.Contains(result.Output.Stdout, dir) {
		t.Fatalf("stdout = %q, want workdir %q", result.Output.Stdout, dir)
	}
}

func TestRunHookTimeoutIsNonBlocking(t *testing.T) {
	withSeamShell(t)
	// R11: the command must outlast the assertion cap by a wide margin —
	// sleep 5 against a 4s cap left only 1s of slack, so under a fully
	// loaded machine the natural exit occasionally beat the timeout kill
	// and failed the test. A 30s command makes the kill the only way the
	// call returns inside the cap; the assertion is unchanged.
	command := "ping -n 30 127.0.0.1 >nul"
	if os.PathSeparator != '\\' {
		command = "sleep 30"
	}
	started := time.Now()
	result := RunHook(CommandHook{Command: command, TimeoutSec: floatPtr(1)}, RunHookOptions{
		Payload:          map[string]any{},
		Signal:           context.Background(),
		DefaultTimeoutMs: 60_000,
	}, fixedClock())
	if result.Output.ExitCode != nil {
		t.Fatalf("exit = %+v, want nil after a timeout kill", result.Output.ExitCode)
	}
	if result.Output.Decision != "" {
		t.Fatalf("decision = %q, want none", result.Output.Decision)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("timeout kill took %v", elapsed)
	}
}

func TestRunHookSpawnFailureIsNonBlocking(t *testing.T) {
	withSeamShell(t)
	previous := resolveInvocation
	resolveInvocation = func(string) (string, []string) {
		return "dsh-hook-definitely-missing-executable", nil
	}
	t.Cleanup(func() { resolveInvocation = previous })
	result := RunHook(CommandHook{Command: "anything"}, RunHookOptions{
		Payload:          map[string]any{},
		Signal:           context.Background(),
		DefaultTimeoutMs: 10_000,
	}, fixedClock())
	if result.Output.ExitCode != nil {
		t.Fatalf("exit = %+v, want nil", result.Output.ExitCode)
	}
	if result.Output.Stderr == "" {
		t.Fatal("spawn failure message should land on stderr for the record")
	}
}

func TestRunHookSignalCancellationIsNonBlocking(t *testing.T) {
	withSeamShell(t)
	// R11: same margin argument as the timeout test — a 30s command
	// ensures the measured return is the signal cancellation, never the
	// command finishing first on a loaded machine.
	command := "ping -n 30 127.0.0.1 >nul"
	if os.PathSeparator != '\\' {
		command = "sleep 30"
	}
	signal, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	result := RunHook(CommandHook{Command: command}, RunHookOptions{
		Payload:          map[string]any{},
		Signal:           signal,
		DefaultTimeoutMs: 60_000,
	}, fixedClock())
	if result.Output.ExitCode != nil {
		t.Fatalf("exit = %+v, want nil after signal cancel", result.Output.ExitCode)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("signal cancel took %v", elapsed)
	}
}

func TestDetachedRunsDrainWaitsForTrackedRuns(t *testing.T) {
	detached := NewDetachedRuns()
	settled := make(chan struct{})
	detached.Track(func(ctx context.Context) {
		// Observe the drain cancel, then finish.
		<-ctx.Done()
		close(settled)
	})
	deadline := time.After(2 * time.Second)
	drained := make(chan struct{})
	go func() {
		detached.Drain()
		close(drained)
	}()
	select {
	case <-settled:
	case <-deadline:
		t.Fatal("tracked run never observed the drain cancel")
	}
	select {
	case <-drained:
	case <-deadline:
		t.Fatal("drain never resolved")
	}
}

func floatPtr(value float64) *float64 { return &value }
