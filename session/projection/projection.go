// Package projection ports the session-projection capability seam: the
// registry that drives every registered unit's pure fold forward eagerly
// over committed session events, the per-session watermark cache, and the
// checkpoint/restore contract shared with the persisted projection cache.
// Port of packages/session/session-projection/src/index.ts at tag
// dsh-v0.1.2-alpha.1.
//
// Whole-value event rule (load-bearing): a state-carrying log event carries
// the complete post-change state, never a bare delta — every unit's
// transition stays trivially cheap and every served value self-describing.
//
// Go adaptation: the merge-extensible SessionProjectionStateMap /
// SessionProjectionMap type tables become string keys with erased `any`
// states; the zod stateSchema role becomes Definition.DecodeState, which
// both validates and reifies a persisted row value. Checkpoint values are
// detached by a JSON round-trip (the plain-JSON unit contract), so
// checkpoint returns json.RawMessage rows — the exact shape the persisted
// cache stores.
package projection

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"dshgo/cordis"
	"dshgo/session"
)

// WireView is one client-visible unit's read side.
type WireView struct {
	// View maps the current state to the whole client value.
	View func(state any) any
}

// Definition is one domain's state-driven computation unit: a pure
// synchronous fold plus declarations and an optional client view. Apply must
// return either the same state reference it received (zero downstream work)
// or a fresh value; reference identity is the change gate.
type Definition struct {
	// Key is the projection key this unit owns.
	Key string
	// StateVersion guards persisted rows: bump whenever the serialized
	// state fields or fold semantics change. Must be a non-negative integer.
	StateVersion int
	// Init builds the state for the empty log from immutable metadata.
	Init func(header session.SessionHeader) any
	// Apply is the pure transition: previous state + one committed event →
	// next state (same reference when the event is not the unit's).
	Apply func(state any, event session.Event) any
	// Wire is the client view; nil for host-only units.
	Wire *WireView
	// DecodeState validates and reifies a persisted row value (the zod
	// stateSchema role: parse both checks and returns). Required for units
	// that participate in the persisted cache; a nil DecodeState makes
	// every restored row unusable for this unit.
	DecodeState func(raw json.RawMessage) (any, error)
}

// Unit is the typed authoring surface for one projection unit. Apply is the
// pure transition returning the next state and an explicit changed flag:
// false passes the previous state through untouched (zero downstream work,
// no change-feed notification), true publishes next. The flag replaces the
// erased reference-identity gate a hand-built Definition relies on — a fold
// that allocates a fresh-but-unchanged state can no longer lie about
// changing.
type Unit[S any] struct {
	// Key is the projection key this unit owns.
	Key string
	// StateVersion guards persisted rows: bump whenever the serialized
	// state fields or fold semantics change. Must be a non-negative integer.
	StateVersion int
	// Init builds the state for the empty log from immutable metadata.
	Init func(header session.SessionHeader) S
	// Apply is the pure transition: previous state + one committed event →
	// (next state, changed). Uninteresting events return (state, false).
	Apply func(state S, event session.Event) (S, bool)
	// View maps the current state to the whole client value; nil for
	// host-only units.
	View func(state S) any
	// DecodeState validates and reifies a persisted row value (the zod
	// stateSchema role); nil makes every restored row unusable for this
	// unit.
	DecodeState func(raw json.RawMessage) (S, error)
}

// Definition erases the typed unit to the registry's runtime record. The
// any assertions live here, at the type boundary, and are guaranteed by
// construction: every state the registry holds for this unit was produced
// by the same unit's Init, Apply, or DecodeState. A false changed flag
// returns the previous state value verbatim, preserving the registry's
// reference-identity fast path exactly.
func (u Unit[S]) Definition() Definition {
	erased := Definition{
		Key:          u.Key,
		StateVersion: u.StateVersion,
		Init: func(header session.SessionHeader) any {
			return u.Init(header)
		},
		Apply: func(state any, event session.Event) any {
			current, _ := state.(S)
			next, changed := u.Apply(current, event)
			if !changed {
				return state
			}
			return next
		},
	}
	if u.View != nil {
		erased.Wire = &WireView{View: func(state any) any {
			current, _ := state.(S)
			return u.View(current)
		}}
	}
	if u.DecodeState != nil {
		erased.DecodeState = func(raw json.RawMessage) (any, error) {
			return u.DecodeState(raw)
		}
	}
	return erased
}

