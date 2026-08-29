package sandbox

import (
	"context"
	"fmt"
	"strings"
)

// The escalation vocabulary and choreography shared by every
// sandbox-enforcing tool family (official dsh-sandbox/escalation): the
// strictly-wider ladder, the argument-pairing validation, the model-facing
// denial/hint markers, and ApproveEscalation — the ordered fail-closed
// sequence that resolves a sandbox_permissions request through a user
// approval channel BEFORE anything executes. One home keeps the fs and bash
// families' approval ordering and verbatim error texts from drifting apart.

// WiderModes is the strictly-wider table: what a call whose effective mode is
// the key may escalate TO. Checked at EXECUTION, never baked into a tool
// schema — the schema enum is EscalationTargets, because schemas are
// registry-global while the effective mode is per-call truth.
func WiderModes(mode string) []string {
	switch mode {
	case "read-only":
		return []string{"workspace-write", "danger-full-access"}
	case "workspace-write":
		return []string{"danger-full-access"}
	}
	return nil
}

// EscalationTargets is the closed escalation-target vocabulary — every mode a
// call could ever escalate TO (read-only is the floor; nothing escalates to
// it). Advertised whenever the mounted capability confines: cutting the enum
// down to the modes wider than the composition's DEFAULT would strand a
// session whose effective mode sits below it.
func EscalationTargets() []string {
	return []string{"workspace-write", "danger-full-access"}
}

// ValidateEscalationArgs validates the escalation argument pairing a tool
// schema cannot express: sandbox_permissions and justification travel
// together, and the justification must be a non-empty sentence. Nil means the
// argument is absent.
func ValidateEscalationArgs(sandboxPermissions *string, justification *string) error {
	if sandboxPermissions != nil && justification == nil {
		return fmt.Errorf("invalid escalation: sandbox_permissions requires a justification")
	}
	if justification != nil && sandboxPermissions == nil {
		return fmt.Errorf("invalid escalation: justification is only valid together with sandbox_permissions")
	}
	if justification != nil && strings.TrimSpace(*justification) == "" {
		return fmt.Errorf("invalid justification: expected a non-empty sentence")
	}
	return nil
}

// SandboxDenialMarker is the model-facing denial marker — the one vocabulary
// both enforcing families teach and report, so the model recognizes a policy
// denial identically whether the kernel refused a bash file effect or the
// filesystem provider's fence refused a mutation.
func SandboxDenialMarker(mode string) string {
	return fmt.Sprintf("[sandbox: file access denied under %s mode]", mode)
}

// EscalationHintMarker is the same-turn escalation hint that rides a denial
// when the composition advertises the escalation fields — the nudge lives at
// the decision point so the sanctioned retry does not depend on the model
// recalling the tool description. Subject is the family's noun for the denied
// action (`command` for bash, `operation` for a filesystem mutation).
func EscalationHintMarker(subject string) string {
	return fmt.Sprintf("[sandbox: escalation available — retry this exact %s once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]", subject)
}

// EscalationAsk is the minimal approval-request shape ApproveEscalation
// needs, structurally the approval seam's request: audit-self-contained.
type EscalationAsk struct {
	// Agent routes the ask; nil fails closed (an agent-less execution has no
	// channel to a human).
	Agent any
	// ToolName is the tool the ask is about (presentation and audit).
	ToolName string
	// CallID is the exact tool call the approval prompt attaches to.
	CallID string
	// Reason is the human-readable explanation of WHY the tool asks.
	Reason string
	// Signal aborting withdraws the question.
	Signal context.Context
}

// EscalationApprover is the minimal approval face — structurally the approval
// seam's request method, generic over the agent type so this package resolves
// escalations without importing the approval or agent packages. It returns
// the closed outcome vocabulary ("allowed-once", "rejected", "cancelled",
// "unavailable").
type EscalationApprover interface {
	RequestApproval(req EscalationAsk) (string, error)
}

// EscalationRequest is one escalation request, as ApproveEscalation judges it.
type EscalationRequest struct {
	// RequestedMode is the target mode (schema-pinned to EscalationTargets
	// when advertised).
	RequestedMode string
	// Justification is the model's one-sentence reason, shown verbatim to the
	// user inside the audit reason.
	Justification string
	// EffectiveMode is the call's current mode the request must strictly widen.
	EffectiveMode string
	// Subject is the family's noun for the escalated action in user-facing
	// texts (`command` for bash, `operation` for fs).
	Subject string
}

// EscalationApproval carries the approval ingredients an escalating tool
// holds: the approval requester (nil when none is composed), the calling
// agent (nil for an agent-less execution, which fails closed), and the call's
// identity.
type EscalationApproval struct {
	Approver EscalationApprover
	Agent    any
	CallID   string
	ToolName string
	Signal   context.Context
}

// ApproveEscalation resolves a sandbox-escalation request BEFORE anything
// executes: check strict widening against the call's effective mode, then
// resolve the approval channel, then map every outcome — the ordered
// fail-closed sequence both enforcing families share. It returns the granted
// mode to stamp onto exactly this call, or the distinct verbatim error for
// every other path (a non-widening request, a missing approval service, an
// agent-less execution, a rejection, a cancellation, an unanswerable ask).
// A non-widening request never prompts a human.
func ApproveEscalation(request EscalationRequest, approval EscalationApproval) (string, error) {
	mode := request.RequestedMode
	// Strict widening is an EXECUTION check against the call's effective
	// mode — deliberately not a schema constraint.
	wider := false
	for _, candidate := range WiderModes(request.EffectiveMode) {
		if candidate == mode {
			wider = true
			break
		}
	}
	if !wider {
		return "", fmt.Errorf("sandbox escalation to %q is not strictly wider than this call's current %q mode", mode, request.EffectiveMode)
	}
	if approval.Approver == nil {
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but no approval service is composed", mode)
	}
	if approval.Agent == nil {
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but the call has no agent to route it through", mode)
	}
	// Self-contained for the audit trail: the approval ask stores this
	// reason, and the target mode is part of the grant's identity.
	outcome, err := approval.Approver.RequestApproval(EscalationAsk{
		Agent:    approval.Agent,
		ToolName: approval.ToolName,
		CallID:   approval.CallID,
		Reason:   fmt.Sprintf("escalate sandbox to %s: %s", mode, request.Justification),
		Signal:   approval.Signal,
	})
	if err != nil {
		return "", err
	}
	switch outcome {
	// The schema enum already pinned mode to the closed target vocabulary;
	// the check above proved it is strictly wider.
	case "allowed-once":
		return mode, nil
	case "rejected":
		return "", fmt.Errorf("the user rejected escalating this %s to %q", request.Subject, mode)
	case "cancelled":
		return "", fmt.Errorf("approval for escalating to %q was cancelled", mode)
	case "unavailable":
		return "", fmt.Errorf("sandbox escalation to %q requires approval, but no approval channel is available", mode)
	default:
		return "", fmt.Errorf("unreachable escalation outcome: %q", outcome)
	}
}
