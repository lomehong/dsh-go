package goal

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// --- error assertion ---------------------------------------------------------

func goalErrorCode(t *testing.T, err error) string {
	t.Helper()
	var harnessErr *llm.Error
	if !errors.As(err, &harnessErr) {
		t.Fatalf("expected a goal error, got %v", err)
	}
	return harnessErr.Code()
}

func expectGoalError(t *testing.T, err error, code, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s %q, got success", code, message)
	}
	if got := goalErrorCode(t, err); got != code {
		t.Fatalf("code = %s, want %s (err = %v)", got, code, err)
	}
	if got := err.Error(); got != message {
		t.Fatalf("message = %q, want %q", got, message)
	}
}

// --- change builders ---------------------------------------------------------

func snapshotChangeJSON(t *testing.T, operation GoalOperation, id GoalID, revision int64,
	objective string, phase GoalPhase, reason *GoalBlockReason, maxGoalRounds,
	roundsStarted, createdAt, updatedAt int64,
) json.RawMessage {
	t.Helper()
	snapshot := GoalSnapshot{
		ID: id, Revision: revision, Objective: objective,
		Phase: phase, BlockedReason: reason, MaxGoalRounds: maxGoalRounds,
	}
	change := newSnapshotChange(operation, snapshot, roundsStarted, createdAt, updatedAt)
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatalf("marshal change: %v", err)
	}
	return raw
}

func clearChangeJSON(t *testing.T, id GoalID, revision, clearedAt int64) json.RawMessage {
	t.Helper()
	change := newClearChange(GoalRef{ID: id, Revision: revision}, clearedAt)
	raw, err := json.Marshal(change)
	if err != nil {
		t.Fatalf("marshal clear: %v", err)
	}
	return raw
}

func changeEvent(seq int64, raw json.RawMessage) session.Event {
	return session.Event{Type: EventChange, Seq: seq, Data: raw}
}

func roundEvent(seq int64, goalID GoalID, revision, round int64) session.Event {
	data, _ := json.Marshal(map[string]any{
		"source": map[string]any{
			"kind": "goal", "goalId": string(goalID),
			"revision": float64(revision), "round": float64(round),
		},
	})
	return session.Event{Type: session.EventUserMessage, Seq: seq, Data: data}
}

// --- strict fold -------------------------------------------------------------

func TestDecodeGoalChangeShapes(t *testing.T) {
	raw := snapshotChangeJSON(t, OperationCreate, "goal-a", 1, "ship it", PhaseActive, nil, 3, 0, 1000, 1000)
	change, err := DecodeGoalChange(raw)
	if err != nil || change == nil {
		t.Fatalf("decode = %v, %v", change, err)
	}
	if change.Operation != OperationCreate || change.Goal.ID != "goal-a" || *change.RoundsStarted != 0 {
		t.Fatalf("change = %+v", change)
	}

	// Unrelated values are not goal changes: nil without error.
	if change, err := DecodeGoalChange([]byte(`{"kind":"other"}`)); change != nil || err != nil {
		t.Fatalf("unrelated = %v, %v", change, err)
	}
	// A wrong version fails replay loudly.
	_, err = DecodeGoalChange([]byte(`{"kind":"goal/change","version":2}`))
	if err == nil || err.Error() != "unsupported goal change version 2" {
		t.Fatalf("version err = %v", err)
	}
	// An unexpected extra field fails the exact-key gate.
	blob := `{"kind":"goal/change","version":1,"operation":"clear","cleared":{"id":"g","revision":2},"clearedAt":5,"extra":1}`
	_, err = DecodeGoalChange([]byte(blob))
	if err == nil || !strings.Contains(err.Error(), "goal clear change must have exactly") {
		t.Fatalf("key-set err = %v", err)
	}
}