// Row is one unit's checkpoint: its detached state (plain JSON), the seq of
// the last event folded into it, and the unit StateVersion that produced it
// — the persisted `(sessionId, key, ver, seq, val)` row minus the two outer
// keys. A row is never authoritative, only a fold shortcut.
type Row struct {
	Ver int
	Seq int64
	Val json.RawMessage
}

// Checkpoint maps projection keys to their rows (one session's persisted
// cache value).
type Checkpoint map[string]Row

// Snapshot is one consistent read cut over registered client-visible units
// for one session.
type Snapshot struct {
	// AsOfSeq is the seq of the last event every value reflects (-1 for an
	// empty log).
	AsOfSeq int64
	// Values holds the whole current client value per registered key.
	Values map[string]any
}

// ChangeListener is one unit's change-feed callback: value is the view
// output; seq is the unit's watermark at emission.
type ChangeListener func(sess *session.Session, key string, value any, seq int64)

// SessionEventPayload is the `session/event` cordis payload the registry
// drives on.
type SessionEventPayload struct {
	Session *session.Session
	Event   session.Event
}

// SessionCreatedPayload is the `session/created` cordis payload.
type SessionCreatedPayload struct {
	Session *session.Session
}

// SessionDisposedPayload is the `session/disposed` cordis payload (the
// live-to-cold moment).
type SessionDisposedPayload struct {
	Session *session.Session
}

// unitCell is one per-session per-unit watermark cache row.
type unitCell struct {
	state any
	// observedSeq is the seq of the last event passed through Apply
	// (regardless of change).
	observedSeq int64
	// views is the change-feed comparison buffer: [previousView,
	// currentView]. A nil slot means no cached comparison. A changed state
	// whose raw view is identical to the previous cached one does not fire
	// the feed, so a unit can buffer working fields in state behind an
	// identity-stable projection.
	views [2]any
}

// registration is one live unit plus its per-session cells, ref-counted
// because one definition serves every session while registrants are
// per-session: the key survives until the last registrant unloads.
type registration struct {
	def   Definition
	cells map[*session.Session]*unitCell
	refs  int
}

// Registry is the projection unit table and its eager drive. Attach
// subscribes to `session/created` and `session/event`; every committed event
// passes every registered unit's Apply, and a changed state reference in a
// client-visible unit notifies the change feed.
type Registry struct {
	mu            sync.Mutex
	registrations map[string]*registration
	// order preserves registration order for deterministic drives, reads,
	// and checkpoints (the JS Map iteration order contract).
	order []string
	// listeners preserves subscription order for deterministic
	// notification; Go function values cannot key a map.
	listeners []ChangeListener
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		registrations: map[string]*registration{},
	}
}

// Attach subscribes the registry to a cordis context's session events. The
// returned disposer removes both listeners.
func (r *Registry) Attach(ctx *cordis.Context) func() {
	created := ctx.On("session/created", func(value any, next func(any) any) any {
		if payload, ok := value.(*SessionCreatedPayload); ok {
			r.onSessionCreated(payload.Session)
		}
		return next(value)
	})
	event := ctx.On("session/event", func(value any, next func(any) any) any {
		if payload, ok := value.(*SessionEventPayload); ok {
			r.Drive(payload.Session, payload.Event)
		}
		return next(value)
	})
	return func() { created(); event() }
}

// onSessionCreated seeds every unit's cell for a brand-new session (cursor
// at 0 means nothing has been logged yet).
func (r *Registry) onSessionCreated(sess *session.Session) {
	if sess.Seq() != 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range r.order {
		reg := r.registrations[key]
		if _, ok := reg.cells[sess]; !ok {
			reg.cells[sess] = &unitCell{state: reg.def.Init(sess.Header()), observedSeq: -1}
		}
	}
}

