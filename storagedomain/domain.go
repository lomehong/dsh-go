package storagedomain

import (
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/cordis"
)

// DomainChanged is one durable domain change, emitted once per write
// strictly after the backend acknowledged durability. Events of one domain
// arrive in its write order. A closed union — switch on Operation. It never
// carries the old value: a diffing consumer keeps its own previous snapshot.
type DomainChanged struct {
	// Domain is the owning domain name.
	Domain string
	// Table is the table name; "" for a global-singleton write.
	Table string
	// Key is the record key; "" for a global-singleton write.
	Key string
	// Operation is "put" (insert or overwrite) or "deleted" (tombstone, no
	// value).
	Operation string
	// Value is the new snapshot on "put".
	Value json.RawMessage
}

// Domain is one open domain runtime: authoritative in-memory state, the
// serialized write path, and change-event emission. Reads are synchronous
// from memory; every write serializes, awaits backend durability FIRST,
// then mutates memory, then emits `domain/changed` — a failed backend write
// leaves memory untouched (no divergence between reads and the medium), and
// events carry values that equal the in-memory state at emission, in write
// order.
//
// Listener dispatch runs OUTSIDE the state mutex: the official single-
// threaded emit lets a listener synchronously reenter the domain (read a
// snapshot, even enqueue another write), so Go dispatches from a queue
// while the mutex is released — listeners may call any Domain method
// freely, and a nested write's event rides the same queue so every event
// still arrives in commit order. Go adaptation: under concurrent writers a
// listener reading memory mid-dispatch can observe a later commit earlier
// than the official in-line emit would show it; the event payload itself
// stays the committed value.
type Domain struct {
	spec DomainSpec
	unit KvUnit

	mu        sync.Mutex
	closed    bool
	records   map[string]map[string]json.RawMessage
	global    json.RawMessage // nil = never written
	hasGlobal bool

	// emitQueue and emitting serialize out-of-lock listener dispatch in
	// commit order. Guarded by mu.
	emitQueue []DomainChanged
	emitting  bool

	listenersMu sync.Mutex
	listeners   map[int]func(DomainChanged)
	nextHandle  int

	logger   cordis.Logger
	onClosed func()
}

// Global is the handle on a domain's global singleton.
type Global struct{ domain *Domain }

// Get reads the current value, synchronously from the authoritative
// in-memory state. Before the first Set this is the spec's initial.
func (g Global) Get() json.RawMessage {
	g.domain.assertReadable()
	if !g.domain.hasGlobal {
		return nil
	}
	return g.domain.global
}

// InitialOrValue returns the initial value when never written.
func (g Global) InitialOrValue() json.RawMessage {
	g.domain.assertReadable()
	if g.domain.global == nil {
		return g.domain.spec.InitialGlobalJSON
	}
	return g.domain.global
}

// Set replaces the value durably. The first Set is what materializes the
// global on the medium.
func (g Global) Set(value json.RawMessage) error {
	d := g.domain
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.assertReadableLocked(); err != nil {
		return err
	}
	if !d.hasGlobal {
		return NewUnitError(CodeMalformedMedium, "domain '%s' declares no global slot", d.spec.Name)
	}
	if err := d.unit.SetGlobal(value); err != nil {
		return err
	}
	d.global = append(json.RawMessage(nil), value...)
	d.afterCommitLocked(DomainChanged{Domain: d.spec.Name, Table: "", Key: "", Operation: "put", Value: d.global})
	return nil
}

// Table is the handle on one declared table. Records are plain immutable
// bytes: returned values are the stored JSON itself (callers must not mutate
// in place) — replace via Put/Update.
type Table struct {
	domain *Domain
	name   string
}

// Name is the table's declared name.
func (t Table) Name() string { return t.name }

// Get reads one record, synchronously from memory: the stored JSON, or nil
// when absent.
func (t Table) Get(key string) json.RawMessage {
	t.domain.assertReadable()
	t.domain.mu.Lock()
	defer t.domain.mu.Unlock()
	return t.domain.records[t.name][key]
}

// Entries is a snapshot of [key, record] pairs — a snapshot, not a live
// view: iteration stays stable while queued writes land.
func (t Table) Entries() map[string]json.RawMessage {
	t.domain.assertReadable()
	t.domain.mu.Lock()
	defer t.domain.mu.Unlock()
	out := make(map[string]json.RawMessage, len(t.domain.records[t.name]))
	for key, value := range t.domain.records[t.name] {
		out[key] = value
	}
	return out
}

// Size is the current record count.
func (t Table) Size() int {
	t.domain.assertReadable()
	t.domain.mu.Lock()
	defer t.domain.mu.Unlock()
	return len(t.domain.records[t.name])
}

// Put inserts or overwrites one record durably: the full new record (no
// partial merge).
func (t Table) Put(key string, value json.RawMessage) error {
	d := t.domain
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.assertReadableLocked(); err != nil {
		return err
	}
	if err := d.unit.PutRecord(t.name, key, value); err != nil {
		return err
	}
	stored := append(json.RawMessage(nil), value...)
	d.records[t.name][key] = stored
	d.afterCommitLocked(DomainChanged{Domain: d.spec.Name, Table: t.name, Key: key, Operation: "put", Value: stored})
	return nil
}

