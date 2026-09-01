package subagent

import (
	"errors"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
)

// SessionObservation is one cold read of a persisted child log.
type SessionObservation struct {
	Header session.SessionHeader
	Events []session.Event
}

// SessionQuery is the cold-observation service the manager's resume path
// requires (the official `ctx.get('sessionQuery')`).
type SessionQuery interface {
	ObserveSession(id session.SessionID) (SessionObservation, error)
}

// SnapshotLister is the persistence seam the duplicate-child check consults
// (`*persistence.Coordinator` satisfies it).
type SnapshotLister interface {
	ListSnapshots() ([]persistence.Snapshot, error)
	FlushSession(sess *session.Session) error
}

// errorChain renders one failure the way the official source joins teardown
// reasons.
func errorChain(err error) string {
	return llm.ErrorChain(err)
}

// continuationHost is what the manager asks back from the owning runtime.
type continuationHost interface {
	PrepareContinuable(name string, request ContinuableCreateRequest) (ContinuableCreateSpec, error)
	ObserveActivation(provider string, childID session.SessionID, parent *agent.Agent) *ActivationObserver
}

// ManagerExt are the host services materialization and teardown consume.
// Go adaptation of the official opportunistic `ctx.get` web: explicit,
// nil-safe fields.
type ManagerExt struct {
	// Host is the owning runtime (prepare + observer factory). Required.
	Host continuationHost
	// Snapshots is the persistence store for duplicate checks and the
	// best-effort final flush. Required for start; nil flush skips flushing.
	Snapshots SnapshotLister
	// Query cold-reads persisted children for resume. Optional until the
	// session-query round; nil → CONTINUATION_UNAVAILABLE on resume.
	Query SessionQuery
	// Prompt/Registry/Presets compose the child's scoped world.
	Composition ChildCompositionDeps
	// Sandbox is the parent's explicit sandbox-override source; nil-safe.
	Sandbox SandboxOverrideService
	// HasApproval pins the approval policy to `never` when composed.
	HasApproval bool
	// LLM is the model registry the image-capability gate reads (the
	// official ctx.get('llm') seam); nil skips the gate and defers to the
	// text-only projection, exactly like an LLM-less composition upstream.
	LLM *llm.Runtime
}

// SetManagerExt installs the extension services once composed.
func (m *SubagentContinuationManager) SetManagerExt(ext ManagerExt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ext = ext
}

// materialization is one admitted materialization tracked through
// publication or rollback, with the exact live ancestry observed at its
// synchronous admission boundary.
type materialization struct {
	lineage []*agent.Agent
	settled chan struct{}
}

// errRetryFollowup marks the lost send-versus-dispose cutoff: the caller
// waits for release, then cold-resumes a new Activation.
var errRetryFollowup = errors.New("subagent: activation released; retry the delivery")

func (m *SubagentContinuationManager) extSnapshot() ManagerExt {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ext
}

func (m *SubagentContinuationManager) requireExt() (ManagerExt, error) {
	ext := m.extSnapshot()
	if ext.Host == nil {
		return ext, newSubagentError("continuable subagents require the owning subagent runtime", CodeContinuationUnavailable, nil)
	}
	if ext.Snapshots == nil {
		return ext, newSubagentError(
			"continuable subagents require session persistence (load a dsh-session-persistence backend)",
			CodePersistenceUnavailable, nil)
	}
	return ext, nil
}

// StartContinuable establishes one durable continuable background child:
// reserve its identity, resolve the provider's detached creation spec,
// create the child through the private activation-owner scope, establish
// parent ownership, and submit the initial prompt. Resolves when inbox
// acceptance yields the message id. Every failure before acceptance leaves
// neither id nor child.
func (m *SubagentContinuationManager) StartContinuable(spec ContinuableStartSpec) (ContinuableStart, error) {
	ext, err := m.requireExt()
	if err != nil {
		return ContinuableStart{}, err
	}
	parent := spec.Request.Parent
	if parent == nil {
		return ContinuableStart{}, newSubagentError("continuable subagents require a delegating parent agent", CodeUnauthorized, nil)
	}
	if err := m.assertAdmitting(parent); err != nil {
		return ContinuableStart{}, err
	}
	if err := AssertSubagentMaxDepth(spec.Request.MaxDepth); err != nil {
		return ContinuableStart{}, err
	}
	// Minted ids skip the persisted leg of the collision check: the mint is
	// a fresh random identity, so the official source only pays the
	// O(sessions) list for caller-chosen ids, where reuse is possible.
	explicit := spec.ChildID != ""
	childID := spec.ChildID
	if childID == "" {
		childID = session.SessionID(newSubagentRunID())
	}
	if err := m.assertChildIDAvailable(ext, childID, explicit); err != nil {
		return ContinuableStart{}, err
	}
	childDepth, err := ResolveChildDepth(parent, spec.Request.MaxDepth)
	if err != nil {
		return ContinuableStart{}, err
	}
	// Snapshot before any await: invalid descriptor JSON rejects the call
	// before a child exists, and the detached value is what reaches the log.
	resolvedOptions := resolveChildAgentOptions(parent, spec.Request.AgentOptions, childDepth)
	descriptor, err := SnapshotSubagentDescriptor(DescriptorInput{
		Mode:                 ModeContinuable,
		Provider:             spec.Provider,
		Label:                spec.Label,
		AgentProvider:        resolvedOptions.Provider,
		AgentModel:           resolvedOptions.Model,
		AgentReasoningEffort: resolvedOptions.ReasoningEffort,
		Persona:              spec.Request.Persona,
		ToolFilter:           spec.Request.ToolFilter,
	})
	if err != nil {
		return ContinuableStart{}, err
	}
	// Capture before the first await: a later parent switch belongs to the
	// parent's future, not to this child.
	delegated := CaptureDelegatedPolicyOverrides(ext.Sandbox, ext.HasApproval, parent)

	prepared, err := ext.Host.PrepareContinuable(spec.Provider, ContinuableCreateRequest{
		SessionID: childID,
		Parent:    parent,
		Signal:    spec.Signal,
	})
	if err != nil {
		return ContinuableStart{}, err
	}
	lineageSeedLength := int64(len(prepared.Seed))
	seed, err := SeedDescriptorTurn(childID, prepared.Seed, descriptor)
	if err != nil {
		return ContinuableStart{}, err
	}
	meta := ChildSessionMeta(ext.Composition.Presets, parent, childDepth, lineageSeedLength)
	var messageID llm.MessageID
	lockErr := m.locks.Run(childID, func() error {
		if err := m.assertChildIDAvailable(ext, childID, explicit); err != nil {
			return err
		}
		activation, err := m.materialize(ext, materializeInputs{
			childID:    childID,
			provider:   spec.Provider,
			parent:     parent,
			agentOptns: resolvedOptions,
			composition: ChildComposition{
				Persona:    spec.Request.Persona,
				ToolFilter: spec.Request.ToolFilter,
			},
			signal: spec.Signal,
			create: &createInputs{
				seed:            seed,
				meta:            meta,
				delegatedPolicy: delegated,
			},
		})
		if err != nil {
			return err
		}
		messageID, err = m.submitMaterialized(activation, spec.Request.Prompt, llm.MessageSource{
			Kind: "user",
		}, parent, spec.Signal)
		return err
	})
	if lockErr != nil {
		return ContinuableStart{}, lockErr
	}
	return ContinuableStart{ChildID: childID, MessageID: messageID}, nil
}

