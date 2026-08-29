package planmode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// noopNotifications satisfies the inbox observer contract; plan tests only
// read the projection.
type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

func newPlanAgent(t *testing.T, id string) (*agent.Agent, *session.Session) {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
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
	return built, sess
}

func TestFoldPlanModeLastWins(t *testing.T) {
	events := []session.Event{
		{Type: EventPlanMode, Data: mustJSON(t, PlanModeData{Active: true})},
		{Type: EventPlanMode, Data: mustJSON(t, PlanModeData{Active: false})},
		{Type: EventPlanMode, Data: mustJSON(t, PlanModeData{Active: true})},
	}
	if FoldPlanMode(events, -1) != true {
		t.Fatal("the last plan/mode must win")
	}
	if FoldPlanMode(events, 1) != true || FoldPlanMode(events, 2) != false {
		t.Fatal("prefix folds must see only their prefix")
	}
	if FoldPlanMode(nil, -1) {
		t.Fatal("a log with none folds to inactive")
	}
}

func TestResolveSectionFailsLoud(t *testing.T) {
	if _, err := ResolveSection(""); err == nil || !strings.Contains(err.Error(), "PlanModeConfig needs a string `section`") {
		t.Fatalf("err = %v, want the missing-section rejection", err)
	}
	if _, err := ResolveSection("   "); err == nil || !strings.Contains(err.Error(), "PlanModeConfig needs a non-empty `section`") {
		t.Fatalf("err = %v, want the blank-section rejection", err)
	}
	if _, err := ResolveSection(" guidance "); err != nil {
		t.Fatalf("valid section: %v", err)
	}
	if _, err := NewController(""); err == nil {
		t.Fatal("NewController must fail loud on missing guidance")
	}
}

func TestFirstHeading(t *testing.T) {
	if got := FirstHeading("# Name\nbody"); got != "Name" {
		t.Fatalf("h1 = %q", got)
	}
	if got := FirstHeading("intro\n#### Deep heading with trailing spaces   \nbody"); got != "Deep heading with trailing spaces" {
		t.Fatalf("h4 = %q", got)
	}
	if got := FirstHeading("no heading here"); got != "" {
		t.Fatalf("none = %q", got)
	}
	if got := FirstHeading("#Hash without space"); got != "" {
		t.Fatalf("malformed = %q", got)
	}
}

func TestHasOpenTurnAndLastHeader(t *testing.T) {
	events := []session.Event{
		{Type: session.EventTurnStart, Data: mustJSON(t, session.TurnStartData{Turn: 1})},
		{Type: session.EventRequestHeader, Data: json.RawMessage(`{}`)},
		{Type: EventPlanMode, Data: mustJSON(t, PlanModeData{Active: true})},
		{Type: session.EventTurnEnd, Data: json.RawMessage(`{}`)},
		{Type: session.EventRequestHeader, Data: json.RawMessage(`{}`)},
	}
	if !HasOpenTurn(events[:1]) {
		t.Fatal("turn/start without turn/end is open")
	}
	if HasOpenTurn(events) {
		t.Fatal("closed turn must not be open")
	}
	// The last header is at index 4; the fold through it includes the
	// plan/mode at index 2.
	told, has := PlanModeAtLastHeader(events)
	if !has || !told {
		t.Fatalf("at last header = %v has = %v, want active", told, has)
	}
	// The first header precedes any plan/mode: inactive.
	told, has = PlanModeAtLastHeader(events[:2])
	if !has || told {
		t.Fatalf("at first header = %v has = %v, want inactive", told, has)
	}
	if _, has = PlanModeAtLastHeader(nil); has {
		t.Fatal("no header must be reported absent")
	}
}

func TestSetCommitsBetweenTurnsAndNarrates(t *testing.T) {
	controller, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	agentObj, sess := newPlanAgent(t, "plan-commit")
	// A request header describing the default mode primes narration.
	if _, err := sess.Append(session.EventRequestHeader, json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("append header: %v", err)
	}
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeCommitted {
		t.Fatalf("outcome = %s err = %v, want committed", outcome, err)
	}
	if !FoldPlanMode(sess.Events(), -1) {
		t.Fatal("the logged state must flip to active")
	}
	// The narration lands in the next-turn inbox, sourced as a plugin notice.
	pending := agentObj.Inbox.NextTurn()
	if len(pending) != 1 || pending[0].Source.Kind != llm.SourcePlugin || pending[0].Source.Plugin != "plan-mode" || pending[0].Source.Form != llm.FormNotice {
		t.Fatalf("inbox = %+v", pending)
	}
	if got := pending[0].Content[0].Text; got != "The user switched this session to plan mode." || pending[0].Source.Summary != got {
		t.Fatalf("narration = %q", got)
	}

	// Re-selecting the current state is a noop; turning off narrates back.
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeNoop {
		t.Fatalf("outcome = %s err = %v, want noop", outcome, err)
	}
	if outcome, err := controller.Set(agentObj, false); err != nil || outcome != OutcomeCommitted {
		t.Fatalf("outcome = %s err = %v, want committed", outcome, err)
	}
	// Turning off does not narrate: the last header described the default
	// mode, so there is nothing to correct.
	off := agentObj.Inbox.NextTurn()
	if len(off) != 1 || off[0].Content[0].Text != "The user switched this session to plan mode." {
		t.Fatalf("off narration = %+v, want no new narration", off)
	}
	// Turning off while already inactive (no header) is a noop.
	if outcome, err := controller.Set(agentObj, false); err != nil || outcome != OutcomeNoop {
		t.Fatalf("outcome = %s err = %v, want noop", outcome, err)
	}
}

