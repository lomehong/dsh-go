package subagent

import (
	"errors"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
)

// fakeChildRuntime records create/resume calls and hands back handles over
// registry agents with parked drivers; Dispose unregisters like the real
// handle does.
type fakeChildRuntime struct {
	creates   int
	ids       []session.SessionID
	agents    *agent.AgentRegistry
	detachers []func()
}

func (f *fakeChildRuntime) CreateAgent(owner *cordis.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	f.creates++
	f.ids = append(f.ids, options.SessionID)
	built, _ := newManagedAgent(testingT, string(options.SessionID), string(options.Meta.ParentSession))
	if f.agents != nil {
		detach, err := f.agents.Enter(built, nil)
		if err != nil {
			return agent.AgentHandle{}, err
		}
		f.detachers = append(f.detachers, detach)
	}
	applyChildSeed(built, options.Seed)
	return agent.AgentHandle{Agent: built, Dispose: func() error {
		if n := len(f.detachers); n > 0 {
			f.detachers[n-1]()
			f.detachers = f.detachers[:n-1]
		}
		return nil
	}}, nil
}

func (f *fakeChildRuntime) Resume(owner *cordis.Context, options agent.ResumeAgentOptions) (agent.AgentHandle, error) {
	return agent.AgentHandle{}, errors.New("resume not expected in this test")
}

// applyChildSeed replays a creation seed through the child session so the
// descriptor turn lands in the log the way the real factory does.
func applyChildSeed(built *agent.Agent, seed []session.Event) {
	for _, event := range seed {
		_, _ = built.Session.Append(event.Type, event.Data, nil)
	}
}

// managerHost adapts the runtime to the manager's host seam.
type managerHost struct{ runtime *SubagentRuntime }

func (h managerHost) PrepareContinuable(name string, request ContinuableCreateRequest) (ContinuableCreateSpec, error) {
	return h.runtime.PrepareContinuable(name, request)
}

func (h managerHost) ObserveActivation(provider string, childID session.SessionID, parent *agent.Agent) *ActivationObserver {
	return h.runtime.ObserveActivation(provider, childID, parent)
}

// testingT carries the live *testing.T into the factory fake (the factory
// signature has no error channel for test plumbing).
var testingT *testing.T

// stubSnapshots satisfies the persistence seam with a configurable store.
type stubSnapshots struct {
	headers  []session.SessionHeader
	failList error
}

func (s *stubSnapshots) ListSnapshots() ([]persistence.Snapshot, error) {
	if s.failList != nil {
		return nil, s.failList
	}
	snapshots := make([]persistence.Snapshot, 0, len(s.headers))
	for _, header := range s.headers {
		snapshots = append(snapshots, persistence.Snapshot{Header: header})
	}
	return snapshots, nil
}
func (s *stubSnapshots) FlushSession(sess *session.Session) error { return nil }

func TestStartContinuableHappyPath(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	provider := &fakeProvider{name: "spawn", continuable: true}
	if _, err := runtime.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{
		Host:        managerHost{runtime},
		Snapshots:   &stubSnapshots{},
		HasApproval: true,
	})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Explore",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go look"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if start.ChildID == "" || start.MessageID == "" {
		t.Fatalf("start identities = %+v", start)
	}
}

func TestFollowupRoutesAndDrainReleasesForest(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-2", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	factory := &fakeChildRuntime{agents: registry}
	manager.SetChildRuntime(factory, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Explore",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "first"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.mu.Lock()
	activation := manager.activations[start.ChildID]
	materializations := len(manager.materializations)
	manager.mu.Unlock()
	if activation == nil {
		t.Fatal("start must leave a resident Activation")
	}
	if materializations != 0 {
		t.Fatal("a settled materialization must leave the barrier set")
	}
	driver := activation.Handle.Agent.Driver().(*fakeDriver)
	// Followup routes to the resident Activation as its next FIFO turn.
	if _, err := manager.Followup(parent, start.ChildID, []llm.ContentBlock{{Type: llm.BlockText, Text: "second"}}, SubagentFollowupOptions{}); err != nil {
		t.Fatalf("followup: %v", err)
	}
	// The initial prompt and the followup both rode the driver's FIFO.
	if len(driver.followups) != 2 {
		t.Fatalf("child followups = %d", len(driver.followups))
	}
	// Release the child and drain the forest: the root Activation releases
	// child-first and leaves the registry.
	driver.closeIdle()
	if err := manager.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	manager.mu.Lock()
	remaining := len(manager.activations)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("activations = %d after drain", remaining)
	}
	if len(driver.cancels) == 0 || driver.cancels[0].Kind != session.CancelParent {
		t.Fatalf("disposal cancels = %v", driver.cancels)
	}
	// A whole-manager drain closes admission for good: the same-id delivery
	// is rejected with DRAINING (NOT_RESUMABLE is only reachable when
	// admission is still open).
	if _, err := manager.Followup(parent, start.ChildID, nil, SubagentFollowupOptions{}); err == nil ||
		asCode(err) != CodeDraining {
		t.Fatalf("post-drain followup = %v, want DRAINING", err)
	}
}

