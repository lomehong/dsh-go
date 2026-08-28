package subagent

import (
	"crypto/rand"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/tools"
)

// Continuation error codes surfaced by the runtime itself (the manager adds
// its own vocabulary in its round).
const (
	CodeDuplicateProvider       = "DUPLICATE_PROVIDER"
	CodeNoProvider              = "NO_PROVIDER"
	CodeUnsupportedCapability   = "UNSUPPORTED_CAPABILITY"
	CodeContinuationUnavailable = "CONTINUATION_UNAVAILABLE"
)

// SubagentRuntime is the subagent capability seam: a named-provider registry
// plus a capability-validating start API. Providers establish a child before
// returning its run, so fulfillment is the single publication and
// ownership-transfer boundary. Multiple providers coexist under unique
// names; callers select one by name.
//
// Go adaptations: the cordis `ctx.subagents` service is an explicit
// constructor holding the agent registry's event bus (scoped lifecycle
// dispatch keys the carrier by the delegating parent's scope, matching the
// official `scopeTarget(this, parent)`); provider insertion order is kept in
// a slice because Go map iteration is random; the typert remote faces ride
// the SDK/typert round.
type SubagentRuntime struct {
	logger cordis.Logger
	events *agent.SubjectEventBus

	mu        sync.Mutex
	providers map[string]SubagentProvider
	order     []string

	setupRegistry *ActivationSetupRegistry

	// continuations is the optional continuable-children manager (the
	// official agents-service injection). Nil means the composition cannot
	// own resident activations and every continuable operation fails with
	// CONTINUATION_UNAVAILABLE — exactly the official manager-less shape.
	continuations ContinuationManager
}

// ContinuationManager is the runtime's seam to the continuable-children
// manager. The concrete manager lands with the continuation round; until
// then a composition without one behaves like the official manager-less
// assembly.
type ContinuationManager interface {
	StartContinuable(spec ContinuableStartSpec) (ContinuableStart, error)
	Followup(parent *agent.Agent, childID session.SessionID, content []llm.ContentBlock, options SubagentFollowupOptions) (llm.MessageID, error)
	Interrupt(targetSessionID session.SessionID, authority SubagentInterruptAuthority)
	ReportFrom(child *agent.Agent, content []llm.ContentBlock, options SubagentReportOptions) (llm.MessageID, error)
	DrainDescendants(parents []*agent.Agent) error
	DrainChildren(parent *agent.Agent, childIDs []session.SessionID) error
}

// RuntimeConfig wires the runtime to its composition.
type RuntimeConfig struct {
	// Logger contains listener failures and registry diagnostics; nil
	// discards.
	Logger cordis.Logger
	// Events is the agent registry's subject bus; lifecycle edges dispatch
	// through it with the delegating parent's scope as the carrier.
	Events *agent.SubjectEventBus
}

// NewSubagentRuntime builds the runtime over the composition's event bus.
func NewSubagentRuntime(config RuntimeConfig) *SubagentRuntime {
	return &SubagentRuntime{
		logger:        config.Logger,
		events:        config.Events,
		providers:     map[string]SubagentProvider{},
		setupRegistry: NewActivationSetupRegistry(),
	}
}

// SetContinuations installs the continuable-children manager (the official
// `ctx.inject(['agents'], ...)` binding). Pass nil to detach.
func (r *SubagentRuntime) SetContinuations(manager ContinuationManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.continuations = manager
}

func (r *SubagentRuntime) currentContinuations() ContinuationManager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.continuations
}

// SetupRegistry exposes the continuable setup registry so compositions can
// contribute child-scope installations before the manager round lands its
// own wiring.
func (r *SubagentRuntime) SetupRegistry() *ActivationSetupRegistry {
	return r.setupRegistry
}

// emit publishes one lifecycle edge through the bus with per-listener
// containment (the bus contract). Run edges carry the delegating parent that
// keys scoped dispatch; provider removal reaches listeners unscoped.
func (r *SubagentRuntime) emit(event string, parent *agent.Agent, payload any) {
	if r.events == nil {
		return
	}
	carrier := scope.ScopeKey(nil)
	if parent != nil {
		carrier = parent.Scope
	}
	r.events.Emit(event, carrier, payload)
}

