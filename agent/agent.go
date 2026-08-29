// Package agent ports packages/core/agent: the live agent registry, the
// durable inbox projection, consumed-work accounting, agent-scoped event
// dispatch, and model selection.
//
// Go adaptations, each mirroring an ambient-JS mechanism with an explicit
// parameter: the initiator AsyncLocalStorage boundary becomes a
// context.Context value threaded through WithInitiator/WithoutInitiator;
// Agent.ctx becomes the pair of an owned scope key (tools, system-prompt, and
// event-bus scoping) and an owned cordis child context (effect-scoped
// registrations that unwind on disposal); the cordis declaration-merged
// agent-subject events become one SubjectEventBus owned by the registry, whose
// listeners register with a scope key and whose dispatch admits by the same
// ancestor rules as @deepseek-ai/dsh-scope; the typert lookup/context
// registration is deferred until the typert runtime exists.
package agent

import (
	"context"
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/systemprompt"
)

// ScopeKey is an opaque scope; each agent owns one minted under its owner's
// scope (nil parent for a runtime root).
type ScopeKey = scope.ScopeKey

// AgentOptions are merge-extensible agent creation options. Persona belongs
// to system-prompt sections.
type AgentOptions struct {
	// Provider is the provider route (must have a registered adapter at
	// call time).
	Provider string
	// Model is the model id interpreted by the selected provider adapter.
	Model string
	// ReasoningEffort is the adapter-owned reasoning effort for the
	// selected provider/model route.
	ReasoningEffort llm.ReasoningEffortID
	// MaxTokens is the maximum output tokens for each conversation-model
	// request.
	MaxTokens *int64
	// SubagentDepth is the delegation depth: zero for a top-level agent and
	// parent depth + 1 for a child (owned by the subagent capability). The
	// persisted session header stays authoritative and monotone.
	SubagentDepth *int64
}

// CancelOptions are options for Agent cancellation.
type CancelOptions struct {
	// KeepInbox preserves queued and steering inbox items instead of
	// discarding them: the active turn is still aborted, but un-started and
	// pending work survives for a later turn and no canceled inbox splice
	// is logged.
	KeepInbox bool
}

// AgentStatus is the agent lifecycle state emitted on every transition as
// agent/status: idle means no driver is active; running begins when waking
// input starts cancellable pre-step processing and lasts while the driver
// drains, closes, or checkpoints turns. Disposal removes the agent from its
// registry; it is not a third observable status.
type AgentStatus string

const (
	AgentIdle    AgentStatus = "idle"
	AgentRunning AgentStatus = "running"
)

// InboxTarget is one of the two ordered pending-message lists owned by an
// agent.
type InboxTarget string

const (
	InboxNextTurn InboxTarget = "next-turn"
	InboxNextStep InboxTarget = "next-step"
)

// SessionStartSource is why a session lifecycle began; seeded creates are
// startup, while persisted loads are resume.
type SessionStartSource string

const (
	SessionStartStartup SessionStartSource = "startup"
	SessionStartResume  SessionStartSource = "resume"
	SessionStartClear   SessionStartSource = "clear"
	SessionStartCompact SessionStartSource = "compact"
)

// PreStepDecision is whether and with which messages the loop enters a
// proposed step.
type PreStepDecision struct {
	// Kind is "reject" or "enter".
	Kind string
	// Messages replace the messages that enter the step (Kind "enter").
	Messages []llm.Message
	// StartsRequestSeries starts a distinct model-message series before
	// this step's admitted messages.
	StartsRequestSeries bool
}

// PreStepReject rejects the proposed step.
func PreStepReject() PreStepDecision { return PreStepDecision{Kind: "reject"} }

// PreStepEnter admits messages into the proposed step.
func PreStepEnter(messages []llm.Message) PreStepDecision {
	return PreStepDecision{Kind: "enter", Messages: messages}
}

// RequestErrorAction is the action returned by a listener that owns
// model-request recovery; the zero value leaves the failure terminal.
type RequestErrorAction struct {
	// Retry requests one more attempt without delegating to the default.
	Retry bool
}