func TestDrainChildrenReleasesSelectedDirectChild(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-3", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Scoped",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.mu.Lock()
	driver := manager.activations[start.ChildID].Handle.Agent.Driver().(*fakeDriver)
	manager.mu.Unlock()
	// An unknown id is an accepted no-op; a live parent must authorize the
	// selection.
	if err := manager.DrainChildren(parent, []session.SessionID{"ghost"}); err != nil {
		t.Fatalf("ghost release = %v", err)
	}
	// While the child is live, re-using its id rejects DUPLICATE_CHILD (the
	// live-registry leg of the three-way id check).
	if _, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Again",
		ChildID:  start.ChildID,
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "again"}},
			Parent: parent,
		},
	}); err == nil || asCode(err) != CodeDuplicateChild {
		t.Fatalf("live same-id start = %v, want DUPLICATE_CHILD", err)
	}
	driver.closeIdle()
	if err := manager.DrainChildren(parent, []session.SessionID{start.ChildID}); err != nil {
		t.Fatalf("release: %v", err)
	}
	manager.mu.Lock()
	remaining := len(manager.activations)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("activations = %d after scoped release", remaining)
	}
	// After the release the id checks clear (empty persistence seam in this
	// fake): a fresh epoch with the same id starts cleanly.
	if _, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Again",
		ChildID:  start.ChildID,
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "again"}},
			Parent: parent,
		},
	}); err != nil {
		t.Fatalf("post-release same-id start = %v", err)
	}
}

func TestNotifySettlementGatingAndFraming(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-5", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Notify",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	parentDriver := parent.Driver().(*fakeDriver)
	manager.mu.Lock()
	activation := manager.activations[start.ChildID]
	manager.mu.Unlock()
	// A rollback child (never accepted a send) owes the parent no account:
	// announce-gating is per-Activation, so exercise it on a synthetic
	// unannounced one.
	child, _ := newManagedAgent(t, "ghost-child", "delegator-5")
	ghost := &Activation{ChildID: child.ID, ParentSession: "delegator-5", Handle: agent.AgentHandle{Agent: child}}
	manager.notifySettlement(ghost, ActivationTerminal{StopReason: StopError})
	if len(parentDriver.followups) != 0 {
		t.Fatal("unannounced settlement must stay silent")
	}
	// Announced + completed: the parent gets one summary framing the child's
	// closing message.
	manager.mu.Lock()
	activation.announced = true
	manager.mu.Unlock()
	manager.notifySettlement(activation, ActivationTerminal{
		StopReason: StopCompleted,
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "found it"}},
	})
	if len(parentDriver.followups) != 1 {
		t.Fatalf("settlement deliveries = %d, want 1", len(parentDriver.followups))
	}
	text := parentDriver.followups[0].Content[0].Text
	if !strings.Contains(text, "finished") || strings.Contains(text, "failed") {
		t.Fatalf("completed summary = %q", text)
	}
	last := parentDriver.followups[0].Content
	if last[len(last)-1].Text != "found it" {
		t.Fatal("closing message must ride the settlement notice")
	}
	// Failure wording flips.
	manager.notifySettlement(activation, ActivationTerminal{StopReason: StopMaxTokens})
	text = parentDriver.followups[1].Content[0].Text
	if !strings.Contains(text, "ran out of room") {
		t.Fatalf("max-tokens summary = %q", text)
	}
}

func TestSettlementSummaryWordingTable(t *testing.T) {
	id := session.SessionID("kid")
	cases := map[StopReason]string{
		StopAborted:           "was stopped before it finished",
		StopRefusal:           "declined the task",
		StopError:             "failed before it finished",
		StopReason("mystery"): "ended abnormally (mystery) before it finished",
	}
	for reason, want := range cases {
		if got := settlementSummary(id, reason); !strings.Contains(got, want) {
			t.Fatalf("summary(%s) = %q, want substring %q", reason, got, want)
		}
	}
	if got := settlementSummary(id, StopCompleted); !strings.Contains(got, "finished and will do no further work") {
		t.Fatalf("completed summary = %q", got)
	}
}

