package hooksclaudecode

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/hookprotocol"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/subagent"
)

// denyOutput is a blocking exit-2 hook output (stderr becomes the reason).
func denyOutput() hookprotocol.HookOutput {
	two := 2
	return hookprotocol.HookOutput{ExitCode: &two, Stderr: "deny-reason"}
}

// contextOutput is a clean-exit structured stdout carrying
// hookSpecificOutput.additionalContext for the given event name.
func contextOutput(t *testing.T, event, text string) hookprotocol.HookOutput {
	t.Helper()
	return stubOutput(t, map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": text,
		},
	})
}

func TestPreToolUseDenyBlocksMatchedToolOnly(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"write","hooks":[{"command":"deny"}]}]}`)
	f.start()

	denied := f.executeTool("write")
	if !denied.IsError {
		t.Fatal("the matched write call should be denied")
	}
	deniedText := ""
	for _, block := range denied.Content {
		deniedText += blockText(block)
	}
	if !strings.Contains(deniedText, "deny-reason") {
		t.Fatalf("denial content = %q, want the hook's stderr", deniedText)
	}

	// A different tool does not match the literal pattern and runs.
	allowed := f.executeTool("read")
	if allowed.IsError {
		t.Fatalf("unmatched read should run: %+v", allowed.Error)
	}
}

func TestPreToolUseDenyLogsInvokedResultPair(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"write","hooks":[{"command":"deny"}]}]}`)
	f.start()
	f.executeTool("write")

	events := hookEvents(t, f.sess)
	if len(events) != 2 {
		t.Fatalf("hook events = %d, want an invoked/result pair", len(events))
	}
	invoked, result := events[0], events[1]
	if invoked["type"] != hookprotocol.EventHookInvoked || result["type"] != hookprotocol.EventHookResult {
		t.Fatalf("event order = %+v", events)
	}
	if invoked["point"] != "PreToolUse" || invoked["dialect"] != "claude-code" {
		t.Fatalf("invoked = %+v", invoked)
	}
	if invoked["handlerId"] != result["handlerId"] {
		t.Fatalf("handler ids must pair: %v vs %v", invoked["handlerId"], result["handlerId"])
	}
	if result["decision"] != "block" || result["exitCode"].(float64) != 2 {
		t.Fatalf("result = %+v", result)
	}
	summary, _ := result["stderrSummary"].(string)
	if !strings.Contains(summary, "deny-reason") {
		t.Fatalf("stderrSummary = %q", summary)
	}
	matcher, ok := invoked["matcher"].(string)
	if !ok || matcher != "write" {
		t.Fatalf("invoked matcher = %+v, want the literal group's matcher", invoked["matcher"])
	}
}

func TestPreToolUseAskSurfacesReasonWithoutApprovalSeam(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{
		"ask": stubOutput(t, map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       "ask",
			"permissionDecisionReason": "confirm delete",
		}}),
	})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"write","hooks":[{"command":"ask"}]}]}`)
	f.start()

	result := f.executeTool("write")
	if !result.IsError {
		t.Fatal("an ask without an approval seam must not run the call")
	}
	text := ""
	for _, block := range result.Content {
		text += blockText(block)
	}
	if !strings.Contains(text, "confirm delete") {
		t.Fatalf("ask denial content = %q, want the hook's reason", text)
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

func TestUserPromptSubmitContextFoldsAfterDownstream(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "UserPromptSubmit", "prompt-context")})
	f := newFixture(t)
	f.writeConfig(`{"UserPromptSubmit":[{"hooks":[{"command":"ctx"}]}]}`)
	f.start()

	claimed := userTextMessage("hello")
	decision := runPreStep(f, 1, claimed)
	if decision.Kind != "enter" {
		t.Fatalf("decision = %q, want enter", decision.Kind)
	}
	// Context alone is not a veto: the claimed messages keep their order
	// and ours appends.
	texts := messageTexts(decision.Messages)
	if len(texts) != 2 || !strings.Contains(texts[len(texts)-1], "prompt-context") {
		t.Fatalf("folded messages = %v, want claimed + context", texts)
	}
	last := decision.Messages[len(decision.Messages)-1]
	if last.Source.Kind != llm.SourcePlugin || last.Source.Plugin != Name {
		t.Fatalf("context source = %+v", last.Source)
	}
}

func TestStopHookSteersPendingInput(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"Stop":[{"hooks":[{"command":"deny"}]}]}`)
	f.start()

	f.registry.Events().Serial(agent.EventTurnStopping, f.agent.Scope, agent.TurnStoppingPayload{
		Agent:  f.agent,
		Turn:   2,
		Signal: nil,
	})
	pending := f.agent.Inbox.NextStep()
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want the steering message", len(pending))
	}
	if pending[0].Source.Plugin != Name {
		t.Fatalf("steer source = %+v", pending[0].Source)
	}
	if texts := messageTexts(pending); len(texts) != 1 || !strings.Contains(texts[0], "deny-reason") {
		t.Fatalf("steer text = %v", texts)
	}
}

