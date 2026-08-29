package toolsjobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/jobs"
	"dshgo/tools"
)

func TestResolveConfigDefaultsAndValidation(t *testing.T) {
	resolved, err := ResolveConfig(Config{})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.WaitTimeoutMs != DefaultWaitTimeoutMs || resolved.MaxWaitTimeoutMs != DefaultMaxWaitTimeoutMs ||
		resolved.CompletionDelivery != DeliveryWakeup || resolved.MaxConsecutiveWakes != DefaultMaxWakes {
		t.Fatalf("defaults = %+v", resolved)
	}
	if _, err := ResolveConfig(Config{WaitTimeoutMs: 700_000, MaxWaitTimeoutMs: 600_000}); err == nil ||
		!strings.Contains(err.Error(), "exceeds maxWaitTimeoutMs") {
		t.Fatalf("oversize wait = %v", err)
	}
	if _, err := ResolveConfig(Config{CompletionDelivery: "loud"}); err == nil {
		t.Fatal("bad delivery accepted")
	}
	// Zero shares the missing-config default; only a negative count is a
	// malformed budget.
	if resolved, err := ResolveConfig(Config{MaxConsecutiveWakes: 0, WaitTimeoutMs: 5, MaxWaitTimeoutMs: 6}); err != nil || resolved.MaxConsecutiveWakes != DefaultMaxWakes {
		t.Fatalf("zero wakes = %+v %v", resolved, err)
	}
	if _, err := ResolveConfig(Config{MaxConsecutiveWakes: -1, WaitTimeoutMs: 5, MaxWaitTimeoutMs: 6}); err == nil ||
		!strings.Contains(err.Error(), "whole number of turns") {
		t.Fatalf("negative wake budget = %v", err)
	}
}

func TestStatusLine(t *testing.T) {
	if got := StatusLine(jobs.StatusRunning, ""); got != "[status: running]" {
		t.Fatalf("plain = %q", got)
	}
	if got := StatusLine(jobs.StatusFailed, "exit code: 2"); got != "[status: failed, exit code: 2]" {
		t.Fatalf("detailed = %q", got)
	}
}

func TestFitWithSuffix(t *testing.T) {
	// Under budget: untouched.
	if got := FitWithSuffix("abc", "\n[status: x]", 100, "\n[output truncated]"); got != "abc\n[status: x]" {
		t.Fatalf("under = %q", got)
	}
	// Over budget: the marker is promoted into the fixed suffix and only
	// the content shrinks.
	if got := FitWithSuffix("0123456789", "|END", 12, "\n[cut]"); got != "89\n[cut]|END" {
		t.Fatalf("promoted = %q", got)
	}
	// The fixed suffix alone overflows: the tail survives.
	if got := FitWithSuffix("0123456789", "|END", 9, "\n[cut]"); got != "[cut]|END" {
		t.Fatalf("tail = %q", got)
	}
	// Content already ending with the marker: no duplicate marker, and
	// the shrinking content keeps the tail.
	if got := FitWithSuffix("x[cut]", "|END", 8, "\n[cut]"); got != "cut]|END" {
		t.Fatalf("dedup = %q", got)
	}
	// No budget: untouched.
	if got := FitWithSuffix("abc", "|END", 0, "\n[cut]"); got != "abc|END" {
		t.Fatalf("unbounded = %q", got)
	}
}

func TestFitCompletionNotice(t *testing.T) {
	snapshot := jobs.Snapshot{ID: "bash-1", Kind: "bash", Label: "build", Status: jobs.StatusCompleted, Detail: "exit code: 0"}
	complete := FitCompletionNotice(snapshot)
	want := "background job bash-1 (bash: build) finished [status: completed, exit code: 0]. Read its output with job_output."
	if complete != want {
		t.Fatalf("full = %q", complete)
	}
	// The producer's exact limit reproduces the full sentence.
	if got := FitCompletionNotice(withLimit(snapshot, len(want))); got != want {
		t.Fatalf("exact limit = %q", got)
	}
	// Tighter budgets degrade through marker, compact, and action paths.
	cases := map[int]string{
		58: "background job bash-1\n[notice truncated]\nDone; job_output.",
		39: "background job bash-1\nDone; job_output.",
		20: "ba\nDone; job_output.",
		10: "ob_output.",
	}
	for limit, want := range cases {
		got := FitCompletionNotice(withLimit(snapshot, limit))
		if got != want {
			t.Fatalf("limit %d = %q, want %q", limit, got, want)
		}
		if len(got) > limit {
			t.Fatalf("limit %d exceeded: %q", limit, got)
		}
	}
}

