package subagent

import (
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// ActivationState is the residency state of one continuable child, derived
// from Agent quiescence and the owned-child set rather than a second state
// machine.
type ActivationState string

// Residency states.
const (
	// StateRunning: the Agent has an active admission or turn, or waking
	// inbox work.
	StateRunning ActivationState = "running"
	// StateWaiting: the Agent is quiescent but still owns undisposed
	// children.
	StateWaiting ActivationState = "waiting"
	// StateSettled: quiescent with every owned child disposed, so the
	// manager disposes the handle and removes the Activation.
	StateSettled ActivationState = "settled"
)

// Activation is one residency epoch for a reconstructed continuable child
// Agent. It directly owns the published handle; the manager's private
// activation-owner scope is its structural owner.
type Activation struct {
	// ChildID is the durable child this Activation is an epoch of.
	ChildID session.SessionID
	// ParentSession is the durable direct parent, stored because settlement
	// delivery must resolve that parent after the child handle is gone.
	// Live ancestry cannot answer it: the child's own header is only
	// reachable through a handle disposal has already released.
	ParentSession session.SessionID
	// Provider is the provider name recorded in the durable descriptor.
	Provider string
	// Handle is the retained live Agent handle, disposed exactly once at
	// settlement.
	Handle agent.AgentHandle
	// ancestry is the exact live Agent ancestry observed when this
	// Activation materialized. Go adaptation: pointer-keyed membership
	// replaces the WeakSet — it retains the ancestors' runtime objects where
	// the official version does not; acceptable because dsh-go Agents are
	// explicit composition objects, and noted in the README decision record.
	ancestry map[*agent.Agent]struct{}
	// ownedChildren holds session ids of the child Activations this one
	// owns. Because one Session has at most one live Activation, the id
	// identifies the live child without another runtime-incarnation
	// reference. Non-empty blocks settlement.
	ownedChildren map[session.SessionID]struct{}
	// Observer emits this epoch's start and terminal edges.
	Observer *ActivationObserver
	// disposal is the memoized disposal transaction; closed while resident.
	// Presence IS the admission cutoff: it is assigned synchronously when
	// disposal begins, so no delivery can join a handle being torn down, and
	// a racing delivery awaits it before cold-resuming a new Activation.
	// dispoErr carries the transaction's outcome.
	disposal chan struct{}
	dispoErr error
	// accepted holds accepted waking message ids this manager has not yet
	// seen leave the inbox. Agent.Status() is still idle in the window
	// between a waking send and the admission that observes it, so
	// settlement must not treat that gap as quiet.
	accepted map[llm.MessageID]struct{}
	// announced records whether any delivery to this child was ever
	// accepted. A materialization rolled back before its first acceptance is
	// a child the caller was told does not exist, so its teardown owes the
	// parent no settlement account.
	announced bool
	// poke wakes the settlement watcher after ownership or inbox changes.
	poke   chan struct{}
	pokeMu sync.Mutex
}

// wakeLocked renews the poke signal; callers hold the manager mutex.
func (a *Activation) wakeLocked() {
	a.pokeMu.Lock()
	defer a.pokeMu.Unlock()
	if a.poke != nil {
		select {
		case <-a.poke:
		default:
			close(a.poke)
		}
	}
	a.poke = make(chan struct{})
}

// disposalOf reads one Activation's current disposal transaction. The
// indirection exists because a long-lived closure must re-read the mutable
// field instead of caching a snapshot.
func disposalOf(activation *Activation) (<-chan struct{}, bool) {
	if activation.disposal == nil {
		return nil, false
	}
	return activation.disposal, true
}

// ChildLock serializes each durable child's delivery, release, and disposal.
// The official promise-chain tail becomes a channel chain: each critical
// section waits for its predecessor to settle (regardless of outcome) before
// running, and one failed section never rejects a later caller.
type ChildLock struct {
	mu    sync.Mutex
	tails map[session.SessionID]*childLink
}

type childLink struct {
	prev *childLink
	done chan struct{}
}

// Run executes operation after every previously queued operation for
// childId.
func (l *ChildLock) Run(childID session.SessionID, operation func() error) error {
	l.mu.Lock()
	if l.tails == nil {
		l.tails = map[session.SessionID]*childLink{}
	}
	prev := l.tails[childID]
	link := &childLink{prev: prev, done: make(chan struct{})}
	l.tails[childID] = link
	l.mu.Unlock()

	// Wait for the predecessor's critical section to settle, whatever its
	// outcome; its error belongs to its own caller.
	if prev != nil {
		<-prev.done
	}
	err := operation()
	close(link.done)

	// The entry lives only while the tail is in flight; an idle child's map
	// slot is reclaimed.
	l.mu.Lock()
	if l.tails[childID] == link {
		delete(l.tails, childID)
	}
	l.mu.Unlock()
	return err
}

// Additional machine-readable codes the continuation seam rejects with.
const (
	CodeDuplicateChild           = "DUPLICATE_CHILD"
	CodeUnauthorized             = "UNAUTHORIZED"
	CodeActivationClosing        = "ACTIVATION_CLOSING"
	CodeParentUnavailable        = "PARENT_UNAVAILABLE"
	CodeDraining                 = "DRAINING"
	CodeNotResumable             = "NOT_RESUMABLE"
	CodePersistenceUnavailable   = "PERSISTENCE_UNAVAILABLE"
	CodeActivationTeardownFailed = "ACTIVATION_TEARDOWN_FAILED"
	// CodeModelDoesNotSupportImages refuses image follow-up content to a
	// child whose resolved model accepts text only (alpha.3, upstream
	// MODEL_DOES_NOT_SUPPORT_IMAGES).
	CodeModelDoesNotSupportImages = "MODEL_DOES_NOT_SUPPORT_IMAGES"
)

// ChildRuntime is the activation-owner's create/resume seam (the official
// `ownerCtx.agents`). *agentloop.AgentLoop satisfies it; tests supply fakes.
type ChildRuntime interface {
	CreateAgent(owner *cordis.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error)
	Resume(owner *cordis.Context, options agent.ResumeAgentOptions) (agent.AgentHandle, error)
}

// ManagerDeps wire the manager to its composition. Go adaptation: the
// official `ctx.inject` web becomes explicit fields; the hooks the manager
// itself needs are split from the host services the materializer uses.
type ManagerDeps struct {
	// Logger receives settlement and teardown warnings; nil discards.
	Logger cordis.Logger
	// Agents resolves live agents for authorization and lineage; required.
	Agents *agent.AgentRegistry
	// Children builds child agents inside the private activation-owner
	// scope; required before any materialization.
	Children ChildRuntime
	// OwnerCtx is the structural owner context every Activation handle is
	// created under; required before any materialization.
	OwnerCtx *cordis.Context
	// Setup owns continuable-child setup registrations; required.
	Setup *ActivationSetupRegistry
}

// SubagentContinuationManager is the continuable-subagent orchestration
// behind the runtime's continuable operations. Tool schema and host adapters
// are consumers of this one contract; foreground one-shot delegation keeps
// calling Start and never enters this lifecycle.
type SubagentContinuationManager struct {
	deps ManagerDeps
	// ext carries the host services materialization and teardown consume.
	ext ManagerExt

	mu sync.Mutex
	// activations: child session id → its live Activation.
	// Process-local, never durable.
	activations map[session.SessionID]*Activation
	// materializations: admitted before drain, tracked through publication
	// or rollback.
	materializations map[*materialization]struct{}
	// closingScopes: exact roots whose host teardown has begun, with the
	// live lineage members observed under each root. Entries remain until
	// that exact root leaves the Agent registry, closing admission
	// throughout its host's teardown without poisoning a later same-id
	// replacement.
	closingScopes map[*agent.Agent]map[*agent.Agent]struct{}
	// draining closes manager-wide admission.
	draining bool

	locks ChildLock
}

// NewSubagentContinuationManager builds the manager.
func NewSubagentContinuationManager(deps ManagerDeps) *SubagentContinuationManager {
	if deps.Setup == nil {
		deps.Setup = NewActivationSetupRegistry()
	}
	return &SubagentContinuationManager{
		deps:             deps,
		activations:      map[session.SessionID]*Activation{},
		materializations: map[*materialization]struct{}{},
		closingScopes:    map[*agent.Agent]map[*agent.Agent]struct{}{},
	}
}

// SetChildRuntime installs the child factory (the official owner-scope
// agents service) once its composition completes.
func (m *SubagentContinuationManager) SetChildRuntime(children ChildRuntime, owner *cordis.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps.Children = children
	m.deps.OwnerCtx = owner
}

// stateOf derives residency from Agent quiescence and the owned-child set.
// Status alone is insufficient: it stays idle between an accepted waking
// send and the admission that observes it, so a synchronous observer would
// see settled while a turn is already queued — `accepted` holds the ids this
// manager admitted but has not yet seen drained.
func (m *SubagentContinuationManager) stateOf(activation *Activation) ActivationState {
	if activation.Handle.Agent.Status() != agent.AgentIdle || len(activation.accepted) > 0 {
		return StateRunning
	}
	if len(activation.ownedChildren) > 0 {
		return StateWaiting
	}
	return StateSettled
}

// liveLineage returns the exact currently resolvable ancestry from agent
// upward. The first element is always the supplied identity, even when it is
// already stale; each ancestor after it must be the registry's current exact
// entry.
func (m *SubagentContinuationManager) liveLineage(a *agent.Agent) []*agent.Agent {
	lineage := []*agent.Agent{a}
	seen := map[session.SessionID]struct{}{a.ID: {}}
	parentSession := a.Session.Header().ParentSession
	for parentSession != "" {
		parent := m.deps.Agents.Get(parentSession)
		if parent == nil {
			break
		}
		if _, cycle := seen[parent.ID]; cycle {
			break
		}
		lineage = append(lineage, parent)
		seen[parent.ID] = struct{}{}
		parentSession = parent.Session.Header().ParentSession
	}
	return lineage
}

// closingTeardownFor names the teardown that closed continuable admission
// for this agent's lineage: nil while admission is open, managerDraining for
// a whole-manager drain, or the exact scoped root whose forest is closing.
var managerDraining = &agent.Agent{}

func (m *SubagentContinuationManager) closingTeardownFor(a *agent.Agent) *agent.Agent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closingTeardownForLocked(a)
}