// Register adds one domain's unit. Duplicate keys must agree on
// StateVersion; the returned disposer unregisters one registrant, and the
// unit (with its cached cells) disappears when the last one unloads.
func (r *Registry) Register(def Definition) (func(), error) {
	if def.StateVersion < 0 {
		return nil, fmt.Errorf("session projection %q stateVersion must be a non-negative integer, got %d", def.Key, def.StateVersion)
	}
	if def.Init == nil || def.Apply == nil {
		return nil, fmt.Errorf("session projection %q must define Init and Apply", def.Key)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.registrations[def.Key]; ok {
		if existing.def.StateVersion != def.StateVersion {
			return nil, fmt.Errorf("session projection key %q is already registered at stateVersion %d; refusing to share it with stateVersion %d", def.Key, existing.def.StateVersion, def.StateVersion)
		}
		existing.refs++
	} else {
		r.registrations[def.Key] = &registration{def: def, cells: map[*session.Session]*unitCell{}, refs: 1}
		r.order = append(r.order, def.Key)
	}
	return func() { r.unregister(def.Key) }, nil
}

func (r *Registry) unregister(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	live, ok := r.registrations[key]
	if !ok {
		return // the disposer runs once per successful registration
	}
	live.refs--
	if live.refs == 0 {
		delete(r.registrations, key)
		for i, k := range r.order {
			if k == key {
				r.order = append(r.order[:i], r.order[i+1:]...)
				break
			}
		}
	}
}

// OnChanged subscribes to the change feed; the returned disposer
// unsubscribes exactly the listener it registered.
func (r *Registry) OnChanged(listener ChangeListener) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listeners = append(r.listeners, listener)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, l := range r.listeners {
			if sameFunc(l, listener) {
				r.listeners = append(r.listeners[:i], r.listeners[i+1:]...)
				return
			}
		}
	}
}

func sameFunc(a, b ChangeListener) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// StateOf reads one unit's current host state after materializing every
// registered unit at the session cursor. ok=false when the key is not
// registered. The returned value is live; callers must not mutate it.
func (r *Registry) StateOf(sess *session.Session, key string) (any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg, ok := r.registrations[key]
	if !ok {
		return nil, false
	}
	r.materializeCellsLocked(sess)
	return reg.cells[sess].state, true
}

// Snapshot reads one consistent cut over every registered client-visible
// unit, materializing all cells at the session cursor. keys optionally
// selects the client-visible outputs; state materialization stays complete.
func (r *Registry) Snapshot(sess *session.Session, keys ...string) Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.materializeCellsLocked(sess)
	return r.snapshotLocked(sess, keys)
}

func (r *Registry) snapshotLocked(sess *session.Session, keys []string) Snapshot {
	selected := keySet(keys)
	values := map[string]any{}
	for _, key := range r.order {
		reg := r.registrations[key]
		if reg.def.Wire == nil {
			continue
		}
		if selected != nil && !selected[key] {
			continue
		}
		values[key] = reg.viewCell(reg.cells[sess])
	}
	return Snapshot{AsOfSeq: int64(sess.Seq() - 1), Values: values}
}

// CachedSnapshot reads only already-materialized client-visible cells
// without folding history. Values may trail the live session and are
// therefore hints. ok=false when no wire cell exists.
func (r *Registry) CachedSnapshot(sess *session.Session, keys ...string) (Snapshot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	selected := keySet(keys)
	values := map[string]any{}
	var asOfSeq int64
	found := false
	for _, key := range r.order {
		reg := r.registrations[key]
		if reg.def.Wire == nil {
			continue
		}
		if selected != nil && !selected[key] {
			continue
		}
		cell, ok := reg.cells[sess]
		if !ok {
			continue
		}
		values[key] = reg.viewCell(cell)
		if !found || cell.observedSeq < asOfSeq {
			asOfSeq = cell.observedSeq
		}
		found = true
	}
	if !found {
		return Snapshot{}, false
	}
	return Snapshot{AsOfSeq: asOfSeq, Values: values}, true
}

