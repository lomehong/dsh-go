package subagent

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

func newChildParent(t *testing.T, id string) (*agent.Agent, *session.Session) {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, childNoopNotifications{})
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
	return built, sess
}

type childNoopNotifications struct{}

func (childNoopNotifications) Inserted(llm.Message)       {}
func (childNoopNotifications) Discarded(llm.Message)      {}
func (childNoopNotifications) Claimed(llm.Message, int64) {}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestResolveChildDepthAndCap(t *testing.T) {
	parent, _ := newChildParent(t, "parent-1")
	depth, err := ResolveChildDepth(parent, nil)
	if err != nil || depth != 1 {
		t.Fatalf("depth = %d/%v, want 1", depth, err)
	}
	cap := int64(0)
	if _, err := ResolveChildDepth(parent, &cap); err == nil ||
		err.Error() != "subagent depth 1 exceeds maxDepth 0" {
		t.Fatalf("cap error = %v", err)
	} else {
		var depthErr *SubagentDepthError
		if !asDepthError(err, &depthErr) || depthErr.MaxDepth != 0 || depthErr.AttemptedDepth != 1 {
			t.Fatalf("cap error type = %v", err)
		}
	}
	// A depth equal to the cap is admitted.
	okCap := int64(1)
	if _, err := ResolveChildDepth(parent, &okCap); err != nil {
		t.Fatalf("depth == cap must pass: %v", err)
	}
	// A deeper parent (via runtime options) raises the floor.
	deeper := int64(3)
	parent.Options.SubagentDepth = &deeper
	depth, err = ResolveChildDepth(parent, nil)
	if err != nil || depth != 4 {
		t.Fatalf("deeper depth = %d/%v, want 4", depth, err)
	}
}

func asDepthError(err error, target **SubagentDepthError) bool {
	if depthErr, ok := err.(*SubagentDepthError); ok {
		*target = depthErr
		return true
	}
	return false
}

func TestParentAgentOptionsForDelegation(t *testing.T) {
	parent, sess := newChildParent(t, "parent-2")
	parent.Options.Provider = "deepseek"
	parent.Options.Model = "m-1"
	parent.Options.ReasoningEffort = "high"
	tokens := int64(512)
	parent.Options.MaxTokens = &tokens
	// No request header yet: creation options stand in.
	resolved := parentAgentOptionsForDelegation(parent)
	if resolved.Provider != "deepseek" || resolved.Model != "m-1" || resolved.ReasoningEffort != "high" || resolved.MaxTokens != &tokens {
		t.Fatalf("pre-request resolved = %+v", resolved)
	}
	// The latest request header owns the route and effort; maxTokens stays.
	if _, err := sess.Append(session.EventRequestHeader, session.RequestHeaderData{
		Header: session.EpochHeader{Config: llm.LlmCallConfig{Provider: "openai", Model: "m-2"}},
		Reason: "initial",
	}, nil); err != nil {
		t.Fatalf("append header: %v", err)
	}
	resolved = parentAgentOptionsForDelegation(parent)
	if resolved.Provider != "openai" || resolved.Model != "m-2" {
		t.Fatalf("post-request route = %+v", resolved)
	}
	if resolved.ReasoningEffort != "" {
		t.Fatalf("header without effort must clear route-owned effort, got %q", resolved.ReasoningEffort)
	}
	if resolved.MaxTokens != &tokens {
		t.Fatal("creation maxTokens must survive request-time selection")
	}
}

