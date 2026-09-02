package goalrounddriver

import (
	"context"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/session"
)

// --- fake loop driver ----------------------------------------------------------

type fakeDriver struct {
	mu       chan struct{}
	followup []llm.Message
	idle     chan struct{}
}

func newFakeDriver() *fakeDriver {
	buffer := make(chan struct{}, 1)
	buffer <- struct{}{}
	idle := make(chan struct{})
	close(idle)
	return &fakeDriver{mu: buffer, idle: idle}
}

func (d *fakeDriver) lock()   { <-d.mu }
func (d *fakeDriver) unlock() { d.mu <- struct{}{} }

func (d *fakeDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (d *fakeDriver) WhenIdle() <-chan struct{}                              { return d.idle }
func (d *fakeDriver) RunMaintenance(func(context.Context) error) error       { return nil }
func (d *fakeDriver) Send(llm.Message, agent.InboxTarget, bool)              {}
func (d *fakeDriver) Steer(llm.Message)                                      {}
func (d *fakeDriver) Inject(llm.Message)                                     {}

func (d *fakeDriver) Followup(message llm.Message) {
	d.lock()
	d.followup = append(d.followup, message)
	d.unlock()
}

func (d *fakeDriver) queued() []llm.Message {
	d.lock()
	defer d.unlock()
	return append([]llm.Message(nil), d.followup...)
}

// --- fixture -------------------------------------------------------------------

type driverFixture struct {
	registry *agent.AgentRegistry
	root     *cordis.Context
	sess     *session.Session
	agent    *agent.Agent
	goals    *goal.Service
	driver   *fakeDriver
	service  *Service
}

func newDriverFixture(t *testing.T) *driverFixture {
	t.Helper()
	f := &driverFixture{registry: agent.NewAgentRegistry(nil, nil), driver: newFakeDriver()}
	f.root = cordis.NewRoot(cordis.Discard{})
	t.Cleanup(func() { _ = f.root.Dispose() })
	header := &session.SessionHeader{ID: session.SessionID("sess-driver")}
	sess, err := session.NewDetached(session.SessionID("sess-driver"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, driverNoopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	f.agent = agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	f.agent.SetDriver(f.driver)
	detach, err := f.registry.Register(f.agent)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detach)
	f.goals, err = goal.NewService(f.root, f.registry, goal.Config{})
	if err != nil {
		t.Fatalf("goal service: %v", err)
	}
	f.service, err = New(f.root, f.registry, f.goals, nil, Config{})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	return f
}

type driverNoopNotifications struct{}

func (driverNoopNotifications) Inserted(llm.Message)       {}
func (driverNoopNotifications) Discarded(llm.Message)      {}
func (driverNoopNotifications) Claimed(llm.Message, int64) {}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}

func (f *driverFixture) statusIdle() {
	// The real loop publishes status transitions outside any driver lock;
	// emit asynchronously to mirror that.
	go f.registry.Events().Emit(agent.EventAgentStatus, f.agent.Scope,
		agent.AgentStatusPayload{Agent: f.agent, Status: agent.AgentIdle})
}

func (f *driverFixture) createArmed(t *testing.T, maxRounds int64) *goal.GoalView {
	t.Helper()
	view, err := f.goals.Create(f.agent, goal.CreateGoalRequest{Objective: "ship it", MaxGoalRounds: &maxRounds})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return view
}

// --- prompt --------------------------------------------------------------------

func TestRenderGoalRoundPromptVerbatim(t *testing.T) {
	view := &goal.GoalView{
		GoalSnapshot: goal.GoalSnapshot{
			ID: "goal-a", Revision: 1, Objective: "ship \"it\"",
			Phase: goal.PhaseActive, MaxGoalRounds: 5,
		},
	}
	blocks := RenderGoalRoundPrompt(view, 2)
	if len(blocks) != 1 || blocks[0].Type != llm.BlockText {
		t.Fatalf("blocks = %+v", blocks)
	}
	text := blocks[0].Text
	for _, fragment := range []string{
		"<goal_round>\n",
		`Objective: "ship \"it\""`,
		"Round: 2/5\n\n",
		"Continue working toward the objective in this same session. Treat the current workspace, ",
		"the configured goal-tool policy before reporting a blocker.\n</goal_round>",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("prompt missing %q:\n%s", fragment, text)
		}
	}
}

// --- reservation ---------------------------------------------------------------