func TestStopContextWithoutDenyDoesNotSteer(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "Stop", "stop-ctx")})
	f := newFixture(t)
	f.writeConfig(`{"Stop":[{"hooks":[{"command":"ctx"}]}]}`)
	f.start()

	f.registry.Events().Serial(agent.EventTurnStopping, f.agent.Scope, agent.TurnStoppingPayload{Agent: f.agent, Turn: 1})
	if len(f.agent.Inbox.NextStep()) != 0 {
		t.Fatal("a non-blocking Stop hook must not steer")
	}
}

func TestSessionStartInjectsDetachedContext(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "SessionStart", "startup-context")})
	f := newFixture(t)
	f.writeConfig(`{"SessionStart":[{"hooks":[{"command":"ctx"}]}]}`)
	f.start()

	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{
		Agent:  f.agent,
		Source: agent.SessionStartStartup,
	})
	waitFor(t, "session-start context injection", func() bool { return len(f.agent.Inbox.NextStep()) == 1 })
	if texts := messageTexts(f.agent.Inbox.NextStep()); !strings.Contains(texts[0], "startup-context") {
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
	f.writeConfig(`{"PostToolUse":[{"matcher":"write","hooks":[{"command":"deny-ctx"}]}]}`)
	f.start()

	result := f.executeTool("write")
	if !result.IsError {
		t.Fatal("a post-tool deny must block the result")
	}
	if len(result.AdditionalContexts) != 1 {
		t.Fatalf("additional contexts = %d, want the hook's context", len(result.AdditionalContexts))
	}
	if texts := messageTexts(result.AdditionalContexts); !strings.Contains(texts[0], "post-ctx") {
		t.Fatalf("context text = %v", texts)
	}
}

func TestPostToolUseContextFoldsOntoDownstream(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "PostToolUse", "folded-ctx")})
	f := newFixture(t)
	f.writeConfig(`{"PostToolUse":[{"matcher":"write","hooks":[{"command":"ctx"}]}]}`)
	f.start()

	result := f.executeTool("write")
	if result.IsError {
		t.Fatalf("write should succeed: %+v", result.Error)
	}
	if len(result.AdditionalContexts) != 1 {
		t.Fatalf("additional contexts = %d, want one", len(result.AdditionalContexts))
	}
	if texts := messageTexts(result.AdditionalContexts); !strings.Contains(texts[0], "folded-ctx") {
		t.Fatalf("context text = %v", texts)
	}
}