// assertChildIDAvailable rejects a child identity already owned by a live
// Agent, a live Activation, or a persisted session. The persisted leg runs
// only for caller-chosen ids (a minted id cannot collide); a list failure
// there is fail loud — the official source awaits the list and lets the
// storage error reject the start rather than risk a duplicate creation.
func (m *SubagentContinuationManager) assertChildIDAvailable(ext ManagerExt, childID session.SessionID, explicit bool) error {
	if m.deps.Agents.Get(childID) != nil {
		return newSubagentError(fmt.Sprintf("subagent %q already exists", childID), CodeDuplicateChild, nil)
	}
	m.mu.Lock()
	_, live := m.activations[childID]
	m.mu.Unlock()
	if live {
		return newSubagentError(fmt.Sprintf("subagent %q already exists", childID), CodeDuplicateChild, nil)
	}
	if !explicit {
		return nil
	}
	persisted, err := ext.Snapshots.ListSnapshots()
	if err != nil {
		return fmt.Errorf("listing persisted subagent sessions: %w", err)
	}
	for _, snapshot := range persisted {
		if snapshot.Header.ID == childID {
			return newSubagentError(fmt.Sprintf("subagent %q already exists", childID), CodeDuplicateChild, nil)
		}
	}
	return nil
}

// createInputs are the creation-only inputs; absent for a cold resume, which
// loads the persisted session — including the delegation policy events a
// fresh creation seeded, so a resume never re-captures the parent's policy.
type createInputs struct {
	seed            []session.Event
	meta            agent.CreateAgentMeta
	delegatedPolicy DelegatedPolicyOverrides
}

type materializeInputs struct {
	childID     session.SessionID
	provider    string
	parent      *agent.Agent
	agentOptns  agent.AgentOptions
	composition ChildComposition
	signal      contextSignal
	create      *createInputs
}

// contextSignal is the cancellation face the manager consumes.
type contextSignal interface {
	Err() error
}

// submitMaterialized submits to a freshly materialized Activation or rolls
// it back completely.
func (m *SubagentContinuationManager) submitMaterialized(activation *Activation, content []llm.ContentBlock, source llm.MessageSource, parent *agent.Agent, signal contextSignal) (llm.MessageID, error) {
	id, err := m.submitAdmitted(activation, content, source, parent, signal)
	if err != nil {
		// Rollback disposal failures must not mask the pre-acceptance signal,
		// drain, or lifecycle failure.
		txn := m.dispose(activation)
		<-txn
		_ = m.disposalOutcome(activation)
		return "", err
	}
	return id, nil
}

// materialize creates or resumes the child through the private
// activation-owner scope, installs the handle in a fresh Activation, and
// registers ownership on a continuation-managed parent. Rejection leaves no
// Activation, no handle, and no ownership membership.
func (m *SubagentContinuationManager) materialize(ext ManagerExt, inputs materializeInputs) (*Activation, error) {
	if err := m.assertAdmitting(inputs.parent); err != nil {
		return nil, err
	}
	m.mu.Lock()
	track := &materialization{lineage: m.liveLineage(inputs.parent), settled: make(chan struct{})}
	m.mu.Unlock()
	activation, err := m.materializeTracked(ext, inputs, track)
	close(track.settled)
	m.mu.Lock()
	delete(m.materializations, track)
	m.mu.Unlock()
	return activation, err
}