func TestDriverReservesNextRoundAfterGoalChanged(t *testing.T) {
	f := newDriverFixture(t)
	f.createArmed(t, 5)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "the armed goal must queue exactly one round")

	message := f.driver.queued()[0]
	if message.Role != llm.RoleUser || message.Source.Kind != llm.SourceGoal ||
		message.Source.GoalRound != 1 || message.Source.GoalRevision != 1 {
		t.Fatalf("source = %+v", message.Source)
	}
	view, err := f.goals.Get(f.agent)
	if err != nil || view == nil || message.Source.GoalID != string(view.ID) {
		t.Fatalf("goal identity = %+v, %v", view, err)
	}
	if expected := RenderGoalRoundPrompt(view, 1); !contentEqual(message.Content, expected) {
		t.Fatalf("content = %+v; the package-owned prompt must ride verbatim", message.Content)
	}
	// The reservation is retained: no second queue until the attempt settles.
	time.Sleep(50 * time.Millisecond)
	if got := len(f.driver.queued()); got != 1 {
		t.Fatalf("queued = %d; at most one round may be reserved", got)
	}
}

func TestDriverBlocksAtRoundLimit(t *testing.T) {
	f := newDriverFixture(t)
	f.createArmed(t, 1)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "round one must be queued")

	// The loop claims the round, then admission walks the durable log: the
	// goal fold advances the round counter and the driver observes the exact
	// message identity.
	message := f.driver.queued()[0]
	f.registry.Events().Emit(agent.EventInboxClaimed, f.agent.Scope,
		agent.AgentClaimedPayload{Agent: f.agent, Message: message, Turn: 1})
	intent := &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
	event, err := f.sess.Append(session.EventUserMessage, message, intent)
	if err != nil {
		t.Fatalf("admit round: %v", err)
	}
	f.service.OnSessionEvent(f.sess, event)

	// Quiescence: the admitted attempt drains, checkpoints, and the budget
	// check blocks the goal with the official round-limit wording.
	f.statusIdle()
	waitFor(t, func() bool {
		view, err := f.goals.Get(f.agent)
		return err == nil && view != nil && view.Phase == goal.PhaseBlocked &&
			view.BlockedReason != nil && view.BlockedReason.Code == "round-limit" &&
			view.BlockedReason.Message == "Goal reached its configured limit of 1 rounds."
	}, "the exhausted budget must block the goal")
	if got := len(f.driver.queued()); got != 1 {
		t.Fatalf("queued = %d; a blocked goal queues no further rounds", got)
	}
}

func TestDriverBlocksQueueFailedWithoutDriver(t *testing.T) {
	f := newDriverFixture(t)
	f.agent.SetDriver(nil)
	f.createArmed(t, 5)
	waitFor(t, func() bool {
		view, err := f.goals.Get(f.agent)
		return err == nil && view != nil && view.Phase == goal.PhaseBlocked &&
			view.BlockedReason != nil && view.BlockedReason.Code == "queue-failed" &&
			strings.HasPrefix(view.BlockedReason.Message, "Could not queue goal round 1: ")
	}, "a failed queue must block the goal")

	// A later idle re-checks and keeps the blocked goal untouched.
	f.statusIdle()
	time.Sleep(50 * time.Millisecond)
	view, err := f.goals.Get(f.agent)
	if err != nil || view.Phase != goal.PhaseBlocked || view.BlockedReason.Code != "queue-failed" {
		t.Fatalf("view = %+v, %v", view, err)
	}
}

// --- pre-step fence --------------------------------------------------------------

func claimAttempt(t *testing.T, f *driverFixture, message llm.Message) {
	t.Helper()
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "the round must be queued")
	f.registry.Events().Emit(agent.EventInboxClaimed, f.agent.Scope,
		agent.AgentClaimedPayload{Agent: f.agent, Message: message, Turn: 1})
}

func TestPreStepAdmitsValidClaimedRound(t *testing.T) {
	f := newDriverFixture(t)
	f.createArmed(t, 5)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "queued")
	message := f.driver.queued()[0]
	claimAttempt(t, f, message)

	decision := f.registry.Events().PreStep().Dispatch(f.agent.Scope, agent.PreStepPayload{
		Agent: f.agent, Messages: []llm.Message{message}, Turn: 1, Step: 1, Signal: context.Background(),
	}, func(payload agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepEnter(payload.Messages)
	})
	if decision.Kind != "enter" || !decision.StartsRequestSeries {
		t.Fatalf("decision = %+v; a valid reservation enters a fresh request series", decision)
	}
}

