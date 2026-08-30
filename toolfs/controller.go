package toolfs

import (
	"context"
	"fmt"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/sandbox"
	"dshgo/tools"
)

// RegisterDeps carries the tool suite's composition inputs.
type RegisterDeps struct {
	// Backend is the mounted filesystem.
	Backend fs.FileSystem
	// Ctx is the plugin context: the fs decision waterfalls and the
	// observation records ride it.
	Ctx *cordis.Context
	// Policy is the sandbox policy service; required only when the backend
	// confines.
	Policy PolicyService
	// ApproverSource supplies the composed approval channel for escalation.
	ApproverSource EscalationApproverSource
	// Agents resolves the calling agent from one execution's scope.
	Agents AgentSource
	// PermissionFolds derives the calling agent's sandbox-mode override (the
	// permissionpresets knob fold); nil skips the override tier.
	PermissionFolds func(caller *agent.Agent) string
	// Attachments is the durable attachment store; read_image registers
	// only while one is mounted (the source's own gate).
	Attachments AttachmentStoreFace
	// Llm resolves the calling route's declared input modalities; nil
	// refuses reads with the route-unresolved wording.
	Llm LlmRouteSource
}

// PolicyService is the sandbox policy face the controller resolves standing
// policies through (the composed *sandboxpolicy.Service).
type PolicyService interface {
	Resolve(sessionCwd string, sessionOverride string, approvedMode string) fs.SandboxExecutionPolicy
}

// EscalationApproverSource supplies the composed approval channel, or nil.
type EscalationApproverSource interface {
	// EscalationApprover returns nil when no approval service is composed.
	EscalationApprover() sandbox.EscalationApprover
}

// controller is the sandbox-escalation API shared by the write and edit
// tools: the per-call policy resolution, the advertised escalation fields,
// and the denial-marker mapping — all keyed off whether the mounted backend
// confines. Built once per plugin and shared by both mutating tools.
type controller struct {
	backend  fs.FileSystem
	ctx      *cordis.Context
	confines bool
	policy   PolicyService
	approver EscalationApproverSource
	agents   AgentSource
	// permissionFold derives the calling agent's sandbox-mode override (the
	// permissionpresets knob fold); nil skips the override tier.
	permissionFold func(caller *agent.Agent) string
}

func newController(backend fs.FileSystem, ctx *cordis.Context, policy PolicyService, approver EscalationApproverSource, agents AgentSource, permissionFold func(caller *agent.Agent) string) (*controller, error) {
	c := &controller{backend: backend, ctx: ctx, policy: policy, approver: approver, agents: agents, permissionFold: permissionFold}
	if backend.SandboxMode() != "" {
		if policy == nil {
			return nil, fmt.Errorf("tool-fs: the mounted filesystem confines but the sandbox policy service is missing")
		}
		c.confines = true
	}
	return c, nil
}

// recordObservation rides the fs/observed cordis event; listeners are
// contractually synchronous, side-effect-only recorders.
func (c *controller) recordObservation(target fs.Target, observation fs.Observation, actor any) {
	c.ctx.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: observation, Actor: actor})
}

// waterfall runs one single-slot decision event; a nil result applies the
// caller's bare default.
func (c *controller) waterfall(event string, payload any) any {
	return c.ctx.Waterfall(event, payload)
}

// schemaFields appends the two escalation parameter specs when the mounted
// backend confines; otherwise the fields are absent so the validator rejects
// them before execute.
func (c *controller) schemaFields() map[string]tools.PropSpec {
	if !c.confines {
		return nil
	}
	enum := make([]any, 0)
	for _, mode := range sandbox.EscalationTargets() {
		enum = append(enum, mode)
	}
	return map[string]tools.PropSpec{
		"sandbox_permissions": {
			ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string",
				Enum: enum,
				Description: "The wider sandbox mode this file operation needs. Only valid as a one-shot retry " +
					"of an operation the sandbox just denied; requires justification and user approval.",
			},
		},
		"justification": {
			ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string",
				Description: "Required with sandbox_permissions: one sentence for the user explaining " +
					"why this exact file operation needs the wider access.",
			},
		},
	}
}

