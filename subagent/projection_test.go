package subagent

import (
	"encoding/json"
	"testing"

	"dshgo/session"
	"dshgo/session/projection"
)

// projectionDescriptorEvent builds one subagent/descriptor event with a
// valid current-version payload.
func projectionDescriptorEvent(t *testing.T, mode string, label string, seq int64, at int64) session.Event {
	t.Helper()
	descriptor := SubagentDescriptorData{Version: SubagentDescriptorVersion, Mode: mode}
	if label != "" {
		descriptor.Label = &label
	}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatalf("marshal descriptor: %v", err)
	}
	return session.Event{Type: EventSubagentDescriptor, Seq: seq, Time: at, Data: raw}
}

func timedEvent(eventType string, seq int64, at int64) session.Event {
	return session.Event{Type: eventType, Seq: seq, Time: at}
}

func foldAll[S any](def projection.Unit[S], events []session.Event) S {
	state := def.Init(session.SessionHeader{})
	for _, event := range events {
		state, _ = def.Apply(state, event)
	}
	return state
}

func TestIdentityProjectionLastWinsAndReset(t *testing.T) {
	events := []session.Event{
		// Fork seed: an ancestor descriptor is folded first.
		projectionDescriptorEvent(t, ModeContinuable, "Ancient", 0, 10),
		// The child's own descriptor overrides it, last-wins.
		projectionDescriptorEvent(t, ModeContinuable, "Own", 5, 20),
	}
	state := foldAll(subagentIdentityUnit, events)
	if state.Identity == nil || *state.Identity.Label != "Own" || state.Identity.Seq != 5 {
		t.Fatalf("identity = %+v, want own descriptor at seq 5", state.Identity)
	}
	if state.Identity.Mode != ModeContinuable {
		t.Fatalf("mode = %s", state.Identity.Mode)
	}
	// A malformed payload resets to the nil sentinel instead of inheriting.
	events = append(events, session.Event{
		Type: EventSubagentDescriptor, Seq: 6, Time: 30,
		Data: json.RawMessage(`{"version":1,"mode":"continuable"}`),
	})
	state = foldAll(subagentIdentityUnit, events)
	if state.Identity != nil {
		t.Fatalf("malformed descriptor must reset to nil, got %+v", state.Identity)
	}
	// One-shot without a label keeps the optional label unset.
	state = foldAll(subagentIdentityUnit, []session.Event{
		projectionDescriptorEvent(t, ModeOneShot, "", 3, 10),
	})
	if state.Identity == nil || state.Identity.Mode != ModeOneShot || state.Identity.Label != nil {
		t.Fatalf("one-shot identity = %+v", state.Identity)
	}
	// Non-descriptor events never change the state.
	before := foldAll(subagentIdentityUnit, events)
	after, changed := subagentIdentityUnit.Apply(before, timedEvent(session.EventTurnStart, 9, 40))
	if changed || before != after {
		t.Fatal("identity fold must be reference-stable on foreign events")
	}
}

func TestTimingProjectionFoldLadder(t *testing.T) {
	// Pre-descriptor turn: tracked as pending, closed without accumulating.
	events := []session.Event{
		timedEvent(session.EventTurnStart, 0, 50),
		timedEvent(session.EventTurnEnd, 1, 60),
	}
	state := foldAll(subagentTimingUnit, events)
	if state.PendingTurnStart != nil || state.SettledMs != 0 || state.DescriptorSeen {
		t.Fatalf("closed pre-descriptor turn = %+v", state)
	}
	// A pending turn is promoted by the child's own descriptor, then closed
	// by turn/end; later turns accumulate normally and foreign events extend
	// the open interval.
	events = append(events,
		timedEvent(session.EventTurnStart, 2, 100),
		projectionDescriptorEvent(t, ModeContinuable, "Own", 3, 150),
		timedEvent("tool/call", 4, 200),
		timedEvent(session.EventTurnEnd, 5, 250),
		timedEvent(session.EventTurnStart, 6, 300),
		timedEvent(session.EventTurnEnd, 7, 400),
	)
	state = foldAll(subagentTimingUnit, events)
	// 150 from the promoted turn (100→250) plus 100 from the second
	// (300→400).
	if state.SettledMs != 250 || state.Active != nil {
		t.Fatalf("settled = %d active = %+v, want 250 closed", state.SettledMs, state.Active)
	}
	if !state.DescriptorSeen {
		t.Fatal("descriptor must be seen")
	}
	// A second descriptor resets the accumulated state (final reset = the
	// child's authoritative origin) and promotes an open turn.
	events = append(events, projectionDescriptorEvent(t, ModeContinuable, "Reset", 8, 500))
	state = foldAll(subagentTimingUnit, events)
	if state.SettledMs != 0 || state.Active != nil || !state.DescriptorSeen {
		t.Fatalf("after reset = %+v", state)
	}
	// A negative interval clamps to zero.
	state = foldAll(subagentTimingUnit, []session.Event{
		projectionDescriptorEvent(t, ModeContinuable, "Own", 0, 10),
		timedEvent(session.EventTurnStart, 1, 100),
		timedEvent(session.EventTurnEnd, 2, 90),
	})
	if state.SettledMs != 0 || state.Active != nil {
		t.Fatalf("clamped = %+v", state)
	}
}