// Checkpoint writes one session's every unit state as detached rows. The
// JSON round-trip is the detachment AND the plain-JSON contract check: a
// non-serializable state fails loud here instead of failing the later
// durable write.
func (r *Registry) Checkpoint(sess *session.Session) (Checkpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := Checkpoint{}
	for _, key := range r.order {
		reg := r.registrations[key]
		cell := r.cellForLocked(reg, sess)
		detached, err := json.Marshal(cell.state)
		if err != nil {
			return nil, fmt.Errorf("projection checkpoint for %q is not losslessly JSON-serializable (a unit state violates the plain-JSON contract): %w", key, err)
		}
		rows[key] = Row{Ver: reg.def.StateVersion, Seq: cell.observedSeq, Val: detached}
	}
	return rows, nil
}

// RestoreFloor is the stored seq a restore tail read must start at: one
// event below the lowest usable watermark, so the tail proves how far the
// stored log still extends and a shrunk log (crash-repair truncation)
// rejects instead of serving stale rows. ok=false when no unit is
// registered (no read needed).
func (r *Registry) RestoreFloor(checkpoint Checkpoint) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.order) == 0 {
		return 0, false
	}
	var floor int64
	first := true
	for _, key := range r.order {
		reg := r.registrations[key]
		var need int64
		if row, ok := checkpoint[key]; ok && row.Ver == reg.def.StateVersion {
			need = row.Seq + 1
			if need < 0 {
				need = 0
			}
		}
		if first || need < floor {
			floor = need
			first = false
		}
	}
	floor--
	if floor < 0 {
		floor = 0
	}
	return floor, true
}

// ViewCheckpoint reads a checkpoint's rows without any log read: every
// registered client-visible unit whose row's version matches serves the
// view of the validated stored state; mismatched, malformed, or absent rows
// leave their key absent. The zero-I/O rung of the read ladder — values are
// as stale as their rows, never wrong.
func (r *Registry) ViewCheckpoint(checkpoint Checkpoint, keys ...string) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	selected := keySet(keys)
	values := map[string]any{}
	for _, key := range r.order {
		reg := r.registrations[key]
		if reg.def.Wire == nil {
			continue
		}
		if selected != nil && !selected[key] {
			continue
		}
		row, ok := checkpoint[key]
		if !ok || row.Ver != reg.def.StateVersion || reg.def.DecodeState == nil {
			continue
		}
		state, err := reg.def.DecodeState(row.Val)
		if err != nil {
			continue
		}
		values[key] = reg.def.Wire.View(state)
	}
	return values
}

// RestoreResult pairs the restored cut with refreshed checkpoint rows ready
// for a durable write-back.
type RestoreResult struct {
	Snapshot   Snapshot
	Checkpoint Checkpoint
}

// Restore folds every persisted unit over a stored log suffix, seeding each
// from its checkpoint row when usable — one read recipe (cached state +
// forward tail replay + view) without a live session. A row is usable iff
// its version matches, it does not predate baseSeq (Seq >= baseSeq-1), and
// it does not claim events past the supplied end (Seq <= endSeq); an
// unusable row with baseSeq > 0 fails loud — a discarded row could only
// refold soundly from the full log, so the caller re-reads from seq 0.
func (r *Registry) Restore(checkpoint Checkpoint, events []session.Event, baseSeq int64, header session.SessionHeader) (RestoreResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.restoreLocked(checkpoint, events, baseSeq, header)
}

func (r *Registry) restoreLocked(checkpoint Checkpoint, events []session.Event, baseSeq int64, header session.SessionHeader) (RestoreResult, error) {
	endSeq := baseSeq - 1
	if len(events) > 0 {
		endSeq = events[len(events)-1].Seq
	}
	values := map[string]any{}
	refreshed := Checkpoint{}
	for _, key := range r.order {
		reg := r.registrations[key]
		def := reg.def
		row, hasRow := checkpoint[key]
		usable := hasRow && row.Ver == def.StateVersion && row.Seq >= baseSeq-1 && row.Seq <= endSeq && def.DecodeState != nil
		if !usable && baseSeq > 0 {
			return RestoreResult{}, fmt.Errorf(
				"session projection %q cannot restore from seq %d: its checkpoint row is missing, version-mismatched, or beyond the supplied log end; re-read from seq 0", key, baseSeq)
		}
		var state any
		var from int64
		if usable {
			decoded, err := def.DecodeState(row.Val)
			if err != nil {
				return RestoreResult{}, fmt.Errorf("session projection %q checkpoint row failed validation: %w", key, err)
			}
			state = decoded
			from = row.Seq
		} else {
			state = def.Init(header)
			from = baseSeq - 1
		}
		startIndex := from - baseSeq + 1
		for index := startIndex; index < int64(len(events)); index++ {
			event := events[index]
			expectedSeq := baseSeq + index
			if event.Seq != expectedSeq {
				return RestoreResult{}, fmt.Errorf("session projection %q cannot restore across missing seq %d", key, expectedSeq)
			}
			state = def.Apply(state, event)
		}
		if def.Wire != nil {
			values[key] = def.Wire.View(state)
		}
		refreshed[key] = Row{Ver: def.StateVersion, Seq: endSeq, Val: mustMarshal(key, state)}
	}
	return RestoreResult{
		Snapshot:   Snapshot{AsOfSeq: endSeq, Values: values},
		Checkpoint: refreshed,
	}, nil
}