// materializeTracked performs one tracked materialization; the drain barrier
// stays registered until this returns a resident Activation or finishes
// rollback.
func (m *SubagentContinuationManager) materializeTracked(ext ManagerExt, inputs materializeInputs, track *materialization) (*Activation, error) {
	if inputs.signal != nil && inputs.signal.Err() != nil {
		return nil, inputs.signal.Err()
	}
	observer := ext.Host.ObserveActivation(inputs.provider, inputs.childID, inputs.parent)
	setup := func(childCtx *cordis.Context) (agent.AgentSetupCommit, error) {
		child := agentFromContext(childCtx)
		// Only fresh creation seeds the delegation policy onto the child's
		// own log (after any fork seed, so fresh policy wins stale seed
		// state); a cold resume replays those persisted events instead.
		if inputs.create != nil && child != nil {
			if err := AppendDelegatedPolicyOverrides(child.Session, inputs.create.delegatedPolicy); err != nil {
				return agent.AgentSetupCommit{}, err
			}
		}
		ApplyChildComposition(child.Scope, inputs.parent, inputs.composition, ext.Composition)
		return m.deps.Setup.Apply(childCtx)
	}
	// Agent creation owns rollback before handle transfer.
	var handle agent.AgentHandle
	var err error
	if inputs.create == nil {
		handle, err = m.deps.Children.Resume(m.deps.OwnerCtx, agent.ResumeAgentOptions{
			ResumeSessionID: inputs.childID,
			AgentOptions:    inputs.agentOptns,
			Setup:           setup,
		})
	} else {
		handle, err = m.deps.Children.CreateAgent(m.deps.OwnerCtx, agent.CreateAgentOptions{
			SessionID:    inputs.childID,
			Meta:         inputs.create.meta,
			Seed:         inputs.create.seed,
			AgentOptions: inputs.agentOptns,
			Setup:        setup,
		})
	}
	if err != nil {
		return nil, err
	}
	activation := &Activation{
		ChildID:       inputs.childID,
		ParentSession: inputs.parent.ID,
		Provider:      inputs.provider,
		Handle:        handle,
		ancestry:      ancestrySet(handle.Agent, track.lineage),
		ownedChildren: map[session.SessionID]struct{}{},
		accepted:      map[llm.MessageID]struct{}{},
		Observer:      observer,
	}
	// After transfer, any failure must dispose the created handle, remove the
	// Activation, and roll back parent ownership before rejecting.
	m.mu.Lock()
	m.activations[inputs.childID] = activation
	m.mu.Unlock()
	if inputs.signal != nil && inputs.signal.Err() != nil {
		err = inputs.signal.Err()
	} else {
		err = m.assertAdmitting(inputs.parent)
	}
	if err == nil {
		err = m.acquireOwnership(inputs.parent, inputs.childID)
	}
	if err == nil {
		m.watchInbox(activation)
		// Publish the start edge before any turn can run, so observers see
		// this epoch before its first request.
		observer.Start(handle.Agent)
	}
	if err != nil {
		// A start publication throw leaves no residency edge to pair.
		txn := m.rollbackUnpublished(activation)
		<-txn
		_ = m.disposalOutcome(activation)
		return nil, err
	}
	m.watchSettlement(activation)
	return activation, nil
}

// watchInbox registers the accepted-id drain listeners on the child's own
// bus: every accepted id leaves the inbox exactly once, through claim or
// discard, and clearing it there is what lets stateOf distinguish a truly
// quiet agent from one whose accepted turn has not been admitted yet. The
// two events carry different payload types (claimed rides a turn), so the
// typed accessors force the split the erased decoder used to paper over —
// it only matched the discard shape, silently dropping the claim drain.
func (m *SubagentContinuationManager) watchInbox(activation *Activation) {
	leave := func(message llm.Message) {
		m.mu.Lock()
		_, wasAccepted := activation.accepted[message.ID]
		delete(activation.accepted, message.ID)
		m.mu.Unlock()
		if wasAccepted {
			m.wake(activation)
		}
	}
	events := activation.Handle.Agent.Events()
	events.InboxClaimed().On(nil, func(claimed agent.AgentClaimedPayload) error {
		leave(claimed.Message)
		return nil
	})
	events.InboxDiscarded().On(nil, func(discarded agent.AgentMessagePayload) error {
		leave(discarded.Message)
		return nil
	})
}

func ancestrySet(self *agent.Agent, lineage []*agent.Agent) map[*agent.Agent]struct{} {
	set := map[*agent.Agent]struct{}{self: {}}
	for _, ancestor := range lineage {
		set[ancestor] = struct{}{}
	}
	return set
}

// rollbackUnpublished releases an Activation whose start edge was not
// published. The memoized transaction remains in the live map until handle
// disposal settles, so a concurrent drain or delivery observes the same
// closing boundary.
func (m *SubagentContinuationManager) rollbackUnpublished(activation *Activation) <-chan struct{} {
	m.mu.Lock()
	if activation.disposal == nil {
		activation.disposal = make(chan struct{})
		go func() {
			dispErr := activation.Handle.Dispose()
			m.mu.Lock()
			activation.dispoErr = dispErr
			delete(m.activations, activation.ChildID)
			m.mu.Unlock()
			m.releaseOwnership(activation.ChildID)
			close(activation.disposal)
		}()
	}
	txn := activation.disposal
	m.mu.Unlock()
	return txn
}

// acquireOwnership registers the child in a continuation-managed parent's
// owned set before the child can run, so that parent cannot settle while the
// child is live. A top-level or other non-continuation Agent has no
// Activation and stays outside the waiting graph.
func (m *SubagentContinuationManager) acquireOwnership(parent *agent.Agent, childID session.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	parentActivation := m.activations[parent.ID]
	if parentActivation == nil {
		return nil
	}
	if parentActivation.disposal != nil {
		return newSubagentError(
			fmt.Sprintf("subagent parent %q is being disposed; the child was not established", parent.ID),
			CodeActivationClosing, nil)
	}
	parentActivation.ownedChildren[childID] = struct{}{}
	return nil
}

// releaseOwnership removes one child from its live owner's set and lets that
// owner re-check settlement.
func (m *SubagentContinuationManager) releaseOwnership(childID session.SessionID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, candidate := range m.activations {
		if _, owned := candidate.ownedChildren[childID]; owned {
			delete(candidate.ownedChildren, childID)
			m.notifyWake(candidate)
		}
	}
}

// notifyWake pokes the settlement watcher; caller holds m.mu.
func (m *SubagentContinuationManager) notifyWake(activation *Activation) {
	activation.wakeLocked()
}

// wake lets a settlement watcher re-observe quiescence after ownership or
// inbox changes.
func (m *SubagentContinuationManager) wake(activation *Activation) {
	m.mu.Lock()
	activation.wakeLocked()
	m.mu.Unlock()
}