func TestFollowupWaitingRoutesThroughWaking(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-10", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Waiting",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// A child that owns a child is `waiting`, not `running`: its followup
	// must ride the waking account (accepted-before-send), not the FIFO.
	manager.mu.Lock()
	activation := manager.activations[start.ChildID]
	// The parked fake driver never claims its inbox, so clear the start
	// message's account to reach the waiting state honestly.
	activation.accepted = map[llm.MessageID]struct{}{}
	activation.ownedChildren["phantom-grandchild"] = struct{}{}
	manager.mu.Unlock()
	if manager.stateOf(activation) != StateWaiting {
		t.Fatal("owning child must read waiting")
	}
	id, err := manager.Followup(parent, start.ChildID, []llm.ContentBlock{{Type: llm.BlockText, Text: "wake"}}, SubagentFollowupOptions{})
	if err != nil {
		t.Fatalf("waiting followup: %v", err)
	}
	manager.mu.Lock()
	_, accepted := activation.accepted[id]
	manager.mu.Unlock()
	if !accepted {
		t.Fatal("waking followup must account the id before release")
	}
}

func TestExplicitIDDurabilityGateFailLoud(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-11", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	failure := errors.New("disk offline")
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{failList: failure}, HasApproval: true})
	runtime.SetContinuations(manager)

	// R9: an explicit id consults the persisted leg; a storage failure there
	// rejects the start instead of silently risking a duplicate creation.
	_, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Chosen",
		ChildID:  "chosen-id",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "disk offline") {
		t.Fatalf("explicit-id start with failing store = %v, want storage error", err)
	}
	// A minted id never consults the persisted leg: the same failing store
	// cannot break ordinary starts.
	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Minted",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("minted-id start with failing store = %v", err)
	}
	if start.ChildID == "" {
		t.Fatal("minted id must be non-empty")
	}
}

func TestInterruptAuthorizationAndDelivery(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-6", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	stale, _ := newManagedAgent(t, "delegator-6", "")
	// A stale caller is rejected even with an absent target (no liveness
	// oracle from the error shape).
	if err := manager.Interrupt("absent", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: stale}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("stale ancestor interrupt = %v, want UNAUTHORIZED", err)
	}
	// The exact live ancestor may probe absent ids harmlessly.
	if err := manager.Interrupt("absent", SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: parent}); err != nil {
		t.Fatalf("ancestor probe of absent id = %v", err)
	}
	// Self-interrupt is always rejected.
	if err := manager.Interrupt(parent.ID, SubagentInterruptAuthority{Kind: InterruptAuthorityAncestor, Agent: parent}); err == nil {
		t.Fatal("self interrupt must fail")
	}

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Interrupted",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.mu.Lock()
	driver := manager.activations[start.ChildID].Handle.Agent.Driver().(*fakeDriver)
	manager.mu.Unlock()
	// A user authority must match the child's durable parent session.
	if err := manager.Interrupt(start.ChildID, SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: "someone-else"}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("mismatched user interrupt = %v, want UNAUTHORIZED", err)
	}
	// The correct user authority interrupts: cancel(user) with KeepInbox so
	// the child still flushes its inbox state.
	if err := manager.Interrupt(start.ChildID, SubagentInterruptAuthority{Kind: InterruptAuthorityUser, ParentSessionID: parent.ID}); err != nil {
		t.Fatalf("user interrupt: %v", err)
	}
	driver.mu.Lock()
	cancels := append([]session.TurnEndCancelCause(nil), driver.cancels...)
	keep := append([]bool(nil), driver.keepBox...)
	driver.mu.Unlock()
	if len(cancels) == 0 || cancels[0].Kind != session.CancelUser {
		t.Fatalf("interrupt cancels = %v", cancels)
	}
	if len(keep) == 0 || !keep[0] {
		t.Fatal("interrupt must keep the inbox")
	}
}