// Hydrate restores an exact cut and installs its states on the supplied
// prepared session; a later publication reuses these cells, and ordinary
// live reads advance any remaining suffix exactly once.
func (r *Registry) Hydrate(sess *session.Session, checkpoint Checkpoint, events []session.Event, baseSeq int64) (Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	endSeq := baseSeq - 1
	if len(events) > 0 {
		endSeq = events[len(events)-1].Seq
	}
	complete := len(r.order) > 0
	for _, key := range r.order {
		reg := r.registrations[key]
		cell, ok := reg.cells[sess]
		if !ok || cell.observedSeq != endSeq {
			complete = false
			break
		}
	}
	if complete {
		values := map[string]any{}
		for _, key := range r.order {
			reg := r.registrations[key]
			if reg.def.Wire == nil {
				continue
			}
			values[key] = reg.viewCell(reg.cells[sess])
		}
		return Snapshot{AsOfSeq: endSeq, Values: values}, nil
	}
	restored, err := r.restoreLocked(checkpoint, events, baseSeq, sess.Header())
	if err != nil {
		return Snapshot{}, err
	}
	for _, key := range r.order {
		row, ok := restored.Checkpoint[key]
		if !ok {
			continue
		}
		reg := r.registrations[key]
		if current, exists := reg.cells[sess]; exists && current.observedSeq > row.Seq {
			continue
		}
		decoded, err := reg.def.DecodeState(row.Val)
		if err != nil {
			return Snapshot{}, fmt.Errorf("session projection %q refreshed row failed validation: %w", key, err)
		}
		reg.cells[sess] = &unitCell{state: decoded, observedSeq: row.Seq}
	}
	return restored.Snapshot, nil
}

// Drive passes one committed event through every registered unit (the eager
// drive) and notifies changed client-visible units.
func (r *Registry) Drive(sess *session.Session, event session.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	events := sess.Events()
	type notification struct {
		reg   *registration
		value any
		seq   int64
	}
	var notifications []notification
	for _, key := range r.order {
		reg := r.registrations[key]
		cell, ok := reg.cells[sess]
		if ok && cell.observedSeq >= event.Seq {
			continue
		}
		if !ok {
			// Late build mid-stream: fold history before this event (seq =
			// log index, so the prefix slice is exact).
			cell = r.buildCellLocked(reg.def, sess.Header(), events[:event.Seq])
			reg.cells[sess] = cell
		} else {
			if err := r.advanceCellLocked(reg.def, cell, events, event.Seq-1); err != nil {
				// The session log is contiguous by contract; a hole can
				// only mean an inconsistent caller-supplied event.
				panic(projectionError{key: key, err: err})
			}
		}
		next := reg.def.Apply(cell.state, event)
		changed := !sameReference(next, cell.state)
		cell.state = next
		cell.observedSeq = event.Seq
		if changed && reg.def.Wire != nil {
			// The view gate (upstream alpha.3, ceadd90e71+8322f804cb):
			// baseline advances on every changed state, heard or not;
			// without listeners the comparison is invalidated so the next
			// heard change always fires. A changed state whose raw view is
			// identical to the last delivered one stays quiet.
			cell.views[0] = cell.views[1]
			if len(r.listeners) > 0 {
				raw := reg.def.Wire.View(cell.state)
				cell.views[1] = raw
				if !sameView(cell.views[0], cell.views[1]) {
					notifications = append(notifications, notification{reg: reg, value: raw, seq: event.Seq})
				}
			} else {
				cell.views[1] = nil
			}
		}
	}
	if len(notifications) == 0 {
		return
	}
	for _, listener := range r.listeners {
		for _, n := range notifications {
			listener(sess, n.reg.def.Key, n.value, n.seq)
		}
	}
}

