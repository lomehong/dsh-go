package toolsjobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
)

// recorderDriver stands in for the loop-owned driver: Followup and Inject
// are recorded, everything else is inert. A notify channel wakes waiters on
// every recorded notice, so tests converge on delivery events instead of
// polling a fixed interval (load-insensitive).
type recorderDriver struct {
	mu       sync.Mutex
	followup []llm.Message
	inject   []llm.Message
	notify   chan struct{}
}

func newRecorderDriver() *recorderDriver {
	return &recorderDriver{notify: make(chan struct{}, 1)}
}

// signal wakes one waiter; callers hold r.mu.
func (r *recorderDriver) signal() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

func (r *recorderDriver) Cancel(cause session.TurnEndCancelCause, options agent.CancelOptions) {}
func (r *recorderDriver) WhenIdle() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (r *recorderDriver) RunMaintenance(task func(context.Context) error) error {
	return errors.New("recorder: no maintenance")
}
func (r *recorderDriver) Send(message llm.Message, target agent.InboxTarget, wakeup bool) {}
func (r *recorderDriver) Followup(message llm.Message) {
	r.mu.Lock()
	r.followup = append(r.followup, message)
	r.signal()
	r.mu.Unlock()
}
func (r *recorderDriver) Steer(message llm.Message) {}
func (r *recorderDriver) Inject(message llm.Message) {
	r.mu.Lock()
	r.inject = append(r.inject, message)
	r.signal()
	r.mu.Unlock()
}

// Followups returns a locked snapshot of the recorded followups.
func (r *recorderDriver) Followups() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.Message(nil), r.followup...)
}

// Injects returns a locked snapshot of the recorded injections.
func (r *recorderDriver) Injects() []llm.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]llm.Message(nil), r.inject...)
}