// submit submits one message as the child's next FIFO turn and returns its
// accepted inbox id. Acceptance is the operation's success boundary.
func (m *SubagentContinuationManager) submit(activation *Activation, content []llm.ContentBlock, source llm.MessageSource, parent *agent.Agent) (llm.MessageID, error) {
	// Parent-originated delivery keeps the parent live through ownership, so
	// establish it before the message can enter the child's inbox.
	if err := m.acquireOwnership(parent, activation.ChildID); err != nil {
		return "", err
	}
	message := llm.NewUserMessage(content, source)
	accepted, err := m.admitWaking(activation, message.ID, func() error {
		activation.Handle.Agent.Driver().Followup(message)
		return nil
	})
	if err != nil {
		return "", err
	}
	// Past this point the caller has an id for this child, so its eventual
	// settlement is something the parent is owed an account of.
	m.mu.Lock()
	activation.announced = true
	m.mu.Unlock()
	return accepted, nil
}

// admitWaking accounts one waking send across a resident Activation's
// settlement window: observers must see the Activation as busy before the
// send begins.
func (m *SubagentContinuationManager) admitWaking(activation *Activation, messageID llm.MessageID, send func() error) (llm.MessageID, error) {
	m.mu.Lock()
	activation.accepted[messageID] = struct{}{}
	m.mu.Unlock()
	if err := send(); err != nil {
		m.mu.Lock()
		delete(activation.accepted, messageID)
		m.mu.Unlock()
		return "", err
	}
	m.wake(activation)
	return messageID, nil
}

// submitAdmitted crosses the final admission cutoff and submits without
// yielding. Signal abort, manager drain, or Activation disposal that wins
// before this synchronous span rejects without inbox acceptance.
func (m *SubagentContinuationManager) submitAdmitted(activation *Activation, content []llm.ContentBlock, source llm.MessageSource, parent *agent.Agent, signal contextSignal) (llm.MessageID, error) {
	if signal != nil && signal.Err() != nil {
		return "", signal.Err()
	}
	if err := m.assertAdmitting(parent); err != nil {
		return "", err
	}
	m.mu.Lock()
	closing := activation.disposal != nil
	m.mu.Unlock()
	if closing {
		return "", newSubagentError(
			fmt.Sprintf("subagent %q activation is being disposed; the message was not accepted", activation.ChildID),
			CodeActivationClosing, nil)
	}
	// Image admission (alpha.3, upstream ba810b3539): an image-bearing
	// follow-up to a child whose resolved model accepts text only is refused
	// loud before the message exists. The check is synchronous, so it shares
	// the disposal-cutoff window above — no await gap to re-check.
	if err := m.assertImageCapable(activation, content); err != nil {
		return "", err
	}
	if err := m.authorizeLineage(parent, activation.ChildID, activation.Handle.Agent.Session.Header().ParentSession); err != nil {
		return "", err
	}
	return m.submit(activation, content, source, parent)
}

// contentHasImage reports whether a follow-up payload carries an inline
// image block (the upstream contentHasImage).
func contentHasImage(content []llm.ContentBlock) bool {
	for _, block := range content {
		if block.Type == llm.BlockImage {
			return true
		}
	}
	return false
}