func TestFoldLifecycle(t *testing.T) {
	reason := &GoalBlockReason{Code: "waiting-user", Message: "needs a decision"}
	events := []session.Event{
		changeEvent(0, snapshotChangeJSON(t, OperationCreate, "goal-a", 1, "ship it", PhaseActive, nil, 3, 0, 1000, 1000)),
		roundEvent(1, "goal-a", 1, 1),
		changeEvent(2, snapshotChangeJSON(t, OperationEdit, "goal-a", 2, "ship it well", PhaseActive, nil, 3, 1, 1000, 2000)),
		changeEvent(3, snapshotChangeJSON(t, OperationPause, "goal-a", 3, "ship it well", PhasePaused, nil, 3, 1, 1000, 3000)),
		changeEvent(4, snapshotChangeJSON(t, OperationResume, "goal-a", 4, "ship it well", PhaseActive, nil, 3, 1, 1000, 4000)),
		changeEvent(5, snapshotChangeJSON(t, OperationBlock, "goal-a", 5, "ship it well", PhaseBlocked, reason, 3, 1, 1000, 5000)),
		changeEvent(6, snapshotChangeJSON(t, OperationResume, "goal-a", 6, "ship it well", PhaseActive, nil, 3, 1, 1000, 6000)),
		changeEvent(7, snapshotChangeJSON(t, OperationComplete, "goal-a", 7, "ship it well", PhaseComplete, nil, 3, 1, 1000, 7000)),
		// A completed goal may be replaced by a fresh one.
		changeEvent(8, snapshotChangeJSON(t, OperationCreate, "goal-b", 1, "next", PhaseActive, nil, 2, 0, 8000, 8000)),
		changeEvent(9, clearChangeJSON(t, "goal-b", 2, 9000)),
	}
	folded, err := FoldGoal(events)
	if err != nil {
		t.Fatalf("fold = %v", err)
	}
	if folded.Goal != nil || folded.RoundsStarted != 0 || folded.CreatedAt != nil {
		t.Fatalf("folded = %+v; clear must empty the current goal", folded)
	}
	if folded.LastRef == nil || folded.LastRef.ID != "goal-b" || folded.LastRef.Revision != 2 {
		t.Fatalf("lastRef = %+v; the tombstone survives", folded.LastRef)
	}

	// Mid-stream projection: after round admission the counter moved.
	mid, err := FoldGoal(events[:2])
	if err != nil {
		t.Fatalf("mid fold = %v", err)
	}
	if mid.RoundsStarted != 1 || mid.Goal == nil || mid.Goal.Revision != 1 {
		t.Fatalf("mid = %+v", mid)
	}
}