func (m *SubagentContinuationManager) closingTeardownForLocked(a *agent.Agent) *agent.Agent {
	if m.draining {
		return managerDraining
	}
	lineage := m.liveLineage(a)
	for root, members := range m.closingScopes {
		if _, member := members[a]; member {
			return root
		}
		for _, ancestor := range lineage {
			if ancestor == root {
				return root
			}
		}
	}
	return nil
}

// assertAdmitting rejects new admission once the manager or this exact
// parent tree began draining.
func (m *SubagentContinuationManager) assertAdmitting(a *agent.Agent) error {
	if closing := m.closingTeardownForLocked(a); closing != nil {
		if closing == managerDraining {
			return newSubagentError(
				"continuable subagents are draining; the operation was not admitted",
				CodeDraining, nil)
		}
		return newSubagentError(
			fmt.Sprintf("continuable subagents below parent %q are draining; the operation was not admitted", closing.ID),
			CodeDraining, nil)
	}
	return nil
}

// authorizeLineage authorizes one operation against the durable
// direct-parent lineage. Other agents, ancestors, teams, workflows, and
// hosts remain rejected until an explicit authority protocol has a
// production consumer.
func (m *SubagentContinuationManager) authorizeLineage(parent *agent.Agent, childID session.SessionID, parentSession session.SessionID) error {
	if m.deps.Agents.Get(parent.ID) != parent {
		return newSubagentError(
			fmt.Sprintf("subagent %q delivery requires the exact live parent agent", childID),
			CodeUnauthorized, nil)
	}
	if parentSession != parent.ID {
		return newSubagentError(
			fmt.Sprintf("subagent %q belongs to another parent session", childID),
			CodeUnauthorized, nil)
	}
	return nil
}