// RegisterProvider registers a provider under its unique name. Duplicate
// registration fails loud (DUPLICATE_PROVIDER) without disturbing the
// existing registration; removing a provider blocks new starts but does not
// revoke runs already returned to their holders. The returned disposer is
// idempotent.
func (r *SubagentRuntime) RegisterProvider(provider SubagentProvider) (func(), error) {
	name := provider.Name()
	r.mu.Lock()
	if _, exists := r.providers[name]; exists {
		r.mu.Unlock()
		return nil, newSubagentError(
			fmt.Sprintf("a subagent provider named %q is already registered", name),
			CodeDuplicateProvider, nil)
	}
	r.providers[name] = provider
	r.order = append(r.order, name)
	r.mu.Unlock()
	removed := false
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if removed {
			return
		}
		removed = true
		if _, ok := r.providers[name]; !ok {
			return
		}
		delete(r.providers, name)
		for i, candidate := range r.order {
			if candidate == name {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
		if r.events != nil {
			r.events.Emit(EventProviderRemoved, nil, name)
		}
	}, nil
}

// Lifecycle event names.
const (
	// EventProviderAdded: a provider became resolvable in the registry.
	EventProviderAdded = "subagent/provider-added"
	// EventProviderRemoved: a provider left the registry; accepted runs
	// remain holder-owned.
	EventProviderRemoved = "subagent/provider-removed"
	// EventSubagentStart: a provider established a published child. Scoped
	// dispatch keys the carrier by the delegating parent.
	EventSubagentStart = "subagent/start"
	// EventSubagentEnd: a published child settled, paired with start by
	// run id.
	EventSubagentEnd = "subagent/end"
)

// GetProvider looks a provider up by name.
func (r *SubagentRuntime) GetProvider(name string) (SubagentProvider, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	provider, ok := r.providers[name]
	return provider, ok
}

// List returns registered provider names in insertion order (the official
// Map iteration order).
func (r *SubagentRuntime) List() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.order...)
}

// Start establishes a published child on the named provider. Capability and
// semantic checks run before delegation; a rejection has no run for the
// caller to dispose and emits no lifecycle events. Provider ownership lasts
// until Start fulfills; post-publication turn and infrastructure failures
// settle through the returned run.
func (r *SubagentRuntime) Start(name string, request SubagentStartRequest) (SubagentRun, error) {
	provider, ok := r.GetProvider(name)
	if !ok {
		return nil, newSubagentError(
			fmt.Sprintf("no subagent provider registered for %q", name),
			CodeNoProvider, nil)
	}
	if err := r.assertCapabilities(provider, request); err != nil {
		return nil, err
	}
	if err := AssertSubagentMaxDepth(request.MaxDepth); err != nil {
		return nil, err
	}
	if request.OutputSchema != nil {
		if err := tools.AssertObjectJsonSchema(request.OutputSchema); err != nil {
			return nil, err
		}
	}
	descriptor, err := SnapshotSubagentDescriptor(DescriptorInput{
		Mode:     ModeOneShot,
		Provider: name,
		Label:    request.Label,
		HasLabel: request.Label != "",
	})
	if err != nil {
		return nil, err
	}
	resolved := ResolvedSubagentStartRequest{SubagentStartRequest: request, Descriptor: descriptor}
	run, err := provider.Start(resolved)
	if err != nil {
		return nil, err
	}
	return r.observeRun(name, request.Parent, run), nil
}

// observeRun emits the start/end lifecycle pair for one accepted one-shot
// run. Start is emitted synchronously BEFORE the terminal watcher launches,
// which preserves start → end exactly like the official promise-reaction
// ordering (reactions run after the synchronous emission).
func (r *SubagentRuntime) observeRun(provider string, parent *agent.Agent, run SubagentRun) SubagentRun {
	identity := SubagentRunInfo{
		RunID:    newSubagentRunID(),
		Provider: provider,
		ID:       run.ID(),
		Local:    run.LocalAgent() != nil,
	}
	r.emit(EventSubagentStart, parent, identity)
	go func() {
		result, err := run.Result()
		if err != nil {
			// Infrastructure fault: the seam cannot represent it as a stop
			// reason, so the terminal edge reports a plain error.
			r.emit(EventSubagentEnd, parent, SubagentRunEndInfo{SubagentRunInfo: identity, StopReason: StopError})
			return
		}
		end := SubagentRunEndInfo{SubagentRunInfo: identity, StopReason: result.StopReason}
		// Omit the field when no output exists, matching continuable epochs.
		if len(result.Output) > 0 {
			end.LastAssistantMessage = result.Output
		}
		r.emit(EventSubagentEnd, parent, end)
	}()
	return run
}

// assertCapabilities rejects the first requested capability the provider
// lacks with UNSUPPORTED_CAPABILITY.
func (r *SubagentRuntime) assertCapabilities(provider SubagentProvider, request SubagentStartRequest) error {
	capabilities := provider.Capabilities()
	needs := []struct {
		present bool
		cap     string
	}{
		{request.AgentOptions != nil, "agentOptions"},
		{request.OutputSchema != nil, "outputSchema"},
		{request.MaxDepth != nil, "depthLimit"},
		{request.ToolFilter != nil, "toolFilter"},
		{request.Persona != "", "persona"},
	}
	for _, need := range needs {
		if !need.present {
			continue
		}
		if !capabilityFlag(capabilities, need.cap) {
			return newSubagentError(
				fmt.Sprintf("subagent provider %q does not support the %q capability", provider.Name(), need.cap),
				CodeUnsupportedCapability, nil)
		}
	}
	return nil
}

