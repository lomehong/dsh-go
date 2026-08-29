package guard

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/tools"
)

func newRepeat(t *testing.T, config RepeatConfig) *RepeatToolReminder {
	t.Helper()
	guard, err := NewRepeatToolReminder(config)
	if err != nil {
		t.Fatalf("reminder: %v", err)
	}
	return guard
}

func repeatExec(name string, args any) *tools.ToolExecution {
	return &tools.ToolExecution{Name: name, Arguments: args, Agent: scope.NewScopeKey(nil)}
}

func TestRepeatConfigValidation(t *testing.T) {
	if _, err := ValidateRepeatConfig(RepeatConfig{Thresholds: []int{}}); err == nil ||
		!strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty = %v", err)
	}
	if _, err := ValidateRepeatConfig(RepeatConfig{Thresholds: []int{1, 3}}); err == nil ||
		!strings.Contains(err.Error(), "integer >= 2") {
		t.Fatalf("low = %v", err)
	}
	if _, err := ValidateRepeatConfig(RepeatConfig{Thresholds: []int{3, 3}}); err == nil ||
		!strings.Contains(err.Error(), "duplicates") {
		t.Fatalf("dup = %v", err)
	}
	if _, err := ValidateRepeatConfig(RepeatConfig{ArgumentsPreviewChars: -1}); err == nil ||
		!strings.Contains(err.Error(), "argumentsPreviewChars") {
		t.Fatalf("preview = %v", err)
	}
	// Defaults fill in and thresholds sort ascending.
	resolved, err := ValidateRepeatConfig(RepeatConfig{Thresholds: []int{8, 3, 5}, ArgumentsPreviewChars: 20})
	if err != nil {
		t.Fatalf("valid = %v", err)
	}
	if len(resolved.Thresholds) != 3 || resolved.Thresholds[0] != 3 || resolved.Thresholds[2] != 8 {
		t.Fatalf("sorted = %v", resolved.Thresholds)
	}
	if resolved.ArgumentsPreviewChars != 20 {
		t.Fatalf("preview kept = %d", resolved.ArgumentsPreviewChars)
	}
	// An omitted preview cap defaults.
	defaulted, _ := ValidateRepeatConfig(RepeatConfig{})
	if defaulted.ArgumentsPreviewChars != 500 {
		t.Fatalf("default preview = %d", defaulted.ArgumentsPreviewChars)
	}
}

func TestRepeatChainEscalationAndReset(t *testing.T) {
	guard := newRepeat(t, RepeatConfig{})
	agentKey := scope.NewScopeKey(nil)
	var reminder *llm.Message

	// No reminder below the first threshold.
	for i := 0; i < 2; i++ {
		if reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"})); reminder != nil {
			t.Fatalf("early reminder at attempt %d", i+1)
		}
	}
	// First threshold: the gentle reminder with the plugin notice source.
	reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"}))
	if reminder == nil || !strings.Contains(reminder.Content[0].Text, "repeating the exact same tool call") {
		t.Fatalf("gentle = %+v", reminder)
	}
	if reminder.Source.Plugin != PluginName || reminder.Source.Form != "notice" || reminder.Source.Summary != "bash × 3" {
		t.Fatalf("source = %+v", reminder.Source)
	}
	// Later thresholds: the detailed reminder naming tool, run, arguments.
	for i := 0; i < 2; i++ {
		reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"}))
	}
	if reminder == nil || !strings.HasPrefix(reminder.Content[0].Text, "Repeated tool call detected:") ||
		!strings.Contains(reminder.Content[0].Text, "- consecutive_calls: 5\n") {
		t.Fatalf("detailed = %+v", reminder)
	}
	// Non-threshold counts stay quiet; different arguments restart the
	// chain at 1.
	if reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"})); reminder != nil {
		t.Fatalf("count 6 reminded: %+v", reminder)
	}
	if reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "pwd"})); reminder != nil {
		t.Fatalf("new args reminded: %+v", reminder)
	}
	// Switching arguments restarts the run at 1: only the exact same call
	// continues the chain.
	if reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"})); reminder != nil {
		t.Fatalf("restarted run reminded: %+v", reminder)
	}
	// A user interjection resets the chain.
	guard.Reset(agentKey)
	if reminder = guard.Observe(agentKey, repeatExec("bash", map[string]any{"cmd": "ls"})); reminder != nil {
		t.Fatalf("post-reset reminded: %+v", reminder)
	}
	// A nil agent key (direct execute caller) never observes.
	if reminder = guard.Observe(nil, repeatExec("bash", map[string]any{"cmd": "ls"})); reminder != nil {
		t.Fatal("agent-less call observed")
	}
}

