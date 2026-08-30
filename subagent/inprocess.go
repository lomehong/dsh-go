// The in-process ONE-SHOT driver and its two providers, the Go adaptation of
// the official in-process-driver + spawn/fork-in-process plugin trio. The
// agent factory's creation transaction owns unpublished setup and rollback;
// after publication the returned run owns signal handoff, one turn, result
// settlement, and quiescent disposal. Continuable children never come
// through here: the continuation manager composes and drives them directly.
//
// Go adaptation note: the official appends the one-shot descriptor inside
// the child's first pre-step; this port appends during unpublished setup
// instead — the same pre-publication window the delegation-policy overrides
// already use, with one ordering documented in README (descriptor precedes
// turn/start on the child log rather than following it).
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// InProcessProviderDeps are the composition inputs the spawn/fork providers
// share: the child factory seam, the structural owner context children are
// created under, the delegation policy sources, and the child-world
// composition inputs.
type InProcessProviderDeps struct {
	// Children builds child agents; required.
	Children ChildRuntime
	// Owner is the structural owner context every child is created under.
	Owner *cordis.Context
	// Sandbox is the parent's sandbox-override source for delegation
	// capture; nil-safe.
	Sandbox SandboxOverrideService
	// HasApproval pins the approval policy to never when composed.
	HasApproval bool
	// Presets joins the parent's preset into the child meta; nil-safe.
	Presets AgentPresetService
	// Prompt is the deployment system prompt for the child's scoped world;
	// nil-safe.
	Prompt *systemprompt.SystemPrompt
	// Registry is the tool registry the child's restriction scopes; nil-safe.
	Registry *tools.ToolRuntime
}

// newSessionID mints one session id in the official randomUUID shape.
func newSessionID() session.SessionID {
	return session.SessionID(newSubagentRunID())
}

// toStopReason maps a session turn-end kind to the subagent seam's terminal
// vocabulary. A pre-step rejection discarded the claimed prompt: the task
// was declined, and the caller must not read the run as done.
func toStopReason(kind string) StopReason {
	switch kind {
	case session.TurnEndCompleted:
		return StopCompleted
	case session.TurnEndMaxTokens:
		return StopMaxTokens
	case session.TurnEndAborted:
		return StopAborted
	case session.TurnEndBlocked:
		return StopRefusal
	default:
		// error, interrupted, and an unrecorded end (cancellation with no
		// accounting turn) never overstate success.
		return StopError
	}
}

// completedTurnPrefix slices the parent's completed-turn prefix: contiguous
// from seq 0 up to and including the last turn/end (the live tool-call turn
// is never inherited). Empty when no turn has completed.
func completedTurnPrefix(parent *agent.Agent) []session.Event {
	events := parent.Session.Events()
	lastSeq := int64(-1)
	for _, event := range events {
		if event.Type == session.EventTurnEnd {
			lastSeq = event.Seq
		}
	}
	if lastSeq < 0 {
		return nil
	}
	prefix := make([]session.Event, 0, lastSeq+1)
	for _, event := range events {
		if event.Seq <= lastSeq {
			prefix = append(prefix, event)
		}
	}
	return prefix
}

// inProcessRun is the published-run lifecycle: signal handoff, one turn,
// result settlement, and idempotent quiescent disposal.
type inProcessRun struct {
	id      session.SessionID
	child   *agent.Agent
	dispose func() error

	once      sync.Once
	result    SubagentResult
	resultErr error
	done      chan struct{}
}

func (r *inProcessRun) ID() session.SessionID           { return r.id }
func (r *inProcessRun) LocalAgent() *agent.Agent        { return r.child }
func (r *inProcessRun) Result() (SubagentResult, error) { <-r.done; return r.result, r.resultErr }

func (r *inProcessRun) Dispose() error {
	r.once.Do(func() { r.dispose() })
	<-r.done
	return nil
}