// Agent-subject event names. The handler's payload always carries the Agent
// subject, and dispatch is scope-filtered by that subject's scope key.
const (
	EventAgentCreated      = "agent/created"
	EventAgentDisposed     = "agent/disposed"
	EventAgentStatus       = "agent/status"
	EventInboxInserted     = "agent/inbox/inserted"
	EventInboxClaimed      = "agent/inbox/claimed"
	EventInboxDiscarded    = "agent/inbox/discarded"
	EventAgentSessionStart = "agent/session-start"
	EventPreStep           = "agent/pre-step"
	EventRequest           = "agent/request"
	EventRequestError      = "agent/request-error"
	EventTurnStopping      = "agent/turn-stopping"
	EventAgentError        = "agent/error"
)

// Payload shapes for the agent-subject events. Waterfall/serial payloads
// carry the turn's control signal when the dispatch belongs to a turn.
type (
	// AgentStatusPayload is the agent/status payload.
	AgentStatusPayload struct {
		Agent  *Agent
		Status AgentStatus
	}
	// AgentMessagePayload is shared by agent/inbox/inserted and
	// agent/inbox/discarded.
	AgentMessagePayload struct {
		Agent   *Agent
		Message llm.Message
	}
	// AgentClaimedPayload is the agent/inbox/claimed payload.
	AgentClaimedPayload struct {
		Agent   *Agent
		Message llm.Message
		Turn    int64
	}
	// AgentSessionStartPayload is the agent/session-start payload.
	AgentSessionStartPayload struct {
		Agent  *Agent
		Source SessionStartSource
	}
	// PreStepPayload is the agent/pre-step waterfall payload.
	PreStepPayload struct {
		Agent    *Agent
		Messages []llm.Message
		Turn     int64
		Step     int64
		Signal   context.Context
	}
	// RequestPayload is the agent/request waterfall payload.
	RequestPayload struct {
		Agent  *Agent
		Turn   int64
		Step   int64
		Signal context.Context
	}
	// RequestErrorPayload is the agent/request-error waterfall payload.
	RequestErrorPayload struct {
		Agent       *Agent
		Turn        int64
		Step        int64
		Provider    string
		Failure     llm.LlmFailure
		RetryPolicy *llm.ResolvedRetryPolicy
		Signal      context.Context
	}
	// TurnStoppingPayload is the agent/turn-stopping serial payload.
	TurnStoppingPayload struct {
		Agent  *Agent
		Turn   int64
		Signal context.Context
	}
	// AgentErrorPayload is the agent/error payload.
	AgentErrorPayload struct {
		Agent *Agent
		Turn  int64
		Step  int64
		Error error
	}
	// AgentLifecyclePayload is shared by agent/created and agent/disposed.
	AgentLifecyclePayload struct {
		Agent *Agent
	}
)

// Driver is the loop-owned runtime behavior of one Agent. The agent package
// defines the data face; the agent-loop package (registered as the creation
// factory) installs the driver. A nil Driver means the agent exists without a
// running loop (registry-level tests).
type Driver interface {
	// Cancel clears queued and steering work — unless options.KeepInbox —
	// and aborts the active turn or between-turn task. The first cause wins
	// for that activity; with no active activity it is a no-op.
	Cancel(cause session.TurnEndCancelCause, options CancelOptions)
	// WhenIdle resolves after the current whole-agent activity reaches
	// quiescence, following replacement work started before the observed
	// driver retires.
	WhenIdle() <-chan struct{}
	// RunMaintenance runs one non-turn maintenance task from the true idle
	// phase; it fails when turn-driving or another maintenance task already
	// owns the agent.
	RunMaintenance(task func(signal context.Context) error) error
	// Send routes identified input to an inbox boundary and optionally
	// wakes the driver.
	Send(message llm.Message, target InboxTarget, wakeup bool)
	// Followup queues an ordinary follow-up turn and wakes the driver.
	Followup(message llm.Message)
	// Steer submits steering for the nearest step boundary.
	Steer(message llm.Message)
	// Inject queues model-facing context for the next pre-step without
	// waking the driver.
	Inject(message llm.Message)
}

