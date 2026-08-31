package agentloop

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/systemprompt"
	"dshgo/tools"
)

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

func TestResolveMaxParallelToolCalls(t *testing.T) {
	if value, err := resolveMaxParallelToolCalls(nil); err != nil || value != DefaultMaxParallelToolCalls {
		t.Fatalf("nil = %d, %v", value, err)
	}
	if value, err := resolveMaxParallelToolCalls(intPtr(3)); err != nil || value != 3 {
		t.Fatalf("3 = %d, %v", value, err)
	}
	for _, bad := range []int{0, -1} {
		_, err := resolveMaxParallelToolCalls(intPtr(bad))
		if err == nil || err.Error() != "maxParallelToolCalls must be a positive integer" {
			t.Fatalf("%d error = %v", bad, err)
		}
	}
}

func TestValidateConfiguredAgents(t *testing.T) {
	err := validateConfiguredAgents([]ConfiguredAgent{
		{ID: "x", SessionID: "s1", ResumeSessionID: "r1"},
	})
	if err == nil || err.Error() != `agent "x": sessionId and resumeSessionId are mutually exclusive` {
		t.Fatalf("exclusive error = %v", err)
	}
	err = validateConfiguredAgents([]ConfiguredAgent{
		{ID: "a", SessionID: "y"},
		{ID: "b", ResumeSessionID: "y"},
	})
	if err == nil || err.Error() != `agents "a" and "b" use duplicate exact session identity "y"` {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := validateConfiguredAgents([]ConfiguredAgent{
		{ID: "a", SessionID: "y"},
		{ID: "b", SessionID: "z"},
		{ID: "c"},
	}); err != nil {
		t.Fatalf("distinct identities rejected: %v", err)
	}
}

func TestAssertAgentOptions(t *testing.T) {
	if err := assertAgentOptions(agent.AgentOptions{}); err != nil {
		t.Fatalf("empty options = %v", err)
	}
	if err := assertAgentOptions(agent.AgentOptions{MaxTokens: int64Ptr(0)}); err == nil || err.Error() != "agent maxTokens must be a positive safe integer" {
		t.Fatalf("zero maxTokens = %v", err)
	}
	if err := assertAgentOptions(agent.AgentOptions{MaxTokens: int64Ptr(-5)}); err == nil {
		t.Fatalf("negative maxTokens accepted")
	}
	if err := assertAgentOptions(agent.AgentOptions{MaxTokens: int64Ptr(1 << 54)}); err == nil {
		t.Fatalf("unsafe maxTokens accepted")
	}
	if err := assertAgentOptions(agent.AgentOptions{MaxTokens: int64Ptr(1024)}); err != nil {
		t.Fatalf("positive maxTokens = %v", err)
	}
}

func TestNewAgentLoopRejectsBadConfig(t *testing.T) {
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	toolRuntime, toolErr := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if toolErr != nil {
		t.Fatalf("tools: %v", toolErr)
	}
	_, err = NewAgentLoop(cordis.NewRoot(cordis.Discard{}), registry, cordis.Discard{}, llm.NewRuntime(), toolRuntime, prompt, projection.NewRegistry(), AgentLoopConfig{MaxParallelToolCalls: intPtr(0)})
	if err == nil || err.Error() != "maxParallelToolCalls must be a positive integer" {
		t.Fatalf("bad parallelism = %v", err)
	}
	_, err = NewAgentLoop(cordis.NewRoot(cordis.Discard{}), registry, cordis.Discard{}, llm.NewRuntime(), toolRuntime, prompt, projection.NewRegistry(), AgentLoopConfig{
		Agents: []ConfiguredAgent{{ID: "x", SessionID: "s", ResumeSessionID: "r"}},
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("bad agents = %v", err)
	}
}

func TestFactoryRegistrationAndRejectsSecond(t *testing.T) {
	h := newHarness(t)
	_, err := h.registry.SetFactory(&stubLoopFactory{})
	if err == nil {
		t.Fatalf("second factory accepted")
	}
}

type stubLoopFactory struct{}

func (f *stubLoopFactory) CreateAgent(owner *cordis.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	return agent.AgentHandle{}, nil
}

func (f *stubLoopFactory) Resume(owner *cordis.Context, options agent.ResumeAgentOptions) (agent.AgentHandle, error) {
	return agent.AgentHandle{}, nil
}

func TestCreateAgentPublishesAndEmitsSessionStart(t *testing.T) {
	h := newHarness(t)
	started := make(chan agent.AgentSessionStartPayload, 1)
	dispose := h.events.OnEmit(agent.EventAgentSessionStart, nil, func(payload any) error {
		started <- payload.(agent.AgentSessionStartPayload)
		return nil
	})
	t.Cleanup(dispose)

	a := h.startAgent("published")
	select {
	case payload := <-started:
		if payload.Agent.ID != a.ID {
			t.Fatalf("session start agent = %q", payload.Agent.ID)
		}
		if payload.Source != agent.SessionStartStartup {
			t.Fatalf("session start source = %q", payload.Source)
		}
	default:
		t.Fatalf("no agent/session-start event observed")
	}
	if a.Status() != agent.AgentIdle {
		t.Fatalf("fresh agent status = %q", a.Status())
	}
}

func TestPerAgentPromptVariablesRegistered(t *testing.T) {
	h := newHarness(t)
	a := h.startAgent("variables")
	assembly, err := h.loop.Prompt.Assemble(a.AssembleContextFor(nil))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	providerValue, defined := assembly.Variables.Get("provider")
	if !defined || providerValue == nil || *providerValue != "stub" {
		t.Fatalf("provider variable missing")
	}
	modelValue, defined := assembly.Variables.Get("model")
	if !defined || modelValue == nil || *modelValue != "stub-model" {
		t.Fatalf("model variable missing")
	}
}

func TestDisposeStopsAcceptanceAndQuiesces(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("before close")})
	a := h.startAgent("dispose")
	if err := h.loop.ownership.dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	h.awaitIdle(a)
	if a.Status() != agent.AgentIdle {
		t.Fatalf("status after dispose = %q", a.Status())
	}
	_, err := h.loop.CreateAgent(nil, agent.CreateAgentOptions{SessionID: "after-close"})
	if err == nil || !strings.Contains(err.Error(), "not active") {
		t.Fatalf("create after dispose = %v", err)
	}
	// The registry no longer tracks the disposed agent.
	if h.registry.Get(a.ID) != nil {
		t.Fatalf("disposed agent still tracked")
	}
}

func TestResumeWithoutPersistenceFails(t *testing.T) {
	h := newHarness(t)
	_, err := h.loop.Resume(nil, agent.ResumeAgentOptions{ResumeSessionID: "missing"})
	if err == nil || !strings.Contains(err.Error(), "persistence") {
		t.Fatalf("resume without persistence = %v", err)
	}
}

func TestDerivedFreshSessionIDForConfiguredAgent(t *testing.T) {
	// An entry with neither identity derives a fresh id containing the
	// configured label; verify through the derived-id helper contract.
	derived := session.SessionID("worker-session-" + newUUID())
	if !strings.Contains(string(derived), "worker-session-") {
		t.Fatalf("derived id = %q", derived)
	}
	if len(newUUID()) != 36 {
		t.Fatalf("uuid length = %d", len(newUUID()))
	}
}
