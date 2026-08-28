// Package storage ports packages/storage/storage: the storage hub
// (`ctx.storage`) — a named backend registry plus mounted data-form
// facilities. The hub itself performs no IO — backends own media, data
// forms (the domain layer first) own semantics.
//
// Go adaptations: the Cordis `ctx.storage` service property is an explicit
// Hub value the assembly composes; the `StorageForms` declaration-merging
// map becomes a string-keyed facility table with a typed Domain accessor;
// lifecycle service keys stay derivable for the loader round.
package storage

import (
	"fmt"
	"sort"
	"strings"

	"dshgo/storagedomain"
)

// StorageErrorCode values carried by every Error the hub raises.
const (
	CodeBackendNotFound  = "backend-not-found"
	CodeFormNotMounted   = "form-not-mounted"
	CodeDuplicateBackend = "duplicate-backend"
	CodeDuplicateMount   = "duplicate-mount"
)

// Error is raised by the hub; Code is the stable discriminant consumers may
// switch on, Message is diagnostic prose. The backend-facing codes
// (`version-mismatch`, `malformed-medium`, `closed`) live in storagedomain's
// UnitError.
type Error struct {
	Code    string
	Message string
}

// Error implements the error interface.
func (e *Error) Error() string { return e.Message }

// NewError builds a hub error.
func NewError(code string, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// StorageBackend is one registered backend. Go: the source's optional
// facets collapse to storagedomain.Backend (the KV facet), which fails loud
// for unsupported data kinds; Close drains in-flight writes across all open
// units and releases the medium.
type StorageBackend = storagedomain.Backend

// BackendRegistry is the mutable name → backend table. Multiple backends
// stay mounted side by side; which backend serves which consumer is the
// consumer's configuration (e.g. the domain layer's route table), never a
// hub-global choice.
type BackendRegistry struct {
	backends map[string]StorageBackend
}

// NewBackendRegistry builds an empty registry.
func NewBackendRegistry() *BackendRegistry {
	return &BackendRegistry{backends: map[string]StorageBackend{}}
}

// Register a named backend. Registration is an effect: the returned
// disposer removes the name. Disposal does NOT close the backend — the
// owning plugin closes it after unregistering. Duplicate names reject.
func (r *BackendRegistry) Register(name string, backend StorageBackend) (func(), error) {
	if _, exists := r.backends[name]; exists {
		return nil, NewError(CodeDuplicateBackend, "storage backend '%s' is already registered", name)
	}
	r.backends[name] = backend
	return func() {
		// Remove only this registration's contribution: after dispose +
		// re-register, a stale disposer firing again must not remove the
		// successor.
		if r.backends[name] == backend {
			delete(r.backends, name)
		}
	}, nil
}

// Get resolves a backend by name; unknown names reject with the registered
// names in the message.
func (r *BackendRegistry) Get(name string) (StorageBackend, error) {
	backend, ok := r.backends[name]
	if !ok {
		registered := r.Names()
		shown := strings.Join(registered, ", ")
		if shown == "" {
			shown = "none"
		}
		return nil, NewError(CodeBackendNotFound, "storage backend '%s' is not registered (registered: %s)", name, shown)
	}
	return backend, nil
}

// Names returns the registered backend names, sorted for deterministic
// diagnostics (the source's insertion-ordered snapshot is a Map artifact).
func (r *BackendRegistry) Names() []string {
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// StorageBackendServiceKey derives the Cordis lifecycle service key that one
// named backend plugin provides. Domain-form providers inject these keys so
// activation cannot race backend registration even though callers continue
// resolving backends through the storage registry.
func StorageBackendServiceKey(name string) string {
	return "storage.backend." + name
}

// Hub is the storage hub service. Backends register under the backend
// registry; data forms mount under their form key.
type Hub struct {
	// Backend is the named backend table; multiple backends stay mounted
	// side by side.
	Backend *BackendRegistry

	forms map[string]any
}

// NewHub builds an empty hub.
func NewHub() *Hub {
	return &Hub{Backend: NewBackendRegistry(), forms: map[string]any{}}
}

// Mount a data-form facility on the hub. Mounting is an effect: the
// returned disposer unmounts the form. Duplicate form keys reject.
func (h *Hub) Mount(form string, facility any) (func(), error) {
	if _, exists := h.forms[form]; exists {
		return nil, NewError(CodeDuplicateMount, "storage form '%s' is already mounted", form)
	}
	h.forms[form] = facility
	return func() {
		// Same stale-disposer guard as BackendRegistry.Register.
		if h.forms[form] == facility {
			delete(h.forms, form)
		}
	}, nil
}

// Form resolves a mounted data form; unmounted keys reject.
func (h *Hub) Form(form string) (any, error) {
	facility, ok := h.forms[form]
	if !ok {
		return nil, NewError(CodeFormNotMounted, "storage form '%s' is not mounted", form)
	}
	return facility, nil
}

// Domain returns the mounted domain data-form facility; absent until the
// domain layer plugin is loaded (fail loud, per the source's `get domain`).
func (h *Hub) Domain() (*storagedomain.Facility, error) {
	facility, err := h.Form("domain")
	if err != nil {
		return nil, err
	}
	domain, ok := facility.(*storagedomain.Facility)
	if !ok {
		return nil, NewError(CodeFormNotMounted, "storage form 'domain' is not mounted as a domain facility")
	}
	return domain, nil
}
