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
	"bufio"
	"errors"
	"fmt"
	"net"
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
	// upgrades are exact-path HTTP upgrade routes; one socket has one
	// protocol owner, so duplicate paths are rejected.
	upgrades map[string]UpgradeRoute
	// conns tracks hijacked upgrade sockets: the registry owns their
	// lifetime and destroys them on Close.
	conns  map[net.Conn]struct{}
	nextID uint64
	logger cordis.Logger
	// indexTaps are raw-HTML transforms applied in registration order after
	// the structured injection rows.
	indexTaps []indexedTransform
}

type indexedTransform struct {
	id        uint64
	transform func(string) string
}

// UpgradeHandler owns protocol negotiation and the hijacked connection after
// dispatch. A returned error or a panic closes the connection; the handler
// that takes over the protocol owns it from then on.
type UpgradeHandler func(conn net.Conn, rw *bufio.ReadWriter, r *http.Request) error

// UpgradeRoute claims one exact pathname for HTTP upgrade dispatch.
type UpgradeRoute struct {
	// Path is the absolute pathname claimed verbatim.
	Path string
	// Handler owns negotiation plus the connection after dispatch.
	Handler UpgradeHandler
}

// New creates a Registry. A nil logger discards records.
func New(logger cordis.Logger) *Registry {
	if logger == nil {
		logger = cordis.Discard{}
	}
	return &Registry{
		logger:   logger,
		upgrades: map[string]UpgradeRoute{},
		conns:    map[net.Conn]struct{}{},
	}
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

// ServeHTTP dispatches to the first registered route that claims the path —
// exact table first, then longest-prefix-wins over the prefix table — and
// routes HTTP upgrade requests to their exact-path owner. When nothing claims
// the request it answers 404, matching node:http behavior with no fallback
// seat registered.
func (rg *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "" {
		rg.serveUpgrade(w, r)
		return
	}
	rg.mu.RLock()
	snapshot := make([]registeredRoute, len(rg.routes))
	copy(snapshot, rg.routes)
	rg.mu.RUnlock()

	rw := &recordedWriter{ResponseWriter: w}
	if entry, ok := rg.matchExact(snapshot, r.URL.Path); ok {
		rg.serve(rw, entry.Route, r)
		return
	}
	if entry, ok := rg.matchPrefix(snapshot, r.URL.Path); ok {
		rg.serve(rw, entry.Route, r)
		return
	}
	for _, entry := range snapshot {
		if entry.Kind == KindFallback {
			rg.serve(rw, entry.Route, r)
			return
		}
	}
	http.NotFound(rw, r)
}

func (rg *Registry) matchExact(snapshot []registeredRoute, path string) (registeredRoute, bool) {
	for _, entry := range snapshot {
		if entry.Kind == KindExact && entry.Path == path {
			return entry, true
		}
	}
	return registeredRoute{}, false
}

// matchPrefix applies longest-prefix-wins: a prefix claims its own path and
// its "/…"-subtree only.
func (rg *Registry) matchPrefix(snapshot []registeredRoute, path string) (registeredRoute, bool) {
	best := registeredRoute{}
	found := false
	for _, entry := range snapshot {
		if entry.Kind != KindPrefix || !claimsPrefix(entry.Path, path) {
			continue
		}
		if !found || len(entry.Path) > len(best.Path) {
			best, found = entry, true
		}
	}
	return best, found
}

func claimsPrefix(prefix, path string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// serveUpgrade hijacks the connection for the exact-path upgrade owner and
// tracks the socket until Close or the handler returns. The hijack fails
// loud with a 400 when the response does not support it.
func (rg *Registry) serveUpgrade(w http.ResponseWriter, r *http.Request) {
	rg.mu.RLock()
	route, ok := rg.upgrades[r.URL.Path]
	rg.mu.RUnlock()
	if !ok {
		rg.logger.Warn(fmt.Sprintf("webserver: upgrade %q has no registered route", r.URL.Path))
		http.NotFound(w, r)
		return
	}
	hijacker, canHijack := w.(http.Hijacker)
	if !canHijack {
		http.Error(w, "upgrade unsupported", http.StatusBadRequest)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		rg.logger.Warn(fmt.Sprintf("webserver: upgrade %q hijack failed: %v", r.URL.Path, err))
		return
	}
	rg.mu.Lock()
	rg.conns[conn] = struct{}{}
	rg.mu.Unlock()
	defer func() {
		rg.mu.Lock()
		delete(rg.conns, conn)
		rg.mu.Unlock()
		conn.Close()
	}()
	defer func() {
		if rec := recover(); rec != nil {
			rg.logger.Error(fmt.Sprintf("webserver: panic in upgrade %q: %v", r.URL.Path, rec))
		}
	}()
	if err := route.Handler(conn, rw, r); err != nil {
		rg.logger.Warn(fmt.Sprintf("webserver: upgrade %q handler failed: %v", r.URL.Path, err))
	}
}

// RegisterUpgrade registers an exact-path HTTP upgrade route and returns its
// disposer. Duplicate paths are rejected because one socket can have only one
// protocol owner.
func (rg *Registry) RegisterUpgrade(rt UpgradeRoute) (cordis.Disposer, error) {
	if rt.Path == "" {
		return nil, errors.New("webserver: upgrade route requires a path")
	}
	if rt.Handler == nil {
		return nil, fmt.Errorf("webserver: upgrade route %q has no handler", rt.Path)
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if _, exists := rg.upgrades[rt.Path]; exists {
		return nil, fmt.Errorf("webserver: duplicate upgrade route %q", rt.Path)
	}
	rg.upgrades[rt.Path] = rt
	return func() {
		rg.mu.Lock()
		defer rg.mu.Unlock()
		delete(rg.upgrades, rt.Path)
	}, nil
}

// Close destroys every tracked upgrade socket and returns after they are
// closed. The composing server's Close does not cover hijacked connections —
// node:http has the same shape, which is why the official server tracks
// upgraded sockets explicitly.
func (rg *Registry) Close() {
	rg.mu.Lock()
	conns := make([]net.Conn, 0, len(rg.conns))
	for conn := range rg.conns {
		conns = append(conns, conn)
	}
	rg.mu.Unlock()
	for _, conn := range conns {
		conn.Close()
	}
}

// TapIndex registers a raw-HTML index transform — the escape hatch for markup
// no IndexInjection row expresses. RenderIndex applies taps in registration
// order after rendering the structured rows.
func (rg *Registry) TapIndex(transform func(string) string) (cordis.Disposer, error) {
	if transform == nil {
		return nil, errors.New("webserver: index tap must not be nil")
	}
	rg.mu.Lock()
	rg.nextID++
	entry := indexedTransform{id: rg.nextID, transform: transform}
	rg.indexTaps = append(rg.indexTaps, entry)
	rg.mu.Unlock()
	return func() {
		rg.mu.Lock()
		defer rg.mu.Unlock()
		for i, existing := range rg.indexTaps {
			if existing.id == entry.id {
				rg.indexTaps = append(rg.indexTaps[:i], rg.indexTaps[i+1:]...)
				return
			}
		}
	}, nil
}

// CollectIndexInjections gathers the structured injection table over one
// `webserver/index-inject` waterfall: every subscriber pushes its current
// rows. Fresh per call, so subscribers read live state at emit time.
func (rg *Registry) CollectIndexInjections(ctx *cordis.Context) []IndexInjection {
	table := &[]IndexInjection{}
	if ctx != nil {
		ctx.Waterfall("webserver/index-inject", table)
	}
	return *table
}

// RenderIndex renders one index.html body: the structured injection table
// first, then the raw tapIndex transforms over the result. Unknown row kinds
// fail loud — rows are composition-authored data.
func (rg *Registry) RenderIndex(ctx *cordis.Context, html string) (string, error) {
	rendered, err := RenderIndexInjections(html, rg.CollectIndexInjections(ctx))
	if err != nil {
		return "", err
	}
	rg.mu.RLock()
	taps := make([]indexedTransform, len(rg.indexTaps))
	copy(taps, rg.indexTaps)
	rg.mu.RUnlock()
	for _, tap := range taps {
		rendered = tap.transform(rendered)
	}
	return rendered, nil
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

// Unwrap exposes the underlying writer to http.ResponseController, so
// streaming handlers (the /plugins/events SSE channel) can reach the
// server's Flusher through the recorded wrapper.
func (w *recordedWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// ContextService is the typed "webServer" service handle; the assertion for
// the registry lookup lives here instead of at every consumer.
var ContextService = cordis.DefineService[*Registry]("webServer")

// AsPlugin exposes the registry as a cordis plugin providing the "webServer"
// service, mirroring the official package's plugin face. Consumers resolve it
// with webserver.ContextService.From(ctx).
func AsPlugin(logger cordis.Logger) *cordis.Plugin {
	registry := New(logger)
	return &cordis.Plugin{
		Name:    "dsh-host-webserver",
		Provide: []string{"webServer"},
		Apply: func(ctx *cordis.Context) error {
			ContextService.Provide(ctx, registry)
			return nil
		},
	}
}