func TestFoldRejectsViolations(t *testing.T) {
	created := changeEvent(0, snapshotChangeJSON(t, OperationCreate, "goal-a", 1, "ship it", PhaseActive, nil, 2, 0, 1000, 1000))
	cases := []struct {
		name   string
		events []session.Event
		want   string
	}{
		{
			"recreate while active",
			[]session.Event{created,
				changeEvent(1, snapshotChangeJSON(t, OperationCreate, "goal-b", 1, "next", PhaseActive, nil, 2, 0, 2000, 2000))},
			"goal create requires a fresh active revision-one goal with zero rounds",
		},
		{
			"stale revision",
			[]session.Event{created,
				changeEvent(1, snapshotChangeJSON(t, OperationEdit, "goal-a", 3, "edited", PhaseActive, nil, 2, 0, 1000, 2000))},
			"goal edit must advance the current goal by one revision",
		},
		{
			"edit changing phase",
			[]session.Event{created,
				changeEvent(1, snapshotChangeJSON(t, OperationEdit, "goal-a", 2, "ship it", PhasePaused, nil, 2, 0, 1000, 2000))},
			"goal edit cannot change phase or blocked reason",
		},
		{
			"counters must be preserved",
			[]session.Event{created, roundEvent(1, "goal-a", 1, 1),
				changeEvent(2, snapshotChangeJSON(t, OperationEdit, "goal-a", 2, "edited", PhaseActive, nil, 2, 0, 1000, 2000))},
			"goal edit does not preserve the current counters and timestamps",
		},
		{
			"pause from paused",
			[]session.Event{created,
				changeEvent(1, snapshotChangeJSON(t, OperationPause, "goal-a", 2, "ship it", PhasePaused, nil, 2, 0, 1000, 2000)),
				changeEvent(2, snapshotChangeJSON(t, OperationPause, "goal-a", 3, "ship it", PhasePaused, nil, 2, 0, 1000, 3000))},
			"goal pause has an invalid phase transition",
		},
		{
			"resume with exhausted budget",
			[]session.Event{created, roundEvent(1, "goal-a", 1, 1), roundEvent(2, "goal-a", 1, 2),
				changeEvent(3, snapshotChangeJSON(t, OperationPause, "goal-a", 2, "ship it", PhasePaused, nil, 2, 2, 1000, 3000)),
				changeEvent(4, snapshotChangeJSON(t, OperationResume, "goal-a", 3, "ship it", PhaseActive, nil, 2, 2, 1000, 4000))},
			"goal resume has an invalid phase transition or exhausted round budget",
		},
		{
			"clear without a current goal",
			[]session.Event{changeEvent(0, clearChangeJSON(t, "goal-a", 1, 1000))},
			"goal clear requires a current goal",
		},
		{
			"round out of order",
			[]session.Event{created, roundEvent(1, "goal-a", 1, 2)},
			"goal round at session event 1 is not the next admitted round of the active goal",
		},
		{
			"round after pause",
			[]session.Event{created,
				changeEvent(1, snapshotChangeJSON(t, OperationPause, "goal-a", 2, "ship it", PhasePaused, nil, 2, 0, 1000, 2000)),
				roundEvent(2, "goal-a", 2, 1)},
			"goal round at session event 2 is not the next admitted round of the active goal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := FoldGoal(tc.events)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFoldRejectsInvalidMessageSource(t *testing.T) {
	created := changeEvent(0, snapshotChangeJSON(t, OperationCreate, "goal-a", 1, "ship it", PhaseActive, nil, 2, 0, 1000, 1000))
	raw, _ := json.Marshal(map[string]any{
		"source": map[string]any{"kind": "goal", "goalId": "goal-a", "revision": float64(1), "round": float64(0)},
	})
	_, err := FoldGoal([]session.Event{created, {Type: session.EventUserMessage, Seq: 1, Data: raw}})
	if err == nil || err.Error() != "goal message source is invalid" {
		t.Fatalf("err = %v", err)
	}
}

// --- projection unit ---------------------------------------------------------

func TestApplyGoalProjectionLastWins(t *testing.T) {
	raw := snapshotChangeJSON(t, OperationCreate, "goal-a", 1, "ship it", PhaseActive, nil, 3, 0, 1000, 1000)
	state, changed := ApplyGoalProjection(nil, changeEvent(0, raw))
	if !changed || state == nil || state.Goal.ID != "goal-a" || state.Goal.MaxGoalRounds != 3 {
		t.Fatalf("state = %+v, changed = %v", state, changed)
	}

	// A non-goal event passes the same reference untouched.
	next, changed := ApplyGoalProjection(state, session.Event{Type: "user/message"})
	if changed || next != state {
		t.Fatalf("non-goal event must return the same reference")
	}

	// A malformed goal change keeps the prior projection (projection-grade
	// lenience; the strict fold is the fail-loud owner).
	next, changed = ApplyGoalProjection(state, changeEvent(1, []byte(`{"kind":"goal/change","version":9}`)))
	if changed || next != state {
		t.Fatalf("malformed change must return the same reference")
	}

	// Clear publishes nil exactly once.
	clearRaw := clearChangeJSON(t, "goal-a", 2, 2000)
	next, changed = ApplyGoalProjection(state, changeEvent(2, clearRaw))
	if !changed || next != nil {
		t.Fatalf("clear = %+v, changed = %v", next, changed)
	}
	next, changed = ApplyGoalProjection(nil, changeEvent(3, clearRaw))
	if changed || next != nil {
		t.Fatalf("clear over null must publish nothing new")
	}
}

func TestDecodeGoalProjectionRows(t *testing.T) {
	state, err := decodeGoalProjection([]byte("null"))
	if err != nil || state != nil {
		t.Fatalf("null row = %+v, %v", state, err)
	}

	row := `{"goal":{"id":"goal-a","revision":2,"objective":"ship it","phase":"blocked",` +
		`"blockedReason":{"code":"waiting-user","message":"needs a decision"},"maxGoalRounds":3},` +
		`"roundsStarted":1,"createdAt":1000,"updatedAt":2000}`
	state, err = decodeGoalProjection([]byte(row))
	if err != nil || state == nil {
		t.Fatalf("row = %+v, %v", state, err)
	}
	if state.Goal.Phase != PhaseBlocked || state.Goal.BlockedReason.Code != "waiting-user" ||
		state.RoundsStarted != 1 || state.UpdatedAt != 2000 {
		t.Fatalf("state = %+v", state)
	}

	if _, err = decodeGoalProjection([]byte(`{"goal":{"id":"","revision":1,"objective":"x","phase":"active","maxGoalRounds":1},"roundsStarted":0,"createdAt":0,"updatedAt":0}`)); err == nil {
		t.Fatal("empty id must be rejected")
	}
	if _, err = decodeGoalProjection([]byte(`{"roundsStarted":0}`)); err == nil {
		t.Fatal("goal-less row must be rejected")
	}
}

// --- service fixture ---------------------------------------------------------

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

type fixture struct {
	registry *agent.AgentRegistry
	sess     *session.Session
	agent    *agent.Agent
	service  *Service
}

func newFixture(t *testing.T, config Config) *fixture {
	t.Helper()
	f := &fixture{registry: agent.NewAgentRegistry(nil, nil)}
	header := &session.SessionHeader{ID: session.SessionID("sess-goal")}
	sess, err := session.NewDetached(session.SessionID("sess-goal"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	f.agent = agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	detach, err := f.registry.Register(f.agent)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detach)
	service, err := NewService(nil, f.registry, config)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(service.Dispose)
	f.service = service
	return f
}

func currentRef(t *testing.T, f *fixture) GoalRef {
	t.Helper()
	view, err := f.service.Get(f.agent)
	if err != nil || view == nil {
		t.Fatalf("get = %+v, %v", view, err)
	}
	return GoalRef{ID: view.ID, Revision: view.Revision}
}

// --- service lifecycle ---------------------------------------------------------

func TestServiceCreateArmsAndPersistsExactWireShape(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "  ship it  "})
	if err != nil {
		t.Fatalf("create = %v", err)
	}
	if view.Activation != ActivationArmed || view.Phase != PhaseActive || view.Revision != 1 ||
		view.MaxGoalRounds != 256 || view.Objective != "ship it" {
		t.Fatalf("view = %+v", view)
	}
	if !strings.HasPrefix(string(view.ID), "goal-") {
		t.Fatalf("id = %s", view.ID)
	}

	// The committed event carries the exact snapshot-change key set —
	// including roundsStarted zero, which an omitempty value would drop.
	events := f.sess.Events()
	last := events[len(events)-1]
	if last.Type != EventChange {
		t.Fatalf("last event = %s", last.Type)
	}
	var record map[string]any
	if err := json.Unmarshal(last.Data, &record); err != nil {
		t.Fatalf("data: %v", err)
	}
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "createdAt,goal,kind,operation,roundsStarted,updatedAt,version" {
		t.Fatalf("keys = %s", got)
	}
}

func TestServiceCreateValidation(t *testing.T) {
	f := newFixture(t, Config{})
	_, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "   "})
	expectGoalError(t, err, CodeInvalidObjective, "goal objective must be a non-empty string")

	zero := int64(0)
	_, err = f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it", MaxGoalRounds: &zero})
	expectGoalError(t, err, CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer")

	// The deployment default is itself validated.
	_, err = NewService(nil, f.registry, Config{DefaultMaxGoalRounds: &zero})
	expectGoalError(t, err, CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer")
}

func TestServiceCreateRejectsDuplicate(t *testing.T) {
	f := newFixture(t, Config{})
	if _, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "first"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "second"})
	view, _ := f.service.Get(f.agent)
	expectGoalError(t, err, CodeAlreadyExists,
		fmt.Sprintf("goal %q already exists with phase \"active\"", view.ID))
}