func withLimit(snapshot jobs.Snapshot, limit int) jobs.Snapshot {
	snapshot.OutputLimitBytes = limit
	return snapshot
}

func TestCompletionSummary(t *testing.T) {
	snapshot := jobs.Snapshot{Kind: "subagent", Label: "explore", Status: jobs.StatusKilled, Detail: "sigterm"}
	summary := CompletionSummary(snapshot)
	if summary != "subagent explore [status: killed, sigterm]" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestPublicJobDropsOwnership(t *testing.T) {
	snapshot := jobs.Snapshot{
		ID: "bash-2", Kind: "bash", Label: "watch", Status: jobs.StatusRunning,
		StartedAt: 100, OutputLimitBytes: 4096, OwnerSession: "agent-1", Reported: true,
	}
	public := PublicJob(snapshot)
	if public.ID != "bash-2" || public.Status != jobs.StatusRunning || public.StartedAt != 100 {
		t.Fatalf("public = %+v", public)
	}
	if strings.Contains(fmt.Sprintf("%v", public), "agent-1") {
		t.Fatalf("ownership leaked: %+v", public)
	}
}

func newToolJobs(t *testing.T) (*jobs.LocalRegistry, *tools.ToolRuntime) {
	t.Helper()
	registry, err := jobs.NewLocalRegistry(jobs.Config{}, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if _, err := RegisterTools(runtime, registry, nil, Config{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return registry, runtime
}

func runTool(t *testing.T, runtime *tools.ToolRuntime, name string, args map[string]any) *tools.ToolExecutionResult {
	t.Helper()
	result := runtime.Execute(&tools.ToolExecutionInput{
		CallID:    "call-" + name,
		Name:      name,
		Arguments: args,
		Signal:    context.Background(),
	})
	if result.IsError {
		t.Fatalf("%s failed: %+v", name, result.Error)
	}
	return result
}

func TestRegisterToolsJobOutputListAndKill(t *testing.T) {
	registry, runtime := newToolJobs(t)
	hooks := &stubHooks{stream: true}
	id, err := registry.Start(jobs.StartSpec{
		Kind: "bash", Label: "long render",
		Run: func() (jobs.Hooks, error) { return hooks.hooks(), nil },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// job_list renders the one-line roster.
	listed := runTool(t, runtime, "job_list", map[string]any{})
	text := listed.Content[0].Text
	if !strings.Contains(text, fmt.Sprintf("%s [bash] running — long render", id)) {
		t.Fatalf("list = %q", text)
	}

	// job_output streams the consuming cursor and ends with the status
	// line.
	output := runTool(t, runtime, "job_output", map[string]any{"job_id": id})
	value, ok := output.Value.(map[string]any)
	if !ok || value["text"] != "delta-1" {
		t.Fatalf("output value = %+v", output.Value)
	}
	if !strings.HasSuffix(output.Content[0].Text, "[status: running]") {
		t.Fatalf("output render = %q", output.Content[0].Text)
	}
	again := runTool(t, runtime, "job_output", map[string]any{"job_id": id})
	if again.Value.(map[string]any)["text"] != "delta-2" {
		t.Fatalf("cursor advanced wrong: %+v", again.Value)
	}

	// wait: true blocks until settlement and returns the terminal state.
	go func() {
		time.Sleep(30 * time.Millisecond)
		hooks.settle(jobs.Outcome{Status: jobs.OutcomeCompleted, Detail: "exit code: 0"})
	}()
	waited := runTool(t, runtime, "job_output", map[string]any{"job_id": id, "wait": true, "timeout_ms": float64(5000)})
	waitedValue := waited.Value.(map[string]any)
	waitedJob := waitedValue["job"].(map[string]any)
	if waitedJob["status"] != jobs.StatusCompleted || !strings.HasSuffix(waited.Content[0].Text, "[status: completed, exit code: 0]") {
		t.Fatalf("waited = %+v %q", waitedValue, waited.Content[0].Text)
	}

	// job_kill requests cancellation; a second kill reports the finished
	// state.
	killed := runTool(t, runtime, "job_kill", map[string]any{"job_id": id, "reason": "cleanup"})
	killValue := killed.Value.(map[string]any)
	if killValue["outcome"] != "already-finished" || !strings.Contains(killed.Content[0].Text, "had already finished") {
		t.Fatalf("kill = %+v %q", killValue, killed.Content[0].Text)
	}

	// Empty and unknown job ids fail loud.
	if result := runtime.Execute(&tools.ToolExecutionInput{CallID: "bad", Name: "job_output", Arguments: map[string]any{"job_id": ""}, Signal: context.Background()}); !result.IsError {
		t.Fatal("empty job id accepted")
	}
	if result := runtime.Execute(&tools.ToolExecutionInput{CallID: "ghost", Name: "job_kill", Arguments: map[string]any{"job_id": "bash-99"}, Signal: context.Background()}); !result.IsError {
		t.Fatal("unknown job accepted")
	}
}

func TestJobOutputWaitTimesOutWithRunningState(t *testing.T) {
	registry, runtime := newToolJobs(t)
	hooks := &stubHooks{stream: true}
	id, err := registry.Start(jobs.StartSpec{
		Kind: "bash", Label: "slow",
		Run: func() (jobs.Hooks, error) { return hooks.hooks(), nil },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	result := runTool(t, runtime, "job_output", map[string]any{"job_id": id, "wait": true, "timeout_ms": float64(25)})
	value := result.Value.(map[string]any)
	job := value["job"].(map[string]any)
	if job["status"] != jobs.StatusRunning {
		t.Fatalf("timed-out wait = %+v", value)
	}
	if !strings.HasSuffix(result.Content[0].Text, "[status: running]") {
		t.Fatalf("timed-out render = %q", result.Content[0].Text)
	}
}

func TestFinalizeClampsToOutputLimit(t *testing.T) {
	registry, runtime := newToolJobs(t)
	hooks := &stubHooks{stream: true, chunks: []string{strings.Repeat("x", 80)}}
	id, err := registry.Start(jobs.StartSpec{
		Kind: "bash", Label: "loud",
		OutputLimitBytes: 60,
		Run:              func() (jobs.Hooks, error) { return hooks.hooks(), nil },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// A wait read consumes the accumulated stream deltas and settles
	// terminal with the detail the tool surfaces in its status line.
	hooks.settle(jobs.Outcome{Status: jobs.OutcomeCompleted, Detail: "done"})
	time.Sleep(20 * time.Millisecond)
	result := runTool(t, runtime, "job_output", map[string]any{"job_id": id, "wait": true, "timeout_ms": float64(1000)})
	text := result.Content[0].Text
	if len(text) > 60 {
		t.Fatalf("unclamped %d bytes: %q", len(text), text)
	}
	if !strings.Contains(text, "[output truncated]") || !strings.HasSuffix(text, "[status: completed, done]") {
		t.Fatalf("clamp = %q", text)
	}
}

func TestRegisterToolsAttachesController(t *testing.T) {
	registry, err := jobs.NewLocalRegistry(jobs.Config{}, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	detach, err := RegisterTools(runtime, registry, nil, Config{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.Start(jobs.StartSpec{Kind: "bash", Label: "x", Run: func() (jobs.Hooks, error) { return (&stubHooks{}).hooks(), nil }}); err != nil {
		t.Fatalf("no controller after register: %v", err)
	}
	detach()
	if _, err := registry.Start(jobs.StartSpec{Kind: "bash", Label: "y", Run: func() (jobs.Hooks, error) { return (&stubHooks{}).hooks(), nil }}); err == nil {
		t.Fatal("detached controller still served")
	}
}

// stubHooks is a producer whose Done channel the test controls.
type stubHooks struct {
	stream  bool
	chunks  []string
	chunk   int
	calls   int
	cancels []string
	done    chan jobs.Result
}

func (h *stubHooks) hooks() jobs.Hooks {
	hook := jobs.Hooks{
		Cancel: func(reason string) error {
			h.cancels = append(h.cancels, reason)
			return nil
		},
		Done: h.doneChannel(),
	}
	if h.stream {
		hook.ReadOutput = func() string {
			h.calls++
			if h.chunk < len(h.chunks) {
				token := h.chunks[h.chunk]
				h.chunk++
				return token
			}
			return fmt.Sprintf("delta-%d", h.calls)
		}
	}
	return hook
}

func (h *stubHooks) doneChannel() chan jobs.Result {
	if h.done == nil {
		h.done = make(chan jobs.Result, 1)
	}
	return h.done
}

func (h *stubHooks) settle(outcome jobs.Outcome) {
	h.doneChannel() <- jobs.Result{Outcome: outcome}
}