// assertImageCapable refuses image content addressed to a child whose
// resolved model accepts text only (upstream assertImageCapable). A child
// without a fixed provider/model route, or a composition without the LLM
// registry, proceeds — the LLM layer's text-only projection replaces each
// image with its stable placeholder, exactly like the upstream deferral.
func (m *SubagentContinuationManager) assertImageCapable(activation *Activation, content []llm.ContentBlock) error {
	if !contentHasImage(content) {
		return nil
	}
	ext := m.extSnapshot()
	if ext.LLM == nil {
		return nil
	}
	agent := activation.Handle.Agent
	provider, model := agent.Options.Provider, agent.Options.Model
	if provider == "" || model == "" {
		return nil
	}
	info, err := ext.LLM.ResolveModelInfo(provider, model)
	if err != nil {
		return err
	}
	if info.InputModalities != nil && !containsString(info.InputModalities, "image") {
		return newSubagentError(
			fmt.Sprintf("Model %q does not support image input.", model),
			CodeModelDoesNotSupportImages, nil)
	}
	return nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// Followup delivers one later message to a known continuable child as its
// next FIFO turn. Routing depends only on Activation residency: running
// enqueues, waiting wakes the same Agent, absent cold-resumes. The inbox is
// the only queue, so every accepted message has one observable order.
func (m *SubagentContinuationManager) Followup(parent *agent.Agent, childID session.SessionID, content []llm.ContentBlock, options SubagentFollowupOptions) (llm.MessageID, error) {
	ext, err := m.requireExt()
	if err != nil {
		return "", err
	}
	if err := m.assertAdmitting(parent); err != nil {
		return "", err
	}
	for {
		var accepted llm.MessageID
		retry := false
		lockErr := m.locks.Run(childID, func() error {
			m.mu.Lock()
			activation := m.activations[childID]
			m.mu.Unlock()
			if activation == nil {
				var err error
				accepted, err = m.coldResume(ext, parent, childID, content, options)
				return err
			}
			m.mu.Lock()
			disposal := activation.disposal
			m.mu.Unlock()
			if disposal != nil {
				// A delivery that arrives after the disposal transaction began
				// must not reach a handle being torn down; wait for release,
				// then cold-resume.
				<-disposal
				retry = true
				return nil
			}
			var err error
			accepted, err = m.submitAdmitted(activation, content, options.Source, parent, options.Signal)
			return err
		})
		if lockErr != nil {
			return "", lockErr
		}
		if !retry {
			return accepted, nil
		}
		if err := m.assertAdmitting(parent); err != nil {
			return "", err
		}
		if options.Signal != nil && options.Signal.Err() != nil {
			return "", options.Signal.Err()
		}
	}
}

// coldResume loads a persisted child: authorize its prepared session, fold
// the descriptor, create the Activation through resume, and submit the
// waiting turn. Never dispatches through a provider — the persisted session
// already holds the initial prefix.
func (m *SubagentContinuationManager) coldResume(ext ManagerExt, parent *agent.Agent, childID session.SessionID, content []llm.ContentBlock, options SubagentFollowupOptions) (llm.MessageID, error) {
	if ext.Query == nil {
		return "", newSubagentError(
			"continuable subagents require session query (load the session-query service)",
			CodeContinuationUnavailable, nil)
	}
	observation, err := ext.Query.ObserveSession(childID)
	if err != nil {
		if options.Signal != nil && options.Signal.Err() != nil {
			return "", options.Signal.Err()
		}
		return "", newSubagentError(fmt.Sprintf("subagent %q is unavailable", childID), CodeNotResumable, err)
	}
	if err := m.assertAdmitting(parent); err != nil {
		return "", err
	}
	// Only the durable child's exact live direct parent may continue it.
	if err := m.authorizeLineage(parent, childID, observation.Header.ParentSession); err != nil {
		return "", err
	}
	// Fold only the child's own suffix: a fork seed replays the parent's log,
	// which may carry an ANCESTOR's descriptor.
	seedLength := int64(0)
	if observation.Header.SeedLength != nil {
		seedLength = *observation.Header.SeedLength
	}
	events := observation.Events
	if seedLength > 0 && int(seedLength) <= len(events) {
		events = events[seedLength:]
	}
	descriptor, err := FoldSubagentDescriptor(events)
	if err != nil || descriptor == nil || descriptor.Mode != ModeContinuable {
		_ = err
		return "", newSubagentError(
			fmt.Sprintf("subagent %q has no supported continuation state and cannot be resumed; do not retry send_message with this id", childID),
			CodeNotResumable, nil)
	}
	activation, err := m.materialize(ext, materializeInputs{
		childID:  childID,
		provider: descriptor.Provider,
		parent:   parent,
		agentOptns: agent.AgentOptions{
			Provider:        derefString(descriptor.AgentProvider),
			Model:           derefString(descriptor.AgentModel),
			ReasoningEffort: llm.ReasoningEffortID(derefString(descriptor.AgentReasoningEffort)),
		},
		composition: ChildComposition{Persona: derefString(descriptor.Persona), ToolFilter: descriptor.ToolFilter},
		signal:      options.Signal,
	})
	if err != nil {
		if options.Signal != nil && options.Signal.Err() != nil {
			return "", options.Signal.Err()
		}
		var subagentErr SubagentError
		if asSubagentError(err, &subagentErr) {
			return "", err
		}
		return "", newSubagentError(fmt.Sprintf("subagent %q is unavailable", childID), CodeNotResumable, err)
	}
	return m.submitMaterialized(activation, content, options.Source, parent, options.Signal)
}

// dispose stops one Activation immediately, then releases it child-first.
// The memoized transaction is installed before cancellation, so admission
// and reentrant teardown converge on the same owner.
func (m *SubagentContinuationManager) dispose(activation *Activation) <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disposeLocked(activation)
}

// reportFrom delivers explicitly selected content from one resident
// continuable child to its durable direct parent. Sender authorization,
// parent resolution, and send acceptance share one no-await span.
func (m *SubagentContinuationManager) ReportFrom(child *agent.Agent, content []llm.ContentBlock, options SubagentReportOptions) (llm.MessageID, error) {
	if options.Signal != nil && options.Signal.Err() != nil {
		return "", options.Signal.Err()
	}
	if err := m.assertAdmitting(child); err != nil {
		return "", err
	}
	activation, err := m.authorizeReporter(child)
	if err != nil {
		return "", err
	}
	parent, err := m.resolveReportParent(child)
	if err != nil {
		return "", err
	}
	return m.deliverReport(activation, parent, content, options.Delivery), nil
}

// authorizeReporter authorizes only the exact Agent of one resident Activation.
func (m *SubagentContinuationManager) authorizeReporter(child *agent.Agent) (*Activation, error) {
	m.mu.Lock()
	activation := m.activations[child.ID]
	m.mu.Unlock()
	if activation == nil || activation.Handle.Agent != child {
		return nil, newSubagentError(
			fmt.Sprintf("agent %q is not a live continuable subagent and cannot report", child.ID),
			CodeUnauthorized, nil)
	}
	m.mu.Lock()
	closing := activation.disposal != nil
	m.mu.Unlock()
	if closing {
		return nil, newSubagentError(
			fmt.Sprintf("subagent %q activation is being disposed; the report was not delivered", child.ID),
			CodeActivationClosing, nil)
	}
	return activation, nil
}

// resolveReportParent resolves the reporting child's live direct parent from
// durable lineage.
func (m *SubagentContinuationManager) resolveReportParent(child *agent.Agent) (*agent.Agent, error) {
	parentID := child.Session.Header().ParentSession
	var parent *agent.Agent
	if parentID != "" {
		parent = m.deps.Agents.Get(parentID)
	}
	if parent == nil {
		return nil, newSubagentError("direct parent is not live; report was not delivered", CodeParentUnavailable, nil)
	}
	return parent, nil
}

// deliverReport frames one report through the selected parent scheduling preset.
func (m *SubagentContinuationManager) deliverReport(activation *Activation, parent *agent.Agent, content []llm.ContentBlock, delivery SubagentReportDelivery) llm.MessageID {
	framed := append([]llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("Background subagent %s reported:", activation.ChildID)}}, content...)
	message := llm.NewUserMessage(framed, llm.MessageSource{
		Kind:            SourceSubagentReport,
		Form:            "relay",
		SenderSessionID: activation.ChildID,
	})
	if delivery == DeliveryNextStep {
		m.sendWaking(parent, message.ID, func() error { return m.sendReport(parent, message, delivery) })
	} else {
		_ = m.sendReport(parent, message, delivery)
	}
	return message.ID
}