func TestServiceEditAndStaleRef(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := GoalRef{ID: view.ID, Revision: view.Revision}

	objective := "ship it well"
	maxRounds := int64(10)
	edited, err := f.service.Edit(f.agent, ref, EditGoalRequest{Objective: &objective, MaxGoalRounds: &maxRounds})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Revision != 2 || edited.Objective != "ship it well" || edited.MaxGoalRounds != 10 ||
		edited.Phase != PhaseActive {
		t.Fatalf("edited = %+v", edited)
	}

	// The stale ref is rejected with the current state named.
	_, err = f.service.Edit(f.agent, ref, EditGoalRequest{Objective: &objective})
	expectGoalError(t, err, CodeStaleRevision, fmt.Sprintf(
		"stale goal ref %q revision 1; current is %q revision 2", view.ID, view.ID))

	// An empty edit is rejected.
	_, err = f.service.Edit(f.agent, currentRef(t, f), EditGoalRequest{})
	expectGoalError(t, err, CodeInvalidEdit, "goal edit requires objective and/or maxGoalRounds")
}

func TestServiceTransitionsAndActivation(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := GoalRef{ID: view.ID, Revision: view.Revision}

	paused, err := f.service.Pause(f.agent, ref)
	if err != nil || paused.Phase != PhasePaused || paused.Activation != ActivationDisarmed {
		t.Fatalf("paused = %+v, %v", paused, err)
	}

	// An active-and-armed resume is rejected; a paused resume arms.
	_, err = f.service.Resume(f.agent, GoalRef{ID: view.ID, Revision: paused.Revision})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	active, err := f.service.Get(f.agent)
	if err != nil || active.Activation != ActivationArmed {
		t.Fatalf("after resume = %+v, %v", active, err)
	}
	_, err = f.service.Resume(f.agent, currentRef(t, f))
	expectGoalError(t, err, CodeInvalidTransition,
		fmt.Sprintf("goal %q is already active and armed", view.ID))

	blocked, err := f.service.Block(f.agent, currentRef(t, f),
		GoalBlockReason{Code: "waiting-user", Message: " needs a decision "})
	if err != nil || blocked.Phase != PhaseBlocked ||
		blocked.BlockedReason.Code != "waiting-user" || blocked.BlockedReason.Message != "needs a decision" {
		t.Fatalf("blocked = %+v, %v", blocked, err)
	}

	// Resume from blocked re-arms; complete then disarms.
	if _, err = f.service.Resume(f.agent, currentRef(t, f)); err != nil {
		t.Fatalf("resume from blocked: %v", err)
	}
	completed, err := f.service.Complete(f.agent, currentRef(t, f))
	if err != nil || completed.Phase != PhaseComplete || completed.Activation != ActivationDisarmed {
		t.Fatalf("completed = %+v, %v", completed, err)
	}

	// A complete goal cannot be completed twice.
	_, err = f.service.Complete(f.agent, currentRef(t, f))
	expectGoalError(t, err, CodeInvalidTransition, fmt.Sprintf(
		"cannot complete goal %q from phase \"complete\"; expected active or paused or blocked", view.ID))

	// A completed goal may be replaced by a fresh create.
	if _, err = f.service.Create(f.agent, CreateGoalRequest{Objective: "next"}); err != nil {
		t.Fatalf("recreate after complete: %v", err)
	}
}

