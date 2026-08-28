package subagent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// fakeDriver records cancellations; a fresh idle channel keeps WhenIdle
// pending forever.
type fakeDriver struct {
	mu       sync.Mutex
	cancels  []session.TurnEndCancelCause
	keepBox  []bool
	idle     chan struct{}
	agentRef *agent.Agent
}

func (d *fakeDriver) Cancel(cause session.TurnEndCancelCause, options agent.CancelOptions) {
	d.mu.Lock()
	d.cancels = append(d.cancels, cause)
	d.keepBox = append(d.keepBox, options.KeepInbox)
	d.mu.Unlock()
}

func (d *fakeDriver) WhenIdle() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idle == nil {
		d.idle = make(chan struct{})
	}
	return d.idle
}

func (d *fakeDriver) RunMaintenance(task func(signal context.Context) error) error {
	return nil
}

func (d *fakeDriver) Send(message llm.Message, target agent.InboxTarget, wakeup bool) {}
func (d *fakeDriver) Followup(message llm.Message)                                    {}
func (d *fakeDriver) Steer(message llm.Message)                                       {}
func (d *fakeDriver) Inject(message llm.Message)                                      {}

func newManagedAgent(t *testing.T, id string, parentID string) (*agent.Agent, *fakeDriver) {
	t.Helper()
	header := &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"}
	if parentID != "" {
		header.ParentSession = session.SessionID(parentID)
	}
	sess, err := session.NewDetached(session.SessionID(id), nil, header)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, childNoopNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	driver := &fakeDriver{agentRef: built}
	built.SetDriver(driver)
	return built, driver
}

func newManagerForTest(t *testing.T) (*SubagentContinuationManager, *agent.AgentRegistry) {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	manager := NewSubagentContinuationManager(ManagerDeps{Agents: registry})
	return manager, registry
}

func TestChildLockSerializesPerChild(t *testing.T) {
	lock := &ChildLock{}
	var mu sync.Mutex
	order := []string{}
	first := make(chan struct{})
	firstErr := errors.New("first fails")

	started := make(chan struct{})
	go func() {
		err := lock.Run("c1", func() error {
			mu.Lock()
			order = append(order, "a:start")
			mu.Unlock()
			close(started)
			<-first // hold the critical section open
			mu.Lock()
			order = append(order, "a:end")
			mu.Unlock()
			return firstErr
		})
		if !errors.Is(err, firstErr) {
			t.Errorf("first = %v", err)
		}
	}()
	<-started
	// The second caller for the same child runs only after the first settles,
	// and its own error path is independent.
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		if err := lock.Run("c1", func() error {
			mu.Lock()
			order = append(order, "b")
			mu.Unlock()
			return nil
		}); err != nil {
			t.Errorf("second = %v", err)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	bRanEarly := len(order) > 0 && order[len(order)-1] == "b"
	mu.Unlock()
	if bRanEarly {
		t.Fatal("second caller must wait for the open critical section")
	}
	close(first)
	<-secondDone
	// A different child never serializes against c1.
	if err := lock.Run("c2", func() error { return nil }); err != nil {
		t.Fatalf("c2: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(order) != "[a:start a:end b]" {
		t.Fatalf("order = %v", order)
	}
}

func TestInterruptAuthorizationMatrix(t *testing.T) {
	manager, registry := newManagerForTest(t)
	parent, _ := newManagedAgent(t, "parent", "")
	child, driver := newManagedAgent(t, "child", "parent")
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	activation := &Activation{
		ChildID:       "child",
		ParentSession: "parent",
		Provider:      "spawn",
		Handle:        agent.AgentHandle{Agent: child},
		ancestry:      map[*agent.Agent]struct{}{child: {}, parent: {}},
		ownedChildren: map[session.SessionID]struct{}{},
		accepted:      map[llm.MessageID]struct{}{},
	}
	manager.mu.Lock()
	manager.activations["child"] = activation
	manager.mu.Unlock()

	// Absent target: accepted no-op.
	if err := manager.Interrupt("ghost", SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: "parent"}); err != nil {
		t.Fatalf("absent = %v", err)
	}
	// User authority: correct durable parent address cancels with keepInbox.
	if err := manager.Interrupt("child", SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: "parent"}); err != nil {
		t.Fatalf("user interrupt = %v", err)
	}
	driver.mu.Lock()
	if len(driver.cancels) != 1 || driver.cancels[0].Kind != session.CancelUser {
		driver.mu.Unlock()
		t.Fatalf("cancels = %v", driver.cancels)
	}
	driver.mu.Unlock()
	// Wrong parent address rejects.
	if err := manager.Interrupt("child", SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: "other"}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("wrong parent = %v", err)
	}
	// Ancestor authority: live lineage member cancels with parent cause.
	if err := manager.Interrupt("child", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: parent}); err != nil {
		t.Fatalf("ancestor interrupt = %v", err)
	}
	// An ancestor outside the recorded lineage rejects.
	outsider, _ := newManagedAgent(t, "outsider", "")
	if err := manager.Interrupt("child", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: outsider}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("outsider = %v", err)
	}
	// Self-targeting rejects even with a live caller.
	if err := manager.Interrupt("parent", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: parent}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("self = %v", err)
	}
	// A stale caller (not the registry's exact entry) rejects even against an
	// absent target.
	stale, _ := newManagedAgent(t, "stale", "")
	if err := manager.Interrupt("ghost", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: stale}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("stale = %v", err)
	}
	// An open disposal transaction makes the cancel an accepted no-op.
	activation.disposal = make(chan struct{})
	close(activation.disposal)
	driver.mu.Lock()
	before := len(driver.cancels)
	driver.mu.Unlock()
	if err := manager.Interrupt("child", SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: "parent"}); err != nil {
		t.Fatalf("closing interrupt = %v", err)
	}
	driver.mu.Lock()
	after := len(driver.cancels)
	driver.mu.Unlock()
	if before != after {
		t.Fatal("a closing Activation must not be cancelled again")
	}
}