func TestSubagentStartInjectsChildContextAndPairedEndRuns(t *testing.T) {
	observed := withStubHooks(t, map[string]hookprotocol.HookOutput{
		"ctx":     contextOutput(t, "SubagentStart", "child-ctx"),
		"observe": contextOutput(t, "SubagentStop", ""),
	})
	f := newFixture(t)
	f.writeConfig(`{"SubagentStart":[{"hooks":[{"command":"ctx"}]}],"SubagentStop":[{"hooks":[{"command":"observe"}]}]}`)
	f.start()

	// A live child agent the registry can resolve.
	childSession, err := session.NewDetached(session.SessionID("child-1"), nil, &session.SessionHeader{ID: session.SessionID("child-1"), CWD: f.dir}, 0)
	if err != nil {
		t.Fatalf("child session: %v", err)
	}
	childInbox, err := agent.NewInbox(childSession, nopNotifications{})
	if err != nil {
		t.Fatalf("child inbox: %v", err)
	}
	child := agent.NewAgent(agent.AgentConfig{ID: childSession.ID(), Session: childSession, Inbox: childInbox}, f.registry.Events())
	if _, err := f.registry.Enter(child, nil); err != nil {
		t.Fatalf("enter child: %v", err)
	}

	f.registry.Events().Emit(subagent.EventSubagentStart, f.agent.Scope, subagent.SubagentRunInfo{
		RunID: "run-1",
		ID:    childSession.ID(),
	})
	waitFor(t, "subagent context injection", func() bool { return len(child.Inbox.NextStep()) == 1 })
	if texts := messageTexts(child.Inbox.NextStep()); !strings.Contains(texts[0], "child-ctx") {
		t.Fatalf("child text = %v", texts)
	}

	// The paired end releases the retained child and runs the stop hook
	// (detached points log no events, so observe the stub runs).
	f.registry.Events().Emit(subagent.EventSubagentEnd, f.agent.Scope, subagent.SubagentRunEndInfo{
		SubagentRunInfo: subagent.SubagentRunInfo{RunID: "run-1", ID: childSession.ID()},
	})
	waitFor(t, "subagent stop run", func() bool { return observed.len() >= 2 })
	if got := observed.at(1).command; got != "observe" {
		t.Fatalf("second hook run = %q, want the SubagentStop hook", got)
	}
}

func TestSubstitutionAndProjectDirEnvAndWorkdir(t *testing.T) {
	observed := withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "SessionStart", "startup-context")})
	f := newFixture(t)
	pluginRoot := "/plugin-root"
	projectDir := "/project-dir"
	f.dispose()
	f.configPath = filepath.Join(f.dir, "hooks-subst.json")
	f.writeConfig(`{"SessionStart":[{"hooks":[{"command":"${CLAUDE_PLUGIN_ROOT}/probe.sh"},{"command":"${CLAUDE_PROJECT_DIR}/echo-env"},{"command":"ctx"}]}]}`)
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, testProjections(t), Config{
		ConfigPath:       f.configPath,
		PluginRoot:       &pluginRoot,
		ProjectDir:       &projectDir,
		DefaultTimeoutMs: 10_000,
		Logger:           f.logger,
		Now:              func() int64 { return 0 },
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	f.dispose = dispose
	f.registry = registry
	f.runtime = runtime
	f.startAgent()

	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{Agent: f.agent, Source: agent.SessionStartStartup})
	waitFor(t, "session-start hooks", func() bool { return observed.len() == 3 })

	// Commands were substituted at parse time.
	if got := observed.at(0).command; got != pluginRoot+"/probe.sh" {
		t.Fatalf("plugin-root command = %q, want %q", got, pluginRoot+"/probe.sh")
	}
	if got := observed.at(1).command; got != projectDir+"/echo-env" {
		t.Fatalf("project-dir command = %q, want %q", got, projectDir+"/echo-env")
	}
	// The explicit config ProjectDir overrides the session-cwd default for
	// the exported env var; the hook still runs in the session workspace.
	for _, seen := range observed.first(2) {
		if seen.options.Env["CLAUDE_PROJECT_DIR"] != projectDir {
			t.Fatalf("CLAUDE_PROJECT_DIR = %q, want the configured override %q", seen.options.Env["CLAUDE_PROJECT_DIR"], projectDir)
		}
		if seen.options.CWD != f.dir {
			t.Fatalf("workdir = %q, want the session cwd %q", seen.options.CWD, f.dir)
		}
		if !seen.options.TrailingNewline {
			t.Fatal("the CC dialect appends a trailing newline to the payload")
		}
		if seen.options.ExpectedEventName != "SessionStart" {
			t.Fatalf("expected event name = %q", seen.options.ExpectedEventName)
		}
	}
	// The workspace check runs after injection.
	waitFor(t, "session-start context injection", func() bool { return len(f.agent.Inbox.NextStep()) == 1 })
}