// StartInProcessRun establishes and drives one in-process one-shot child.
// seed is nil for a fresh spawn, or the completed-turn prefix for a fork.
func StartInProcessRun(deps InProcessProviderDeps, request ResolvedSubagentStartRequest, seed []session.Event) (SubagentRun, error) {
	if err := AssertSubagentMaxDepth(request.MaxDepth); err != nil {
		return nil, err
	}
	if request.Signal != nil && request.Signal.Err() != nil {
		return nil, fmt.Errorf("subagent request was aborted before child publication")
	}
	parent := request.Parent
	depth, err := ResolveChildDepth(parent, request.MaxDepth)
	if err != nil {
		return nil, err
	}
	childID := newSessionID()
	activationBoundary := len(seed)
	// Capture before any await: a later parent switch belongs to the
	// parent's future, not to this child.
	inherited := CaptureDelegatedPolicyOverrides(deps.Sandbox, deps.HasApproval, parent)

	setup := func(childCtx *cordis.Context) (agent.AgentSetupCommit, error) {
		child := agentFromContext(childCtx)
		if child == nil {
			return agent.AgentSetupCommit{}, fmt.Errorf("in-process child: setup context carries no agent")
		}
		if err := AppendDelegatedPolicyOverrides(child.Session, inherited); err != nil {
			return agent.AgentSetupCommit{}, err
		}
		if _, err := child.Session.Append(EventSubagentDescriptor, request.Descriptor, nil); err != nil {
			return agent.AgentSetupCommit{}, err
		}
		ApplyChildComposition(child.Scope, parent, ChildComposition{
			Persona:    request.Persona,
			ToolFilter: request.ToolFilter,
		}, ChildCompositionDeps{
			Prompt:   deps.Prompt,
			Registry: deps.Registry,
			Presets:  deps.Presets,
		})
		return agent.AgentSetupCommit{}, nil
	}

	handle, err := deps.Children.CreateAgent(deps.Owner, agent.CreateAgentOptions{
		SessionID:    childID,
		Meta:         ChildSessionMeta(deps.Presets, parent, depth, int64(activationBoundary)),
		Seed:         seed,
		AgentOptions: resolveChildAgentOptions(parent, request.AgentOptions, depth),
		Setup:        setup,
	})
	if err != nil {
		return nil, err
	}
	return drivePublishedRun(handle, request.Signal, request.Prompt, childID, activationBoundary), nil
}

// drivePublishedRun wraps a published child in the single run lifecycle.
func drivePublishedRun(handle agent.AgentHandle, signal context.Context, prompt []llm.ContentBlock, childID session.SessionID, boundary int) SubagentRun {
	child := handle.Agent
	run := &inProcessRun{id: childID, child: child, done: make(chan struct{})}
	var cancelOnce sync.Once
	cancelChild := func() {
		cancelOnce.Do(func() {
			child.Cancel(session.TurnEndCancelCause{Kind: "parent"}, agent.CancelOptions{})
		})
	}
	run.dispose = func() error {
		cancelChild()
		return handle.Dispose()
	}
	if signal != nil {
		go func() {
			select {
			case <-signal.Done():
				cancelChild()
			case <-run.done:
			}
		}()
	}

	go func() {
		defer close(run.done)
		cancelled := false
		if signal != nil && signal.Err() != nil {
			cancelled = true
		}
		if !cancelled {
			child.Driver().Followup(llm.NewUserMessage(prompt, llm.MessageSource{Kind: llm.SourceUser}))
			<-child.Driver().WhenIdle()
			if signal != nil && signal.Err() != nil {
				cancelled = true
			}
		}
		run.result = readResult(child, boundary, cancelled)
	}()
	return run
}

// readResult reads one settled child's terminal result from events after its
// activation boundary.
func readResult(child *agent.Agent, boundary int, cancelled bool) SubagentResult {
	events := child.Session.Events()
	if boundary > 0 {
		if boundary <= len(events) {
			events = events[boundary:]
		} else {
			events = nil
		}
	}
	kind := ""
	for _, event := range events {
		if event.Type == session.EventTurnEnd {
			if end, ok := decodeTurnEndData(event); ok {
				kind = end.Reason.Kind
			}
		}
	}
	if kind == "" {
		if cancelled {
			return SubagentResult{
				Output:     FinalAssistantOutput(events),
				StopReason: StopAborted,
				Diagnostic: "cancelled before the child recorded a turn end",
			}
		}
		return SubagentResult{
			StopReason: StopError,
			Diagnostic: "child reached idle with no recorded turn end",
		}
	}
	return SubagentResult{
		Output:     FinalAssistantOutput(events),
		StopReason: toStopReason(kind),
	}
}