// capabilityFlag reads one named flag without a reflection round-trip.
func capabilityFlag(capabilities SubagentCapabilities, name string) bool {
	switch name {
	case "agentOptions":
		return capabilities.AgentOptions
	case "outputSchema":
		return capabilities.OutputSchema
	case "depthLimit":
		return capabilities.DepthLimit
	case "toolFilter":
		return capabilities.ToolFilter
	case "persona":
		return capabilities.Persona
	}
	return false
}

// PrepareContinuable resolves one provider's detached continuable-creation
// contribution. Method presence on the provider IS the capability, so a
// ContinuableProvider check runs before the manager reserves any child
// resources.
func (r *SubagentRuntime) PrepareContinuable(name string, request ContinuableCreateRequest) (ContinuableCreateSpec, error) {
	provider, ok := r.GetProvider(name)
	if !ok {
		return ContinuableCreateSpec{}, newSubagentError(
			fmt.Sprintf("no subagent provider registered for %q", name),
			CodeNoProvider, nil)
	}
	continuable, ok := provider.(ContinuableProvider)
	if !ok {
		return ContinuableCreateSpec{}, newSubagentError(
			fmt.Sprintf("subagent provider %q does not support continuable children (no prepareContinuable capability)", provider.Name()),
			CodeUnsupportedCapability, nil)
	}
	return continuable.PrepareContinuable(request)
}

// requireContinuations resolves the optional manager or fails loud.
func (r *SubagentRuntime) requireContinuations() (ContinuationManager, error) {
	manager := r.currentContinuations()
	if manager == nil {
		return nil, newSubagentError(
			"continuable subagents require the agents service",
			CodeContinuationUnavailable, nil)
	}
	return manager, nil
}

// StartContinuable establishes one durable continuable child and delivers
// its initial prompt. Delegates to the manager; a manager-less composition
// fails with CONTINUATION_UNAVAILABLE.
func (r *SubagentRuntime) StartContinuable(spec ContinuableStartSpec) (ContinuableStart, error) {
	manager, err := r.requireContinuations()
	if err != nil {
		return ContinuableStart{}, err
	}
	return manager.StartContinuable(spec)
}

// Followup delivers one later message to a continuable child as its next
// FIFO turn.
func (r *SubagentRuntime) Followup(parent *agent.Agent, childID session.SessionID, content []llm.ContentBlock, options SubagentFollowupOptions) (llm.MessageID, error) {
	manager, err := r.requireContinuations()
	if err != nil {
		return "", err
	}
	return manager.Followup(parent, childID, content, options)
}

// Interrupt one live continuable child's current turn. A manager-less
// composition is an accepted no-op (nothing can own a live Activation).
func (r *SubagentRuntime) Interrupt(targetSessionID session.SessionID, authority SubagentInterruptAuthority) {
	if manager := r.currentContinuations(); manager != nil {
		manager.Interrupt(targetSessionID, authority)
	}
}

// ReportFrom delivers selected content from one live continuable child to
// its durable direct parent.
func (r *SubagentRuntime) ReportFrom(child *agent.Agent, content []llm.ContentBlock, options SubagentReportOptions) (llm.MessageID, error) {
	manager, err := r.requireContinuations()
	if err != nil {
		return "", err
	}
	return manager.ReportFrom(child, content, options)
}

// DrainContinuableDescendants closes continuable admission below the exact
// parent agents entering teardown. A manager-less composition returns nil.
func (r *SubagentRuntime) DrainContinuableDescendants(parents []*agent.Agent) error {
	manager := r.currentContinuations()
	if manager == nil {
		return nil
	}
	return manager.DrainDescendants(parents)
}

// DrainContinuableChildren releases selected resident continuable direct
// children of one exact live parent. A manager-less composition is a no-op.
func (r *SubagentRuntime) DrainContinuableChildren(parent *agent.Agent, childIDs []session.SessionID) error {
	manager := r.currentContinuations()
	if manager == nil {
		return nil
	}
	return manager.DrainChildren(parent, childIDs)
}

// ObserveActivation builds the lifecycle observer for one continuable
// Activation's residency epoch, so the manager publishes its edges without
// owning event dispatch.
func (r *SubagentRuntime) ObserveActivation(provider string, childID session.SessionID, parent *agent.Agent) *ActivationObserver {
	return createActivationObserver(r, provider, childID, parent)
}

// newSubagentRunID mints one v4 UUID (the official randomUUID run id).
func newSubagentRunID() SubagentRunID {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return SubagentRunID("00000000-0000-4000-8000-000000000000")
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return SubagentRunID(fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]))
}
