package subagent

import (
	"context"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// Durable message-source kinds the continuation seam attributes.
const (
	// SourceCoordinatorRelay: a message another agent addressed to this one
	// (relay context form); SenderSessionID is the agent whose tool call
	// produced the follow-up.
	SourceCoordinatorRelay = "coordinator"
	// SourceSubagentReport: durable attribution for a continuable child's
	// explicit parent report.
	SourceSubagentReport = "subagent-report"
	// SourceSubagentSettled: the runtime's own account of a continuable child
	// settling — deliberately a different kind from a report, because a
	// report is content the child chose while this message is the manager
	// stating what became of the child; a merged transcript would credit the
	// child with words it never wrote.
	SourceSubagentSettled = "subagent-settled"
)

// SubagentReportDelivery is the deployment scheduling policy for accepted
// child reports.
type SubagentReportDelivery string

// Report delivery policies.
const (
	// DeliveryQuiet: hold for the parent's next natural turn.
	DeliveryQuiet SubagentReportDelivery = "quiet"
	// DeliveryNextStep: wake the parent with the report at its next step.
	DeliveryNextStep SubagentReportDelivery = "next-step"
)

// SubagentReportOptions are the options for one continuable child's report
// to its direct parent.
type SubagentReportOptions struct {
	// Delivery is the already-resolved parent scheduling policy.
	Delivery SubagentReportDelivery
	// Signal owns authorization and admission until acceptance.
	Signal context.Context
}

// ContinuableStartSpec is what a caller asks for when starting a continuable
// background child. Go adaptation: the official
// `Omit<SubagentStartRequest, 'label' | 'signal' | 'outputSchema'>` becomes
// the explicit subset struct.
type ContinuableStartSpec struct {
	// Provider is the ctx.subagents provider whose continuable-creation
	// capability establishes the child.
	Provider string
	// Label is the initial delegation's short description, persisted as the
	// child's creation label.
	Label string
	// ChildID is the optional caller-reserved child identity. Empty
	// preserves the manager's UUID allocation; supplying one lets a durable
	// parent record provisioning before child materialization without a
	// second identity handshake.
	ChildID session.SessionID
	// Request is the delegation request. The manager reserves the stable
	// child id, resolves the durable descriptor, and composes the child
	// itself.
	Request ContinuableDelegationRequest
	// Signal owns the operation only until inbox acceptance.
	Signal context.Context
}

// ContinuableDelegationRequest is the continuable subset of a start request:
// everything except label, signal, and outputSchema.
type ContinuableDelegationRequest struct {
	// Prompt is the content delivered as the child's initial user message.
	Prompt []llm.ContentBlock
	// Parent is the spawning agent.
	Parent *agent.Agent
	// AgentOptions carries host overrides; the manager's own capability gate
	// requires a continuable composition able to honor them.
	AgentOptions *agent.AgentOptions
	// MaxDepth is the optional absolute delegation-depth cap.
	MaxDepth *int64
	// ToolFilter scopes the child's tools.
	ToolFilter *ToolRestriction
	// Persona is the optional per-child persona.
	Persona string
}

// ContinuableStart holds the identities returned once a continuable child
// accepted its initial prompt.
type ContinuableStart struct {
	// ChildID is the durable child session id, stable across activations.
	ChildID session.SessionID
	// MessageID is the accepted initial prompt's inbox message id.
	MessageID llm.MessageID
}

// SubagentInterruptAuthorityKind discriminates the interrupt authority sum.
type SubagentInterruptAuthorityKind string

// Interrupt authority kinds.
const (
	// InterruptAuthorityUser carries the durable direct-parent address a
	// human client presented.
	InterruptAuthorityUser SubagentInterruptAuthorityKind = "user"
	// InterruptAuthorityAncestor carries the exact live Agent object whose
	// recorded lineage must contain the caller.
	InterruptAuthorityAncestor SubagentInterruptAuthorityKind = "ancestor"
)

// SubagentInterruptAuthority is the authority under which one interrupt
// request is admitted. Go adaptation: the tagged union becomes a struct with
// a kind discriminant; exactly one member is meaningful per kind.
type SubagentInterruptAuthority struct {
	Kind SubagentInterruptAuthorityKind
	// ParentSessionID is the claimed durable direct parent (kind `user`).
	ParentSessionID session.SessionID
	// Agent is the exact live ancestor agent (kind `ancestor`).
	Agent *agent.Agent
}

// SubagentFollowupOptions are the options for following up with one
// continuable child.
type SubagentFollowupOptions struct {
	// Source is the durable attribution retained on the delivered message;
	// it grants no authority.
	Source llm.MessageSource
	// Signal owns the operation only until inbox acceptance.
	Signal context.Context
}