func TestResolveChildAgentOptions(t *testing.T) {
	parent, sess := newChildParent(t, "parent-3")
	parent.Options.Provider = "deepseek"
	parent.Options.Model = "m-1"
	parent.Options.ReasoningEffort = "high"
	tokens := int64(512)
	parent.Options.MaxTokens = &tokens
	if _, err := sess.Append(session.EventRequestHeader, session.RequestHeaderData{
		Header: session.EpochHeader{Config: llm.LlmCallConfig{Provider: "deepseek", Model: "m-1", ReasoningEffort: "high"}},
		Reason: "initial",
	}, nil); err != nil {
		t.Fatalf("append header: %v", err)
	}
	// Inheritance without overrides stamps the child depth and keeps effort.
	depth := int64(2)
	resolved := resolveChildAgentOptions(parent, nil, depth)
	if resolved.Provider != "deepseek" || resolved.Model != "m-1" || resolved.ReasoningEffort != "high" {
		t.Fatalf("inherited = %+v", resolved)
	}
	if resolved.SubagentDepth == nil || *resolved.SubagentDepth != 2 || resolved.MaxTokens != &tokens {
		t.Fatalf("depth/maxTokens = %+v", resolved)
	}
	// A route change without a named effort clears the parent's route-owned
	// effort so the selected model resolves its own default.
	resolved = resolveChildAgentOptions(parent, &agent.AgentOptions{Provider: "openai", Model: "m-9"}, depth)
	if resolved.Provider != "openai" || resolved.Model != "m-9" || resolved.ReasoningEffort != "" {
		t.Fatalf("route-change = %+v", resolved)
	}
	// Naming an effort alongside the route change keeps it.
	resolved = resolveChildAgentOptions(parent, &agent.AgentOptions{Provider: "openai", Model: "m-9", ReasoningEffort: "low"}, depth)
	if resolved.ReasoningEffort != "low" {
		t.Fatalf("named effort lost: %+v", resolved)
	}
	// Same route keeps the inherited effort.
	resolved = resolveChildAgentOptions(parent, &agent.AgentOptions{}, depth)
	if resolved.ReasoningEffort != "high" {
		t.Fatalf("same-route effort lost: %+v", resolved)
	}
}

type stubPresets struct {
	joined []scope.ScopeKey
}

func (s *stubPresets) ComposedPreset() string { return "explorer" }
func (s *stubPresets) ComposeFrom(child scope.ScopeKey, parent *agent.Agent) {
	s.joined = append(s.joined, child)
}

func TestChildSessionMeta(t *testing.T) {
	parent, _ := newChildParent(t, "parent-4")
	meta := ChildSessionMeta(&stubPresets{}, parent, 2, 0)
	if meta.Origin != "subagent" || meta.ParentSession != "parent-4" || meta.CWD != "D:\\work" {
		t.Fatalf("meta = %+v", meta)
	}
	if meta.DelegationDepth == nil || *meta.DelegationDepth != 2 {
		t.Fatalf("delegationDepth = %+v", meta.DelegationDepth)
	}
	if meta.SeedLength != nil {
		t.Fatal("zero lineage must not record a seed length")
	}
	if meta.AgentPreset != "explorer" {
		t.Fatalf("preset = %q", meta.AgentPreset)
	}
	seeded := ChildSessionMeta(nil, parent, 1, 7)
	if seeded.SeedLength == nil || *seeded.SeedLength != 7 {
		t.Fatalf("seedLength = %+v", seeded.SeedLength)
	}
	if seeded.AgentPreset != "" {
		t.Fatal("a rosterless deployment must not invent a preset")
	}
}

func TestApplyChildCompositionRegistersScopedRows(t *testing.T) {
	parent, _ := newChildParent(t, "parent-5")
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("system prompt: %v", err)
	}
	registry, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	childScope := systemprompt.NewScopeKey(parent.Scope)
	presets := &stubPresets{}
	ApplyChildComposition(childScope, parent, ChildComposition{
		Persona:    "scout",
		ToolFilter: &ToolRestriction{Deny: []string{"shell"}},
	}, ChildCompositionDeps{Prompt: prompt, Registry: registry, Presets: presets})
	// The join happened against the child scope.
	if len(presets.joined) != 1 || presets.joined[0] != childScope {
		t.Fatalf("joined = %v", presets.joined)
	}
	// The delegation context rides the context snapshot; the persona is a
	// prompt section.
	assembly, err := prompt.Assemble(systemprompt.AssembleContext{Scope: childScope})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	text, err := systemprompt.RenderContextSnapshot(assembly)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(text, "delegated subagent") {
		t.Fatalf("child prompt missing delegation context: %q", text)
	}
	personaFound := false
	for _, section := range assembly.Sections {
		if section.Name == systemprompt.PERSONA_SECTION && strings.Contains(section.Text, "scout") {
			personaFound = true
		}
	}
	if !personaFound {
		t.Fatalf("persona section missing: %+v", assembly.Sections)
	}
	// Siblings at the parent scope see neither row.
	parentAssembly, err := prompt.Assemble(systemprompt.AssembleContext{Scope: parent.Scope})
	if err != nil {
		t.Fatalf("assemble parent: %v", err)
	}
	parentText, err := systemprompt.RenderContextSnapshot(parentAssembly)
	if err != nil {
		t.Fatalf("render parent: %v", err)
	}
	if strings.Contains(parentText, "scout") || strings.Contains(parentText, "delegated subagent") {
		t.Fatalf("parent leaked child rows: %q", parentText)
	}
	// The restriction denies only the named tool at the child scope.
	if _, ok := registry.Get("shell", childScope); ok {
		t.Fatal("denied tool must vanish at the child scope")
	}
}