// projectionError carries a drive-time fold failure out of Drive.
type projectionError struct {
	key string
	err error
}

func (e projectionError) Error() string {
	return fmt.Sprintf("session projection %q: %v", e.key, e.err)
}

// Unwrap exposes the cause.
func (e projectionError) Unwrap() error { return e.err }

// materializeCellsLocked brings every registered unit to the cursor.
func (r *Registry) materializeCellsLocked(sess *session.Session) {
	for _, key := range r.order {
		r.cellForLocked(r.registrations[key], sess)
	}
}

// cellForLocked reads or lazily builds (folding the full in-memory log) one
// unit's cell.
func (r *Registry) cellForLocked(reg *registration, sess *session.Session) *unitCell {
	cell, ok := reg.cells[sess]
	if !ok {
		events := sess.Events()
		cell = r.buildCellLocked(reg.def, sess.Header(), events)
		reg.cells[sess] = cell
		return cell
	}
	if err := r.advanceCellLocked(reg.def, cell, sess.Events(), int64(sess.Seq()-1)); err != nil {
		panic(projectionError{key: reg.def.Key, err: err})
	}
	return cell
}

// buildCellLocked folds one unit from init over events, watermarked at the
// last folded event.
func (r *Registry) buildCellLocked(def Definition, header session.SessionHeader, events []session.Event) *unitCell {
	state := def.Init(header)
	for _, event := range events {
		state = def.Apply(state, event)
	}
	var observed int64 = -1
	if len(events) > 0 {
		observed = events[len(events)-1].Seq
	}
	return &unitCell{state: state, observedSeq: observed}
}

// advanceCellLocked moves one existing cell through a contiguous prefix.
func (r *Registry) advanceCellLocked(def Definition, cell *unitCell, events []session.Event, throughSeq int64) error {
	if cell.observedSeq >= throughSeq {
		return nil
	}
	for seq := cell.observedSeq + 1; seq <= throughSeq; seq++ {
		if int(seq) >= len(events) || events[seq].Seq != seq {
			return fmt.Errorf("cannot advance across missing seq %d", seq)
		}
		cell.state = def.Apply(cell.state, events[seq])
		cell.observedSeq = seq
	}
	return nil
}

func (reg *registration) viewCell(cell *unitCell) any {
	if reg.def.Wire == nil {
		panic(fmt.Sprintf("session projection %q has no wire view", reg.def.Key))
	}
	return reg.def.Wire.View(cell.state)
}

// keySet builds an optional selection set.
func keySet(keys []string) map[string]bool {
	if len(keys) == 0 {
		return nil
	}
	set := make(map[string]bool, len(keys))
	for _, key := range keys {
		set[key] = true
	}
	return set
}

// sameReference reports reference identity for the change gate. Map, slice,
// pointer, and func states compare by runtime pointer; distinct values are
// never the same reference.
func sameReference(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() != vb.Kind() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return va.Pointer() == vb.Pointer()
	default:
		return false
	}
}

// sameView is the Object.is analogue for raw view outputs: reference kinds
// compare by runtime pointer, comparable kinds by value. An identity-stable
// view (the unit returns the same backing slice/map/pointer) reports equal;
// a fresh-but-equal value reports unequal for reference kinds, matching the
// upstream gate where only Object.is-identical projections stay quiet.
func sameView(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Kind() != vb.Kind() {
		return false
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return va.Pointer() == vb.Pointer()
	default:
		return va.Comparable() && vb.Comparable() && va.Equal(vb)
	}
}

func mustMarshal(key string, state any) json.RawMessage {
	raw, err := json.Marshal(state)
	if err != nil {
		panic(projectionError{key: key, err: fmt.Errorf("restored state is not losslessly JSON-serializable (a unit state violates the plain-JSON contract): %w", err)})
	}
	return raw
}
