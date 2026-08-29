package subagent

import (
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"dshgo/llm"
)

func TestLimitSubagentDiagnostic(t *testing.T) {
	if got := limitSubagentDiagnostic("short"); got != "short" {
		t.Fatalf("short = %q", got)
	}
	// Multibyte payload: truncation must not split a UTF-8 sequence.
	long := strings.Repeat("好", 3000)
	got := limitSubagentDiagnostic(long)
	if len(got) > maxSubagentDiagnosticBytes {
		t.Fatalf("truncated = %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated diagnostic is not valid UTF-8")
	}
	if !strings.HasSuffix(got, diagnosticTruncationSuffix) {
		t.Fatalf("truncated = %q, want suffix", got)
	}
	if strings.HasPrefix(strings.TrimSuffix(got, diagnosticTruncationSuffix), string([]byte{0x80})) {
		t.Fatal("cut landed inside a sequence")
	}
}

func TestAssertPositiveFinite(t *testing.T) {
	for _, bad := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if err := AssertPositiveFinite("pfx", "timeout", bad); err == nil ||
			err.Error() != "pfx: timeout must be a positive finite number" {
			t.Fatalf("%v = %v", bad, err)
		}
	}
	if err := AssertPositiveFinite("pfx", "timeout", 1.5); err != nil {
		t.Fatalf("1.5 = %v", err)
	}
}

func TestAssertUsableCwdAndConfigured(t *testing.T) {
	dir := t.TempDir()
	if got, err := AssertUsableCwd("pfx", "test cwd", dir); err != nil || got != dir {
		t.Fatalf("usable = %q %v", got, err)
	}
	if _, err := AssertUsableCwd("pfx", "test cwd", "relative/path"); err == nil ||
		!strings.Contains(err.Error(), "must be an absolute path") {
		t.Fatalf("relative = %v", err)
	}
	missing := filepath.Join(dir, "nope")
	if _, err := AssertUsableCwd("pfx", "test cwd", missing); err == nil ||
		!strings.Contains(err.Error(), "is not an accessible directory") {
		t.Fatalf("missing = %v", err)
	}
	// Load-time validation: omitted ok, empty loud, relative resolved then
	// probed.
	if got, err := ValidateConfiguredCwd("pfx", "", false); err != nil || got != "" {
		t.Fatalf("omitted = %q %v", got, err)
	}
	if _, err := ValidateConfiguredCwd("pfx", "", true); err == nil ||
		!strings.Contains(err.Error(), "config cwd must not be empty") {
		t.Fatalf("empty = %v", err)
	}
	if _, err := ValidateConfiguredCwd("pfx", filepath.Join(dir, "sub"), true); err == nil {
		t.Fatal("resolved relative into a missing dir must fail")
	}
	if got, err := ValidateConfiguredCwd("pfx", dir, true); err != nil || got != dir {
		t.Fatalf("abs = %q %v", got, err)
	}
	// Resolution: configured override wins; neither → loud.
	if got, err := ResolveChildCwd("pfx", dir, true, "elsewhere", true); err != nil || got != dir {
		t.Fatalf("override = %q %v", got, err)
	}
	if got, err := ResolveChildCwd("pfx", "", false, dir, true); err != nil || got != dir {
		t.Fatalf("parent = %q %v", got, err)
	}
	if _, err := ResolveChildCwd("pfx", "", false, "", false); err == nil ||
		!strings.Contains(err.Error(), "no working directory for the child") {
		t.Fatalf("none = %v", err)
	}
}

func TestSettleRunResultPaths(t *testing.T) {
	collectOutput := func() []llm.ContentBlock {
		return []llm.ContentBlock{{Type: llm.BlockText, Text: "partial"}}
	}
	stopAbort := 0
	// A normal result passes through with its diagnostic bounded.
	settled := SettleRunResult(RunResultSettlement{
		Attempt: func() (SubagentResult, error) {
			return SubagentResult{StopReason: StopCompleted, Diagnostic: "ok"}, nil
		},
		CollectOutput: collectOutput,
		Cancelled:     func() bool { return false },
		StopAbort:     func() { stopAbort++ },
	})
	if settled.StopReason != StopCompleted || settled.Diagnostic != "ok" {
		t.Fatalf("normal = %+v", settled)
	}
	// A completed attempt after local cancellation settles as aborted.
	settled = SettleRunResult(RunResultSettlement{
		Attempt:       func() (SubagentResult, error) { return SubagentResult{StopReason: StopCompleted}, nil },
		CollectOutput: collectOutput,
		Cancelled:     func() bool { return true },
		StopAbort:     func() { stopAbort++ },
	})
	if settled.StopReason != StopAborted || len(settled.Output) != 1 {
		t.Fatalf("cancelled = %+v", settled)
	}
	// A failed attempt flattens to StopError with a bounded collected
	// diagnostic; the sink runs; a panicking sink is contained.
	sinkCalls := 0
	failing := SettleRunResult(RunResultSettlement{
		Attempt:           func() (SubagentResult, error) { return SubagentResult{}, errors.New("transport died") },
		CollectOutput:     collectOutput,
		CollectDiagnostic: func() (string, bool) { return strings.Repeat("x", 5000), true },
		Cancelled:         func() bool { return false },
		OnError:           func(err error, reason StopReason) { sinkCalls++; panic("sink blew up") },
		StopAbort:         func() { stopAbort++ },
	})
	if failing.StopReason != StopError || len(failing.Diagnostic) > maxSubagentDiagnosticBytes || sinkCalls != 1 {
		t.Fatalf("failed = %+v sinkCalls = %d", failing, sinkCalls)
	}
	// The abort watcher is released on every path.
	if stopAbort != 3 {
		t.Fatalf("stopAbort = %d, want 3", stopAbort)
	}
}

func TestSubprocessRunHandleIdempotentDispose(t *testing.T) {
	teardowns := 0
	cancels := 0
	releases := 0
	done := make(chan struct{})
	once := &sync.Once{}
	resultCalls := 0
	handle := SubprocessRunHandle(SubprocessRunHandleParts{
		ID: "remote-1",
		Result: func() (SubagentResult, error) {
			resultCalls++
			return SubagentResult{StopReason: StopAborted}, nil
		},
		RequestCancel: func() { cancels++ },
		Teardown:      func() error { teardowns++; return nil },
		Done:          done,
		Once:          once,
		StopAbort:     func() { releases++ },
	})
	if handle.LocalAgent() != nil || handle.ID() != "remote-1" {
		t.Fatalf("handle identity = %s", handle.ID())
	}
	for range 3 {
		if err := handle.Dispose(); err != nil {
			t.Fatalf("dispose: %v", err)
		}
	}
	select {
	case <-done:
	default:
		t.Fatal("dispose must release the abort watcher")
	}
	if teardowns != 1 || cancels != 3 || releases != 3 || resultCalls != 0 {
		t.Fatalf("teardowns=%d cancels=%d releases=%d resultCalls=%d", teardowns, cancels, releases, resultCalls)
	}
}
