// Scoped-context primitives shared by scope-aware registries: opaque scope
// keys with parent chains, insertion-ordered entry storage, scoped layer
// ownership, and scope-filtered waterfall dispatch. Port of
// packages/core/scope/src/{index,store}.ts.
//
// Registration views inherit DOWN the chain (a child sees its ancestors'
// layers); event admission extends UP it (a listener tagged with an ancestor
// receives events dispatched to a descendant key; a tag below the dispatch
// key stays excluded).
package scope

import (
	"fmt"
	"sync/atomic"
)

// ScopeKey is an opaque, pointer-identity scope. The zero value (nil) denotes
// the unscoped global view.
type ScopeKey = *scopeKey

type scopeKey struct {
	parent ScopeKey
}

// NewScopeKey mints one scope under an optional parent (nil = root scope).
func NewScopeKey(parent ScopeKey) ScopeKey {
	return &scopeKey{parent: parent}
}

// ChainOf returns the keys from a key to its root ancestor, nearest-first;
// nil yields the empty chain.
func ChainOf(key ScopeKey) []ScopeKey {
	var chain []ScopeKey
	for cursor := key; cursor != nil; cursor = cursor.parent {
		chain = append(chain, cursor)
	}
	return chain
}

// Admits reports the scopeTarget admission rule: an untagged listener is
// admitted globally; a tagged listener is admitted when its tag equals the
// dispatch key or any of its ancestors.
func Admits(tag, key ScopeKey) bool {
	if tag == nil {
		return true
	}
	for cursor := key; cursor != nil; cursor = cursor.parent {
		if cursor == tag {
			return true
		}
	}
	return false
}

var nextID atomic.Uint64

// NextEntryID mints one process-unique registration identity.
func NextEntryID() uint64 { return nextID.Add(1) }

// NamedEntries is insertion-ordered named storage with caller-owned
// duplicate diagnostics. Insert returns an idempotent undo for that exact
// entry.
type NamedEntries[V any] struct {
	onDuplicate func(name string) error
	data        map[string]V
	keys        []string
}

// NewNamedEntries builds an empty table; onDuplicate produces the loud
// duplicate-registration error.
func NewNamedEntries[V any](onDuplicate func(name string) error) *NamedEntries[V] {
	return &NamedEntries[V]{onDuplicate: onDuplicate, data: map[string]V{}}
}

// Insert one unique name; a duplicate fails with the table's diagnostic.
func (t *NamedEntries[V]) Insert(name string, value V) error {
	if _, exists := t.data[name]; exists {
		return t.onDuplicate(name)
	}
	t.data[name] = value
	t.keys = append(t.keys, name)
	return nil
}

// Get reads one named value.
func (t *NamedEntries[V]) Get(name string) (V, bool) {
	value, ok := t.data[name]
	return value, ok
}

// Has tests one name for membership.
func (t *NamedEntries[V]) Has(name string) bool {
	_, ok := t.data[name]
	return ok
}

// Len reports the live entry count.
func (t *NamedEntries[V]) Len() int { return len(t.keys) }

// IsEmpty reports whether no live entries remain.
func (t *NamedEntries[V]) IsEmpty() bool { return len(t.keys) == 0 }

// Keys returns the live names in insertion order (a copy).
func (t *NamedEntries[V]) Keys() []string {
	out := make([]string, len(t.keys))
	copy(out, t.keys)
	return out
}

// Entry is one name/value pair.
type Entry[V any] struct {
	Name  string
	Value V
}

// Entries returns the live name/value pairs in insertion order.
func (t *NamedEntries[V]) Entries() []Entry[V] {
	out := make([]Entry[V], 0, len(t.keys))
	for _, key := range t.keys {
		out = append(out, Entry[V]{Name: key, Value: t.data[key]})
	}
	return out
}

// Values returns the live values in insertion order.
func (t *NamedEntries[V]) Values() []V {
	out := make([]V, 0, len(t.keys))
	for _, key := range t.keys {
		out = append(out, t.data[key])
	}
	return out
}

// Remove deletes one name (idempotent).
func (t *NamedEntries[V]) Remove(name string) {
	if !t.Has(name) {
		return
	}
	delete(t.data, name)
	for i, key := range t.keys {
		if key == name {
			t.keys = append(t.keys[:i], t.keys[i+1:]...)
			return
		}
	}
}

