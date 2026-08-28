// Package webserver ports the route-registration semantics of
// packages/host/webserver (@deepseek-ai/dsh-host-webserver, official tag
// dsh-v0.1.2-alpha.1): an ordered registry of exact/prefix routes plus one
// fallback seat, handler-owned responses, and containment of handler
// failures — a returned error or a panic degrades to an HTTP error response
// and never escapes the server. The JavaScript harness treats an escaping
// handler failure as process-fatal; that crash class is made unrepresentable
// here.
package webserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"dshgo/cordis"
)

// Handler serves one request and returns an error when it could not complete.
// Status and body belong to the handler; the returned error is a failure
// report for the containment layer, never a process-fatal escape.
type Handler func(w http.ResponseWriter, r *http.Request) error

// Route kinds, matching the official WebRoute contract.
const (
	KindExact    = "exact"
	KindPrefix   = "prefix"
	KindFallback = "fallback"
)

// Route claims a request whose path matches Kind/Path; a fallback Route owns
// everything no earlier route claimed and must not set a Path.
type Route struct {
	Kind    string
	Path    string
	Handler Handler
}

type registeredRoute struct {
	id uint64
	Route
}

// Registry is the webServer service face: ordered registration, first claim
// wins, one fallback seat. The zero value is usable; prefer New to keep logs.
type Registry struct {
	mu     sync.RWMutex
	routes []registeredRoute
	nextID uint64
	logger cordis.Logger
}

// New creates a Registry. A nil logger discards records.
func New(logger cordis.Logger) *Registry {
	if logger == nil {
		logger = cordis.Discard{}
	}
	return &Registry{logger: logger}
}

// Register adds a route in registration order and returns its disposer.
// Duplicate (Kind, Path) pairs and a second fallback seat are rejected — the
// official registry treats both as registration errors.
func (rg *Registry) Register(rt Route) (cordis.Disposer, error) {
	switch rt.Kind {
	case KindExact, KindPrefix:
		if rt.Path == "" {
			return nil, fmt.Errorf("webserver: %s route requires a path", rt.Kind)
		}
	case KindFallback:
		if rt.Path != "" {
			return nil, errors.New("webserver: fallback route must not set a path")
		}
	default:
		return nil, fmt.Errorf("webserver: unknown route kind %q", rt.Kind)
	}
	if rt.Handler == nil {
		return nil, fmt.Errorf("webserver: route %s %q has no handler", rt.Kind, rt.Path)
	}

	rg.mu.Lock()
	defer rg.mu.Unlock()
	for _, existing := range rg.routes {
		if existing.Kind != rt.Kind {
			continue
		}
		if rt.Kind == KindFallback || existing.Path == rt.Path {
			return nil, fmt.Errorf("webserver: duplicate %s route %q", rt.Kind, rt.Path)
		}
	}
	rg.nextID++
	entry := registeredRoute{id: rg.nextID, Route: rt}
	rg.routes = append(rg.routes, entry)
	return func() {
		rg.mu.Lock()
		defer rg.mu.Unlock()
		for i, existing := range rg.routes {
			if existing.id == entry.id {
				rg.routes = append(rg.routes[:i], rg.routes[i+1:]...)
				return
			}
		}
	}, nil
}

// ServeHTTP dispatches to the first registered route that claims the path;
// when nothing claims the request it answers 404, matching node:http behavior
// with no fallback seat registered.
func (rg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rg.mu.RLock()
	snapshot := make([]registeredRoute, len(rg.routes))
	copy(snapshot, rg.routes)
	rg.mu.RUnlock()

	rw := &recordedWriter{ResponseWriter: w}
	for _, entry := range snapshot {
		if !claims(entry.Route, r.URL.Path) {
			continue
		}
		rg.serve(rw, entry.Route, r)
		return
	}
	http.NotFound(rw, r)
}

func claims(rt Route, path string) bool {
	switch rt.Kind {
	case KindExact:
		return path == rt.Path
	case KindPrefix:
		return strings.HasPrefix(path, rt.Path)
	default:
		return true
	}
}

// serve runs one handler under containment. A returned error or a panic is
// logged and becomes a 400 while the response is uncommitted — the official
// registry makes the same choice — and only logged after the handler
// committed the response, since the stream is then handler-owned.
func (rg *Registry) serve(rw *recordedWriter, rt Route, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			rg.logger.Error(fmt.Sprintf("webserver: panic in %s %q: %v", rt.Kind, rt.Path, rec))
			rg.fail(rw)
		}
	}()
	if err := rt.Handler(rw, r); err != nil {
		rg.logger.Warn(fmt.Sprintf("webserver: handler %s %q failed: %v", rt.Kind, rt.Path, err))
		rg.fail(rw)
	}
}

func (rg *Registry) fail(rw *recordedWriter) {
	if !rw.wrote {
		http.Error(rw, "bad request", http.StatusBadRequest)
	}
}

// recordedWriter tracks whether the handler committed a response — the Go
// equivalent of the headersSent check in the official containment path.
type recordedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *recordedWriter) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordedWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// AsPlugin exposes the registry as a cordis plugin providing the "webServer"
// service, mirroring the official package's plugin face. Consumers resolve it
// with ctx.Get("webServer").(*Registry).
func AsPlugin(logger cordis.Logger) *cordis.Plugin {
	registry := New(logger)
	return &cordis.Plugin{
		Name:    "dsh-host-webserver",
		Provide: []string{"webServer"},
		Apply: func(ctx *cordis.Context) error {
			ctx.Provide("webServer", registry)
			return nil
		},
	}
}