func TestStateOfResidencyDerivation(t *testing.T) {
	manager, _ := newManagerForTest(t)
	child, _ := newManagedAgent(t, "child", "")
	activation := &Activation{
		ChildID:       "child",
		ParentSession: "parent",
		Provider:      "spawn",
		Handle:        agent.AgentHandle{Agent: child},
		ancestry:      map[*agent.Agent]struct{}{},
		ownedChildren: map[session.SessionID]struct{}{},
		accepted:      map[llm.MessageID]struct{}{},
	}
	// Idle with no owned children: settled.
	if got := manager.stateOf(activation); got != StateSettled {
		t.Fatalf("quiet = %s, want settled", got)
	}
	// Accepted waking work keeps it running even while the agent is idle.
	activation.accepted["m1"] = struct{}{}
	if got := manager.stateOf(activation); got != StateRunning {
		t.Fatalf("accepted = %s, want running", got)
	}
	delete(activation.accepted, "m1")
	// Undisposed owned children block settlement.
	activation.ownedChildren["grandchild"] = struct{}{}
	if got := manager.stateOf(activation); got != StateWaiting {
		t.Fatalf("owned = %s, want waiting", got)
	}
}

func TestAuthorizeLineageAndAdmission(t *testing.T) {
	manager, registry := newManagerForTest(t)
	parent, _ := newManagedAgent(t, "parent", "")
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter: %v", err)
	}
	if err := manager.authorizeLineage(parent, "child", "parent"); err != nil {
		t.Fatalf("live parent = %v", err)
	}
	// Wrong lineage id rejects.
	if err := manager.authorizeLineage(parent, "child", "other"); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("other lineage = %v", err)
	}
	// A stale caller rejects.
	stale, _ := newManagedAgent(t, "parent", "")
	if err := manager.authorizeLineage(stale, "child", "parent"); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("stale parent = %v", err)
	}
	// liveLineage follows exact registry entries upward.
	grand, _ := newManagedAgent(t, "grand", "")
	if _, err := registry.Enter(grand, nil); err != nil {
		t.Fatalf("enter grand: %v", err)
	}
	mid, _ := newManagedAgent(t, "mid", "grand")
	if _, err := registry.Enter(mid, nil); err != nil {
		t.Fatalf("enter mid: %v", err)
	}
	lineage := manager.liveLineage(mid)
	if len(lineage) != 2 || lineage[0] != mid || lineage[1] != grand {
		t.Fatalf("lineage = %v", lineage)
	}
	// Scoped drain closes admission below the root.
	manager.mu.Lock()
	manager.closingScopes[grand] = map[*agent.Agent]struct{}{}
	manager.mu.Unlock()
	if err := manager.assertAdmitting(mid); err == nil || asCode(err) != CodeDraining {
		t.Fatalf("scoped drain = %v", err)
	}
	// The root itself is inside its own closing scope (the scoped drain adds
	// it to its own member set), so its admission closes too.
	if err := manager.assertAdmitting(grand); err == nil || asCode(err) != CodeDraining {
		t.Fatalf("root admission = %v, want DRAINING", err)
	}
	// Manager-wide drain closes everything.
	manager.mu.Lock()
	manager.draining = true
	manager.mu.Unlock()
	if err := manager.assertAdmitting(grand); err == nil || asCode(err) != CodeDraining {
		t.Fatalf("manager drain = %v", err)
	}
}