// AnonymousEntries is insertion-ordered storage where equal values remain
// separate registrations; each append returns an idempotent undo for that
// exact entry.
type AnonymousEntries[V any] struct {
	entries []anonymousEntry[V]
}

type anonymousEntry[V any] struct {
	id     uint64
	value  V
	active bool
}

// Append one independently owned value; the return is an idempotent undo.
func (t *AnonymousEntries[V]) Append(value V) func() {
	id := NextEntryID()
	t.entries = append(t.entries, anonymousEntry[V]{id: id, value: value, active: true})
	return func() {
		for i := range t.entries {
			if t.entries[i].id == id && t.entries[i].active {
				t.entries[i].active = false
				return
			}
		}
	}
}

// Values returns the live values in insertion order.
func (t *AnonymousEntries[V]) Values() []V {
	out := make([]V, 0, len(t.entries))
	for _, entry := range t.entries {
		if entry.active {
			out = append(out, entry.value)
		}
	}
	return out
}

// IsEmpty reports whether no live entries remain.
func (t *AnonymousEntries[V]) IsEmpty() bool {
	for _, entry := range t.entries {
		if entry.active {
			return false
		}
	}
	return true
}

// Merged is one effective name/value view with first-registration order and
// nearest-scope values: a scoped shadow of an existing name keeps that name's
// original position, exactly like Map.set on an existing key.
type Merged[V any] struct {
	keys []string
	data map[string]V
}

// Keys returns the merged names in first-registration order.
func (m *Merged[V]) Keys() []string {
	out := make([]string, len(m.keys))
	copy(out, m.keys)
	return out
}

// Entries returns the merged pairs in first-registration order.
func (m *Merged[V]) Entries() []Entry[V] {
	out := make([]Entry[V], 0, len(m.keys))
	for _, key := range m.keys {
		out = append(out, Entry[V]{Name: key, Value: m.data[key]})
	}
	return out
}

// Values returns the merged values in first-registration order.
func (m *Merged[V]) Values() []V {
	out := make([]V, 0, len(m.keys))
	for _, key := range m.keys {
		out = append(out, m.data[key])
	}
	return out
}

// Get reads one merged value.
func (m *Merged[V]) Get(name string) (V, bool) {
	value, ok := m.data[name]
	return value, ok
}

// Len reports the merged entry count.
func (m *Merged[V]) Len() int { return len(m.keys) }

// Layers owns the global and exact-scope layers for one registry. Reads never
// create scoped layers; a completely emptied aggregate layer is reclaimed.
// L is the layer type itself (usually a struct); layer handles are *L.
type Layers[L any] struct {
	// Global is the eagerly constructed context-global layer.
	Global *L

	scoped   map[ScopeKey]*L
	newLayer func(scope ScopeKey) *L
	isEmpty  func(layer *L) bool
	onChange func()
}

// NewLayers builds the layer store, constructing the global layer eagerly.
// isEmpty decides reclamation of a drained scoped layer.
func NewLayers[L any](newLayer func(scope ScopeKey) *L, isEmpty func(layer *L) bool, onChange func()) *Layers[L] {
	return &Layers[L]{
		Global:   newLayer(nil),
		scoped:   map[ScopeKey]*L{},
		newLayer: newLayer,
		isEmpty:  isEmpty,
		onChange: onChange,
	}
}

// Peek reads an existing exact-scope overlay. Deliberately chain-blind:
// callers addressing one scope's OWN contributions must not silently pick up
// an ancestor's.
func (l *Layers[L]) Peek(scope ScopeKey) *L {
	if scope == nil {
		return nil
	}
	return l.scoped[scope]
}

// ChainLayers returns the existing overlays along the scope's parent chain,
// farthest ancestor first and the exact scope last, so a caller layering them
// in order gives the nearest scope the final word.
func (l *Layers[L]) ChainLayers(scope ScopeKey) []*L {
	chain := ChainOf(scope)
	layers := make([]*L, 0, len(chain))
	for i := len(chain) - 1; i >= 0; i-- {
		if layer, ok := l.scoped[chain[i]]; ok {
			layers = append(layers, layer)
		}
	}
	return layers
}

