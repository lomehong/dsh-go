package subagent

import (
	"context"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// SubagentRunID identifies one accepted subagent run across its lifecycle
// event pair. A string keeps the wire identity; only the service mints one.
type SubagentRunID = string

// SubagentRunInfo is the observe-only identifying detail carried by
// `subagent/start`. One-shot runs and continuable Activation epochs share
// this payload, so an observer sees the same vocabulary for both.
type SubagentRunInfo struct {
	// RunID is the unique identity shared with the paired terminal event.
	RunID SubagentRunID `json:"runId"`
	// Provider is the provider name recorded when the child was first
	// created. It may be empty when an accepted one-shot run becomes ready or
	// a persisted Activation cold-resumes, because neither lifecycle depends
	// on continued registration.
	Provider string `json:"provider"`
	// ID is the child agent's id.
	ID session.SessionID `json:"id"`
	// Local snapshots whether SubagentRun.LocalAgent was present when start
	// fulfilled.
	Local bool `json:"local"`
}

// SubagentRunEndInfo is the observe-only outcome detail carried by
// `subagent/end`, paired with one SubagentRunInfo by RunID.
type SubagentRunEndInfo struct {
	SubagentRunInfo
	// StopReason is the terminal stop reason.
	StopReason StopReason `json:"stopReason"`
	// LastAssistantMessage is the child's final assistant output, selected by
	// the same rule as SubagentResult.Output; nil on infrastructure rejection
	// or when the child produced none.
	LastAssistantMessage []llm.ContentBlock `json:"lastAssistantMessage,omitempty"`
}

// SubagentCapabilities describes which START-TIME features a provider
// supports. The service checks it before delegating to Start: a request that
// needs a capability the chosen provider lacks is rejected with a typed
// error rather than accepted-then-ignored (fail loud, no silent
// degradation). The flags describe the ONE-SHOT Start path; continuable
// children are gated by the ContinuableProvider interface instead. Each flag
// corresponds one-to-one to a SubagentStartRequest option.
type SubagentCapabilities struct {
	AgentOptions bool
	OutputSchema bool
	DepthLimit   bool
	ToolFilter   bool
	Persona      bool
}

// SubagentStartRequest is what a caller asks for when starting a ONE-SHOT
// subagent. The tool layer builds it from the model's
// `{ description, prompt }` plus its own config; the service validates the
// capabilities against the named provider and resolves the durable
// descriptor before dispatching to Start.
type SubagentStartRequest struct {
	// Label is the optional short display label persisted with a
	// session-backed child.
	Label string
	// Prompt is the content delivered as the child's user message.
	Prompt []llm.ContentBlock
	// Parent is the spawning agent. In-process providers derive workspace,
	// lineage, and delegation depth from its durable session state; a remote
	// transport reads only its cwd, and only when no deployment cwd override
	// is configured.
	Parent *agent.Agent
	// Signal is the cancellation from the spawning context (the tool's
	// exec signal). It is the canonical cancellation channel both before and
	// after startup.
	Signal context.Context
	// AgentOptions carries host provider/model/reasoning-effort/output-token
	// overrides; requires the AgentOptions capability.
	AgentOptions *agent.AgentOptions
	// OutputSchema carries an object-rooted JSON Schema; requires the
	// OutputSchema capability. Data must be plain host-realm JSON; a
	// successful child returns the matching value as SubagentResult.Structured.
	OutputSchema map[string]any
	// MaxDepth is the optional absolute delegation-depth cap for the child
	// being started: its computed depth must be less than or equal to this
	// non-negative safe integer; requires the DepthLimit capability.
	MaxDepth *int64
	// ToolFilter scopes the child's tools; requires the ToolFilter
	// capability. In-process backends apply it as a child-scoped
	// restriction: the named tools vanish from the child's prompt AND refuse
	// to execute (one visibility), with loud unknown-name validation.
	ToolFilter *ToolRestriction
	// Persona is the optional per-child persona; requires the Persona
	// capability. In-process backends register it as a child-scoped
	// `deployment:persona` section shadowing the deployment persona for this
	// child alone.
	Persona string
}

// ResolvedSubagentStartRequest is the provider-facing one-shot request after
// the runtime resolves the durable child descriptor.
type ResolvedSubagentStartRequest struct {
	SubagentStartRequest
	// Descriptor is the detached descriptor a session-backed provider
	// persists in the child log.
	Descriptor SubagentDescriptorData
}

// ContinuableCreateRequest is what the continuation manager asks a provider
// for while materializing one continuable child's FIRST activation. The
// manager has already reserved the durable child identity and owns every
// later operation, so the request carries only what distinguishes a fresh
// child from one seeded with parent history.
type ContinuableCreateRequest struct {
	// SessionID is the reserved durable child session id, for provider
	// diagnostics.
	SessionID session.SessionID
	// Parent is the delegating parent agent whose history a seeding provider
	// reads.
	Parent *agent.Agent
	// Signal is the caller cancellation, which owns preparation only until
	// the manager accepts the initial prompt into the child's inbox.
	Signal context.Context
}

// ContinuableCreateSpec is a provider's detached contribution to one
// continuable child's creation. This is DATA, never a capability: it carries
// no Agent, handle, prompt delivery, result, disposal, or resume operation,
// because the continuation manager owns the child's whole lifecycle after
// preparation.
type ContinuableCreateSpec struct {
	// Seed is the completed-turn prefix of the parent's log to seed the
	// child session with, or nil for a fresh child. Same durable contract as
	// CreateAgentOptions.Seed: contiguous from seq 0, lossless JSON,
	// balanced.
	Seed []session.Event
}

// StopReason is why a subagent run ended. Merge-extensible in the source (a
// backend may add variants); consumers branch on the known cases and fall
// through the empty-value default. The known cases mirror the harness
// turn-end vocabulary so the tool layer can map a non-completed result to an
// isError tool result.
type StopReason string

// Known stop reasons.
const (
	// StopCompleted: the child finished its turn normally.
	StopCompleted StopReason = "completed"
	// StopAborted: cancelled through the request signal or disposal.
	StopAborted StopReason = "aborted"
	// StopError: model or transport failure.
	StopError StopReason = "error"
	// StopMaxTokens: the child hit its token ceiling before finishing.
	StopMaxTokens StopReason = "max-tokens"
	// StopRefusal: the child declined the task.
	StopRefusal StopReason = "refusal"
)

// SubagentResult is the terminal outcome of a subagent run.
type SubagentResult struct {
	// Output is the child's final assistant output: the content of its last
	// non-empty assistant message. Empty-content messages, including
	// usage-only messages, are skipped. Without a non-empty message, the
	// output is its accumulated assistant text stream, or empty when the
	// child produced neither.
	Output []llm.ContentBlock `json:"output"`
	// Structured is the result after a requested OutputSchema was
	// successfully satisfied. Requesting a schema does not guarantee
	// presence: a provider can end with StopError when the child fails or
	// finishes without a valid capture.
	Structured any `json:"structured,omitempty"`
	// Diagnostic is provider-authored, non-assistant failure detail for a
	// non-completed result. Providers keep it free of tool inputs, file
	// contents, environment values, credentials, and raw protocol payloads,
	// and limit it to 4096 UTF-8 bytes. Consumers present it separately from
	// Output.
	Diagnostic string `json:"diagnostic,omitempty"`
	// StopReason says why the run ended. A non-completed reason means Output
	// may be partial.
	StopReason StopReason `json:"stopReason"`
}

// SubagentRun is the ONE-SHOT child handle returned after publication.
// Prompt submission, turn work, and infrastructure faults after that
// boundary belong to Result. Consumers wait on Result and must always
// Dispose to cancel remaining work and reach quiescence. A run is one
// disposable foreground delegation with one result; continuable
// conversations have no run — the continuation manager holds their
// AgentHandle directly and orders every turn through the child's own inbox.
type SubagentRun interface {
	// ID is the parent-scoped run id. For a local run it MUST equal the
	// published child session id, whose parentSession records the parent's
	// session id; a remote provider mints an id unique in the parent
	// namespace.
	ID() session.SessionID
	// LocalAgent is the exact published in-process child, or nil for a
	// remote run. When present, its id equals ID; the provider retains no
	// ownership implication beyond the ordinary Dispose contract.
	LocalAgent() *agent.Agent
	// Result resolves with the child's terminal SubagentResult when the run
	// settles. A child-level failure is a VALUE (StopError), not an error;
	// the error return is reserved for an infrastructure fault the seam
	// cannot represent as a stop reason.
	Result() (SubagentResult, error)
	// Dispose cancels remaining work, reaches child quiescence, and releases
	// resources. Idempotent.
	Dispose() error
}

// SubagentProvider is one registered transport for running child agents.
// Providers are trusted same-process implementations; callers treat
// descriptors and returned values as borrowed immutable data. The service
// may call one provider concurrently for distinct children. Providers
// isolate operation-local mutable state; a shared capacity controller may
// delay an operation but must not couple its settlement or cleanup to a
// sibling.
type SubagentProvider interface {
	// Name is the unique registry name (e.g. `spawn`, `fork`, `acp`).
	Name() string
	// Capabilities are the start-time features this provider supports.
	Capabilities() SubagentCapabilities
	// InheritsParentContext reports whether the child sees the parent's
	// completed-turn prefix. This is descriptive, not a service-validated
	// start capability: the model-facing tool derives truthful wording from
	// it. It says nothing about tool registration, injected services, or
	// authority inheritance.
	InheritsParentContext() bool
	// Start establishes a ONE-SHOT child and returns its handle after
	// publication. The service has already validated that every requested
	// start-time capability is supported and resolved request.Descriptor, so
	// a session-backed implementation appends that descriptor inside the
	// child's initial turn. Before fulfillment, the provider owns setup and
	// cleans any unpublished partial resources before failing. Ownership
	// transfers on fulfillment; subsequent turn or infrastructure failure
	// settles through the returned run. Distinct starts may overlap;
	// cancellation, failure, result settlement, and disposal remain
	// independent for each run.
	Start(request ResolvedSubagentStartRequest) (SubagentRun, error)
}

// ContinuableProvider is the OPTIONAL continuable-creation capability:
// method presence IS the capability. The service rejects continuable starts
// on providers that do not implement it, while a provider that does may
// still serve ordinary one-shot delegations. PrepareContinuable is the
// provider's ONLY participation in a continuable child: the continuation
// manager owns identity reservation, composition, Agent creation, prompt
// delivery, cold resume, ownership, and disposal, so a provider never sees
// the child's Agent, handle, turns, or teardown.
type ContinuableProvider interface {
	// PrepareContinuable contributes the detached creation inputs that
	// distinguish this provider's continuable children — only whether the
	// child session is seeded with parent history. Distinct preparations may
	// overlap; each follows its own signal and returns data belonging only
	// to request.SessionID.
	PrepareContinuable(request ContinuableCreateRequest) (ContinuableCreateSpec, error)
}