func TestSetQueuesDuringOpenTurnAndPreStepCommits(t *testing.T) {
	controller, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	agentObj, sess := newPlanAgent(t, "plan-queue")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(agentObj, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	undo := controller.RegisterPreStep(registry, cordis.Discard{})
	t.Cleanup(undo)

	// Open a turn and log a request header describing the default mode.
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if _, err := sess.Append(session.EventRequestHeader, json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("header: %v", err)
	}

	// During an open turn the selection queues.
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeQueued {
		t.Fatalf("outcome = %s err = %v, want queued", outcome, err)
	}
	if FoldPlanMode(sess.Events(), -1) {
		t.Fatal("a queued selection must not be logged yet")
	}
	active, pending, hasPending := controller.Get(sess)
	if active || !pending || !hasPending {
		t.Fatalf("get = %v %v %v, want inactive with pending active", active, pending, hasPending)
	}

	// The next accepted in-turn pre-step appends the mode and the narration.
	decision := registry.Events().PreStep().Dispatch(nil, agent.PreStepPayload{
		Agent: agentObj, Messages: nil, Turn: 1, Step: 1, Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(nil) })
	if !FoldPlanMode(sess.Events(), -1) {
		t.Fatal("the pre-step must have committed the pending selection")
	}
	if len(decision.Messages) != 1 || decision.Messages[0].Content[0].Text != "The user switched this session to plan mode." {
		t.Fatalf("decision = %+v, want the narration appended", decision)
	}
	// The pending selection is consumed.
	if _, pending, hasPending := controller.Get(sess); pending || hasPending {
		t.Fatal("the pending selection must be cleared after commit")
	}

	// A subsequent pre-step with nothing pending changes nothing.
	decision2 := registry.Events().PreStep().Dispatch(nil, agent.PreStepPayload{
		Agent: agentObj, Messages: nil, Turn: 1, Step: 2, Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(nil) })
	if len(decision2.Messages) != 0 {
		t.Fatalf("decision = %+v, want no further narration", decision2)
	}
}

func TestSetCancelClearsOppositePending(t *testing.T) {
	controller, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	agentObj, sess := newPlanAgent(t, "plan-cancel")
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeQueued {
		t.Fatalf("outcome = %s err = %v, want queued", outcome, err)
	}
	// The opposite selection while queued: cancelled, and nothing logged.
	if outcome, err := controller.Set(agentObj, false); err != nil || outcome != OutcomeCancelled {
		t.Fatalf("outcome = %s err = %v, want cancelled", outcome, err)
	}
	if FoldPlanMode(sess.Events(), -1) {
		t.Fatal("cancelled selection must leave the log unchanged")
	}
	// The map keeps the already-matching (harmless) intent until a boundary
	// clears it; the readable pending VALUE is false.
	if _, pending, _ := controller.Get(sess); pending {
		t.Fatal("the cleared selection must not read as pending")
	}
	// Repeating the queued state is a noop (already pending active).
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeQueued {
		t.Fatalf("re-queue = %s err = %v, want queued", outcome, err)
	}
	if outcome, err := controller.Set(agentObj, true); err != nil || outcome != OutcomeNoop {
		t.Fatalf("outcome = %s err = %v, want noop (already pending)", outcome, err)
	}
}

func TestSectionTextFollowsState(t *testing.T) {
	controller, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	_, sess := newPlanAgent(t, "plan-section")
	if got := controller.SectionText(sess); got != "" {
		t.Fatalf("inactive section text = %q", got)
	}
	if _, err := sess.Append(EventPlanMode, PlanModeData{Active: true}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := controller.SectionText(sess); got != "plan guidance section" {
		t.Fatalf("active section text = %q", got)
	}
}

func TestSectionTextPendingExitHidesImmediately(t *testing.T) {
	controller, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	agentObj, sess := newPlanAgent(t, "plan-section-pending")
	// Log active, then queue an EXIT inside an open turn: the section hides
	// immediately (official `pending?.active ?? fold` — a pending selection
	// replaces the fold, it does not OR with it).
	if _, err := sess.Append(EventPlanMode, PlanModeData{Active: true}, nil); err != nil {
		t.Fatalf("append active: %v", err)
	}
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if got := controller.SectionText(sess); got != "plan guidance section" {
		t.Fatalf("pre-selection section text = %q", got)
	}
	if outcome, err := controller.Set(agentObj, false); err != nil || outcome != OutcomeQueued {
		t.Fatalf("outcome = %s err = %v, want queued exit", outcome, err)
	}
	if got := controller.SectionText(sess); got != "" {
		t.Fatalf("pending exit must hide the section despite the active log, got %q", got)
	}
	// A pending ENTER on an inactive log shows it immediately.
	if _, err := sess.Append(EventPlanMode, PlanModeData{Active: false}, nil); err != nil {
		t.Fatalf("append inactive: %v", err)
	}
	controller2, err := NewController("plan guidance section")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	if outcome, err := controller2.Set(agentObj, true); err != nil || outcome != OutcomeQueued {
		t.Fatalf("outcome = %s err = %v, want queued enter", outcome, err)
	}
	if got := controller2.SectionText(sess); got != "plan guidance section" {
		t.Fatalf("pending enter must show the section despite the inactive log, got %q", got)
	}
}

func TestProjectionFoldAndWire(t *testing.T) {
	definition := ProjectionDefinition()
	if definition.Key != "plan" || definition.StateVersion != 2 {
		t.Fatalf("definition = %+v", definition)
	}
	state := definition.Init(session.SessionHeader{})
	state = definition.Apply(state, session.Event{
		Type: "command/run",
		Data: json.RawMessage(`{"name":"plan","commandId":"c1","args":"enter planning"}`),
	})
	view := definition.Wire.View(state).(PlanProjection)
	if view.Active || !view.Pending {
		t.Fatalf("view = %+v, want pending entry", view)
	}
	state = definition.Apply(state, session.Event{
		Type: "command/done",
		Data: json.RawMessage(`{"commandId":"c1","kind":"success"}`),
	})
	state = definition.Apply(state, session.Event{
		Type: "command/done",
		Data: json.RawMessage(`{"commandId":"c1","kind":"success"}`),
	})
	view = definition.Wire.View(state).(PlanProjection)
	if view.Active || !view.Pending {
		t.Fatalf("view = %+v, want still-pending (inactive) until plan/mode", view)
	}
	state = definition.Apply(state, session.Event{
		Type: EventPlanMode,
		Data: mustJSON(t, PlanModeData{Active: true}),
	})
	view = definition.Wire.View(state).(PlanProjection)
	if !view.Active || view.Pending {
		t.Fatalf("view = %+v, want committed active", view)
	}

	// A failed command settlement keeps nothing pending.
	failed := definition.Apply(definition.Init(session.SessionHeader{}), session.Event{
		Type: "command/run",
		Data: json.RawMessage(`{"name":"plan","commandId":"c2","args":"on"}`),
	})
	failed = definition.Apply(failed, session.Event{
		Type: "command/done",
		Data: json.RawMessage(`{"commandId":"c2","kind":"error"}`),
	})
	view = definition.Wire.View(failed).(PlanProjection)
	if view.Pending {
		t.Fatalf("view = %+v, want the failed selection dropped", view)
	}

	// A no-change /plan re-selection (target equals logged active) is not
	// pending.
	reselect := definition.Apply(state, session.Event{
		Type: "command/run",
		Data: json.RawMessage(`{"name":"plan","commandId":"c3","args":"on"}`),
	})
	reselect = definition.Apply(reselect, session.Event{
		Type: "command/done",
		Data: json.RawMessage(`{"commandId":"c3","kind":"success"}`),
	})
	view = definition.Wire.View(reselect).(PlanProjection)
	if !view.Active || view.Pending {
		t.Fatalf("view = %+v, want target-equals-active to be idle", view)
	}

	// /plan off with args "off" targets inactive.
	off := definition.Apply(reselect, session.Event{
		Type: "command/run",
		Data: json.RawMessage(`{"name":"plan","commandId":"c4","args":"off"}`),
	})
	off = definition.Apply(off, session.Event{
		Type: "command/done",
		Data: json.RawMessage(`{"commandId":"c4","kind":"success"}`),
	})
	view = definition.Wire.View(off).(PlanProjection)
	if !view.Active || !view.Pending {
		t.Fatalf("view = %+v, want pending off", view)
	}

	// Restore path: DecodeState reifies a persisted row.
	decoded, err := definition.DecodeState(json.RawMessage(`{"active":true,"wanted":null,"running":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	view = definition.Wire.View(decoded).(PlanProjection)
	if !view.Active || view.Pending {
		t.Fatalf("restored view = %+v", view)
	}

	// Non-plan events are same-reference no-ops.
	if definition.Apply(reselect, session.Event{Type: "turn/start", Data: json.RawMessage(`{}`)}) != reselect {
		t.Fatal("non-unit events must return the same state reference")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
