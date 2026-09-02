package toolsubagent

import (
	"iter"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// catalogAdapter advertises one model with reasoning efforts through the
// advisory catalog and the exact-route resolution.
type catalogAdapter struct {
	llm.BaseAdapter
}

func (a *catalogAdapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	return func(yield func(llm.StreamChunk) bool) { return }
}

func (a *catalogAdapter) ListModels(provider string) ([]llm.LlmModelInfo, error) {
	return []llm.LlmModelInfo{
		{Provider: provider, ID: "deepseek-chat", Name: "DeepSeek Chat", Description: "fast"},
		{Provider: provider, ID: "deepseek-reasoner", Name: "DeepSeek Reasoner"},
	}, nil
}

func (a *catalogAdapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	info := llm.LlmResolvedModelInfo{LlmModelInfo: llm.LlmModelInfo{Provider: provider, ID: model, Name: model}}
	if model == "deepseek-reasoner" {
		info.Reasoning = &llm.LlmModelReasoningInfo{
			Efforts: []llm.LlmReasoningEffortInfo{
				{ID: "low", Name: "Low"},
				{ID: "high", Name: "High", Description: "thorough"},
			},
			DefaultEffort: "low",
		}
	}
	return info, nil
}

func newCatalogRuntime(t *testing.T) *llm.Runtime {
	t.Helper()
	rt := llm.NewRuntime()
	if _, err := rt.RegisterAdapter([]string{"deepseek"}, &catalogAdapter{}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	return rt
}

func policyOf(routes ...AllowedModelRoute) *ModelSelectionPolicy {
	return &ModelSelectionPolicy{Routes: routes}
}

func TestAssertAllowedModelRoutesRejectsBadEntries(t *testing.T) {
	if err := assertAllowedModelRoutes([]AllowedModelRoute{{Provider: "deepseek", Model: ""}}); err == nil ||
		!strings.Contains(err.Error(), "requires non-empty provider and model ids") {
		t.Fatalf("empty model: %v", err)
	}
	dup := []AllowedModelRoute{{Provider: "deepseek", Model: "m"}, {Provider: "deepseek", Model: "m"}}
	if err := assertAllowedModelRoutes(dup); err == nil || !strings.Contains(err.Error(), `repeats route "deepseek/m"`) {
		t.Fatalf("duplicate: %v", err)
	}
}

func strp(v string) *string { return &v }

func TestRequestedAgentOptionsMatrix(t *testing.T) {
	parent := agent.AgentOptions{Provider: "deepseek", Model: "deepseek-chat", ReasoningEffort: "low"}
	configured := &agent.AgentOptions{Provider: "deepseek", Model: "deepseek-chat", ReasoningEffort: "high"}

	// No request: configured passes through untouched.
	out, err := requestedAgentOptions(parent, configured, DelegationModelRequest{}, true)
	if err != nil || out != configured {
		t.Fatalf("no request: (%v, %v)", out, err)
	}
	// Disabled instance rejects any explicit field.
	_, err = requestedAgentOptions(parent, nil, DelegationModelRequest{ReasoningEffort: strp("low")}, false)
	if err == nil || !strings.Contains(err.Error(), "child model selection is disabled for this tool instance") {
		t.Fatalf("disabled: %v", err)
	}
	// Empty values fail at the JSON boundary.
	_, err = requestedAgentOptions(parent, nil, DelegationModelRequest{Provider: strp(""), Model: strp("m")}, true)
	if err == nil || !strings.Contains(err.Error(), "child LLM `provider` must be non-empty") {
		t.Fatalf("empty provider: %v", err)
	}
	// Provider without model fails the together rule.
	_, err = requestedAgentOptions(parent, nil, DelegationModelRequest{Provider: strp("deepseek")}, true)
	if err == nil || !strings.Contains(err.Error(), "child LLM `provider` and `model` must be supplied together") {
		t.Fatalf("together: %v", err)
	}
	// A route change without an effort clears the configured route-owned
	// effort.
	out, err = requestedAgentOptions(parent, configured, DelegationModelRequest{
		Provider: strp("deepseek"), Model: strp("deepseek-reasoner"),
	}, true)
	if err != nil {
		t.Fatalf("route change: %v", err)
	}
	if out.Provider != "deepseek" || out.Model != "deepseek-reasoner" || out.ReasoningEffort != "" {
		t.Fatalf("route change options: %+v", out)
	}
	// The same route keeps the configured effort.
	out, err = requestedAgentOptions(parent, configured, DelegationModelRequest{
		Provider: strp("deepseek"), Model: strp("deepseek-chat"),
	}, true)
	if err != nil || out.ReasoningEffort != "high" {
		t.Fatalf("same route: (%+v, %v)", out, err)
	}
	// An explicit effort wins and the parent supplies missing baselines.
	out, err = requestedAgentOptions(parent, nil, DelegationModelRequest{ReasoningEffort: strp("medium")}, true)
	if err != nil || out.ReasoningEffort != "medium" || out.Provider != "" {
		t.Fatalf("effort only: (%+v, %v)", out, err)
	}
}

func TestAssertAllowedModelSelectionMatrix(t *testing.T) {
	parent := agent.AgentOptions{Provider: "deepseek", Model: "deepseek-chat"}
	request := DelegationModelRequest{ReasoningEffort: strp("low")}
	// No policy or no explicit route request: outside the gate.
	if err := assertAllowedModelSelection(nil, parent, nil, request); err != nil {
		t.Fatalf("nil policy: %v", err)
	}
	if err := assertAllowedModelSelection(policyOf(), parent, nil, DelegationModelRequest{}); err != nil {
		t.Fatalf("no request: %v", err)
	}
	// Same-route effort inherits the parent's allowed route.
	policy := policyOf(AllowedModelRoute{Provider: "deepseek", Model: "deepseek-chat"})
	if err := assertAllowedModelSelection(policy, parent, nil, request); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	// An explicit off-policy route is rejected verbatim.
	offPolicy := &agent.AgentOptions{Provider: "deepseek", Model: "deepseek-reasoner"}
	err := assertAllowedModelSelection(policy, parent, offPolicy, DelegationModelRequest{
		Provider: strp("deepseek"), Model: strp("deepseek-reasoner"),
	})
	if err == nil || !strings.Contains(err.Error(), `child LLM route "deepseek/deepseek-reasoner" is not allowed for this Session`) {
		t.Fatalf("off policy: %v", err)
	}
	// A route request with no effective route at all.
	err = assertAllowedModelSelection(policy, agent.AgentOptions{}, nil, DelegationModelRequest{ReasoningEffort: strp("low")})
	if err == nil || !strings.Contains(err.Error(), "cannot select child LLM values without an effective provider and model") {
		t.Fatalf("no effective route: %v", err)
	}
}

func TestPreflightChildLlmRoute(t *testing.T) {
	rt := newCatalogRuntime(t)
	parent := agent.AgentOptions{Provider: "deepseek", Model: "deepseek-chat"}
	// Pure inheritance preflights the parent route.
	if err := preflightChildLlmRoute(rt, parent, nil); err != nil {
		t.Fatalf("inherit preflight: %v", err)
	}
	// A configured effort not advertised by the resolved model fails.
	requested := &agent.AgentOptions{ReasoningEffort: "ultra"}
	if err := preflightChildLlmRoute(rt, parent, requested); err == nil {
		t.Fatal("unsupported effort accepted")
	}
	// An unregistered provider fails loud.
	orphan := &agent.AgentOptions{Provider: "ghost", Model: "m"}
	if err := preflightChildLlmRoute(rt, parent, orphan); err == nil {
		t.Fatal("unregistered provider accepted")
	}
	// No effective route.
	if err := preflightChildLlmRoute(rt, agent.AgentOptions{}, &agent.AgentOptions{}); err == nil ||
		!strings.Contains(err.Error(), "cannot select child LLM values without an effective provider and model") {
		t.Fatalf("no route: %v", err)
	}
}

func TestSessionPolicyEventRoundTripAndAppendOnce(t *testing.T) {
	header := &session.SessionHeader{ID: "model-selection-sess", CWD: "D:\\work"}
	sess, err := session.NewDetached("model-selection-sess", nil, header, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	// Absence reads as the fixed-route definition.
	policy, err := sessionSubagentModelSelectionPolicy(sess)
	if err != nil || policy != nil {
		t.Fatalf("absent: (%v, %v)", policy, err)
	}
	routes := []AllowedModelRoute{{Provider: "deepseek", Model: "deepseek-chat"}}
	if err := recordSubagentModelSelection(sess, routes); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := recordSubagentModelSelection(sess, []AllowedModelRoute{{Provider: "other", Model: "m"}}); err != nil {
		t.Fatalf("second record: %v", err)
	}
	policy, err = sessionSubagentModelSelectionPolicy(sess)
	if err != nil || policy == nil || len(policy.Routes) != 1 || policy.Routes[0].Model != "deepseek-chat" {
		t.Fatalf("round trip: (%+v, %v)", policy, err)
	}
	// The event count stays at one.
	events := 0
	for _, event := range sess.Events() {
		if event.Type == EventSubagentModelSelectionPolicy {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("event count: %d", events)
	}
}

func TestListSubagentModelsFlows(t *testing.T) {
	rt := newCatalogRuntime(t)
	policy := policyOf(
		AllowedModelRoute{Provider: "deepseek", Model: "deepseek-chat"},
		AllowedModelRoute{Provider: "deepseek", Model: "deepseek-reasoner"},
		AllowedModelRoute{Provider: "other", Model: "m"},
	)

	// No arguments: the policy's registered providers.
	text, err := listSubagentModels(rt, policy, map[string]any{})
	if err != nil || text != "deepseek — deepseek" {
		t.Fatalf("providers: (%q, %v)", text, err)
	}
	// An empty policy lists nothing.
	text, err = listSubagentModels(rt, policyOf(), map[string]any{})
	if err != nil || text != "(no LLM providers)" {
		t.Fatalf("empty policy: (%q, %v)", text, err)
	}
	// Provider listing filters to allowed models.
	text, err = listSubagentModels(rt, policy, map[string]any{"provider": "deepseek"})
	if err != nil || !strings.Contains(text, "deepseek/deepseek-chat — DeepSeek Chat: fast") ||
		!strings.Contains(text, "deepseek/deepseek-reasoner — DeepSeek Reasoner") {
		t.Fatalf("models: (%q, %v)", text, err)
	}
	// An unallowed provider is rejected verbatim.
	_, err = listSubagentModels(rt, policy, map[string]any{"provider": "ghost"})
	if err == nil || !strings.Contains(err.Error(), `LLM provider "ghost" is not allowed for this Session`) {
		t.Fatalf("unallowed: %v", err)
	}
	// A policy-allowed but unregistered provider reports availability.
	allowed := policyOf(AllowedModelRoute{Provider: "anthropic", Model: "m"})
	_, err = listSubagentModels(rt, allowed, map[string]any{"provider": "anthropic"})
	if err == nil || !strings.Contains(err.Error(), "not registered; available providers: (none)") {
		t.Fatalf("absent route: %v", err)
	}
	// The exact model face renders reasoning efforts with the default mark.
	text, err = listSubagentModels(rt, policy, map[string]any{"provider": "deepseek", "model": "deepseek-reasoner"})
	if err != nil || !strings.Contains(text, "low (default) — Low") || !strings.Contains(text, "high — High: thorough") {
		t.Fatalf("efforts: (%q, %v)", text, err)
	}
	// An unadvertised but allowed model reports the empty face.
	text, err = listSubagentModels(rt, policy, map[string]any{"provider": "deepseek", "model": "deepseek-chat"})
	if err != nil || !strings.Contains(text, "(no advertised reasoning efforts)") {
		t.Fatalf("no efforts: (%q, %v)", text, err)
	}
	// Argument shapes: model without provider, empty values.
	if _, err := listSubagentModels(rt, policy, map[string]any{"model": "m"}); err == nil ||
		!strings.Contains(err.Error(), "`model` requires `provider`") {
		t.Fatalf("model only: %v", err)
	}
	if _, err := listSubagentModels(rt, policy, map[string]any{"provider": ""}); err == nil ||
		!strings.Contains(err.Error(), "`provider` must be non-empty") {
		t.Fatalf("empty provider: %v", err)
	}
	if _, err := listSubagentModels(rt, policy, map[string]any{"provider": "deepseek", "model": ""}); err == nil ||
		!strings.Contains(err.Error(), "`model` must be non-empty") {
		t.Fatalf("empty model: %v", err)
	}
}

func TestResolveDelegationPolicySamplesSettingsOnce(t *testing.T) {
	header := &session.SessionHeader{ID: "policy-parent", CWD: "D:\\work"}
	sess, err := session.NewDetached("policy-parent", nil, header, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	parent := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess}, registry.Events())

	// No settings service: fixed-route definition.
	policy, err := resolveDelegationPolicy(nil, parent)
	if err != nil || policy != nil {
		t.Fatalf("nil selection: (%v, %v)", policy, err)
	}
	// A disabled preference stays outside the gate.
	disabled, err := NewModelSelectionConfig(ModelSelectionSettings{})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	policy, err = resolveDelegationPolicy(disabled, parent)
	if err != nil || policy != nil {
		t.Fatalf("disabled: (%v, %v)", policy, err)
	}
	// An enabled preference is sampled and recorded once.
	enabled, err := NewModelSelectionConfig(ModelSelectionSettings{
		Enabled:       true,
		AllowedModels: []AllowedModelRoute{{Provider: "deepseek", Model: "deepseek-chat"}},
	})
	if err != nil {
		t.Fatalf("enabled config: %v", err)
	}
	policy, err = resolveDelegationPolicy(enabled, parent)
	if err != nil || policy == nil || len(policy.Routes) != 1 {
		t.Fatalf("enabled: (%v, %v)", policy, err)
	}
	// A later settings change cannot rewrite the recorded policy.
	enabled.SetSource(func() ModelSelectionSettings {
		return ModelSelectionSettings{Enabled: true, AllowedModels: []AllowedModelRoute{{Provider: "other", Model: "m"}}}
	})
	policy, err = resolveDelegationPolicy(enabled, parent)
	if err != nil || policy == nil || policy.Routes[0].Provider != "deepseek" {
		t.Fatalf("recorded once: (%v, %v)", policy, err)
	}
	// Enabled without routes fails at the settings boundary.
	if _, err := NewModelSelectionConfig(ModelSelectionSettings{Enabled: true}); err == nil ||
		!strings.Contains(err.Error(), "enabled subagent model selection requires at least one allowed model") {
		t.Fatalf("enabled without routes: %v", err)
	}
}