// escalationArgs extracts the pair; an absent or null value counts as absent
// (JSON null is not a string), which is what the pairing validation judges.
func escalationArgs(args map[string]any) (*string, *string) {
	var perms, justification *string
	if raw, ok := args["sandbox_permissions"].(string); ok {
		perms = &raw
	}
	if raw, ok := args["justification"].(string); ok {
		justification = &raw
	}
	return perms, justification
}

// standingPolicy resolves the session's standing mode: the session's last
// sandbox/mode override over the deployment default, with the session cwd —
// or the policy fallback — as the workspace boundary.
func (c *controller) standingPolicy(scope any) fs.SandboxExecutionPolicy {
	var cwd, override string
	caller := c.agents.ResolveAgent(scope)
	if caller != nil && caller.Session != nil {
		cwd = caller.Session.Header().CWD
		if c.permissionFold != nil {
			override = c.permissionFold(caller)
		}
	}
	return c.policy.Resolve(cwd, override, "")
}

// resolvePolicy stamps the policy onto one mutation: an approved escalation
// grant (a strictly wider retry resolved through the approval channel before
// anything executes), else the session's standing mode. Validates the
// escalation argument pairing first.
func (c *controller) resolvePolicy(toolName string, args map[string]any, exec *tools.ToolRunContext) (*fs.SandboxExecutionPolicy, error) {
	perms, justification := escalationArgs(args)
	if err := sandbox.ValidateEscalationArgs(perms, justification); err != nil {
		return nil, err
	}
	if perms == nil || justification == nil {
		if !c.confines {
			return nil, nil
		}
		standing := c.standingPolicy(execScope(exec))
		return &standing, nil
	}
	if !c.confines {
		return nil, fmt.Errorf("sandbox_permissions is not available in this composition (no sandboxing filesystem to escalate)")
	}
	standing := c.standingPolicy(execScope(exec))
	var caller *agent.Agent
	if c.agents != nil {
		caller = c.agents.ResolveAgent(execScope(exec))
	}
	var approver sandbox.EscalationApprover
	if c.approver != nil {
		approver = c.approver.EscalationApprover()
	}
	granted, err := sandbox.ApproveEscalation(sandbox.EscalationRequest{
		RequestedMode: *perms,
		Justification: *justification,
		EffectiveMode: standing.Mode,
		Subject:       "operation",
	}, sandbox.EscalationApproval{
		Approver: approver,
		Agent:    caller,
		CallID:   execCallID(exec),
		ToolName: toolName,
		Signal:   execSignal(exec),
	})
	if err != nil {
		return nil, err
	}
	standing.Mode = granted
	return &standing, nil
}

// mapError rewrites a thrown provider error for the model: a FS_SANDBOX_DENIED
// becomes the shared `[sandbox: …]` denial marker plus the same-turn
// escalation hint, keeping the structured code so retry layers keep routing.
// Anything else passes through unchanged.
func (c *controller) mapError(err error, policy *fs.SandboxExecutionPolicy) error {
	codeErr, ok := err.(*fs.Error)
	if !ok || codeErr.Code != fs.CodeSandboxDenied || policy == nil {
		return err
	}
	marker := sandbox.SandboxDenialMarker(policy.Mode) + "\n" + sandbox.EscalationHintMarker("operation")
	return fs.NewError(fs.CodeSandboxDenied, marker, err)
}

// resolveTarget resolves one model-supplied path against the session cwd (or
// the mutation's policy root).
func (c *controller) resolveTarget(ctx context.Context, requestedPath string, policyRoot string, scope any) (fs.Target, error) {
	cwd := sessionCwd(c.agents, scope, requestedPath)
	if policyRoot != "" {
		cwd = policyRoot
	}
	return c.backend.Resolve(ctx, requestedPath, cwd)
}

func execScope(exec *tools.ToolRunContext) any {
	if exec == nil {
		return nil
	}
	return exec.Agent
}

func execCallID(exec *tools.ToolRunContext) string {
	if exec == nil {
		return ""
	}
	return exec.CallID
}

func execSignal(exec *tools.ToolRunContext) context.Context {
	if exec != nil && exec.Signal != nil {
		return exec.Signal
	}
	return context.Background()
}
