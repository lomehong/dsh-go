// Tools-runtime bindings to the shared scope primitives. The generic
// machinery lives in dshgo/scope (the port of @deepseek-ai/dsh-scope); only
// the tool registry's own layer type and thin aliases remain here.
package tools

import (
	"fmt"

	"dshgo/scope"
)

// ScopeKey is an opaque, pointer-identity scope; nil denotes the global view.
type ScopeKey = scope.ScopeKey

// NewScopeKey mints one scope under an optional parent (nil = root scope).
func NewScopeKey(parent ScopeKey) ScopeKey { return scope.NewScopeKey(parent) }

// NamedEntries is insertion-ordered named storage with loud duplicates.
type NamedEntries[V any] = scope.NamedEntries[V]

// AnonymousEntries is insertion-ordered storage of independent registrations.
type AnonymousEntries[V any] = scope.AnonymousEntries[V]

// newNamedEntries builds a table with the registry's historical duplicate text.
func newNamedEntries[V any]() *NamedEntries[V] {
	return scope.NewNamedEntries[V](func(name string) error { return fmt.Errorf("entry %q is already registered", name) })
}

// nextAnonymousID mints one process-unique registration identity.
func nextAnonymousID() uint64 { return scope.NextEntryID() }

// scopeAdmits reports the listener admission rule for one dispatch key.
func scopeAdmits(tag, key ScopeKey) bool { return scope.Admits(tag, key) }

// waterfallEvent is the scope-tagged waterfall used by the pipeline seams.
type waterfallEvent[Tin any, Tout any] = scope.WaterfallEvent[Tin, Tout]

// runWaterfall executes admitted listener functions with cordis semantics.
func runWaterfall[Tin any, Tout any](listeners []func(Tin, func(Tin) Tout) Tout, value Tin, base func(Tin) Tout) Tout {
	return scope.RunWaterfall(listeners, value, base)
}

// IsEmpty satisfies the shared layer interface for reclamation.
func (l *toolLayer) IsEmpty() bool { return l.isEmpty() }