// awaitNotices waits until the driver records the wanted counts, waking on
// every recorded notice instead of polling a fixed interval. Settlement
// dispatch runs the delivery listener after the settled channel closes, so
// the status-poll in settle() is not sufficient synchronization on its own.
func awaitNotices(t *testing.T, driver *recorderDriver, wantFollowup, wantInject int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		followups, injects := driver.Followups(), driver.Injects()
		if len(followups) == wantFollowup && len(injects) == wantInject {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notices: followup=%d (want %d) inject=%d (want %d)", len(followups), wantFollowup, len(injects), wantInject)
		}
		select {
		case <-driver.notify:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// assertNoNotices lets any mis-directed delivery land before asserting
// absence.
func assertNoNotices(t *testing.T, driver *recorderDriver) {
	t.Helper()
	time.Sleep(25 * time.Millisecond)
	if followups, injects := driver.Followups(), driver.Injects(); len(followups) != 0 || len(injects) != 0 {
		t.Fatalf("unexpected notices: followup=%d inject=%d", len(followups), len(injects))
	}
}

// liveAgent is one entered registry agent with a recorded driver; it doubles
// as the registry's owner record.
type liveAgent struct {
	*agent.Agent
	driver *recorderDriver
}

func newLiveAgent(t *testing.T, id string) *liveAgent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, registry.Events())
	if _, err := registry.Enter(built, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	driver := newRecorderDriver()
	built.SetDriver(driver)
	return &liveAgent{Agent: built, driver: driver}
}

func (l *liveAgent) OwnerID() string            { return string(l.ID) }
func (l *liveAgent) OwnerScope() scope.ScopeKey { return l.Scope }

// strangerOwner never resolves to a live agent.
type strangerOwner struct{}

func (strangerOwner) OwnerID() string            { return "stranger" }
func (strangerOwner) OwnerScope() scope.ScopeKey { return scope.NewScopeKey(nil) }

func newDeliveryRegistry(t *testing.T) *jobs.LocalRegistry {
	t.Helper()
	registry, err := jobs.NewLocalRegistry(jobs.Config{MaxConcurrentJobsPerOwner: 8}, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	registry.AttachControllerIn(nil)
	return registry
}

// settle starts one job for the owner and settles it unreported, waiting
// for the observation to commit.
func settle(t *testing.T, registry *jobs.LocalRegistry, owner jobs.Owner) jobs.Snapshot {
	t.Helper()
	hooks := &stubHooks{}
	id, err := registry.Start(jobs.StartSpec{
		Kind:  "bash",
		Label: "sleepy",
		Owner: owner,
		Run:   func() (jobs.Hooks, error) { return hooks.hooks(), nil },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	hooks.settle(jobs.Outcome{Status: jobs.OutcomeCompleted, Output: "done"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, err := registry.Get(id, owner.OwnerID())
		if err == nil && snapshot.Status != jobs.StatusRunning && snapshot.Status != jobs.StatusStopping {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never settled: %v", id, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func resolverFor(live ...*liveAgent) AgentResolver {
	return func(owner jobs.Owner) (*agent.Agent, bool) {
		for _, candidate := range live {
			if owner.OwnerID() == candidate.OwnerID() {
				return candidate.Agent, true
			}
		}
		return nil, false
	}
}

func TestDeliveryWakesIdleOwnerWithinBudget(t *testing.T) {
	registry := newDeliveryRegistry(t)
	owner := newLiveAgent(t, "wake-1")
	deliverer, detach, err := AttachDelivery(registry, resolverFor(owner), Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()

	snapshot := settle(t, registry, owner)
	awaitNotices(t, owner.driver, 1, 0)
	notices := owner.driver.Followups()
	notice := notices[0]
	if notice.Source.Kind != llm.SourcePlugin || notice.Source.Plugin != PluginName || notice.Source.Form != NoticeForm {
		t.Fatalf("source = %+v", notice.Source)
	}
	if notice.Content[0].Text != FitCompletionNotice(snapshot) {
		t.Fatalf("notice body drifted: %q", notice.Content[0].Text)
	}
	if deliverer.SpentWakes(owner.Agent) != 1 {
		t.Fatalf("spent = %d", deliverer.SpentWakes(owner.Agent))
	}
}

func TestDeliveryBudgetExhaustsAndUserInputRefills(t *testing.T) {
	registry := newDeliveryRegistry(t)
	owner := newLiveAgent(t, "wake-2")
	deliverer, detach, err := AttachDelivery(registry, resolverFor(owner), Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()

	for i := 0; i < DefaultMaxWakes; i++ {
		settle(t, registry, owner)
	}
	if deliverer.SpentWakes(owner.Agent) != DefaultMaxWakes {
		t.Fatalf("spent = %d", deliverer.SpentWakes(owner.Agent))
	}
	// The next notice degrades to injection: the budget bounds the
	// self-exciting chain where a woken turn starts the job whose
	// completion wakes it again.
	settle(t, registry, owner)
	awaitNotices(t, owner.driver, DefaultMaxWakes, 1)

	// A plugin-kind claim must not refill the budget.
	owner.Events().Emit(agent.EventInboxClaimed, owner.Scope, agent.AgentClaimedPayload{
		Agent: owner.Agent, Message: pluginNotice(), Turn: 1,
	})
	settle(t, registry, owner)
	awaitNotices(t, owner.driver, DefaultMaxWakes, 2)
	// Claiming human input is the point it enters a step: the budget
	// refills there.
	owner.Events().Emit(agent.EventInboxClaimed, owner.Scope, agent.AgentClaimedPayload{
		Agent: owner.Agent, Message: userMessage(), Turn: 2,
	})
	if deliverer.SpentWakes(owner.Agent) != 0 {
		t.Fatalf("user claim did not refill: %d", deliverer.SpentWakes(owner.Agent))
	}
	settle(t, registry, owner)
	awaitNotices(t, owner.driver, DefaultMaxWakes+1, 2)
}

func TestDeliveryInjectsBusyOwnerAndQuietModeNeverWakes(t *testing.T) {
	registry := newDeliveryRegistry(t)
	busy := newLiveAgent(t, "busy-1")
	busy.SetStatus(agent.AgentRunning)

	_, detachBusy, err := AttachDelivery(registry, resolverFor(busy), Config{})
	if err != nil {
		t.Fatalf("attach busy: %v", err)
	}
	defer detachBusy()
	settle(t, registry, busy)
	awaitNotices(t, busy.driver, 0, 1)

	// Quiet delivery never opens a turn, even on an idle owner with a
	// full budget.
	quiet := newLiveAgent(t, "quiet-1")
	_, detachQuiet, err := AttachDelivery(registry, resolverFor(quiet), Config{CompletionDelivery: DeliveryQuiet})
	if err != nil {
		t.Fatalf("attach quiet: %v", err)
	}
	defer detachQuiet()
	settle(t, registry, quiet)
	awaitNotices(t, quiet.driver, 0, 1)
}

func TestDeliverySkipsReportedAndUnknownOwners(t *testing.T) {
	registry := newDeliveryRegistry(t)
	owner := newLiveAgent(t, "skip-1")
	_, detach, err := AttachDelivery(registry, resolverFor(owner), Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	// Kill marks reported; the delivery arm stays silent for it.
	hooks := &stubHooks{}
	id, err := registry.Start(jobs.StartSpec{
		Kind: "bash", Label: "killed", Owner: owner,
		Run: func() (jobs.Hooks, error) { return hooks.hooks(), nil },
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := registry.Kill(id, owner.OwnerID(), "done"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	assertNoNotices(t, owner.driver)
	// An unresolved owner is dropped, not delivered to the wrong agent.
	settle(t, registry, strangerOwner{})
	assertNoNotices(t, owner.driver)
}

func TestDeliveryDetachStopsDelivery(t *testing.T) {
	registry := newDeliveryRegistry(t)
	owner := newLiveAgent(t, "detach-1")
	_, detach, err := AttachDelivery(registry, resolverFor(owner), Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	detach()
	settle(t, registry, owner)
	assertNoNotices(t, owner.driver)
}

func pluginNotice() llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "notice"}}, llm.MessageSource{Kind: llm.SourcePlugin, Plugin: PluginName})
}

func userMessage() llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}, llm.MessageSource{Kind: llm.SourceUser})
}