// sendWaking performs one waking send to a parent, accounted against that
// parent's own Activation when it has one: registering the id before the send
// keeps a continuation-managed parent from being judged quiescent in the
// window between a waking send and the admission that observes it.
func (m *SubagentContinuationManager) sendWaking(parent *agent.Agent, messageID llm.MessageID, send func() error) {
	m.mu.Lock()
	parentActivation := m.activations[parent.ID]
	exact := parentActivation != nil && parentActivation.Handle.Agent == parent
	m.mu.Unlock()
	if exact {
		_, _ = m.admitWaking(parentActivation, messageID, send)
		return
	}
	_ = send()
}

// sendReport sends one report, translating only the parent's own rejection.
// A driver-less parent or a delivery panic cannot lose the report silently:
// the failure is logged and surfaced to the caller.
func (m *SubagentContinuationManager) sendReport(parent *agent.Agent, message llm.Message, delivery SubagentReportDelivery) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("report delivery panicked: %v", rec)
		}
		if err != nil && m.deps.Logger != nil {
			m.deps.Logger.Warn(fmt.Sprintf("subagent %q report was not delivered to its parent: %v", parent.ID, err))
		}
	}()
	driver := parent.Driver()
	if driver == nil {
		return fmt.Errorf("parent %q has no live driver", parent.ID)
	}
	if delivery == DeliveryNextStep {
		driver.Steer(message)
	} else {
		driver.Inject(message)
	}
	return nil
}

// Drain closes admission, awaits every already-admitted materialization,
// then disposes the stable live Activation forest child-first. Sibling
// branches drain independently; the aggregate rejects after all settle.
func (m *SubagentContinuationManager) Drain() error {
	m.mu.Lock()
	m.draining = true
	tracked := make([]*materialization, 0, len(m.materializations))
	for materialization := range m.materializations {
		tracked = append(tracked, materialization)
	}
	owned := map[session.SessionID]struct{}{}
	for _, activation := range m.activations {
		for child := range activation.ownedChildren {
			owned[child] = struct{}{}
		}
	}
	var roots []*Activation
	for _, activation := range m.activations {
		if _, isOwned := owned[activation.ChildID]; !isOwned {
			roots = append(roots, activation)
		}
	}
	m.mu.Unlock()
	for _, materialization := range tracked {
		<-materialization.settled
	}
	// A root is an Activation no live Activation owns: disposing roots
	// recurses child-first into the forest.
	return m.disposeRoots(roots, "activation(s)")
}

// DrainDescendants stops only the continuable descendants of exact live
// host-owned parents; unrelated trees and manager-wide admission stay live.
func (m *SubagentContinuationManager) DrainDescendants(parents []*agent.Agent) error {
	m.mu.Lock()
	roots := map[*agent.Agent]struct{}{}
	for _, parent := range parents {
		if m.deps.Agents.Get(parent.ID) == parent {
			roots[parent] = struct{}{}
		}
	}
	if len(roots) == 0 {
		m.mu.Unlock()
		return nil
	}
	// Publish the scoped admission cutoff; merge with an earlier call for the
	// same exact root so a converging drain cannot forget descendants whose
	// release is already in flight.
	var targets []*Activation
	ownedTargets := map[session.SessionID]struct{}{}
	for _, activation := range m.activations {
		var owners []*agent.Agent
		for root := range roots {
			if activation.Handle.Agent != root {
				if _, live := activation.ancestry[root]; live {
					owners = append(owners, root)
				}
			}
		}
		if len(owners) == 0 {
			continue
		}
		targets = append(targets, activation)
		for child := range activation.ownedChildren {
			ownedTargets[child] = struct{}{}
		}
		for _, owner := range owners {
			members := m.closingMembersLocked(owner)
			members[activation.Handle.Agent] = struct{}{}
			for _, ancestor := range m.liveLineage(activation.Handle.Agent) {
				members[ancestor] = struct{}{}
			}
		}
	}
	tracked := make([]*materialization, 0, len(m.materializations))
	for materialization := range m.materializations {
		relevant := false
		for _, ancestor := range materialization.lineage {
			if _, isRoot := roots[ancestor]; isRoot {
				relevant = true
				members := m.closingMembersLocked(ancestor)
				for _, member := range materialization.lineage {
					members[member] = struct{}{}
				}
			}
		}
		if relevant {
			tracked = append(tracked, materialization)
		}
	}
	var targetRoots []*Activation
	for _, activation := range targets {
		if _, isOwned := ownedTargets[activation.ChildID]; !isOwned {
			targetRoots = append(targetRoots, activation)
		}
	}
	// Open every selected transaction before the materialization barrier:
	// cancellation propagates top-down in this synchronous span while handle
	// release remains child-first.
	transactions := make([]<-chan struct{}, 0, len(targets))
	for _, activation := range targets {
		transactions = append(transactions, m.disposeLocked(activation))
	}
	m.mu.Unlock()
	for _, materialization := range tracked {
		<-materialization.settled
	}
	for _, txn := range transactions {
		<-txn
	}
	return m.disposeRoots(targetRoots, "scoped activation(s)")
}

// DrainChildren releases selected resident direct children of one exact live
// parent without closing admission for the parent's other continuable
// children.
func (m *SubagentContinuationManager) DrainChildren(parent *agent.Agent, childIDs []session.SessionID) error {
	if m.deps.Agents.Get(parent.ID) != parent {
		return newSubagentError("selected child teardown requires the exact live parent agent", CodeUnauthorized, nil)
	}
	unique := map[session.SessionID]struct{}{}
	var targets []*Activation
	m.mu.Lock()
	for _, childID := range childIDs {
		if _, seen := unique[childID]; seen {
			continue
		}
		unique[childID] = struct{}{}
		activation := m.activations[childID]
		if activation == nil {
			continue
		}
		if activation.ParentSession != parent.ID {
			m.mu.Unlock()
			return newSubagentError(
				fmt.Sprintf("subagent %q is not a direct child of agent %q", childID, parent.ID),
				CodeUnauthorized, nil)
		}
		if _, live := activation.ancestry[parent]; !live {
			m.mu.Unlock()
			return newSubagentError(
				fmt.Sprintf("subagent %q is not a direct child of agent %q", childID, parent.ID),
				CodeUnauthorized, nil)
		}
		targets = append(targets, activation)
	}
	// Open every transaction before the first await so cancellation
	// propagates across the selected roots in one synchronous span.
	transactions := make([]<-chan struct{}, 0, len(targets))
	for _, activation := range targets {
		transactions = append(transactions, m.disposeLocked(activation))
	}
	m.mu.Unlock()
	for _, txn := range transactions {
		<-txn
	}
	return m.disposeRoots(targets, "selected activation(s)")
}