// Interrupt one live continuable child's current turn. Admission is
// synchronous and the effect is asynchronous: this authorizes the caller,
// requests Cancel(cause, KeepInbox) on the target, and returns without
// waiting for the target to observe the signal or reach quiescence. The
// Activation, its handle, accepted unclaimed inbox work, and
// already-published descendants are untouched; work already claimed into the
// interrupted turn is not requeued.
//
// An absent target is an accepted no-op, which uniformly covers natural
// completion races, repeated requests, one-shot ids, and unknown ids without
// consulting the durable catalog. A target whose disposal transaction is
// already open is likewise an accepted no-op after authorization.
func (m *SubagentContinuationManager) Interrupt(targetSessionID session.SessionID, authority SubagentInterruptAuthority) error {
	if authority.Kind == InterruptAuthorityAncestor {
		caller := authority.Agent
		// A stale caller is rejected even when the target is absent, so a
		// replaced same-id Agent can never probe this manager's state.
		if m.deps.Agents.Get(caller.ID) != caller {
			return newSubagentError(
				fmt.Sprintf("interrupting %q requires the exact live ancestor agent", targetSessionID),
				CodeUnauthorized, nil)
		}
		if caller.ID == targetSessionID {
			return newSubagentError(
				fmt.Sprintf("agent %q cannot interrupt itself", caller.ID),
				CodeUnauthorized, nil)
		}
	}
	m.mu.Lock()
	activation := m.activations[targetSessionID]
	m.mu.Unlock()
	if activation == nil {
		return nil
	}
	if authority.Kind == InterruptAuthorityUser {
		if activation.Handle.Agent.Session.Header().ParentSession != authority.ParentSessionID {
			return newSubagentError(
				fmt.Sprintf("subagent %q belongs to another parent session", targetSessionID),
				CodeUnauthorized, nil)
		}
	} else if _, live := activation.ancestry[authority.Agent]; !live {
		return newSubagentError(
			fmt.Sprintf("subagent %q is not a live descendant of agent %q", targetSessionID, authority.Agent.ID),
			CodeUnauthorized, nil)
	}
	// Disposal already stopped the target with a whole-Activation teardown;
	// a second cancel would be a redundant signal on a closing handle.
	if _, closing := disposalOf(activation); closing {
		return nil
	}
	cause := session.TurnEndCancelCause{Kind: session.CancelParent}
	if authority.Kind == InterruptAuthorityUser {
		cause.Kind = session.CancelUser
	}
	activation.Handle.Agent.Cancel(cause, agent.CancelOptions{KeepInbox: true})
	return nil
}