func TestRepeatWildcardTracking(t *testing.T) {
	guard := newRepeat(t, RepeatConfig{
		Thresholds: []int{2},
		Include:    []string{"browser_*"},
		Exclude:    []string{"browser_hidden*"},
	})
	agentKey := scope.NewScopeKey(nil)
	// Included and not excluded: tracked (second identical call reaches
	// the threshold).
	guard.Observe(agentKey, repeatExec("browser_click", nil))
	if reminder := guard.Observe(agentKey, repeatExec("browser_click", nil)); reminder == nil {
		t.Fatal("included tool not tracked")
	}
	// Excluded wins: transparent.
	guard.Observe(agentKey, repeatExec("browser_hidden_click", nil))
	if reminder := guard.Observe(agentKey, repeatExec("browser_hidden_click", nil)); reminder != nil {
		t.Fatal("excluded tool tracked")
	}
	// Outside the include list: transparent.
	guard.Observe(agentKey, repeatExec("bash", nil))
	if reminder := guard.Observe(agentKey, repeatExec("bash", nil)); reminder != nil {
		t.Fatal("untracked tool observed")
	}
	// Wildcard metacharacters stay literal: "browser_click" does not
	// match "browserXclick".
	guard.Observe(agentKey, repeatExec("browserXclick", nil))
	if reminder := guard.Observe(agentKey, repeatExec("browserXclick", nil)); reminder != nil {
		t.Fatal("dot-matching star tracked")
	}
}

func TestRepeatCanonicalizationAndPreviewCap(t *testing.T) {
	guard := newRepeat(t, RepeatConfig{Thresholds: []int{2}, ArgumentsPreviewChars: 5})
	agentKey := scope.NewScopeKey(nil)
	// Property order does not matter to the chain key: {a,b} equals {b,a},
	// so the reordered call is consecutive repeat #2.
	if guard.Observe(agentKey, repeatExec("write", map[string]any{"a": 1, "b": map[string]any{"y": 2, "x": 1}})) != nil {
		t.Fatal("first call reminded")
	}
	reminder := guard.Observe(agentKey, repeatExec("write", map[string]any{"b": map[string]any{"x": 1, "y": 2}, "a": 1}))
	if reminder == nil || reminder.Source.Summary != "write × 2" {
		t.Fatalf("reordered = %+v", reminder)
	}
	if got := previewArguments("abcdefgh", 5); got != "abcde… (+3 more chars)" {
		t.Fatalf("preview = %q", got)
	}
	// The preview cap bounds the quoted arguments in the detailed tier,
	// not the detection itself.
	capped := newRepeat(t, RepeatConfig{Thresholds: []int{2, 4}, ArgumentsPreviewChars: 5})
	for i := 0; i < 3; i++ {
		capped.Observe(agentKey, repeatExec("write", map[string]any{"payload": "0123456789"}))
	}
	detailed := capped.Observe(agentKey, repeatExec("write", map[string]any{"payload": "0123456789"}))
	if detailed == nil || !strings.Contains(detailed.Content[0].Text, "- arguments: {\"pay… (+19 more chars)\n") {
		t.Fatalf("detailed preview = %+v", detailed)
	}
}

func TestRepeatPostExecuteFoldsOntoBlockAndAccept(t *testing.T) {
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	echo, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "bash", Description: "echo",
		Parameters: map[string]tools.PropSpec{
			"cmd": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"ran": args["cmd"]}, nil
		},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := runtime.Register(echo); err != nil {
		t.Fatalf("register: %v", err)
	}
	guard := newRepeat(t, RepeatConfig{Thresholds: []int{2}})
	detach := guard.Attach(runtime)
	defer detach()

	// A later listener blocks the call; the reminder still rides the
	// block decision the pipeline applies.
	deferLateBlock := runtime.OnPostExecute(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult, next func(*tools.ToolExecutionResult) *tools.PostToolDecision) *tools.PostToolDecision {
		downstream := next(result)
		downstream.Kind = "block"
		downstream.Feedback = []llm.ContentBlock{{Type: "text", Text: "denied by policy"}}
		return downstream
	})
	defer deferLateBlock()

	agentKey := scope.NewScopeKey(nil)
	run := func() *tools.ToolExecutionResult {
		return runtime.Execute(&tools.ToolExecutionInput{
			CallID: "c", Name: "bash",
			Arguments: map[string]any{"cmd": "ls"},
			Agent:     agentKey,
			Signal:    context.Background(),
		})
	}
	first := run()
	if !first.IsError || len(first.AdditionalContexts) != 0 {
		t.Fatalf("first = %+v", first)
	}
	second := run()
	if !second.IsError {
		t.Fatalf("second not blocked: %+v", second)
	}
	// The reminder rides the blocked call as additional context, keeping
	// the policy feedback as the error body.
	if len(second.AdditionalContexts) != 1 ||
		!strings.Contains(second.AdditionalContexts[0].Content[0].Text, "repeating the exact same tool call") {
		t.Fatalf("second contexts = %+v", second.AdditionalContexts)
	}
	if len(second.Content) != 1 || second.Content[0].Text != "denied by policy" {
		t.Fatalf("feedback lost: %+v", second.Content)
	}
}