func TestServiceBlockValidation(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref := GoalRef{ID: view.ID, Revision: view.Revision}
	_, err = f.service.Block(f.agent, ref, GoalBlockReason{Code: "Bad Code", Message: "x"})
	expectGoalError(t, err, CodeInvalidBlockReason,
		"goal block reason requires a lower-kebab-case code and a non-empty message")
}

func TestServiceClearRetainsTombstone(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tombstone, err := f.service.Clear(f.agent, GoalRef{ID: view.ID, Revision: view.Revision})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if tombstone.ID != view.ID || tombstone.Revision != view.Revision+1 {
		t.Fatalf("tombstone = %+v", tombstone)
	}
	if after, err := f.service.Get(f.agent); err != nil || after != nil {
		t.Fatalf("after clear = %+v, %v", after, err)
	}

	// The tombstone survives replay: a fresh service over the same log sees
	// no current goal yet refuses a stale clear.
	again, err := NewService(nil, f.registry, Config{})
	if err != nil {
		t.Fatalf("replay service: %v", err)
	}
	defer again.Dispose()
	if replayed, err := again.Get(f.agent); err != nil || replayed != nil {
		t.Fatalf("replayed = %+v, %v", replayed, err)
	}

	// A fresh create after clear starts a new identity.
	fresh, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "next"})
	if err != nil || fresh.ID == view.ID {
		t.Fatalf("fresh = %+v, %v", fresh, err)
	}
}

func TestServiceSessionStartDisarms(t *testing.T) {
	f := newFixture(t, Config{})
	if _, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f.registry.Events().Emit(agent.EventAgentSessionStart, f.agent.Scope,
		agent.AgentSessionStartPayload{Agent: f.agent, Source: agent.SessionStartStartup})
	view, err := f.service.Get(f.agent)
	if err != nil || view == nil || view.Activation != ActivationDisarmed || view.Phase != PhaseActive {
		t.Fatalf("view = %+v, %v; session-start must disarm without touching phase", view, err)
	}
	// Rearm through a human-authorized resume.
	if _, err := f.service.Resume(f.agent, currentRef(t, f)); err != nil {
		t.Fatalf("rearm: %v", err)
	}
}

