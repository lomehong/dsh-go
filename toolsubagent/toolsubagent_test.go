package toolsubagent

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/subagent"
	"dshgo/tools"
)

// fakeProvider is a one-shot provider with configurable capabilities.
type fakeProvider struct {
	name       string
	caps       subagent.SubagentCapabilities
	inherits   bool
	startCount int
}

func (p *fakeProvider) Name() string                                { return p.name }
func (p *fakeProvider) Capabilities() subagent.SubagentCapabilities { return p.caps }
func (p *fakeProvider) InheritsParentContext() bool                 { return p.inherits }
func (p *fakeProvider) Start(request subagent.ResolvedSubagentStartRequest) (subagent.SubagentRun, error) {
	p.startCount++
	return &fakeRun{}, nil
}

// continuableFakeProvider additionally satisfies the continuable face.
type continuableFakeProvider struct{ fakeProvider }

func (p *continuableFakeProvider) PrepareContinuable(request subagent.ContinuableCreateRequest) (subagent.ContinuableCreateSpec, error) {
	return subagent.ContinuableCreateSpec{}, nil
}

// fakeRun settles completed with one text block unless overridden.
type fakeRun struct {
	disposed bool
	result   subagent.SubagentResult
}

func (r *fakeRun) ID() session.SessionID    { return "child-1" }
func (r *fakeRun) LocalAgent() *agent.Agent { return nil }
func (r *fakeRun) Result() (subagent.SubagentResult, error) {
	if r.result.StopReason == "" {
		return subagent.SubagentResult{
			Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "child answer"}},
			StopReason: subagent.StopCompleted,
		}, nil
	}
	return r.result, nil
}
func (r *fakeRun) Dispose() error { r.disposed = true; return nil }

// newToolRuntime builds an empty tool runtime for tests.
func newToolRuntime(t *testing.T) *tools.ToolRuntime {
	t.Helper()
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	return runtime
}

func newTestRuntime(t *testing.T, provider subagent.SubagentProvider) *subagent.SubagentRuntime {
	t.Helper()
	runtime := subagent.NewSubagentRuntime(subagent.RuntimeConfig{})
	if _, err := runtime.RegisterProvider(provider); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	return runtime
}

func TestResolveConfigDefaultsAndValidations(t *testing.T) {
	if _, err := ResolveConfig(Config{}); err == nil || !strings.Contains(err.Error(), "provider is required") {
		t.Fatalf("empty provider: %v", err)
	}
	if _, err := ResolveConfig(Config{Provider: "spawn", BackgroundMode: "whenever"}); err == nil || !strings.Contains(err.Error(), `backgroundMode must be "one-shot" or "continuable"`) {
		t.Fatalf("bad backgroundMode: %v", err)
	}
	if _, err := ResolveConfig(Config{Provider: "spawn", ToolFilter: &ToolFilterConfig{}}); err == nil || !strings.Contains(err.Error(), "names neither `allow` nor `deny`") {
		t.Fatalf("empty filter: %v", err)
	}
	if _, err := ResolveConfig(Config{Provider: "spawn", MaxDepth: -1}); err == nil {
		t.Fatal("negative maxDepth accepted")
	}
	resolved, err := ResolveConfig(Config{Provider: "spawn"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ToolName != "subagent" || !resolved.EnableRunInBackground || resolved.BackgroundMode != BackgroundOneShot {
		t.Fatalf("defaults: %+v", resolved)
	}
}

func TestAssertProviderConfigurationGates(t *testing.T) {
	config := Config{Provider: "spawn", MaxDepth: 3}
	noDepth := &fakeProvider{name: "spawn", caps: subagent.SubagentCapabilities{AgentOptions: true}}
	err := assertProviderConfiguration(noDepth, config, false)
	if err == nil || !strings.Contains(err.Error(), "cannot enforce maxDepth (no depthLimit capability)") {
		t.Fatalf("depth gate: %v", err)
	}
	noOptions := &fakeProvider{name: "spawn", caps: subagent.SubagentCapabilities{DepthLimit: true}}
	err = assertProviderConfiguration(noOptions, Config{Provider: "spawn", AgentOptions: &agent.AgentOptions{}}, false)
	if err == nil || !strings.Contains(err.Error(), "does not support child agentOptions") {
		t.Fatalf("agentOptions gate: %v", err)
	}
	err = assertProviderConfiguration(noDepth, Config{Provider: "spawn", MaxDepthProviderManaged: true}, true)
	if err == nil || !strings.Contains(err.Error(), "does not support `backgroundMode: continuable`") {
		t.Fatalf("continuable gate: %v", err)
	}
	full := &continuableFakeProvider{fakeProvider{name: "spawn", caps: subagent.SubagentCapabilities{
		AgentOptions: true, DepthLimit: true, ToolFilter: true, Persona: true,
	}}}
	if err := assertProviderConfiguration(full, config, true); err != nil {
		t.Fatalf("complete provider rejected: %v", err)
	}
}

func TestResolveDelegationRunMatrix(t *testing.T) {
	if _, err := resolveDelegationRun(delegationArgs{runInBackground: true}, false, false); err == nil ||
		!strings.Contains(err.Error(), "run_in_background is disabled for this tool instance (enableRunInBackground: false)") {
		t.Fatalf("forced background rejected: %v", err)
	}
	if background, _ := resolveDelegationRun(delegationArgs{}, true, false); background {
		t.Fatal("one-shot default must stay foreground")
	}
	if background, _ := resolveDelegationRun(delegationArgs{}, true, true); !background {
		t.Fatal("continuable default must run in background")
	}
	if background, _ := resolveDelegationRun(delegationArgs{runInBackground: true}, true, false); !background {
		t.Fatal("explicit background honored")
	}
	if background, _ := resolveDelegationRun(delegationArgs{runInBackground: true}, false, true); background {
		t.Fatal("disabled instance must reject even under continuable")
	}
}

func TestStopReasonHeadlinesAndPartialText(t *testing.T) {
	cases := map[subagent.StopReason]string{
		subagent.StopCompleted:       "",
		subagent.StopAborted:         "subagent run was cancelled",
		subagent.StopError:           "subagent run failed",
		subagent.StopMaxTokens:       "subagent run hit its token limit before finishing",
		subagent.StopRefusal:         "subagent declined the task",
		subagent.StopReason("weird"): "subagent run ended abnormally (weird)",
	}
	for reason, want := range cases {
		if got := stopReasonError(subagent.SubagentResult{StopReason: reason}); got != want {
			t.Fatalf("reason %q: got %q want %q", reason, got, want)
		}
	}
	composed := withDiagnosticAndPartialText("subagent run failed", subagent.SubagentResult{
		StopReason: subagent.StopError,
		Diagnostic: "provider boom",
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "partial"}},
	})
	for _, fragment := range []string{"subagent run failed", "\nDiagnostic: provider boom", "\nPartial output before the run ended:\npartial"} {
		if !strings.Contains(composed, fragment) {
			t.Fatalf("missing %q in %q", fragment, composed)
		}
	}
	plain := withDiagnosticAndPartialText("subagent run failed", subagent.SubagentResult{StopReason: subagent.StopError})
	if strings.Contains(plain, "Diagnostic") || strings.Contains(plain, "Partial output") {
		t.Fatalf("empty diagnostic/partial must be omitted: %q", plain)
	}
}