func TestSkippedNonCommandHookWarnsButCommandRuns(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"PreToolUse":[{"matcher":"write","hooks":[{"type":"prompt","prompt":"review"},{"command":"deny"}]}]}`)
	f.start()
	f.executeTool("write")
	found := false
	for _, warn := range f.logger.warns() {
		if strings.Contains(warn, `skipping unsupported "prompt" hook on PreToolUse`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", f.logger.warns())
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
	dispose, err := Apply(registry, runtime, testProjections(t), Config{ConfigPath: f.configPath, Logger: f.logger})
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

func TestInvalidMatcherFailsTheConfigLoad(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{})
	f := newFixture(t)
	f.dispose()
	f.writeConfig(`{"PreToolUse":[{"matcher":"([bad","hooks":[{"command":"deny"}]}]}`)
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, testProjections(t), Config{ConfigPath: f.configPath, Logger: f.logger})
	if err != nil {
		t.Fatalf("an invalid regex matcher logs instead of failing apply: %v", err)
	}
	defer dispose()
	// The fixture's config-less first start logs a benign warn; the matcher
	// diagnostic arrives with this second Apply.
	for _, warn := range f.logger.warns() {
		if strings.Contains(warn, "could not load hook config") && strings.Contains(warn, `on event "PreToolUse"`) {
			return
		}
	}
	t.Fatalf("warnings = %v, want the event-qualified matcher error", f.logger.warns())
}

func TestDisposalUnregistersListeners(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"ctx": contextOutput(t, "SessionStart", "after-dispose")})
	f := newFixture(t)
	f.writeConfig(`{"SessionStart":[{"hooks":[{"command":"ctx"}]}]}`)
	f.dispose()
	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope, agent.AgentSessionStartPayload{Agent: f.agent, Source: agent.SessionStartStartup})
	time.Sleep(50 * time.Millisecond)
	if len(f.agent.Inbox.NextStep()) != 0 {
		t.Fatal("disposed bridge must not inject context")
	}
}

func TestStderrSummaryCapConfigurable(t *testing.T) {
	long := strings.Repeat("x", 40)
	withStubHooks(t, map[string]hookprotocol.HookOutput{"long": {ExitCode: intPtr(2), Stderr: long}})
	f := newFixture(t)
	f.dispose()
	f.writeConfig(`{"PreToolUse":[{"hooks":[{"command":"long"}]}]}`)
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, testProjections(t), Config{ConfigPath: f.configPath, StderrSummaryMaxChars: 10, DefaultTimeoutMs: 10_000, Logger: f.logger})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer dispose()
	f.registry = registry
	f.runtime = runtime
	f.startAgent()
	f.executeTool("write")
	var last map[string]any
	for _, event := range hookEvents(t, f.sess) {
		if event["type"] == hookprotocol.EventHookResult {
			last = event
		}
	}
	if last == nil {
		t.Fatal("no hook/result recorded")
	}
	summary := last["stderrSummary"].(string)
	if len([]rune(summary)) != 11 || !strings.HasSuffix(summary, "…") {
		t.Fatalf("summary = %q, want 10 chars + ellipsis", summary)
	}
}

func TestNonPositiveStderrSummaryCapFailsApply(t *testing.T) {
	f := newFixture(t)
	if _, err := Apply(agent.NewAgentRegistry(nil, nil), mustRuntime(t), testProjections(t), Config{ConfigPath: f.configPath, StderrSummaryMaxChars: -1}); err == nil {
		t.Fatal("a non-positive summary cap must fail the apply")
	}
}

func TestSettingsWrapperConfigIsAccepted(t *testing.T) {
	withStubHooks(t, map[string]hookprotocol.HookOutput{"deny": denyOutput()})
	f := newFixture(t)
	f.writeConfig(`{"hooks":{"PreToolUse":[{"hooks":[{"command":"deny"}]}]}}`)
	f.start()
	if !f.executeTool("write").IsError {
		t.Fatal("the settings-wrapped hook config should run")
	}
}