// Agent is one live registry entry: the shared agent/session identity, its
// options, its live session, the inbox projection over that session's log,
// and its scoped world. Runtime creation and driving belong to the loop's
// Driver; cancellation input forwarded without one fails loudly.
type Agent struct {
	// ID is the session-backed Agent identity, equal to Session.ID.
	ID session.SessionID
	// Options are the provider route and model this agent's requests use.
	Options AgentOptions
	// Session is the live session this agent drives; its log is the
	// durable source of truth.
	Session *session.Session
	// Inbox is the agent-owned projection of durable pending work.
	Inbox *Inbox
	// Scope is the agent's scope key: agent-scoped tools, prompt
	// contributions, and bus listeners select it.
	Scope ScopeKey
	// Ctx is the agent-scoped context; its contributions are agent-local,
	// unwind on disposal, and reject registration afterward.
	Ctx *cordis.Context

	mu     sync.Mutex
	status AgentStatus
	driver Driver
	events *SubjectEventBus
}

// Status reports the current lifecycle state, mirrored on every agent/status
// transition.
func (a *Agent) Status() AgentStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

func (a *Agent) setStatus(status AgentStatus) AgentStatus {
	a.mu.Lock()
	previous := a.status
	a.status = status
	a.mu.Unlock()
	if previous != status {
		a.Events().Emit(EventAgentStatus, a.Scope, AgentStatusPayload{Agent: a, Status: status})
	}
	return status
}

// SetStatus publishes a driver-owned lifecycle transition. The loop driver
// calls it on every phase swap; registry callers observe the transitions
// through agent/status.
func (a *Agent) SetStatus(status AgentStatus) {
	a.setStatus(status)
}

// Driver returns the installed loop driver, or nil.
func (a *Agent) Driver() Driver {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.driver
}

// SetDriver installs the loop-owned runtime behavior. The agent-loop factory
// calls it once while preparing the agent, before publication.
func (a *Agent) SetDriver(driver Driver) {
	a.setDriver(driver)
}

func (a *Agent) setDriver(driver Driver) {
	a.mu.Lock()
	a.driver = driver
	a.mu.Unlock()
}

// Events returns the fused subject dispatcher built once per agent; the
// scope key and the payload's Agent cannot diverge through it.
func (a *Agent) Events() *SubjectEventBus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.events
}

func (a *Agent) bind(events *SubjectEventBus) {
	a.mu.Lock()
	a.events = events
	a.mu.Unlock()
}

// AgentConfig is the construction input for NewAgent: the durable identity,
// the live session, and the agent's scoped world.
type AgentConfig struct {
	// ID is the shared agent/session identity; it must equal Session.ID
	// (enforced at registration).
	ID session.SessionID
	// Options are the provider route and model this agent's requests use.
	Options AgentOptions
	// Session is the live session this agent drives.
	Session *session.Session
	// Inbox is the agent-owned projection over Session's log.
	Inbox *Inbox
	// Scope is the agent's scope key; nil mints a fresh root scope.
	Scope ScopeKey
	// ParentScope is the scope under which a nil Scope mints one (the
	// owner agent's scope, or nil for a runtime root).
	ParentScope ScopeKey
	// Ctx is the agent-scoped cordis context; nil creates a detached
	// context (registry-level tests).
	Ctx *cordis.Context
}

// NewAgent constructs one agent from its config and binds the subject event
// bus it dispatches through. Publication still runs through the registry
// (Enter + Announce) or the loop factory.
func NewAgent(config AgentConfig, events *SubjectEventBus) *Agent {
	scopeKey := config.Scope
	if scopeKey == nil {
		scopeKey = scope.NewScopeKey(config.ParentScope)
	}
	if config.Ctx == nil {
		config.Ctx = cordis.NewRoot(nil)
	}
	agent := &Agent{
		ID:      config.ID,
		Options: config.Options,
		Session: config.Session,
		Inbox:   config.Inbox,
		Scope:   scopeKey,
		Ctx:     config.Ctx,
		status:  AgentIdle,
	}
	agent.bind(events)
	return agent
}

// Cancel forwards cancellation to the installed driver.
func (a *Agent) Cancel(cause session.TurnEndCancelCause, options CancelOptions) {
	a.mu.Lock()
	driver := a.driver
	a.mu.Unlock()
	if driver == nil {
		panic("agent " + a.ID + " has no driver installed")
	}
	driver.Cancel(cause, options)
}

