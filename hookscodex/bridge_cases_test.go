package hookscodex

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/hookprotocol"
	"dshgo/session"
)

func TestPreToolUseDenyBlocksAndLogsPair(t *testing.T) {
	observed := withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"shell","hooks":[{"command":"deny"}]}]}`)
	f.start()
	// Open turn 3 so the payload's turn_id and the log pair carry it.
	if _, err := f.sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 3}, nil); err != nil {
		t.Fatalf("append turn start: %v", err)
	}

	result := f.executeTool("shell", "rm -rf /", nil)
	if !result.IsError {
		t.Fatal("the matched call should be denied")
	}
	text := ""
	for _, block := range result.Content {
		text += blockText(block)
	}
	if !strings.Contains(text, "codex-deny") {
		t.Fatalf("denial content = %q, want the hook's stderr", text)
	}

	events := hookEvents(t, f.sess)
	if len(events) != 2 {
		t.Fatalf("hook events = %d, want a pair", len(events))
	}
	invoked, hookResult := events[0], events[1]
	if invoked["point"] != "PreToolUse" || invoked["dialect"] != "codex" {
		t.Fatalf("invoked = %+v", invoked)
	}
	if invoked["handlerId"] != hookResult["handlerId"] {
		t.Fatalf("handler ids must pair: %v vs %v", invoked["handlerId"], hookResult["handlerId"])
	}
	if hookResult["decision"] != "block" {
		t.Fatalf("result = %+v", hookResult)
	}

	// The Codex payload shape: snake_case, model + permission_mode,
	// turn_id, and tool_input { command }.
	seen := (*observed)[0].options
	payload, _ := seen.Payload.(map[string]any)
	if payload == nil {
		t.Fatal("payload is not a map")
	}
	if payload["model"] != "gpt-5-codex" || payload["permission_mode"] != "default" {
		t.Fatalf("payload model fields = %v/%v", payload["model"], payload["permission_mode"])
	}
	if payload["turn_id"] != "3" {
		t.Fatalf("turn_id = %v, want the stringified turn", payload["turn_id"])
	}
	toolInput, _ := payload["tool_input"].(map[string]any)
	if toolInput["command"] != "rm -rf /" {
		t.Fatalf("tool_input = %v, want the { command } shape", payload["tool_input"])
	}
	if payload["tool_name"] != "shell" {
		t.Fatalf("tool_name = %v", payload["tool_name"])
	}
	// Codex sends no hook environment.
	if len(seen.Env) != 0 {
		t.Fatalf("env = %v, want none", seen.Env)
	}
	if seen.TrailingNewline {
		t.Fatal("Codex writes stdin without a trailing newline")
	}
}

func TestPreToolUseRegexMatcherScoping(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	// Codex matchers are always regexes.
	f.writeConfig(`{"PreToolUse":[{"matcher":"^(shell|exec)$","hooks":[{"command":"deny"}]}]}`)
	f.start()
	if !f.executeTool("shell", "ls", nil).IsError {
		t.Fatal("a regex-matched tool must be denied")
	}
	if f.executeTool("read", "cat", nil).IsError {
		t.Fatal("an unmatched tool must run")
	}
}

func TestPreToolUseAllowAndAskAreNotHonored(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{
		"approve": stubOutput(t, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "allow",
		}}),
		"ask": stubOutput(t, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName": "PreToolUse", "permissionDecision": "ask", "permissionDecisionReason": "check",
		}}),
	})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"shell","hooks":[{"command":"approve"},{"command":"ask"}]}]}`)
	f.start()
	// Codex honors only blocking decisions: allow/ask fall through.
	if f.executeTool("shell", "ls", nil).IsError {
		t.Fatal("non-blocking decisions must not block a Codex tool call")
	}
}

