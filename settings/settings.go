// Package settings re-implements the user-settings seam of
// @deepseek-ai/dsh-settings (packages/settings/settings, official tag
// dsh-v0.1.2-alpha.1): one user-owned document of per-namespace sections,
// resolution as schema defaults → composition base → user section, serialized
// writes with monotonic revisions, and write-time validation that refuses
// values the owner could not act on.
//
// The schema travels as an opaque JSON envelope (the configuration-UI wire
// format, locked): the Go host enforces values through the Schema.Validate
// hook, which corresponds to the official SettingsRegisterOptions.validate.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"dshgo/cordis"
)

// UpdateSource reports whether a change entered through Update/Replace/Mutate
// or was pushed by the document provider (external edit).
type UpdateSource string

const (
	SourceUpdate   UpdateSource = "update"
	SourceProvider UpdateSource = "provider"
)

// Schema describes one namespace. Envelope is the schema envelope for
// configuration surfaces; Defaults produces the schema defaults layer;
// Validate refuses a resolved section the owner could not act on (a rejected
// write never lands).
type Schema struct {
	Envelope json.RawMessage
	Defaults func() map[string]any
	Validate func(value map[string]any) error
}

// UpdateEvent reports one namespace change. Listeners that fail are contained
// and logged — a listener must never break a completed write.
type UpdateEvent struct {
	Namespace string
	Next      map[string]any
	Prev      map[string]any
	Source    UpdateSource
}

// Watcher observes update events.
type Watcher func(*UpdateEvent)

type registration struct {
	schema *Schema
	base   map[string]any
}

// Store holds the user settings document and every registered namespace.
// Writes to one namespace are serialized; the zero value is not usable —
// construct with NewStore.
type Store struct {
	logger cordis.Logger

	mu       sync.Mutex
	user     map[string]map[string]any
	revision map[string]uint64
	register map[string]*registration
	watchers []watcherEntry
	nextID   uint64
}

type watcherEntry struct {
	id uint64
	w  Watcher
}

// NewStore creates an empty store. A nil logger discards records.
func NewStore(logger cordis.Logger) *Store {
	if logger == nil {
		logger = cordis.Discard{}
	}
	return &Store{
		logger:   logger,
		user:     map[string]map[string]any{},
		revision: map[string]uint64{},
		register: map[string]*registration{},
	}
}

// Scope is the owner-facing handle for one registered namespace.
type Scope struct {
	store *Store
	ns    string
}

// Register binds a schema to a namespace. Duplicate registration fails loudly:
// two owners of one namespace is a composition error, not a fallback case.
func (st *Store) Register(ns string, schema *Schema, base map[string]any) (*Scope, error) {
	if ns == "" {
		return nil, errors.New("settings: namespace must not be empty")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, exists := st.register[ns]; exists {
		return nil, fmt.Errorf("settings: namespace %q already registered", ns)
	}
	if schema == nil {
		schema = &Schema{}
	}
	st.register[ns] = &registration{schema: schema, base: base}
	return &Scope{store: st, ns: ns}, nil
}

// HasNamespace reports whether the namespace is registered — the
// `ctx.settings.get(ns) === undefined` registration probe of consumer plugins.
func (st *Store) HasNamespace(ns string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	_, ok := st.register[ns]
	return ok
}

// Section returns the raw user-layer section without resolution; absent means
// the user never wrote this namespace.
func (st *Store) Section(ns string) map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.user[ns]
}

// Get returns the resolved snapshot: schema defaults, overlaid by the
// registrant's composition base, overlaid by the user section.
func (s *Scope) Get() map[string]any {
	st := s.store
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.resolveLocked(s.ns)
}

func (st *Store) resolveLocked(ns string) map[string]any {
	resolved := map[string]any{}
	if reg := st.register[ns]; reg != nil {
		if reg.schema.Defaults != nil {
			mergeInto(resolved, reg.schema.Defaults())
		}
		mergeInto(resolved, reg.base)
	}
	mergeInto(resolved, st.user[ns])
	return resolved
}

// Update merges a sparse patch over the user section only — never into base.
func (s *Scope) Update(patch map[string]any) error {
	return s.write(func(user map[string]any) map[string]any {
		merged := map[string]any{}
		mergeInto(merged, user)
		mergeInto(merged, patch)
		return merged
	}, SourceUpdate)
}

// Replace sets the section wholesale: keys absent from the replacement
// re-inherit base and schema defaults.
func (s *Scope) Replace(section map[string]any) error {
	return s.write(func(map[string]any) map[string]any {
		return section
	}, SourceUpdate)
}

// PathOp is one structured write inside Mutate: set a value at a path or
// unset it ("set" / "unset").
type PathOp struct {
	Op    string
	Path  []string
	Value any
}

// Mutate applies structured path operations atomically and refuses the write
// when expectedRevision does not match the namespace's current revision.
func (s *Scope) Mutate(ops []PathOp, expectedRevision *uint64) error {
	var expected []uint64
	if expectedRevision != nil {
		expected = append(expected, *expectedRevision)
	}
	return s.write(func(user map[string]any) map[string]any {
		next := map[string]any{}
		mergeInto(next, user)
		for _, op := range ops {
			applyPathOp(next, op)
		}
		return next
	}, SourceUpdate, expected...)
}