// closingMembersLocked returns the retained member set for one exact root;
// caller holds m.mu.
func (m *SubagentContinuationManager) closingMembersLocked(root *agent.Agent) map[*agent.Agent]struct{} {
	members, ok := m.closingScopes[root]
	if !ok {
		members = map[*agent.Agent]struct{}{}
		m.closingScopes[root] = members
	}
	return members
}

// disposeRoots disposes independent roots and reports every branch failure
// after all settle.
func (m *SubagentContinuationManager) disposeRoots(roots []*Activation, failureSubject string) error {
	m.mu.Lock()
	transactions := make([]<-chan struct{}, 0, len(roots))
	owners := make([]*Activation, 0, len(roots))
	for _, activation := range roots {
		transactions = append(transactions, m.disposeLocked(activation))
		owners = append(owners, activation)
	}
	m.mu.Unlock()
	var reasons []string
	for i, txn := range transactions {
		<-txn
		if err := m.disposalOutcome(owners[i]); err != nil {
			reasons = append(reasons, errorChain(err))
		}
	}
	if len(reasons) > 0 {
		return newSubagentError(
			fmt.Sprintf("continuable subagent teardown failed for %d %s: %s", len(reasons), failureSubject, strings.Join(reasons, "; ")),
			CodeActivationTeardownFailed, nil)
	}
	return nil
}

// disposalOutcome reads one activation's settled disposal error.
func (m *SubagentContinuationManager) disposalOutcome(activation *Activation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return activation.dispoErr
}

// watchSettlement follows one Activation to settlement: wait for Agent
// quiescence, then for every owned child to complete disposal, and dispose
// the handle once both hold. A delivery while `waiting` wakes the same Agent
// back to `running`, so this re-observes rather than settling early.
func (m *SubagentContinuationManager) watchSettlement(activation *Activation) {
	go func() {
		for {
			m.mu.Lock()
			closing := activation.disposal != nil
			poked := activation.poke
			m.mu.Unlock()
			if closing {
				return
			}
			select {
			case <-activation.Handle.Agent.Driver().WhenIdle():
			case <-poked:
			}
			m.mu.Lock()
			closing = activation.disposal != nil
			m.mu.Unlock()
			if closing {
				return
			}
			// Re-check settlement INSIDE the child lock and begin disposal in
			// the same critical section, so a concurrent delivery either wins
			// admission before the transaction opens or waits for release and
			// cold-resumes.
			settling := false
			var txn <-chan struct{}
			_ = m.locks.Run(activation.ChildID, func() error {
				m.mu.Lock()
				closing := activation.disposal != nil
				quiet := m.stateOf(activation) == StateSettled
				if !closing && quiet {
					settling = true
					txn = m.disposeLocked(activation)
				}
				m.mu.Unlock()
				return nil
			})
			if !settling {
				continue
			}
			<-txn
			if err := m.disposalOutcome(activation); err != nil {
				if m.deps.Logger != nil {
					m.deps.Logger.Warn(fmt.Sprintf("subagent %q activation teardown failed: %s", activation.ChildID, errorChain(err)))
				}
			}
			return
		}
	}()
}

// disposeLocked stops one Activation immediately; caller holds m.mu.
func (m *SubagentContinuationManager) disposeLocked(activation *Activation) <-chan struct{} {
	if activation.disposal != nil {
		return activation.disposal
	}
	activation.disposal = make(chan struct{})
	txn := activation.disposal
	go func() {
		err := m.finishDisposal(activation)
		m.mu.Lock()
		activation.dispoErr = err
		m.mu.Unlock()
		close(txn)
	}()
	return txn
}

