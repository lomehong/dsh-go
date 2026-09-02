package toolsubagentreport

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/subagent"
	"dshgo/systemprompt"
	"dshgo/tools"
)

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

// newChildAgent builds one live scoped child, mirroring the subagent
// package's managed-agent fixture.
func newChildAgent(t *testing.T, id string) *agent.Agent {
	t.Helper()
	header := &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"}
	sess, err := session.NewDetached(session.SessionID(id), nil, header, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

func TestResolveConfigValidatesDelivery(t *testing.T) {
	resolved, err := ResolveConfig(Config{})
	if err != nil || resolved.ReportDelivery != subagent.DeliveryNextStep {
		t.Fatalf("default: (%+v, %v)", resolved, err)
	}
	if _, err := ResolveConfig(Config{ReportDelivery: subagent.DeliveryQuiet}); err != nil {
		t.Fatalf("quiet: %v", err)
	}
	if _, err := ResolveConfig(Config{ReportDelivery: "whenever"}); err == nil ||
		!strings.Contains(err.Error(), `reportDelivery must be "quiet" or "next-step"`) {
		t.Fatalf("bad delivery: %v", err)
	}
}

func TestRegisterValidatesDeps(t *testing.T) {
	if _, err := Register(Deps{}, Config{}); err == nil ||
		!strings.Contains(err.Error(), "the subagent runtime, tool runtime, system prompt, and agent resolver are required") {
		t.Fatalf("deps: %v", err)
	}
}

func newReportDeps(t *testing.T) (Deps, *subagent.SubagentRuntime) {
	t.Helper()
	runtime := subagent.NewSubagentRuntime(subagent.RuntimeConfig{})
	toolRuntime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("system prompt: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	deps := Deps{
		Subagents: runtime,
		Tools:     toolRuntime,
		Prompt:    prompt,
		ResolveAgent: func(key tools.ScopeKey) *agent.Agent {
			if key == nil {
				return nil
			}
			for _, candidate := range registry.List() {
				if candidate.Scope == key {
					return candidate
				}
			}
			return nil
		},
	}
	return deps, runtime
}

func TestInstallReportToolScopesToolAndDisposes(t *testing.T) {
	deps, _ := newReportDeps(t)
	child := newChildAgent(t, "report-child-1")

	dispose := installReportTool(child, deps, subagent.DeliveryNextStep)
	definition, ok := deps.Tools.Get("report", child.Scope)
	if !ok {
		t.Fatal("report tool not visible in the child scope")
	}
	if !strings.Contains(definition.Description, "Report selected content to the agent that started you") {
		t.Fatalf("description: %q", definition.Description)
	}
	if _, ok := deps.Tools.Get("report", nil); ok {
		t.Fatal("report tool leaked to the global scope")
	}
	// The execute path demands a live calling agent at the exact scope.
	_, err := definition.Execute(map[string]any{"output": "hello"}, &tools.ToolRunContext{
		ToolExecution: tools.ToolExecution{Agent: scope.NewScopeKey(nil)},
	})
	if err == nil || !strings.Contains(err.Error(), "the report tool requires a calling agent") {
		t.Fatalf("stranger execution: %v", err)
	}
	// Disposal revokes the child-scoped registration.
	dispose()
	if _, ok := deps.Tools.Get("report", child.Scope); ok {
		t.Fatal("report tool survived disposal")
	}
}

func TestInstallReportToolNilChildPanics(t *testing.T) {
	deps, _ := newReportDeps(t)
	defer func() {
		if recover() == nil {
			t.Fatal("nil child accepted")
		}
	}()
	installReportTool(nil, deps, subagent.DeliveryNextStep)
}

func TestRegisterContributesThroughSetupRegistry(t *testing.T) {
	deps, runtime := newReportDeps(t)
	detach, err := Register(deps, Config{ReportDelivery: subagent.DeliveryQuiet})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	// The contribution installs through a real child context carrying the
	// agent service (the manager's Apply is the only production caller).
	child := newChildAgent(t, "report-child-2")
	childCtx := cordis.NewRoot(cordis.Discard{}).Child()
	childCtx.Provide("agent", child)
	if _, err := runtime.SetupRegistry().Apply(childCtx); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := deps.Tools.Get("report", child.Scope); !ok {
		t.Fatal("contribution did not install the report tool")
	}
	// The execute path without a live resolver rejects at the authority
	// boundary before reaching the runtime.
	definition, ok := deps.Tools.Get("report", child.Scope)
	if !ok {
		t.Fatal("report tool missing")
	}
	_, err = definition.Execute(map[string]any{"output": "hello"}, &tools.ToolRunContext{
		ToolExecution: tools.ToolExecution{Agent: scope.NewScopeKey(nil)},
	})
	if err == nil || !strings.Contains(err.Error(), "the report tool requires a calling agent") {
		t.Fatalf("no-resolver execution: %v", err)
	}
}