func TestUserPromptSubmitDenyRejects(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"UserPromptSubmit":[{"hooks":[{"command":"deny"}]}]}`)
	f.start()
	decision := runPreStep(f, 1, userTextMessage("hello"))
	if decision.Kind != "reject" {
		t.Fatalf("decision = %q, want reject", decision.Kind)
	}
}

func TestUserPromptSubmitPlainStdoutBecomesContext(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"plain": rawOutput(0, "plain context line", "")})
	f := newFixture(t)
	f.writeConfig(`{"UserPromptSubmit":[{"hooks":[{"command":"plain"}]}]}`)
	f.start()

	claimed := userTextMessage("hello")
	decision := runPreStep(f, 1, claimed)
	if decision.Kind != "enter" {
		t.Fatalf("decision = %q, want enter", decision.Kind)
	}
	texts := messageTexts(decision.Messages)
	if len(texts) != 2 || !strings.Contains(texts[len(texts)-1], "plain context line") {
		t.Fatalf("folded messages = %v, want claimed + plain stdout", texts)
	}
	if last := decision.Messages[len(decision.Messages)-1]; last.Source.Plugin != Name {
		t.Fatalf("context source = %+v", last.Source)
	}
}

func TestUserPromptSubmitStructuredJsonDoesNotLeakAsProse(t *testing.T) {
	// A clean-exit stdout that IS raw JSON never becomes context, and its
	// non-context body carries no decision.
	withStubHooks(t, map[string]hookprotocol.HookOutput{"jsony": rawOutput(0, `{"note":"not context"}`, "")})
	f := newFixture(t)
	f.writeConfig(`{"UserPromptSubmit":[{"hooks":[{"command":"jsony"}]}]}`)
	f.start()
	decision := runPreStep(f, 1, userTextMessage("hello"))
	if decision.Kind != "enter" || len(decision.Messages) != 1 {
		t.Fatalf("decision = %+v, want a clean pass-through", decision)
	}
}

func TestStopHookSteersPendingInput(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"Stop":[{"hooks":[{"command":"deny"}]}]}`)
	f.start()

	f.registry.Events().Serial(agent.EventTurnStopping, f.agent.Scope, agent.TurnStoppingPayload{Agent: f.agent, Turn: 4})
	pending := f.agent.Inbox.NextStep()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want the steering message", len(pending))
	}
	if texts := messageTexts(pending); len(texts) != 1 || !strings.Contains(texts[0], "codex-deny") {
		t.Fatalf("steer text = %v", texts)
	}
}

func TestStopPlainStdoutContextDoesNotSteer(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"plain": rawOutput(0, "context only", "")})
	f := newFixture(t)
	f.writeConfig(`{"Stop":[{"hooks":[{"command":"plain"}]}]}`)
	f.start()
	f.registry.Events().Serial(agent.EventTurnStopping, f.agent.Scope, agent.TurnStoppingPayload{Agent: f.agent, Turn: 1})
	if len(f.agent.Inbox.NextStep()) != 0 {
		t.Fatal("a non-blocking Stop hook must not steer")
	}
}

func TestSessionStartPlainStdoutInjectsDetached(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"plain": rawOutput(0, "boot context", "")})
	f := newFixture(t)
	f.writeConfig(`{"SessionStart":[{"hooks":[{"command":"plain"}]}]}`)
	f.start()

	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{
		Agent:  f.agent,
		Source: agent.SessionStartStartup,
	})
	waitFor(t, "session-start injection", func() bool { return len(f.agent.Inbox.NextStep()) == 1 })
	if texts := messageTexts(f.agent.Inbox.NextStep()); !strings.Contains(texts[0], "boot context") {
		t.Fatalf("injected text = %v", texts)
	}
}