func TestReportFromAuthorizationAndRouting(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-7", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	outsider, _ := newManagedAgent(t, "outsider-reporter", "")
	if _, err := registry.Enter(outsider, nil); err != nil {
		t.Fatalf("enter outsider: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	// Only the exact Agent of a resident Activation may report.
	if _, err := manager.ReportFrom(outsider, []llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}, SubagentReportOptions{}); err == nil ||
		asCode(err) != CodeUnauthorized {
		t.Fatalf("outsider report = %v, want UNAUTHORIZED", err)
	}

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Reporter",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.mu.Lock()
	child := manager.activations[start.ChildID].Handle.Agent
	manager.mu.Unlock()
	parentDriver := parent.Driver().(*fakeDriver)

	// Quiet reports inject into the parent's current turn.
	if _, err := manager.ReportFrom(child, []llm.ContentBlock{{Type: llm.BlockText, Text: "found"}}, SubagentReportOptions{Delivery: DeliveryQuiet}); err != nil {
		t.Fatalf("quiet report: %v", err)
	}
	if len(parentDriver.injects) != 1 {
		t.Fatalf("quiet deliveries = %d, want 1", len(parentDriver.injects))
	}
	// Next-step reports steer the parent's next step (mergeable with other
	// children's reports).
	if _, err := manager.ReportFrom(child, []llm.ContentBlock{{Type: llm.BlockText, Text: "more"}}, SubagentReportOptions{Delivery: DeliveryNextStep}); err != nil {
		t.Fatalf("next-step report: %v", err)
	}
	if len(parentDriver.steers) != 1 {
		t.Fatalf("next-step deliveries = %d, want 1", len(parentDriver.steers))
	}
	// Both report messages carry the reporting child as sender.
	if parentDriver.injects[0].Source.SenderSessionID != start.ChildID {
		t.Fatalf("quiet sender = %q", parentDriver.injects[0].Source.SenderSessionID)
	}
}

func TestPersistedChildIDAndResumeGate(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-8", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	snapshots := &stubSnapshots{headers: []session.SessionHeader{{ID: "durable-child", Version: 1}}}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: snapshots, HasApproval: true})
	runtime.SetContinuations(manager)

	// The persisted leg of the id check rejects a restart of a durable child
	// that is neither live nor resident.
	if _, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Dup",
		ChildID:  "durable-child",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	}); err == nil || asCode(err) != CodeDuplicateChild {
		t.Fatalf("persisted-id start = %v, want DUPLICATE_CHILD", err)
	}
	// Without a session-query service the absent-id followup fails loud
	// instead of guessing resumability (verbatim official message).
	_, err := manager.Followup(parent, "unknown-child", []llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}, SubagentFollowupOptions{})
	if err == nil || asCode(err) != CodeContinuationUnavailable {
		t.Fatalf("resume without query = %v, want CONTINUATION_UNAVAILABLE", err)
	}
}

func TestStartAfterWholeManagerDrainIsDraining(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-9", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "First",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	manager.mu.Lock()
	manager.activations[start.ChildID].Handle.Agent.Driver().(*fakeDriver).closeIdle()
	manager.mu.Unlock()
	if err := manager.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// A brand-new start after the whole-manager drain hits the admission
	// gate, not the id check.
	if _, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Second",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	}); err == nil || asCode(err) != CodeDraining {
		t.Fatalf("post-drain start = %v, want DRAINING", err)
	}
}

func TestDrainDescendantsStopsScopedForest(t *testing.T) {
	testingT = t
	parent, _ := newManagedAgent(t, "delegator-4", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	// An unrelated tree stays live.
	outsider, _ := newManagedAgent(t, "outsider", "")
	if _, err := registry.Enter(outsider, nil); err != nil {
		t.Fatalf("enter outsider: %v", err)
	}
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn", continuable: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: runtime.SetupRegistry()})
	manager.SetChildRuntime(&fakeChildRuntime{}, cordis.NewRoot(cordis.Discard{}))
	manager.SetManagerExt(ManagerExt{Host: managerHost{runtime}, Snapshots: &stubSnapshots{}, HasApproval: true})
	runtime.SetContinuations(manager)

	start, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Scoped forest",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
			Parent: parent,
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// A stale root is skipped silently (best-effort cleanup API, unlike the
	// loud per-child Interrupt authorization).
	stale, _ := newManagedAgent(t, "delegator-4", "")
	if err := manager.DrainDescendants([]*agent.Agent{stale}); err != nil {
		t.Fatalf("stale scoped drain = %v, want silent skip", err)
	}
	manager.mu.Lock()
	driver := manager.activations[start.ChildID].Handle.Agent.Driver().(*fakeDriver)
	manager.mu.Unlock()
	driver.closeIdle()
	// The exact live root scopes the stop; the outsider tree is untouched.
	if err := manager.DrainDescendants([]*agent.Agent{parent}); err != nil {
		t.Fatalf("scoped drain: %v", err)
	}
	manager.mu.Lock()
	remaining := len(manager.activations)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("activations = %d after scoped drain", remaining)
	}
	// Admission stays open manager-wide after a SCOPED drain: the outsider
	// tree can still start.
	if _, err := manager.StartContinuable(ContinuableStartSpec{
		Provider: "spawn",
		Label:    "Outsider child",
		Request: ContinuableDelegationRequest{
			Prompt: []llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}},
			Parent: outsider,
		},
	}); err != nil {
		t.Fatalf("unrelated tree must stay admissible: %v", err)
	}
}