func TestServiceRoundAdmissionAdvancesCounter(t *testing.T) {
	f := newFixture(t, Config{})
	view, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendRound := func(round int64) {
		t.Helper()
		message := llm.NewUserMessage(
			[]llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("round %d", round)}},
			llm.MessageSource{Kind: llm.SourceGoal, GoalID: string(view.ID), GoalRevision: 1, GoalRound: round})
		intent := &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
		if _, err := f.sess.Append("user/message", message, intent); err != nil {
			t.Fatalf("append round: %v", err)
		}
	}
	appendRound(1)
	after, err := f.service.Get(f.agent)
	if err != nil || after.RoundsStarted != 1 {
		t.Fatalf("after round = %+v, %v", after, err)
	}

	// A foreign round is rejected at the next sync.
	appendRound(5)
	_, err = f.service.Get(f.agent)
	if err == nil || !strings.Contains(err.Error(), "is not the next admitted round of the active goal") {
		t.Fatalf("foreign round err = %v", err)
	}
}

func TestServiceRejectsForeignAgent(t *testing.T) {
	f := newFixture(t, Config{})
	header := &session.SessionHeader{ID: session.SessionID("sess-foreign")}
	sess, err := session.NewDetached(session.SessionID("sess-foreign"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	stranger := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	_, err = f.service.Create(stranger, CreateGoalRequest{Objective: "ship it"})
	expectGoalError(t, err, CodeAgentNotLive,
		fmt.Sprintf("agent %q is not live in this registry", sess.ID()))
}

func TestServiceEmitsGoalChanged(t *testing.T) {
	f := newFixture(t, Config{})
	var notifications []GoalChanged
	dispose := f.agent.Events().OnEmit(EventChanged, nil, func(payload any) error {
		changed, ok := payload.(ChangedPayload)
		if !ok {
			return fmt.Errorf("unexpected payload %T", payload)
		}
		notifications = append(notifications, changed.Change)
		return nil
	})
	defer dispose()

	if _, err := f.service.Create(f.agent, CreateGoalRequest{Objective: "ship it"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.service.Clear(f.agent, currentRef(t, f)); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(notifications) != 2 {
		t.Fatalf("notifications = %d", len(notifications))
	}
	first, second := notifications[0], notifications[1]
	if first.Operation != OperationCreate || first.Goal == nil || first.Goal.Objective != "ship it" {
		t.Fatalf("first = %+v", first)
	}
	if second.Operation != OperationClear || second.Goal != nil || second.Ref.Revision != 2 {
		t.Fatalf("second = %+v", second)
	}
}

func TestServiceRegistersProjectionUnit(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	t.Cleanup(func() { _ = root.Dispose() })
	registry := projection.NewRegistry()
	detach := registry.Attach(root)
	t.Cleanup(detach)
	root.Provide("projections", registry)

	f := &fixture{registry: agent.NewAgentRegistry(nil, nil)}
	header := &session.SessionHeader{ID: session.SessionID("sess-proj")}
	sess, err := session.NewDetached(session.SessionID("sess-proj"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	f.agent = agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	detachAgent, err := f.registry.Register(f.agent)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detachAgent)

	// NewService over a context carrying the registry installs the unit
	// child (the official ctx.inject seam).
	service, err := NewService(root, f.registry, Config{})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(service.Dispose)

	view, err := service.Create(f.agent, CreateGoalRequest{Objective: "ship it"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	snapshot := registry.Snapshot(sess, ProjectionKey)
	value := snapshot.Values[ProjectionKey]
	projected, ok := value.(*GoalProjection)
	if !ok || projected == nil {
		t.Fatalf("projection = %#v; the goal unit must fold the change", value)
	}
	if projected.Goal.ID != view.ID || projected.Goal.Phase != PhaseActive {
		t.Fatalf("projected = %+v", projected)
	}

	// Clear collapses the projection to null.
	if _, err := service.Clear(f.agent, GoalRef{ID: view.ID, Revision: view.Revision}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	snapshot = registry.Snapshot(sess, ProjectionKey)
	if value := snapshot.Values[ProjectionKey]; value != nil {
		t.Fatalf("cleared projection = %#v", value)
	}
}