func TestPostToolUseDenyAttachesContext(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{
		"deny-ctx": stubOutput(t, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PostToolUse",
			"permissionDecision":       "deny",
			"permissionDecisionReason": "no audit",
			"additionalContext":        "post-ctx",
		}}),
	})
	f := newFixture(t)
	f.writeConfig(`{"PostToolUse":[{"matcher":"shell","hooks":[{"command":"deny-ctx"}]}]}`)
	f.start()

	result := f.executeTool("shell", "ls", nil)
	if !result.IsError {
		t.Fatal("a post-tool deny must block the result")
	}
	if len(result.AdditionalContexts) != 1 {
		t.Fatalf("additional contexts = %d, want one", len(result.AdditionalContexts))
	}
	if texts := messageTexts(result.AdditionalContexts); !strings.Contains(texts[0], "post-ctx") {
		t.Fatalf("context text = %v", texts)
	}
}

func TestSessionPayloadCarriesStopFields(t *testing.T) {
	observed := withStubHooks(t, map[string]hookprotocol.HookOutput{"plain": rawOutput(0, "x", "")})
	f := newFixture(t)
	f.writeConfig(`{"Stop":[{"hooks":[{"command":"plain"}]}],"SessionStart":[{"hooks":[{"command":"plain"}]}]}`)
	f.start()

	f.registry.Events().Serial(agent.EventTurnStopping, f.agent.Scope, agent.TurnStoppingPayload{Agent: f.agent, Turn: 2})
	waitFor(t, "stop run", func() bool { return len(*observed) >= 1 })
	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{Agent: f.agent, Source: agent.SessionStartStartup})
	waitFor(t, "session-start run", func() bool { return len(*observed) >= 2 })

	stopPayload, _ := (*observed)[0].options.Payload.(map[string]any)
	if _, ok := stopPayload["stop_hook_active"]; !ok {
		t.Fatal("the Stop payload carries stop_hook_active")
	}
	if value, ok := stopPayload["last_assistant_message"]; !ok || value != nil {
		t.Fatalf("last_assistant_message = %v/%v, want a present null", value, ok)
	}
	if stopPayload["turn_id"] != "2" {
		t.Fatalf("turn_id = %v", stopPayload["turn_id"])
	}

	startPayload, _ := (*observed)[1].options.Payload.(map[string]any)
	if _, ok := startPayload["turn_id"]; ok {
		t.Fatal("SessionStart has no turn_id (not a turn-scoped event)")
	}
	if _, ok := startPayload["source"]; !ok {
		t.Fatal("the SessionStart payload carries the source")
	}
}

func TestConfigLoadFailureRegistersNothing(t *testing.T) {
	f := newFixture(t)
	f.dispose()
	f.configPath = filepath.Join(f.dir, "missing.json")
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, Config{ConfigPath: f.configPath, Logger: f.logger})
	if err != nil {
		t.Fatalf("apply should not fail on a missing config: %v", err)
	}
	defer dispose()
	if len(f.logger.warns()) == 0 || !strings.Contains(f.logger.warns()[0], "could not load hook config") {
		t.Fatalf("warnings = %v", f.logger.warns())
	}
	f.registry = registry
	f.runtime = runtime
	f.startAgent()
	decision := runPreStep(f, 1, userTextMessage("hello"))
	if decision.Kind != "enter" || len(decision.Messages) != 1 {
		t.Fatalf("decision = %+v, want a clean pass-through", decision)
	}
}

func TestDisposalUnregistersListeners(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"plain": rawOutput(0, "after-dispose", "")})
	f := newFixture(t)
	f.writeConfig(`{"SessionStart":[{"hooks":[{"command":"plain"}]}]}`)
	f.dispose()
	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{Agent: f.agent, Source: agent.SessionStartStartup})
	time.Sleep(50 * time.Millisecond)
	if len(f.agent.Inbox.NextStep()) != 0 {
		t.Fatal("disposed bridge must not inject context")
	}
}

func TestNonPositiveStderrSummaryCapFailsApply(t *testing.T) {
	f := newFixture(t)
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if _, err := Apply(agent.NewAgentRegistry(nil, nil), runtime, Config{ConfigPath: f.configPath, StderrSummaryMaxChars: -1}); err == nil {
		t.Fatal("a non-positive summary cap must fail the apply")
	}
}