func TestPreStepRejectsStaleRoundAndRestoresOthers(t *testing.T) {
	f := newDriverFixture(t)
	view := f.createArmed(t, 5)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "queued")
	message := f.driver.queued()[0]
	claimAttempt(t, f, message)

	// The goal revision advances underneath the queued prompt.
	objective := "ship it well"
	if _, err := f.goals.Edit(f.agent, goal.GoalRef{ID: view.ID, Revision: view.Revision},
		goal.EditGoalRequest{Objective: &objective}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	other := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "competing"}},
		llm.MessageSource{Kind: llm.SourceUser})

	decision := f.registry.Events().PreStep().Dispatch(f.agent.Scope, agent.PreStepPayload{
		Agent: f.agent, Messages: []llm.Message{message, other}, Turn: 1, Step: 1, Signal: context.Background(),
	}, func(payload agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepEnter(payload.Messages)
	})
	if decision.Kind != "reject" {
		t.Fatalf("decision = %+v; a stale revision must reject", decision)
	}
	// The competing claimed message survives at the front of next-step.
	waitFor(t, func() bool {
		next := f.agent.Inbox.NextStep()
		return len(next) == 1 && next[0].ID == other.ID
	}, "the other claimed message must be restored to next-step")
}

// --- terminal fences ---------------------------------------------------------------

func messageData(t *testing.T, id llm.MessageID) []byte {
	t.Helper()
	return []byte(`{"id":"` + string(id) + `"}`)
}

func TestTurnEndMaxTokensDisarms(t *testing.T) {
	f := newDriverFixture(t)
	f.createArmed(t, 5)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "queued")

	f.service.OnSessionEvent(f.sess, session.Event{Type: session.EventTurnEnd, Seq: 1, Data: []byte(
		`{"turn":1,"reason":{"kind":"max-tokens"}}`)})
	waitFor(t, func() bool {
		view, err := f.goals.Get(f.agent)
		return err == nil && view != nil && view.Activation == goal.ActivationDisarmed &&
			view.Phase == goal.PhaseActive
	}, "max-tokens must disarm without touching the phase")
}

func TestAbortedTurnCancelsClaimedAttemptThenPauses(t *testing.T) {
	f := newDriverFixture(t)
	f.createArmed(t, 5)
	waitFor(t, func() bool { return len(f.driver.queued()) == 1 }, "queued")
	message := f.driver.queued()[0]
	claimAttempt(t, f, message)

	// Admit, then abort: the attempt is cancelled, not disarmed outright.
	f.service.OnSessionEvent(f.sess, session.Event{Type: session.EventUserMessage, Seq: 1, Data: messageData(t, message.ID)})
	f.service.OnSessionEvent(f.sess, session.Event{Type: session.EventTurnEnd, Seq: 2, Data: []byte(
		`{"turn":1,"reason":{"kind":"aborted","reason":{"kind":"user"}}}`)})
	view, err := f.goals.Get(f.agent)
	if err != nil || view == nil || view.Activation != goal.ActivationArmed {
		t.Fatalf("view = %+v, %v; an aborted claimed round keeps authority", view, err)
	}

	// Quiescence pauses the cancelled round's goal.
	f.statusIdle()
	waitFor(t, func() bool {
		view, err := f.goals.Get(f.agent)
		return err == nil && view != nil && view.Phase == goal.PhasePaused
	}, "the cancelled attempt must pause the goal at idle")
}

// --- lifecycle fences ---------------------------------------------------------------

func TestStartupDisarmsExistingAgents(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	root := cordis.NewRoot(cordis.Discard{})
	defer func() { _ = root.Dispose() }()
	header := &session.SessionHeader{ID: session.SessionID("sess-late")}
	sess, err := session.NewDetached(session.SessionID("sess-late"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	inbox, err := agent.NewInbox(sess, driverNoopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	live := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, registry.Events())
	live.SetDriver(newFakeDriver())
	if _, err := registry.Register(live); err != nil {
		t.Fatalf("register: %v", err)
	}
	goals, err := goal.NewService(root, registry, goal.Config{})
	if err != nil {
		t.Fatalf("goal service: %v", err)
	}
	if _, err := goals.Create(live, goal.CreateGoalRequest{Objective: "ship it"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The driver loads over an armed goal and removes the hidden authority.
	if _, err := New(root, registry, goals, nil, Config{}); err != nil {
		t.Fatalf("driver: %v", err)
	}
	view, err := goals.Get(live)
	if err != nil || view == nil || view.Activation != goal.ActivationDisarmed || view.Phase != goal.PhaseActive {
		t.Fatalf("view = %+v, %v; startup must disarm without touching the phase", view, err)
	}
}