// MergeLayers materializes global named entries followed by scope-chain
// shadows, farthest ancestor first, so the nearest scope's entry wins a name
// while keeping its first-registration position.
func MergeLayers[L any, V any](layers *Layers[L], scope ScopeKey, pick func(*L) *NamedEntries[V]) *Merged[V] {
	merged := &Merged[V]{data: map[string]V{}}
	for _, entry := range pick(layers.Global).Entries() {
		if _, seen := merged.data[entry.Name]; !seen {
			merged.keys = append(merged.keys, entry.Name)
		}
		merged.data[entry.Name] = entry.Value
	}
	for _, layer := range layers.ChainLayers(scope) {
		for _, entry := range pick(layer).Entries() {
			if _, seen := merged.data[entry.Name]; !seen {
				merged.keys = append(merged.keys, entry.Name)
			}
			merged.data[entry.Name] = entry.Value
		}
	}
	return merged
}

// Mutate applies one layer mutation with scope-layer lifecycle: create the
// overlay on first contribution, reclaim it when it drains empty. The changed
// flag reports that a notification is owed; callers fire it AFTER releasing
// their registry lock (the notification re-enters the registry).
func (l *Layers[L]) Mutate(scope ScopeKey, action func(layer *L) (func(), error)) (func(), bool, error) {
	var layer *L
	created := false
	if scope == nil {
		layer = l.Global
	} else {
		existing, ok := l.scoped[scope]
		if !ok {
			layer = l.newLayer(scope)
			l.scoped[scope] = layer
			created = true
		} else {
			layer = existing
		}
	}
	undo, err := action(layer)
	if err != nil {
		if scope != nil && created && l.isEmpty(layer) {
			delete(l.scoped, scope)
		}
		return nil, false, err
	}
	return func() {
		undo()
		if scope != nil && l.isEmpty(layer) {
			delete(l.scoped, scope)
		}
		if l.onChange != nil {
			l.onChange()
		}
	}, true, nil
}

// WaterfallListener is one scope-tagged listener: untagged listeners are
// admitted for every dispatch; tagged listeners follow Admits.
type waterfallListener[Tin any, Tout any] struct {
	scope ScopeKey
	id    uint64
	fn    func(value Tin, next func(Tin) Tout) Tout
}

// WaterfallEvent stores scope-tagged listeners for one event family and
// dispatches them with cordis waterfall semantics — first-registered
// outermost, next() delegates, calling next twice panics.
type WaterfallEvent[Tin any, Tout any] struct {
	listeners []waterfallListener[Tin, Tout]
}

// On appends one listener and returns its idempotent undo.
func (e *WaterfallEvent[Tin, Tout]) On(scope ScopeKey, fn func(value Tin, next func(Tin) Tout) Tout) func() {
	id := NextEntryID()
	e.listeners = append(e.listeners, waterfallListener[Tin, Tout]{scope: scope, id: id, fn: fn})
	return func() {
		for i := range e.listeners {
			if e.listeners[i].id == id {
				e.listeners = append(e.listeners[:i], e.listeners[i+1:]...)
				return
			}
		}
	}
}

// Snapshot returns the admitted listener functions in registration order,
// so a dispatcher can release its registry mutex before running listeners
// (listeners re-enter their registry).
func (e *WaterfallEvent[Tin, Tout]) Snapshot(scope ScopeKey) []func(Tin, func(Tin) Tout) Tout {
	var admitted []func(Tin, func(Tin) Tout) Tout
	for _, listener := range e.listeners {
		if Admits(listener.scope, scope) {
			admitted = append(admitted, listener.fn)
		}
	}
	return admitted
}

// Dispatch runs the admitted listeners over the value in registration order.
func (e *WaterfallEvent[Tin, Tout]) Dispatch(scope ScopeKey, value Tin, base func(Tin) Tout) Tout {
	return RunWaterfall(e.Snapshot(scope), value, base)
}

// Len reports the live listener count.
func (e *WaterfallEvent[Tin, Tout]) Len() int { return len(e.listeners) }

// RunWaterfall executes listener functions with cordis waterfall semantics:
// first-registered outermost, next() delegates, calling next twice panics.
// `base` is the innermost fallthrough.
func RunWaterfall[Tin any, Tout any](listeners []func(Tin, func(Tin) Tout) Tout, value Tin, base func(Tin) Tout) Tout {
	var run func(i int, value Tin) Tout
	run = func(i int, value Tin) Tout {
		if i >= len(listeners) {
			return base(value)
		}
		delegated := false
		return listeners[i](value, func(v Tin) Tout {
			if delegated {
				panic(fmt.Sprintf("cordis: waterfall listener %d called next twice", i))
			}
			delegated = true
			return run(i+1, v)
		})
	}
	return run(0, value)
}