func TestRepeatPreStepResetWiring(t *testing.T) {
	registry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	guard := newRepeat(t, RepeatConfig{Thresholds: []int{2}})
	detach := guard.AttachPreStepReset(registry)
	defer detach()
	built := scope.NewScopeKey(nil)
	// Two observed repeats arm the chain…
	guard.Observe(built, repeatExec("bash", map[string]any{"cmd": "ls"}))
	if guard.Observe(built, repeatExec("bash", map[string]any{"cmd": "ls"})) == nil {
		t.Fatal("chain never armed")
	}
	guard.Observe(built, repeatExec("bash", map[string]any{"cmd": "ls"}))
	guard.Observe(built, repeatExec("bash", map[string]any{"cmd": "ls"}))
	// …a user interjection in the claimed input resets it.
	fake := &agent.Agent{Scope: built}
	registry.Events().PreStep().Dispatch(nil, agent.PreStepPayload{
		Agent:    fake,
		Messages: []llm.Message{{Source: llm.MessageSource{Kind: llm.SourceUser}}},
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(nil) })
	if guard.Observe(built, repeatExec("bash", map[string]any{"cmd": "ls"})) != nil {
		t.Fatal("chain survived user interjection")
	}
}

func TestTimeoutPolicyReplacesOwnDeadlineOnly(t *testing.T) {
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	slow, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "slow", Description: "slow tool",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
			},
		},
		TimeoutMs: 40,
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			// The body honors its signal: it observes the abort and
			// returns its own abort-shaped result.
			select {
			case <-exec.Signal.Done():
				return map[string]any{"aborted": true}, nil
			case <-time.After(2 * time.Second):
				return map[string]any{"done": true}, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("define slow: %v", err)
	}
	fast, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "fast", Description: "fast tool",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"done": true}, nil
		},
	})
	if err != nil {
		t.Fatalf("define fast: %v", err)
	}
	if _, err := runtime.Register(slow); err != nil {
		t.Fatalf("register slow: %v", err)
	}
	if _, err := runtime.Register(fast); err != nil {
		t.Fatalf("register fast: %v", err)
	}
	detach := AttachTimeoutPolicy(runtime)
	defer detach()

	// A tool within its budget is untouched, and post-dispatch listeners
	// see the caller's signal, not the deadline.
	fastResult := runtime.Execute(&tools.ToolExecutionInput{CallID: "f", Name: "fast", Arguments: map[string]any{}, Signal: context.Background()})
	if fastResult.IsError {
		t.Fatalf("fast errored: %+v", fastResult.Error)
	}
	// Our own timer firing replaces the tool's abort-shaped result with
	// the structured TOOL_TIMEOUT.
	started := time.Now()
	slowResult := runtime.Execute(&tools.ToolExecutionInput{CallID: "s", Name: "slow", Arguments: map[string]any{}, Signal: context.Background()})
	if !slowResult.IsError || slowResult.Error == nil || slowResult.Error.Info == nil || slowResult.Error.Info.Code != ToolTimeout {
		t.Fatalf("slow = %+v", slowResult)
	}
	if !strings.Contains(slowResult.Content[0].Text, "tool call timed out after 40ms") {
		t.Fatalf("timeout text = %q", slowResult.Content[0].Text)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout waited for the body: %v", elapsed)
	}
}

func TestTimeoutPolicyCallerCancelIsNotToolTimeout(t *testing.T) {
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	aborting, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "aborting", Description: "aborts with caller",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: fmt.Sprintf("%v", value)}}
			},
		},
		TimeoutMs: 10_000,
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			<-exec.Signal.Done()
			return nil, exec.Signal.Err()
		},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := runtime.Register(aborting); err != nil {
		t.Fatalf("register: %v", err)
	}
	detach := AttachTimeoutPolicy(runtime)
	defer detach()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	result := runtime.Execute(&tools.ToolExecutionInput{CallID: "a", Name: "aborting", Arguments: map[string]any{}, Signal: ctx})
	// The upstream cancel propagates as the ordinary caller cancellation —
	// never as TOOL_TIMEOUT.
	if result.IsError && result.Error != nil && result.Error.Info != nil && result.Error.Info.Code == ToolTimeout {
		t.Fatalf("caller cancel misread: %+v", result)
	}
}