// AssembleContextFor builds the prompt assembly context with the agent's
// scope set, so agent-scoped prompt and tool contributions cannot be silently
// omitted. Signal is the current turn's explicit control signal, when the
// assembly belongs to a turn.
func (a *Agent) AssembleContextFor(signal context.Context) systemprompt.AssembleContext {
	return systemprompt.AssembleContext{Scope: a.Scope, Signal: signal}
}

// agentEntry is all mutable lifecycle state for one exact registry entry.
type agentEntry struct {
	id              session.SessionID
	agent           *Agent
	owner           *Agent
	announced       bool
	announcing      bool
	detachRequested bool
}

// AgentSetupCommit validates and commits the prepared setup immediately
// before publication.
type AgentSetupCommit struct {
	// Commit rolls publication back when it fails.
	Commit func() error
}

// AgentSetup composes an unpublished Agent scope and optionally returns its
// publication commit. Setup composes, it never drives.
type AgentSetup func(agentCtx *cordis.Context) (AgentSetupCommit, error)

// ContextService is the typed "agent" context service: the factory publishes
// the built agent into its own context so creation-window setup closures can
// reach it (the assertion lives here, not at every consumer).
var ContextService = cordis.DefineService[*Agent]("agent")

// CreateAgentMeta is the session creation metadata a factory caller supplies.
// Durable session data, so the session boundary validates and snapshots it
// before asynchronous setup begins.
type CreateAgentMeta struct {
	CWD             string
	ParentSession   session.SessionID
	SeedLength      *int64
	Origin          string
	DelegationDepth *int64
	AgentPreset     string
}

// CreateAgentOptions are options for creating an agent through the registry
// factory. The caller supplies the single live sessionId shared by the agent
// registry and session log.
type CreateAgentOptions struct {
	// SessionID is the live agent/session identity.
	SessionID session.SessionID
	// Meta is session creation metadata (validated cwd, fork lineage, seed
	// boundary, origin classification, recursion budget).
	Meta CreateAgentMeta
	// Seed is the initial replay/fork history: a balanced completed-turn
	// prefix, contiguous from seq 0, lossless JSON, no open turn/step or
	// dangling tool call.
	Seed []session.Event
	// AgentOptions are per-agent options (model, …).
	AgentOptions AgentOptions
	// Setup is creation-time composition of the agent's scoped world,
	// awaited BEFORE anything is published.
	Setup AgentSetup
}

// ResumeAgentOptions are options for resuming an agent on a persisted
// session.
type ResumeAgentOptions struct {
	// ResumeSessionID is the persisted session id to load and use as the
	// live identity.
	ResumeSessionID session.SessionID
	// AgentOptions are per-agent options (model, …).
	AgentOptions AgentOptions
	// Setup composes the agent's fresh scoped world while the reconstructed
	// session and agent remain unpublished.
	Setup AgentSetup
}

// AgentHandle is an owned agent plus its disposer, returned by create/resume.
// The disposer is a CAPABILITY: among consumers, only the holder can tear
// this agent down. Dispose stops the loop, awaits its exit, unregisters the
// agent, removes its session from the store, and finally unwinds its scoped
// world.
type AgentHandle struct {
	Agent   *Agent
	Dispose func() error
}

// AgentFactory is the agent-creation factory the loop implementation provides
// to the registry via SetFactory. Consumers program against the registry
// without depending on the concrete loop package.
type AgentFactory interface {
	// CreateAgent creates a new agent on a caller-supplied session id,
	// awaits unpublished setup, publishes both records, and starts the loop.
	CreateAgent(owner *cordis.Context, options CreateAgentOptions) (AgentHandle, error)
	// Resume prepares a persisted session and resumes an agent on it.
	Resume(owner *cordis.Context, options ResumeAgentOptions) (AgentHandle, error)
}

// Sentinels matching the source's message constants.
var (
	// ErrNoFactory is thrown when create/resume is called before an agent
	// factory is registered.
	ErrNoFactory = fmt.Errorf("no agent factory registered (load an agent-loop plugin)")
	// ErrNoInitiator is thrown when no initiator boundary is active.
	ErrNoInitiator = fmt.Errorf("no initiating agent is active")
	// ErrInitiatorDisposed is thrown when the initiator scope is closing or
	// disposed.
	ErrInitiatorDisposed = fmt.Errorf("agent initiator scope is disposed")
	// ErrFactoryExists is thrown when a second factory registers.
	ErrFactoryExists = fmt.Errorf("an agent factory is already registered")
)