// SpawnInProcessProvider runs each child as a fresh child agent on the same
// process: its own session, own system prompt, zero parent context.
type SpawnInProcessProvider struct {
	name string
	deps InProcessProviderDeps
}

// NewSpawnInProcessProvider builds the spawn provider under `name`.
func NewSpawnInProcessProvider(name string, deps InProcessProviderDeps) *SpawnInProcessProvider {
	return &SpawnInProcessProvider{name: name, deps: deps}
}

func (p *SpawnInProcessProvider) Name() string { return p.name }

// Capabilities: depthLimit (it constructs the child), agentOptions,
// toolFilter, and persona (scoped composition in the creation window).
// OutputSchema stays false until the structured-capture round — advertised
// capability must match reality.
func (p *SpawnInProcessProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{
		AgentOptions: true,
		DepthLimit:   true,
		ToolFilter:   true,
		Persona:      true,
	}
}

func (p *SpawnInProcessProvider) InheritsParentContext() bool { return false }

func (p *SpawnInProcessProvider) Start(request ResolvedSubagentStartRequest) (SubagentRun, error) {
	// Fresh child: no seed. The shared driver mints ids, stamps
	// cwd/lineage/depth, drives the one-shot, and maps the result.
	return StartInProcessRun(p.deps, request, nil)
}

// ForkInProcessProvider runs each child seeded with the parent's
// completed-turn prefix: the child sees the parent's conversation history
// but starts fresh on its own session.
type ForkInProcessProvider struct {
	name string
	deps InProcessProviderDeps
}

// NewForkInProcessProvider builds the fork provider under `name`.
func NewForkInProcessProvider(name string, deps InProcessProviderDeps) *ForkInProcessProvider {
	return &ForkInProcessProvider{name: name, deps: deps}
}

func (p *ForkInProcessProvider) Name() string { return p.name }

// Capabilities mirror spawn minus nothing: the seed is the fork difference.
// OutputSchema stays false until the structured-capture round.
func (p *ForkInProcessProvider) Capabilities() SubagentCapabilities {
	return SubagentCapabilities{
		AgentOptions: true,
		DepthLimit:   true,
		ToolFilter:   true,
		Persona:      true,
	}
}

func (p *ForkInProcessProvider) InheritsParentContext() bool { return true }

func (p *ForkInProcessProvider) Start(request ResolvedSubagentStartRequest) (SubagentRun, error) {
	return StartInProcessRun(p.deps, request, completedTurnPrefix(request.Parent))
}

// PrepareContinuable contributes the creation spec for a continuable child.
// A spawned child starts fresh, so it contributes no seed; the continuation
// manager owns every later operation on it.
func (p *SpawnInProcessProvider) PrepareContinuable(request ContinuableCreateRequest) (ContinuableCreateSpec, error) {
	return ContinuableCreateSpec{}, nil
}

// PrepareContinuable captures the fork prefix once, at creation: it becomes
// part of the child's own durable transcript, so a later cold resume replays
// that prefix instead of re-forking the parent's newer history.
func (p *ForkInProcessProvider) PrepareContinuable(request ContinuableCreateRequest) (ContinuableCreateSpec, error) {
	seed := completedTurnPrefix(request.Parent)
	if len(seed) == 0 {
		return ContinuableCreateSpec{}, nil
	}
	return ContinuableCreateSpec{Seed: seed}, nil
}

// NewInProcessProvider builds the spawn or fork provider per kind (the
// default registry name selects the transport); unknown kinds fail loud.
func NewInProcessProvider(name, kind string, deps InProcessProviderDeps) (SubagentProvider, error) {
	switch kind {
	case "spawn":
		return NewSpawnInProcessProvider(name, deps), nil
	case "fork":
		return NewForkInProcessProvider(name, deps), nil
	default:
		return nil, fmt.Errorf("in-process provider: unknown kind %q", kind)
	}
}

// decodeTurnEndData extracts one turn-end record from its event; event data
// carries raw JSON decoded into the typed record.
func decodeTurnEndData(event session.Event) (session.TurnEndData, bool) {
	var end session.TurnEndData
	if err := json.Unmarshal(event.Data, &end); err != nil {
		return session.TurnEndData{}, false
	}
	return end, true
}
