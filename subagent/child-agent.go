package subagent

import (
	"fmt"

	"dshgo/agent"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// SubagentDepthError is thrown when starting a child would exceed the
// requested depth cap.
type SubagentDepthError struct {
	AttemptedDepth int64
	MaxDepth       int64
}

func (e *SubagentDepthError) Error() string {
	return fmt.Sprintf("subagent depth %d exceeds maxDepth %d", e.AttemptedDepth, e.MaxDepth)
}

// ResolveChildDepth resolves the child's delegation depth from its parent and
// enforces an optional cap. The persisted parent header is the monotone
// floor, so a resumed parent cannot delegate as if it were top-level.
func ResolveChildDepth(parent *agent.Agent, maxDepth *int64) (int64, error) {
	parentDepth, err := DelegationDepthOf(parent)
	if err != nil {
		return 0, err
	}
	childDepth := parentDepth + 1
	if !isSafeInteger(childDepth) {
		return 0, fmt.Errorf("subagent child depth exceeds the safe-integer range")
	}
	if maxDepth != nil && childDepth > *maxDepth {
		return 0, &SubagentDepthError{AttemptedDepth: childDepth, MaxDepth: *maxDepth}
	}
	return childDepth, nil
}

// parentAgentOptionsForDelegation resolves the parent values inherited by a
// child. The latest request header owns provider, model, and reasoning effort
// after request-time selection; creation options remain the fallback before
// the first request and retain the configured output-token limit.
func parentAgentOptionsForDelegation(parent *agent.Agent) agent.AgentOptions {
	requestConfig := session.FoldRequestHeader(parent.Session.Events(), nil)
	if requestConfig == nil {
		return parent.Options
	}
	resolved := parent.Options
	resolved.Provider = requestConfig.Config.Provider
	resolved.Model = requestConfig.Config.Model
	if requestConfig.Config.ReasoningEffort != "" {
		resolved.ReasoningEffort = requestConfig.Config.ReasoningEffort
	} else {
		resolved.ReasoningEffort = ""
	}
	return resolved
}

// resolveChildAgentOptions resolves the child's AgentOptions: the parent's
// provider/model, reasoning-effort, and maxTokens values unless the request
// overrides them, stamped with the child's own delegation depth. Changing the
// route without naming an effort clears the parent's route-owned effort so
// the selected model resolves its own default.
func resolveChildAgentOptions(parent *agent.Agent, requested *agent.AgentOptions, childDepth int64) agent.AgentOptions {
	parentOptions := parentAgentOptionsForDelegation(parent)
	resolved := agent.AgentOptions{
		Provider:        parentOptions.Provider,
		Model:           parentOptions.Model,
		ReasoningEffort: parentOptions.ReasoningEffort,
		MaxTokens:       parentOptions.MaxTokens,
		SubagentDepth:   &childDepth,
	}
	if requested != nil {
		if requested.Provider != "" {
			resolved.Provider = requested.Provider
		}
		if requested.Model != "" {
			resolved.Model = requested.Model
		}
		if requested.ReasoningEffort != "" {
			resolved.ReasoningEffort = requested.ReasoningEffort
		}
		if requested.MaxTokens != nil {
			resolved.MaxTokens = requested.MaxTokens
		}
	}
	routeChanged := resolved.Provider != parentOptions.Provider || resolved.Model != parentOptions.Model
	if routeChanged && (requested == nil || requested.ReasoningEffort == "") {
		resolved.ReasoningEffort = ""
	}
	return resolved
}

// ChildSessionMeta builds the child session's durable creation metadata: the
// parent's workspace, its direct lineage, coarse product origin, the
// recursion budget that must survive persistence, the seed boundary that
// separates inherited parent history from child work, and the preset the
// child runs under.
//
// The preset is read from the parent's LIVE scope rather than from its
// header, because a parent that switched preset while blank runs on the newer
// composition and its header still names the older one. Recording it is what
// makes a child's history reconstructable. Go adaptation: the agent-presets
// service lookup is an explicit seam (nil-safe), matching the official
// opportunistic `ctx.get`.
func ChildSessionMeta(presetService any, parent *agent.Agent, childDepth int64, lineageSeedLength int64) agent.CreateAgentMeta {
	parentHeader := parent.Session.Header()
	meta := agent.CreateAgentMeta{
		CWD:             parentHeader.CWD,
		ParentSession:   parentHeader.ID,
		Origin:          "subagent",
		DelegationDepth: &childDepth,
	}
	if lineageSeedLength > 0 {
		meta.IsSeeded = true
		meta.InheritedEventCount = session.SessionLogOffset(lineageSeedLength)
	}
	if presets, ok := presetService.(AgentPresetService); ok && presets != nil {
		if preset := presets.ComposedPreset(); preset != "" {
			meta.AgentPreset = preset
		}
	}
	return meta
}

// AgentPresetService is the opportunistic agent-presets seam children join
// through (the documented ctx.get pattern: absent when not composed).
type AgentPresetService interface {
	// ComposedPreset returns the composition in force at the asking scope.
	ComposedPreset() string
	// ComposeFrom joins the parent's composition into the child scope.
	ComposeFrom(child scope.ScopeKey, parent *agent.Agent)
}

// ChildComposition is the scoped composition a child agent's creation window
// applies.
type ChildComposition struct {
	// Persona is the per-child persona shadowing the deployment persona.
	Persona string
	// ToolFilter is the per-child tool scoping.
	ToolFilter *ToolRestriction
}

// SubagentDelegationContext is the model-facing delegation-scope statement
// for every in-process child. A runtime-context contribution rather than a
// system-prompt section, so the deployment's system prompt stays uniform
// across parents and children.
const SubagentDelegationContext = "You are a delegated subagent: your permission scope was fixed when you were started and cannot be " +
	"widened from inside this session — operations that require approval are rejected automatically. " +
	"When the task needs access beyond that scope, do not retry the denied operation; state the " +
	"limitation in your reply so the delegating agent can handle it."

// delegationContextOrder places the delegation sentence after the
// sandbox:policy (110) and approval:policy (115) contexts.
const delegationContextOrder = 120.0

// ChildCompositionDeps are the host-plane seams applyChildComposition
// registers through. Go adaptation: the child's scoped world has no ambient
// service table, so the composition hands over exactly what the child needs;
// nil seams skip their step (a rosterless deployment keeps its model-facing
// rows on the host plane).
type ChildCompositionDeps struct {
	// Prompt is the deployment system prompt; nil skips the delegation
	// context and persona section.
	Prompt *systemprompt.SystemPrompt
	// Registry is the tool registry; nil skips the tool restriction.
	Registry *tools.ToolRuntime
	// Presets is the optional agent-presets roster; nil skips the join.
	Presets AgentPresetService
}

// ApplyChildComposition composes one child inside its creation window: join
// its parent's preset, register the fixed delegation-scope statement, then
// apply the child's own shadowing persona section and tool restriction, all
// owned by the child's scope and therefore invisible to its parent and
// siblings. Creation and cold resume both pass through here.
//
// The join comes first and the child's own registrations second, which is the
// order the layering already implies — the nearest scope wins a name, and a
// per-child restriction intersects with everything its chain admits — but
// stating it here keeps the two steps from being read as independent.
//
// The join and the per-child registrations live in ONE call because a child
// composed without the join is exactly the defect this function exists to
// prevent: with every model-facing row on the agent plane, a child that joins
// no preset sees an empty tool registry and none of its parent's prompt
// sections. Taking the parent as a parameter is what makes that omission
// unrepresentable at the call sites.
func ApplyChildComposition(childScope scope.ScopeKey, parent *agent.Agent, composition ChildComposition, deps ChildCompositionDeps) {
	if deps.Presets != nil {
		deps.Presets.ComposeFrom(childScope, parent)
	}
	if deps.Prompt != nil {
		_, _ = deps.Prompt.Context(childScope, systemprompt.PromptContext{
			Name:  "subagent:delegation",
			Order: delegationContextOrder,
			Text:  SubagentDelegationContext,
		})
		if composition.Persona != "" {
			_, _ = deps.Prompt.Section(childScope, systemprompt.PromptSection{
				Name:  systemprompt.PERSONA_SECTION,
				Order: systemprompt.OrderDeploymentPersona,
				Text:  composition.Persona,
			})
		}
	}
	if composition.ToolFilter != nil && deps.Registry != nil {
		_, _ = deps.Registry.RestrictIn(childScope, composition.ToolFilter.Allow, composition.ToolFilter.Deny)
	}
}

// DelegatedPolicyOverrides is the policy seeded onto a child session's log at
// the delegation boundary.
type DelegatedPolicyOverrides struct {
	// SandboxMode is the parent session's explicit sandbox-mode override;
	// empty without one.
	SandboxMode string
	// ApprovalNever is the approval pin: a delegated child acts only within
	// the sandbox scope fixed at delegation, so its asks are rejected
	// deterministically. Set whenever the approval capability is composed.
	ApprovalNever bool
}

// SandboxOverrideService is the opportunistic sandbox-policy seam: the
// parent's explicit session override only — never deployment defaults or
// one-shot grants.
type SandboxOverrideService interface {
	// OverrideOf returns the session's explicit sandbox-mode override, or
	// empty without one.
	OverrideOf(s *session.Session) string
}

// CaptureDelegatedPolicyOverrides captures the policy to seed into one
// delegation. Call synchronously before the child start's first await: a
// later parent switch belongs to the parent's future, not to this child. The
// approval policy is pinned to `never` regardless of the parent's own policy.
// Go adaptation: both services are explicit seams (nil-safe) instead of
// ambient cordis lookups.
func CaptureDelegatedPolicyOverrides(sandbox SandboxOverrideService, hasApproval bool, parent *agent.Agent) DelegatedPolicyOverrides {
	captured := DelegatedPolicyOverrides{}
	if sandbox != nil {
		captured.SandboxMode = sandbox.OverrideOf(parent.Session)
	}
	captured.ApprovalNever = hasApproval
	return captured
}

// AppendDelegatedPolicyOverrides appends the captured delegation policy onto
// the child's own log as `source: "delegation"` events inside the unpublished
// creation window, so the child's effective policy is reconstructable from its
// log alone. Appends land after any fork seed, so fresh policy wins stale
// seed state; later child switches still win over these events.
//
// Both pins append through typed vocabulary (userapproval policy,
// permissionpresets sandbox/mode), so the child log never carries a type
// this build cannot fold back.
func AppendDelegatedPolicyOverrides(childSession *session.Session, overrides DelegatedPolicyOverrides) error {
	if overrides.ApprovalNever {
		if _, err := childSession.Append(userapproval.EventApprovalPolicy, userapproval.PolicyData{
			Policy: "never",
			Source: "delegation",
		}, nil); err != nil {
			return err
		}
	}
	if overrides.SandboxMode != "" {
		if err := permissionpresets.SetSandboxMode(childSession, overrides.SandboxMode); err != nil {
			return err
		}
	}
	return nil
}