func TestProjectionWireViewsAndDecode(t *testing.T) {
	// Empty identity state serves the serializable nil sentinel.
	if view := subagentIdentityUnit.View(&identityState{}); view != nil {
		t.Fatalf("empty identity view = %+v, want nil", view)
	}
	identity := &SubagentIdentityProjection{Mode: ModeContinuable, Label: strPtr2("L"), Seq: 4}
	if view := subagentIdentityUnit.View(&identityState{Identity: identity}); view != identity {
		t.Fatal("identity view must pass the value through")
	}
	timingView := subagentTimingUnit.View(&timingState{SettledMs: 7, Active: &TimingInterval{Since: 1, Through: 2}}).(SubagentTimingProjection)
	if timingView.SettledMs != 7 || timingView.Active == nil || timingView.Active.Through != 2 {
		t.Fatalf("timing view = %+v", timingView)
	}
	// DecodeState: strict, guarded, and usable.
	if _, err := decodeIdentityState(json.RawMessage(`{"identity":{"mode":"continuable","label":"L","seq":1}}`)); err != nil {
		t.Fatalf("valid identity row rejected: %v", err)
	}
	if _, err := decodeIdentityState(json.RawMessage(`{"identity":{"mode":"nope","label":"L","seq":1}}`)); err == nil {
		t.Fatal("unknown mode must reject")
	}
	if _, err := decodeIdentityState(json.RawMessage(`{"identity":{"mode":"continuable","seq":1}}`)); err == nil {
		t.Fatal("continuable without label must reject")
	}
	if _, err := decodeTimingState(json.RawMessage(`{"settledMs":1,"extra":true}`)); err == nil {
		t.Fatal("unknown fields must reject")
	}
	if _, err := decodeTimingState(json.RawMessage(`{"settledMs":-1}`)); err == nil {
		t.Fatal("negative settledMs must reject")
	}
}

// strPtr2 avoids clashing with other test helpers in the package.
func strPtr2(v string) *string { return &v }

func TestRegisterSubagentProjectionsDrivesThroughRegistry(t *testing.T) {
	registry := projection.NewRegistry()
	undo, err := RegisterSubagentProjections(registry)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	header := session.SessionHeader{Version: 0, ID: "proj-child", CWD: "D:\\work"}
	sess, err := session.NewDetached("proj-child", nil, &header)
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	drive := func(event session.Event) {
		appended, appendErr := sess.Append(event.Type, event.Data, nil)
		if appendErr != nil {
			t.Fatalf("append: %v", appendErr)
		}
		registry.Drive(sess, appended)
	}
	drive(projectionDescriptorEvent(t, ModeContinuable, "Drive", 0, 0))
	drive(timedEvent(session.EventTurnStart, 0, 0))
	drive(timedEvent(session.EventTurnEnd, 0, 0))
	snapshot := registry.Snapshot(sess, ProjectionKeySubagent, ProjectionKeySubagentTiming)
	identity, ok := snapshot.Values[ProjectionKeySubagent].(*SubagentIdentityProjection)
	if !ok || identity == nil || *identity.Label != "Drive" {
		t.Fatalf("subagent value = %+v", snapshot.Values[ProjectionKeySubagent])
	}
	timing, ok := snapshot.Values[ProjectionKeySubagentTiming].(SubagentTimingProjection)
	if !ok || timing.SettledMs < 0 {
		t.Fatalf("timing value = %+v", snapshot.Values[ProjectionKeySubagentTiming])
	}
}