// Delete deletes one record durably. It reports false when the record was
// already absent (no write and no event in that case).
func (t Table) Delete(key string) (bool, error) {
	d := t.domain
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.assertReadableLocked(); err != nil {
		return false, err
	}
	if _, exists := d.records[t.name][key]; !exists {
		return false, nil
	}
	if err := d.unit.DeleteRecord(t.name, key); err != nil {
		return false, err
	}
	delete(d.records[t.name], key)
	d.afterCommitLocked(DomainChanged{Domain: d.spec.Name, Table: t.name, Key: key, Operation: "deleted"})
	return true, nil
}

// Update is the atomic read-modify-write on the domain's write
// serialization: fn sees the value current at its slot, so concurrent
// updates never interleave. A missing key fails with `missing-key`.
func (t Table) Update(key string, fn func(current json.RawMessage) json.RawMessage) (json.RawMessage, error) {
	d := t.domain
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.assertReadableLocked(); err != nil {
		return nil, err
	}
	current, exists := d.records[t.name][key]
	if !exists {
		return nil, NewUnitError(CodeMissingKey,
			"domain '%s' table '%s' has no record '%s' to update", d.spec.Name, t.name, key)
	}
	next := fn(current)
	if err := d.unit.PutRecord(t.name, key, next); err != nil {
		return nil, err
	}
	stored := append(json.RawMessage(nil), next...)
	d.records[t.name][key] = stored
	d.afterCommitLocked(DomainChanged{Domain: d.spec.Name, Table: t.name, Key: key, Operation: "put", Value: stored})
	return stored, nil
}

// Open builds the runtime over an already-validated, already-loaded unit
// snapshot; records/global must have passed the spec's validators (the
// facility owns that boundary). onClosed runs strictly after teardown
// completes.
func Open(spec DomainSpec, unit KvUnit, tables map[string]map[string]json.RawMessage, global json.RawMessage, logger cordis.Logger, onClosed func()) *Domain {
	domain := &Domain{
		spec:      spec,
		unit:      unit,
		records:   tables,
		global:    global,
		hasGlobal: spec.HasGlobal,
		listeners: map[int]func(DomainChanged){},
		logger:    logger,
		onClosed:  onClosed,
	}
	return domain
}

// Spec is the domain's declaration.
func (d *Domain) Spec() DomainSpec { return d.spec }

// Table returns the handle on one declared table.
func (d *Domain) Table(name string) Table { return Table{domain: d, name: name} }

// Global returns the handle on the global singleton (declared specs only).
func (d *Domain) Global() Global { return Global{domain: d} }

// OnChanged registers a change listener and returns its idempotent undo.
// Listener failures are contained per listener (logged, never propagated
// into the write path).
func (d *Domain) OnChanged(listener func(DomainChanged)) func() {
	d.listenersMu.Lock()
	defer d.listenersMu.Unlock()
	handle := d.nextHandle
	d.nextHandle++
	d.listeners[handle] = listener
	return func() {
		d.listenersMu.Lock()
		defer d.listenersMu.Unlock()
		delete(d.listeners, handle)
	}
}

// afterCommitLocked enqueues one change and drains the dispatch queue with
// d.mu RELEASED during listener calls, so a listener may synchronously
// reenter any Domain method — read a snapshot or land another write — the
// way the official single-threaded emit allows. A nested write's event
// rides the same queue, so every event still arrives in commit order.
// Caller holds d.mu; it is re-held on return (the write paths' deferred
// unlock stays balanced).
func (d *Domain) afterCommitLocked(change DomainChanged) {
	d.emitQueue = append(d.emitQueue, change)
	if d.emitting {
		return
	}
	d.emitting = true
	for len(d.emitQueue) > 0 {
		next := d.emitQueue[0]
		d.emitQueue = d.emitQueue[1:]
		handlers := make([]func(DomainChanged), 0, len(d.listeners))
		d.listenersMu.Lock()
		for _, listener := range d.listeners {
			handlers = append(handlers, listener)
		}
		d.listenersMu.Unlock()
		d.mu.Unlock()
		for _, handler := range handlers {
			d.dispatch(handler, next)
		}
		d.mu.Lock()
	}
	d.emitting = false
}

// dispatch contains one listener failure so it cannot starve the others.
func (d *Domain) dispatch(handler func(DomainChanged), change DomainChanged) {
	defer func() {
		if rec := recover(); rec != nil && d.logger != nil {
			d.logger.Warn(fmt.Sprintf("domain '%s': domain/changed listener failed: %v", d.spec.Name, rec))
		}
	}()
	handler(change)
}

// assertReadable fails every operation after Close with the `closed` code.
func (d *Domain) assertReadable() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		panic(NewUnitError(CodeClosed, "domain '%s' is closed", d.spec.Name))
	}
}

func (d *Domain) assertReadableLocked() error {
	if d.closed {
		return NewUnitError(CodeClosed, "domain '%s' is closed", d.spec.Name)
	}
	return nil
}

// Close drains in-flight writes and releases the unit. Idempotent; the
// onClosed hook runs strictly after teardown completes: writes landing
// during the drain still emit domain/changed, and the domain stays readable
// until fully closed — only then does the name free up for reopening.
func (d *Domain) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	unit := d.unit
	onClosed := d.onClosed
	d.mu.Unlock()
	err := unit.Close()
	if onClosed != nil {
		onClosed()
	}
	return err
}