func TestSettleForegroundRunShapes(t *testing.T) {
	value, err := settleForegroundRun(&fakeRun{})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	foreground, ok := value.(map[string]any)
	if !ok || foreground["kind"] != "foreground" || foreground["runId"] != session.SessionID("child-1") {
		t.Fatalf("foreground value: %#v", value)
	}
	output, ok := foreground["output"].([]llm.ContentBlock)
	if !ok || len(output) != 1 || output[0].Text != "child answer" {
		t.Fatalf("output: %#v", foreground["output"])
	}
	// A non-completed stop reason errors with partial text; disposal still
	// ran.
	run := &fakeRun{result: subagent.SubagentResult{
		StopReason: subagent.StopError,
		Diagnostic: "boom",
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "half"}},
	}}
	_, err = settleForegroundRun(run)
	if err == nil || !run.disposed || !strings.Contains(err.Error(), "subagent run failed") ||
		!strings.Contains(err.Error(), "Diagnostic: boom") || !strings.Contains(err.Error(), "half") {
		t.Fatalf("stop-reason error: %v (disposed=%v)", err, run.disposed)
	}
}

func TestRegisterMountsTool(t *testing.T) {
	runtime := newToolRuntime(t)
	provider := &continuableFakeProvider{fakeProvider{name: "spawn", caps: subagent.SubagentCapabilities{
		AgentOptions: true, DepthLimit: true, ToolFilter: true, Persona: true,
	}}}
	deps := Deps{Runtime: runtime, Subagents: newTestRuntime(t, provider), ResolveAgent: func(tools.ScopeKey) *agent.Agent { return nil }}
	detach, err := Register(deps, Config{Provider: "spawn", BackgroundMode: BackgroundContinuable})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	definition, ok := runtime.Get("subagent", nil)
	if !ok {
		t.Fatal("tool not mounted")
	}
	if !strings.Contains(definition.Description, "runs in the background by default") {
		t.Fatalf("continuable description missing: %q", definition.Description)
	}
	detach()
	if _, ok := runtime.Get("subagent", nil); ok {
		t.Fatal("tool still mounted after dispose")
	}
	// A background-enabled one-shot instance exposes the scheduling
	// parameter; the fork-style standalone prompt wording shows through.
	oneShotProvider := &fakeProvider{name: "spawn", caps: subagent.SubagentCapabilities{DepthLimit: true}}
	deps2 := Deps{Runtime: newToolRuntime(t), Subagents: newTestRuntime(t, oneShotProvider)}
	detach2, err := Register(deps2, Config{Provider: "spawn"})
	if err != nil {
		t.Fatalf("register one-shot: %v", err)
	}
	defer detach2()
	definition2, _ := deps2.Runtime.Get("subagent", nil)
	properties, _ := definition2.Parameters["properties"].(map[string]any)
	if _, has := properties["run_in_background"]; !has {
		t.Fatal("run_in_background parameter missing")
	}
	if !strings.Contains(definition2.Description, "does not see this conversation") {
		t.Fatalf("standalone wording missing: %q", definition2.Description)
	}
	// The inherited-context provider flips the wording.
	inheriting := &fakeProvider{name: "fork", inherits: true, caps: subagent.SubagentCapabilities{DepthLimit: true}}
	deps3 := Deps{Runtime: newToolRuntime(t), Subagents: newTestRuntime(t, inheriting)}
	detach3, err := Register(deps3, Config{Provider: "fork"})
	if err != nil {
		t.Fatalf("register fork: %v", err)
	}
	defer detach3()
	definition3, _ := deps3.Runtime.Get("subagent", nil)
	if !strings.Contains(definition3.Description, "inherits this conversation") {
		t.Fatalf("inherited wording missing: %q", definition3.Description)
	}
	// An absent provider fails loud at registration.
	if _, err := Register(Deps{Runtime: newToolRuntime(t), Subagents: subagent.NewSubagentRuntime(subagent.RuntimeConfig{})}, Config{Provider: "ghost"}); err == nil ||
		!strings.Contains(err.Error(), "not registered yet") {
		t.Fatalf("absent provider: %v", err)
	}
}
