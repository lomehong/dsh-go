package projection

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dshgo/session"
)

// --- fold fixtures -----------------------------------------------------------

// countState counts the events a fold consumed; pointer identity is the
// change gate (a fresh pointer on every change, same pointer when idle).
type countState struct {
	N int
}

func countDef(key string, stateVersion int, wire *WireView) Definition {
	return Definition{
		Key:          key,
		StateVersion: stateVersion,
		Init:         func(session.SessionHeader) any { return &countState{} },
		Apply: func(state any, event session.Event) any {
			current := state.(*countState)
			return &countState{N: current.N + 1}
		},
		Wire:        wire,
		DecodeState: decodeCountState,
	}
}

func decodeCountState(raw json.RawMessage) (any, error) {
	var state countState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func countWire() *WireView {
	return &WireView{View: func(state any) any {
		return map[string]any{"count": state.(*countState).N}
	}}
}

func testHeader(id session.SessionID) session.SessionHeader {
	return session.SessionHeader{ID: id, Version: session.SESSION_FORMAT_VERSION, CreatedAt: 42, CWD: "D:\\work"}
}

func newTestSession(t *testing.T, id string) *session.Session {
	t.Helper()
	header := testHeader(session.SessionID(id))
	sess, err := session.NewDetached(session.SessionID(id), nil, &header)
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	return sess
}

// appendTurn folds one completed turn into the log (non-surface events).
func appendTurn(t *testing.T, sess *session.Session, turn int) {
	t.Helper()
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": turn}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if _, err := sess.Append(session.EventTurnEnd, map[string]any{"turn": turn, "reason": map[string]any{"kind": "completed"}}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
}

// --- registration ------------------------------------------------------------

func TestRegisterValidatesAndRefCounts(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Register(countDef("bad", -1, nil)); err == nil || !strings.Contains(err.Error(), "non-negative integer") {
		t.Fatalf("negative stateVersion err = %v", err)
	}
	dispose1, err := registry.Register(countDef("count", 3, nil))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Same key + same version shares one unit.
	dispose2, err := registry.Register(countDef("count", 3, countWire()))
	if err != nil {
		t.Fatalf("register share: %v", err)
	}
	// Same key + different version fails loud instead of silently sharing.
	if _, err := registry.Register(countDef("count", 4, nil)); err == nil || !strings.Contains(err.Error(), "already registered at stateVersion 3") {
		t.Fatalf("version clash err = %v", err)
	}
	sess := newTestSession(t, "s1")
	appendTurn(t, sess, 1)
	registry.Drive(sess, sess.Events()[0])
	if _, ok := registry.StateOf(sess, "count"); !ok {
		t.Fatal("shared unit missing")
	}
	dispose1()
	if _, ok := registry.StateOf(sess, "count"); !ok {
		t.Fatal("key removed before the last registrant unloaded")
	}
	dispose2()
	if _, ok := registry.StateOf(sess, "count"); ok {
		t.Fatal("key survived its last unregistration")
	}
	// Re-registering after full removal builds a fresh unit.
	if _, err := registry.Register(countDef("count", 3, nil)); err != nil {
		t.Fatalf("re-register: %v", err)
	}
}

// --- drive + snapshot --------------------------------------------------------

func TestDriveFoldsAndNotifiesOnChange(t *testing.T) {
	registry := NewRegistry()
	dispose, err := registry.Register(countDef("count", 0, countWire()))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	// Host-only units fold but never appear in client values.
	hostDispose, err := registry.Register(countDef("host", 0, nil))
	if err != nil {
		t.Fatalf("register host: %v", err)
	}
	defer hostDispose()

	sess := newTestSession(t, "s1")
	var notified []string
	var mu sync.Mutex
	unsubscribe := registry.OnChanged(func(_ *session.Session, key string, value any, seq int64) {
		mu.Lock()
		defer mu.Unlock()
		notified = append(notified, key)
		if int64(value.(map[string]any)["count"].(int)) != seq+1 {
			t.Errorf("notification value %v does not match seq %d", value, seq)
		}
	})
	appendTurn(t, sess, 1)
	for _, event := range sess.Events() {
		registry.Drive(sess, event)
	}
	mu.Lock()
	if len(notified) != 2 {
		t.Fatalf("notifications = %v (one per changed event)", notified)
	}
	mu.Unlock()
	unsubscribe()

	// The drive is idempotent per event: an already-observed seq is skipped.
	registry.Drive(sess, sess.Events()[0])
	state, ok := registry.StateOf(sess, "count")
	if !ok || state.(*countState).N != 2 {
		t.Fatalf("state = %+v (replayed event must not refold)", state)
	}
	snapshot := registry.Snapshot(sess)
	if snapshot.AsOfSeq != 1 {
		t.Fatalf("asOfSeq = %d (cursor-1)", snapshot.AsOfSeq)
	}
	if snapshot.Values["count"].(map[string]any)["count"] != 2 {
		t.Fatalf("values = %+v", snapshot.Values)
	}
	// Host-only keys never surface in client values.
	if _, present := snapshot.Values["host"]; present {
		t.Fatal("host-only unit surfaced in client values")
	}
	if _, ok := registry.StateOf(sess, "host"); !ok {
		t.Fatal("host-only unit lost its state")
	}
}

func TestSnapshotSelectsKeysAndEmptyLogIsMinusOne(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Register(countDef("a", 0, countWire())); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, err := registry.Register(countDef("b", 0, countWire())); err != nil {
		t.Fatalf("register b: %v", err)
	}
	sess := newTestSession(t, "s1")
	empty := registry.Snapshot(sess)
	if empty.AsOfSeq != -1 || len(empty.Values) != 2 {
		t.Fatalf("empty = %+v", empty)
	}
	appendTurn(t, sess, 1)
	for _, event := range sess.Events() {
		registry.Drive(sess, event)
	}
	selected := registry.Snapshot(sess, "a")
	if len(selected.Values) != 1 {
		t.Fatalf("selected = %+v", selected.Values)
	}
}

func TestLateRegistrationFoldsHistory(t *testing.T) {
	registry := NewRegistry()
	sess := newTestSession(t, "s1")
	appendTurn(t, sess, 1)
	for _, event := range sess.Events() {
		registry.Drive(sess, event)
	}
	// A unit registered after events flowed folds init over the log on
	// first touch.
	dispose, err := registry.Register(countDef("late", 0, countWire()))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	state, ok := registry.StateOf(sess, "late")
	if !ok || state.(*countState).N != 2 {
		t.Fatalf("late state = %+v", state)
	}
	// The next events advance exactly once more (the drive gate advances
	// the cell through the prefix, then folds the new event).
	appendTurn(t, sess, 2)
	events := sess.Events()
	registry.Drive(sess, events[len(events)-1])
	state, _ = registry.StateOf(sess, "late")
	if state.(*countState).N != 4 {
		t.Fatalf("late state after next event = %+v", state)
	}
}

// --- checkpoint / restore ----------------------------------------------------

func TestCheckpointDetachesAndFloors(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Register(countDef("a", 2, countWire())); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, err := registry.Register(countDef("b", 5, nil)); err != nil {
		t.Fatalf("register b: %v", err)
	}
	sess := newTestSession(t, "s1")
	rows, err := registry.Checkpoint(sess)
	if err != nil {
		t.Fatalf("checkpoint empty: %v", err)
	}
	if rows["a"].Seq != -1 || rows["a"].Ver != 2 {
		t.Fatalf("empty row = %+v", rows["a"])
	}
	appendTurn(t, sess, 1)
	for _, event := range sess.Events() {
		registry.Drive(sess, event)
	}
	rows, err = registry.Checkpoint(sess)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if rows["a"].Seq != 1 || rows["b"].Seq != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	var decoded countState
	if err := json.Unmarshal(rows["a"].Val, &decoded); err != nil || decoded.N != 2 {
		t.Fatalf("row val = %s (%v)", rows["a"].Val, err)
	}

	// restoreFloor: one below the lowest usable watermark.
	floor, ok := registry.RestoreFloor(rows)
	if !ok || floor != 1 {
		t.Fatalf("floor = %d, %v (min(seq+1)-1)", floor, ok)
	}
	// A missing or version-mismatched row pulls the floor to 0.
	rows["b"] = Row{Ver: 4, Seq: 1}
	floor, _ = registry.RestoreFloor(rows)
	if floor != 0 {
		t.Fatalf("stale-row floor = %d", floor)
	}
	// No registered units: no read needed.
	empty := NewRegistry()
	if _, ok := empty.RestoreFloor(rows); ok {
		t.Fatal("empty registry claims a floor")
	}
}

func TestRestoreSeedsUsableRowsAndRejectsHoles(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Register(countDef("count", 1, countWire())); err != nil {
		t.Fatalf("register: %v", err)
	}
	events := []session.Event{
		{Type: session.EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: session.EventTurnEnd, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
		{Type: session.EventTurnStart, Seq: 2, Time: 3, Data: json.RawMessage(`{"turn":2}`)},
	}
	usable := Checkpoint{"count": {Ver: 1, Seq: 1, Val: json.RawMessage(`{"N":2}`)}}
	restored, err := registry.Restore(usable, events[2:], 2, testHeader("r1"))
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.Snapshot.AsOfSeq != 2 {
		t.Fatalf("asOfSeq = %d", restored.Snapshot.AsOfSeq)
	}
	// The row seeded the fold: only the tail event passed through Apply.
	if restored.Snapshot.Values["count"].(map[string]any)["count"] != 3 {
		t.Fatalf("values = %+v", restored.Snapshot.Values)
	}
	if restored.Checkpoint["count"].Seq != 2 {
		t.Fatalf("refreshed = %+v", restored.Checkpoint["count"])
	}

	// An unusable row with baseSeq > 0 refuses: refolding soundly needs the
	// full log, so the caller must re-read from seq 0.
	stale := Checkpoint{"count": {Ver: 9, Seq: 1, Val: json.RawMessage(`{"N":2}`)}}
	if _, err := registry.Restore(stale, events[2:], 2, testHeader("r1")); err == nil || !strings.Contains(err.Error(), "re-read from seq 0") {
		t.Fatalf("stale row err = %v", err)
	}
	// The same stale row at baseSeq 0 is fine: refold from init is sound.
	refolded, err := registry.Restore(stale, events, 0, testHeader("r1"))
	if err != nil || refolded.Snapshot.Values["count"].(map[string]any)["count"] != 3 {
		t.Fatalf("refold = %+v, %v", refolded, err)
	}

	// A row claiming events past the supplied end is unusable, but at
	// baseSeq 0 refolding from init is sound — no error, full replay.
	overreaching := Checkpoint{"count": {Ver: 1, Seq: 5, Val: json.RawMessage(`{"N":6}`)}}
	refolded2, err := registry.Restore(overreaching, events, 0, testHeader("r1"))
	if err != nil || refolded2.Snapshot.Values["count"].(map[string]any)["count"] != 3 {
		t.Fatalf("overreaching refold = %+v, %v", refolded2, err)
	}
	// The same overreaching row over a suffix read fails loud: the
	// discarded row could only refold soundly from the full log.
	if _, err := registry.Restore(overreaching, events[2:], 2, testHeader("r1")); err == nil || !strings.Contains(err.Error(), "re-read from seq 0") {
		t.Fatalf("overreaching suffix err = %v", err)
	}

	// A hole in the supplied events fails loud.
	if _, err := registry.Restore(nil, []session.Event{events[0], events[2]}, 0, testHeader("r1")); err == nil || !strings.Contains(err.Error(), "missing seq 1") {
		t.Fatalf("hole err = %v", err)
	}
}

func TestHydrateInstallsCellsAndShortCircuits(t *testing.T) {
	registry := NewRegistry()
	dispose, err := registry.Register(countDef("count", 1, countWire()))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	// The prepared session OWNS the restored log prefix (the official
	// hydrate precondition): the constructor seeds two turn events plus the
	// end-seed marker, and every logged event — marker included — passes
	// through the fold.
	seed := []session.Event{
		{Type: session.EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: session.EventTurnEnd, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	header := testHeader("h1")
	sess, err := session.NewDetached("h1", seed, &header)
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	logEvents := sess.Events() // 2 seed events + the end-seed marker
	rows := Checkpoint{"count": {Ver: 1, Seq: 2, Val: json.RawMessage(`{"N":3}`)}}
	snapshot, err := registry.Hydrate(sess, rows, logEvents, 0)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if snapshot.AsOfSeq != 2 || snapshot.Values["count"].(map[string]any)["count"] != 3 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// A second hydrate at the same cut is a no-op short circuit.
	again, err := registry.Hydrate(sess, rows, logEvents, 0)
	if err != nil || again.AsOfSeq != 2 {
		t.Fatalf("second hydrate = %+v, %v", again, err)
	}
	// Ordinary live drive advances the installed cells exactly once.
	third, err := sess.Append(session.EventTurnStart, map[string]any{"turn": 2}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	registry.Drive(sess, third)
	state, _ := registry.StateOf(sess, "count")
	if state.(*countState).N != 4 {
		t.Fatalf("state after live drive = %+v", state)
	}
}

// --- cached snapshot + view checkpoint ---------------------------------------

func TestCachedSnapshotIsHintsOnly(t *testing.T) {
	registry := NewRegistry()
	dispose, err := registry.Register(countDef("count", 0, countWire()))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	sess := newTestSession(t, "s1")
	appendTurn(t, sess, 1)
	// Nothing driven yet: no materialized cells, no cached snapshot.
	if _, ok := registry.CachedSnapshot(sess); ok {
		t.Fatal("cached snapshot exists before any drive")
	}
	registry.Drive(sess, sess.Events()[0])
	snapshot, ok := registry.CachedSnapshot(sess)
	if !ok || snapshot.AsOfSeq != 0 {
		t.Fatalf("cached = %+v, %v", snapshot, ok)
	}
}

func TestViewCheckpointFiltersUnusableRows(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.Register(countDef("count", 7, countWire())); err != nil {
		t.Fatalf("register: %v", err)
	}
	values := registry.ViewCheckpoint(Checkpoint{
		"count":  {Ver: 7, Seq: 4, Val: json.RawMessage(`{"N":9}`)},
		"stale":  {Ver: 6, Seq: 4, Val: json.RawMessage(`{"N":1}`)},
		"broken": {Ver: 7, Seq: 4, Val: json.RawMessage(`not json`)},
	})
	if len(values) != 1 || values["count"].(map[string]any)["count"] != 9 {
		t.Fatalf("values = %+v", values)
	}
}