type factorySlot struct {
	target AgentFactory
}

// AgentRegistry is the agents service: it tracks live agents and carries the
// initiating Agent through one process-local asynchronous driver chain.
// Agent creation is provided by whichever plugin implements AgentFactory,
// registered via SetFactory.
type AgentRegistry struct {
	ctx    *cordis.Context
	logger cordis.Logger
	events *SubjectEventBus

	mu      sync.Mutex
	store   map[session.SessionID]*agentEntry
	order   []session.SessionID // registration order for List/Roots
	factory *factorySlot

	initiatorMu          sync.Mutex
	initiatorState       int // 0 active, 1 closing, 2 disposed
	activeInitiatorRuns  int
	initiatorDrain       chan struct{}
	initiatorDrainClosed bool
}

// NewAgentRegistry builds the registry. The logger receives contained
// listener-failure warnings; nil becomes the discard logger. The returned
// value is expected to be provided as the "agents" service on ctx.
func NewAgentRegistry(ctx *cordis.Context, logger cordis.Logger) *AgentRegistry {
	if logger == nil {
		logger = cordis.Discard{}
	}
	registry := &AgentRegistry{
		ctx:    ctx,
		logger: logger,
		store:  map[session.SessionID]*agentEntry{},
	}
	registry.events = newSubjectEventBus(logger)
	return registry
}

// Events exposes the registry-scoped subject bus: lifecycle events
// (agent/created, agent/disposed) and every agent-subject event dispatch
// through it, scope-filtered by the agent subject.
func (r *AgentRegistry) Events() *SubjectEventBus { return r.events }

// SetFactory registers the agent-creation factory (the loop calls this on
// construction, effect-scoped). Throws if a factory is already registered.
// The returned disposer clears the factory slot; yielding it into the
// caller's own effect nests the teardown in order.
func (r *AgentRegistry) SetFactory(factory AgentFactory) (cordis.Disposer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factory != nil {
		return nil, ErrFactoryExists
	}
	r.factory = &factorySlot{target: factory}
	return func() {
		r.mu.Lock()
		r.factory = nil
		r.mu.Unlock()
	}, nil
}

func (r *AgentRegistry) requireFactory() (AgentFactory, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factory == nil {
		return nil, ErrNoFactory
	}
	return r.factory.target, nil
}

// Create creates and publishes a new agent through the registered factory.
// Distinct from Register (which records an already-constructed agent): this
// constructs the agent and its session.
func (r *AgentRegistry) Create(ctx context.Context, options CreateAgentOptions) (AgentHandle, error) {
	factory, err := r.requireFactory()
	if err != nil {
		return AgentHandle{}, err
	}
	return factory.CreateAgent(r.ctx, options)
}

// Resume loads a persisted session and resumes an agent on it through the
// registered factory. Must be called after the persistence service exists.
func (r *AgentRegistry) Resume(ctx context.Context, options ResumeAgentOptions) (AgentHandle, error) {
	factory, err := r.requireFactory()
	if err != nil {
		return AgentHandle{}, err
	}
	return factory.Resume(r.ctx, options)
}

// Register records a live agent. Fails if an agent with the same id is
// already registered. Emits agent/created on registration and agent/disposed
// when the returned disposer runs — both scope-filtered through the agent's
// carrier. The returned disposer is single-shot.
func (r *AgentRegistry) Register(agent *Agent) (cordis.Disposer, error) {
	detach, err := r.Enter(agent, nil)
	if err != nil {
		return nil, err
	}
	announceErr := r.Announce(agent)
	if announceErr != nil {
		detach()
		return nil, announceErr
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		detach()
	}, nil
}