// ExpectedRevision returns the namespace's current write revision.
func (s *Scope) ExpectedRevision() uint64 {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	return s.store.revision[s.ns]
}

func (s *Scope) write(produce func(user map[string]any) map[string]any, source UpdateSource, expected ...uint64) error {
	st := s.store
	// Watchers run after the store lock is released: a watcher that writes
	// the document back (persistence) must be able to take the lock.
	var event *UpdateEvent
	defer func() {
		if event != nil {
			st.dispatch(event)
		}
	}()

	st.mu.Lock()
	defer st.mu.Unlock()

	if len(expected) > 0 && st.revision[s.ns] != expected[0] {
		return fmt.Errorf("settings: namespace %q revision conflict: expected %d, have %d",
			s.ns, expected[0], st.revision[s.ns])
	}

	prevResolved := st.resolveLocked(s.ns)
	prev := st.user[s.ns]
	next := produce(prev)
	nextResolved := st.resolveOverLocked(s.ns, next)

	// A change whose resolved value is deep-equal is never emitted — the
	// official seam's deep-equality rule. This also keeps provider reloads of
	// an unchanged document from echoing back through persistence.
	if reflect.DeepEqual(prevResolved, nextResolved) {
		return nil
	}

	reg := st.register[s.ns]
	if reg != nil && reg.schema.Validate != nil {
		// Validate sees the section exactly as the owner will: defaults and
		// base applied. A throw refuses the write that produced it.
		if err := reg.schema.Validate(nextResolved); err != nil {
			return fmt.Errorf("settings: namespace %q rejected the write: %w", s.ns, err)
		}
	}

	st.user[s.ns] = next
	st.revision[s.ns]++

	event = &UpdateEvent{Namespace: s.ns, Next: nextResolved, Prev: prevResolved, Source: source}
	return nil
}

// Document returns a copy of the raw user-layer document, one section per
// registered-or-written namespace — the persistence unit of settings-file.
func (st *Store) Document() map[string]map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make(map[string]map[string]any, len(st.user))
	for ns, section := range st.user {
		copied := map[string]any{}
		mergeInto(copied, section)
		out[ns] = copied
	}
	return out
}

// resolveOverLocked resolves a namespace as if the user layer were `over`,
// without touching the store.
func (st *Store) resolveOverLocked(ns string, over map[string]any) map[string]any {
	saved := st.user[ns]
	st.user[ns] = over
	resolved := st.resolveLocked(ns)
	st.user[ns] = saved
	return resolved
}

// dispatch runs watchers with containment: a panicking or failing watcher is
// logged and never reaches the writer.
func (st *Store) dispatch(event *UpdateEvent) {
	for _, entry := range st.watchers {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					st.logger.Error(fmt.Sprintf("settings: watcher panicked: %v", rec))
				}
			}()
			entry.w(event)
		}()
	}
}

// OnUpdated registers a watcher and returns its disposer. Watcher failures are
// contained and logged; they never reach the writer.
func (st *Store) OnUpdated(w Watcher) cordis.Disposer {
	st.mu.Lock()
	st.nextID++
	id := st.nextID
	st.watchers = append(st.watchers, watcherEntry{id: id, w: w})
	st.mu.Unlock()
	return func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		for i, entry := range st.watchers {
			if entry.id == id {
				st.watchers = append(st.watchers[:i], st.watchers[i+1:]...)
				return
			}
		}
	}
}

// ProviderPush writes one namespace section from the document provider
// (external edit). A section that fails validation keeps the namespace's last
// good value and warns — an externally edited document must never strand a
// running owner.
func (st *Store) ProviderPush(ns string, section map[string]any) error {
	scope := &Scope{store: st, ns: ns}
	return scope.write(func(map[string]any) map[string]any {
		return section
	}, SourceProvider)
}

func applyPathOp(section map[string]any, op PathOp) {
	switch op.Op {
	case "set":
		setPath(section, op.Path, op.Value)
	case "unset":
		unsetPath(section, op.Path)
	default:
		panic(fmt.Sprintf("settings: unknown path op %q", op.Op))
	}
}

func setPath(section map[string]any, path []string, value any) {
	if len(path) == 0 {
		return
	}
	cursor := section
	for _, key := range path[:len(path)-1] {
		next, ok := cursor[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cursor[key] = next
		}
		cursor = next
	}
	cursor[path[len(path)-1]] = value
}

func unsetPath(section map[string]any, path []string) {
	if len(path) == 0 {
		return
	}
	cursor := section
	for _, key := range path[:len(path)-1] {
		next, ok := cursor[key].(map[string]any)
		if !ok {
			return
		}
		cursor = next
	}
	delete(cursor, path[len(path)-1])
}

// mergeInto deep-merges src over dst: maps merge recursively, every other
// value replaces. Values are used as-is (callers pass private copies).
func mergeInto(dst, src map[string]any) {
	for key, value := range src {
		if srcMap, ok := value.(map[string]any); ok {
			if dstMap, ok := dst[key].(map[string]any); ok {
				mergeInto(dstMap, srcMap)
				continue
			}
			copied := map[string]any{}
			mergeInto(copied, srcMap)
			dst[key] = copied
			continue
		}
		dst[key] = value
	}
}