// finishDisposal propagates stop synchronously, then finishes the
// child-first release. The final session flush is best effort and never
// prevents handle disposal or ownership release, because retaining a child
// would permanently pin its ancestors in `waiting`.
func (m *SubagentContinuationManager) finishDisposal(activation *Activation) error {
	m.wake(activation)
	// Stop top-down before the first await: slow descendant cleanup may delay
	// release, but it cannot let this ancestor continue model or tool work.
	cancelSafely(activation.Handle.Agent, session.TurnEndCancelCause{Kind: session.CancelParent})
	idle := activation.Handle.Agent.Driver().WhenIdle()
	m.mu.Lock()
	children := make([]*Activation, 0, len(activation.ownedChildren))
	for child := range activation.ownedChildren {
		if childActivation := m.activations[child]; childActivation != nil {
			children = append(children, childActivation)
		}
	}
	childTransactions := make([]<-chan struct{}, 0, len(children))
	for _, childActivation := range children {
		childTransactions = append(childTransactions, m.disposeLocked(childActivation))
	}
	m.mu.Unlock()

	var failures []string
	// Release remains child-first even though cancellation propagated
	// top-down: every owned child completes before this handle is removed.
	for _, txn := range childTransactions {
		<-txn
	}
	for _, childActivation := range children {
		if err := m.disposalOutcome(childActivation); err != nil {
			failures = append(failures, fmt.Sprintf("subagent %q child teardown failed: %s", activation.ChildID, errorChain(err)))
		}
	}
	// Quiesce before the flush: a turn still running would keep appending
	// events the flush cannot cover.
	<-idle
	ext := m.extSnapshot()
	if ext.Snapshots != nil {
		if err := ext.Snapshots.FlushSession(activation.Handle.Agent.Session); err != nil && m.deps.Logger != nil {
			m.deps.Logger.Warn(fmt.Sprintf("subagent %q best-effort final session flush failed; the persisted state may be unavailable or stale on resume: %s", activation.ChildID, errorChain(err)))
		}
	}
	// Capture the child-dependent edge data while the child is still live:
	// handle disposal unregisters it, and consumers read its log and scope.
	activation.Observer.Capture(activation.Handle.Agent)
	if err := activation.Handle.Dispose(); err != nil {
		failures = append(failures, fmt.Sprintf("subagent %q activation handle disposal failed: %s", activation.ChildID, errorChain(err)))
	}

	var failure error
	if len(failures) == 1 {
		failure = newSubagentError(failures[0], CodeActivationTeardownFailed, nil)
	} else if len(failures) > 1 {
		failure = newSubagentError(
			fmt.Sprintf("subagent %q activation teardown failed at %d boundaries: %s", activation.ChildID, len(failures), strings.Join(failures, "; ")),
			CodeActivationTeardownFailed, errors.New(strings.Join(failures, "; ")))
	}
	// Only now is the Activation gone: keeping the entry until disposal
	// settles makes a racing delivery wait for release rather than
	// cold-resume into the still-registered agent.
	m.mu.Lock()
	delete(m.activations, activation.ChildID)
	m.mu.Unlock()
	// Deliver BEFORE releasing ownership, while the parent still counts this
	// child and therefore cannot be judged settled.
	m.notifySettlement(activation, activation.Observer.Terminal(failure))
	// Release ownership even on failure: a retained failed child would pin
	// its ancestors in `waiting` forever.
	m.releaseOwnership(activation.ChildID)
	// Emit once the disposal outcome is known, so a rejecting scoped cleanup
	// cannot be reported as a successful epoch.
	activation.Observer.Settle(failure)
	return failure
}

// cancelSafely cancels through the driver, treating a driver-less agent as
// already stopped (only possible for tests' bare handles).
func cancelSafely(a *agent.Agent, cause session.TurnEndCancelCause) {
	defer func() { _ = recover() }()
	a.Cancel(cause, agent.CancelOptions{})
}

// notifySettlement tells the durable direct parent that this child produced
// everything it is going to. Unconditional for every child the caller
// received an id for: the cases that most need it — a token ceiling, a model
// failure, cancellation, teardown — are exactly the ones where the child
// never got to choose. Never blocks disposal; a delivery failure is logged
// and dropped.
func (m *SubagentContinuationManager) notifySettlement(activation *Activation, terminal ActivationTerminal) {
	m.mu.Lock()
	announced := activation.announced
	m.mu.Unlock()
	if !announced {
		return
	}
	parent := m.deps.Agents.Get(activation.ParentSession)
	if parent == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil && m.deps.Logger != nil {
			m.deps.Logger.Warn(fmt.Sprintf("subagent %q settlement notice was not delivered to its parent: %v", activation.ChildID, err))
		}
	}()
	summary := settlementSummary(activation.ChildID, terminal.StopReason)
	closing := []llm.ContentBlock{{Type: llm.BlockText, Text: "It left no closing message."}}
	if len(terminal.Output) > 0 {
		closing = append([]llm.ContentBlock{{Type: llm.BlockText, Text: "Its closing message:"}}, terminal.Output...)
	}
	message := llm.NewUserMessage(append([]llm.ContentBlock{{Type: llm.BlockText, Text: summary}}, closing...), llm.MessageSource{
		Kind:            SourceSubagentSettled,
		Form:            "notice",
		Summary:         summary,
		SenderSessionID: activation.ChildID,
	})
	// A parent whose own teardown already began must not be woken: waking is
	// not a queue operation, and a notice arriving during teardown would
	// spend a model request on an Agent its host is about to dispose.
	// Injecting delivers to a parent still reading its inbox and records the
	// account in the log either way.
	if m.closingTeardownFor(parent) != nil {
		parent.Driver().Inject(message)
		return
	}
	// An idle parent gets one ordinary turn; a busy parent is steered instead
	// of woken: several children settling together cost one step rather than
	// one turn each.
	m.sendWaking(parent, message.ID, func() error {
		if parent.Status() == agent.AgentIdle {
			parent.Driver().Followup(message)
		} else {
			parent.Driver().Steer(message)
		}
		return nil
	})
}

// settlementSummary is one line telling a parent that a background child is
// finished and why, in the parent's own task vocabulary.
func settlementSummary(childID session.SessionID, stopReason StopReason) string {
	subject := fmt.Sprintf("Background subagent %s", childID)
	switch stopReason {
	case StopCompleted:
		return fmt.Sprintf("%s finished and will do no further work unless you send it more.", subject)
	case StopAborted:
		return fmt.Sprintf("%s was stopped before it finished.", subject)
	case StopMaxTokens:
		return fmt.Sprintf("%s ran out of room before it finished.", subject)
	case StopRefusal:
		// A pre-step rejection discarded input the child had claimed, so the
		// parent must not treat the task as done.
		return fmt.Sprintf("%s declined the task.", subject)
	case StopError:
		return fmt.Sprintf("%s failed before it finished.", subject)
	default:
		// StopReason is merge-extensible; an unnameable ending is reported as
		// unfinished rather than silently as success.
		return fmt.Sprintf("%s ended abnormally (%s) before it finished.", subject, string(stopReason))
	}
}

// satisfy the runtime's ContinuationManager contract.
var _ ContinuationManager = (*SubagentContinuationManager)(nil)

// agentFromContext resolves the agent that owns a creation-window context.
// The factory publishes the built agent into its own context so setup
// closures can reach the child's session and scope.
func agentFromContext(childCtx *cordis.Context) *agent.Agent {
	built, _ := agent.ContextService.From(childCtx)
	return built
}

// derefString reads an optional descriptor scalar; nil and empty both stay
// empty on the Go side ("" ↔ undefined).
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