// Enter inserts an already-constructed agent without announcing it. This is
// the advanced ordered-lifecycle primitive used by the async agent factory:
// it first completes setup while the agent is unpublished, then calls
// Announce. Owner is the live agent whose scoped context created this agent
// (runtime ownership, not durable lineage). Returns an idempotent closure
// that removes this exact entry and emits the paired agent/disposed; when
// called from a synchronous agent/created listener, removal and disposal wait
// until that creation dispatch unwinds.
func (r *AgentRegistry) Enter(agent *Agent, owner *Agent) (func(), error) {
	id := agent.ID
	if id != agent.Session.ID() {
		return nil, fmt.Errorf("agent id %q does not match session id %q", id, agent.Session.ID())
	}
	r.mu.Lock()
	if _, exists := r.store[id]; exists {
		r.mu.Unlock()
		return nil, fmt.Errorf("agent %q is already registered", id)
	}
	// The authoritative collision boundary: concurrent create/resume
	// operations may both prepare, but only one exact entry can publish.
	entry := &agentEntry{id: id, agent: agent, owner: owner}
	r.store[id] = entry
	r.order = append(r.order, id)
	r.mu.Unlock()
	entered := true
	var detach func()
	detach = func() {
		r.mu.Lock()
		if !entered {
			r.mu.Unlock()
			return
		}
		entered = false
		if entry.announcing {
			entry.detachRequested = true
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		r.detachEntered(entry)
	}
	return detach, nil
}

// detachEntered removes one exact entered agent and emits its paired
// disposal when announced. The captured entry identity is the final
// boundary: a stale capability can never delete a later same-id lifecycle.
func (r *AgentRegistry) detachEntered(entry *agentEntry) {
	r.mu.Lock()
	entry.detachRequested = false
	if r.store[entry.id] != entry {
		r.mu.Unlock()
		return
	}
	delete(r.store, entry.id)
	for i, id := range r.order {
		if id == entry.id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
	announced := entry.announced
	r.mu.Unlock()
	// An insertion rolled back before announce was never externally created,
	// so emitting disposed would invent an impossible lifecycle edge.
	if !announced {
		return
	}
	r.emitDisposed(entry)
}

// emitDisposed emits the paired disposal edge, containing listener failures
// so teardown never strands later listeners.
func (r *AgentRegistry) emitDisposed(entry *agentEntry) {
	r.events.emitContained(EventAgentDisposed, entry.agent.Scope, AgentLifecyclePayload{Agent: entry.agent})
}

// Announce announces an agent previously inserted with Enter. Fails if the
// agent is not the exact live registry entry for its id, or its creation
// announcement already began (including a reentrant call from a creation
// listener). A synchronous creation listener failure vetoes publication:
// the caller rolls the entry back with the detach closure.
func (r *AgentRegistry) Announce(agent *Agent) error {
	r.mu.Lock()
	entry := r.store[agent.ID]
	if entry == nil || entry.agent != agent {
		r.mu.Unlock()
		return fmt.Errorf("agent %q is not live in this registry", agent.ID)
	}
	if entry.announced || entry.announcing {
		r.mu.Unlock()
		return fmt.Errorf("agent %q was already announced", entry.id)
	}
	// Mark before dispatch so a listener cannot recursively create a second
	// lifecycle edge; detach still pairs a partially delivered first edge.
	entry.announcing = true
	entry.announced = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		entry.announcing = false
		requested := entry.detachRequested
		r.mu.Unlock()
		if requested {
			r.detachEntered(entry)
		}
	}()
	return r.events.emitVeto(EventAgentCreated, agent.Scope, AgentLifecyclePayload{Agent: agent})
}

// Get looks up a live agent by shared agent/session id; nil when absent.
func (r *AgentRegistry) Get(id session.SessionID) *Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.store[id]
	if entry == nil {
		return nil
	}
	return entry.agent
}

// IsOwnedBy tests whether a live agent was created through one exact parent
// agent's scoped context. Runtime ownership is independent of durable session
// lineage and remains unambiguous when unrelated providers reuse an id.
func (r *AgentRegistry) IsOwnedBy(id session.SessionID, owner *Agent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.store[id]
	return entry != nil && entry.owner == owner
}

// List returns all live agents in registration order; mutating the returned
// slice does not affect the registry.
func (r *AgentRegistry) List() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Agent, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.store[id].agent)
	}
	return out
}

// Roots returns all live top-level agents in registration order: created
// without an owning agent context. Durable session lineage does not affect
// this runtime relation, so a resumed fork may still be a root.
func (r *AgentRegistry) Roots() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Agent
	for _, id := range r.order {
		if entry := r.store[id]; entry.owner == nil {
			out = append(out, entry.agent)
		}
	}
	return out
}