func TestDelegatedPolicyCaptureAndAppend(t *testing.T) {
	parent, sess := newChildParent(t, "parent-6")
	// No sandbox service: capture keeps only the approval pin.
	captured := CaptureDelegatedPolicyOverrides(nil, true, parent)
	if captured.SandboxMode != "" || !captured.ApprovalNever {
		t.Fatalf("captured = %+v", captured)
	}
	if err := AppendDelegatedPolicyOverrides(sess, captured); err != nil {
		t.Fatalf("append: %v", err)
	}
	var found *session.Event
	for i := range sess.Events() {
		if sess.Events()[i].Type == userapproval.EventApprovalPolicy {
			found = &sess.Events()[i]
		}
	}
	if found == nil {
		t.Fatal("approval pin missing from the child log")
	}
	var data userapproval.PolicyData
	if err := json.Unmarshal(found.Data, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if data.Policy != userapproval.PolicyNever || data.Source != "delegation" {
		t.Fatalf("pin = %+v", data)
	}
	// A parent's explicit sandbox override appends the sandbox/mode pin and
	// folds back to the same value from the child log alone.
	if err := AppendDelegatedPolicyOverrides(sess, DelegatedPolicyOverrides{
		ApprovalNever: true,
		SandboxMode:   permissionpresets.SandboxWorkspaceWrite,
	}); err != nil {
		t.Fatalf("append sandbox pin: %v", err)
	}
	if got, ok := permissionpresets.EffectiveSandboxMode(sess.Events()); !ok || got != permissionpresets.SandboxWorkspaceWrite {
		t.Fatalf("child sandbox fold = %q %v, want %q true", got, ok, permissionpresets.SandboxWorkspaceWrite)
	}
	// The real service seam reads the same explicit override from the log
	// (and never invents a deployment default).
	seam := &permissionpresets.Service{}
	if got := seam.OverrideOf(sess); got != permissionpresets.SandboxWorkspaceWrite {
		t.Fatalf("OverrideOf = %q, want %q", got, permissionpresets.SandboxWorkspaceWrite)
	}
	// Without the approval capability nothing is seeded: a no-capability
	// append must leave both pins untouched.
	policyCount := 0
	sandboxCount := 0
	for i := range sess.Events() {
		switch sess.Events()[i].Type {
		case userapproval.EventApprovalPolicy:
			policyCount++
		case permissionpresets.EventSandboxMode:
			sandboxCount++
		}
	}
	if err := AppendDelegatedPolicyOverrides(sess, CaptureDelegatedPolicyOverrides(nil, false, parent)); err != nil {
		t.Fatalf("append none: %v", err)
	}
	afterPolicy := 0
	afterSandbox := 0
	for i := range sess.Events() {
		switch sess.Events()[i].Type {
		case userapproval.EventApprovalPolicy:
			afterPolicy++
		case permissionpresets.EventSandboxMode:
			afterSandbox++
		}
	}
	if afterPolicy != policyCount || afterSandbox != sandboxCount {
		t.Fatalf("no-capability append changed pins: policy %d→%d, sandbox %d→%d",
			policyCount, afterPolicy, sandboxCount, afterSandbox)
	}
}
