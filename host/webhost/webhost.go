// Package webhost ports the serve half of the official dsh web surface
// (@deepseek-ai/dsh-frontend-static over @deepseek-ai/dsh-host-webserver):
// the frontend dist fallback owner and the http.Server bind. The webserver
// package owns route registration and containment; this package owns the
// bind lifecycle that the official webserver plugin performs in-process —
// the Go port keeps listen with the consumer (recorded decision, STATUS.md).
package webhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dshgo/cordis"
	"dshgo/host/webserver"
)

// defaultDistIndex is the frontend package's built entry, resolved through
// the package layout the official resolveDistIndex walks (require.resolve of
// @deepseek-ai/dsh-frontend/dist/index.html).
const defaultDistIndex = "node_modules/@deepseek-ai/dsh-frontend/dist/index.html"

// ResolveFrontendDist resolves the built frontend dist directory by walking
// upward from the installation anchor for the official package layout. A
// missing dist fails loud with the official build hint: the Web surface has
// no source-serving fallback.
func ResolveFrontendDist(anchor string) (string, error) {
	dir := filepath.Dir(anchor)
	for {
		index := filepath.Join(dir, defaultDistIndex)
		if _, err := os.Stat(index); err == nil {
			return filepath.Dir(index), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("web: frontend dist not built; run the frontend build so %q exists", defaultDistIndex)
}

// Host serves the browser surface: the fallback seat owns the frontend dist
// (index.html through the registry's injection rendering, static assets from
// the dist tree, SPA fallback to index for extension-less paths), the
// composed client boot graph and its plugin combo responses (official
// ClientModuleRegistry serve half), and the bound http.Server owns the
// listen lifecycle.
type Host struct {
	registry  *webserver.Registry
	ctx       *cordis.Context
	dist      string
	logger    cordis.Logger
	graph     *bootGraph
	responses map[string]servedResponse
	unary     http.HandlerFunc

	server *http.Server
	addr   net.Addr
	closed bool
}

// SetUnaryHandler installs the /api/ prefix carrier (the browser unary RPC
// POST route). A nil handler leaves /api/ unserved (the fallback 404s it).
func (h *Host) SetUnaryHandler(handler http.HandlerFunc) { h.unary = handler }

// Mount registers the frontend fallback owner on the registry. The dist dir
// must already be resolved; a missing index.html fails loud at mount. The
// client boot graph composes from the node_modules root above the dist —
// the official ClientModuleRegistry activation scan runs synchronously and
// fails loud on a malformed declaration or missing bundle.
func Mount(registry *webserver.Registry, ctx *cordis.Context, dist string, logger cordis.Logger) (*Host, error) {
	index := filepath.Join(dist, "index.html")
	if _, err := os.Stat(index); err != nil {
		return nil, fmt.Errorf("web: frontend dist has no index.html at %s: %w", index, err)
	}
	graph, responses, err := composeBootGraph(nodeModulesFromDist(dist))
	if err != nil {
		return nil, err
	}
	host := &Host{registry: registry, ctx: ctx, dist: dist, logger: logger, graph: graph, responses: responses}
	_, err = registry.Register(webserver.Route{
		Kind:    webserver.KindFallback,
		Handler: host.serve,
	})
	if err != nil {
		return nil, err
	}
	return host, nil
}

// Listen binds the http.Server over the registry. An empty host defaults to
// 127.0.0.1 and a zero port asks the OS for a free one. It returns once the
// listener is live; Close stops it.
func (h *Host) Listen(host string, port string) error {
	if host == "" {
		host = "127.0.0.1"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("web: bind %s: %w", net.JoinHostPort(host, port), err)
	}
	h.addr = listener.Addr()
	h.server = &http.Server{
		Handler:           h.registry,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := h.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			h.logger.Error(fmt.Sprintf("web: server failed: %v", err))
		}
	}()
	return nil
}

// Addr reports the bound address; nil until Listen succeeds.
func (h *Host) Addr() net.Addr { return h.addr }

// Close stops the listener and returns after in-flight requests settle.
func (h *Host) Close() error {
	if h == nil || h.server == nil || h.closed {
		return nil
	}
	h.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		return err
	}
	h.registry.Close()
	return nil
}

// serve is the fallback owner: index.html goes through the registry's
// injection rendering; files under the dist tree are served verbatim with
// their MIME type; any other path falls back to the rendered index (the SPA
// entry). Path traversal outside the dist tree is rejected.
func (h *Host) serve(w http.ResponseWriter, r *http.Request) error {
	if strings.HasPrefix(r.URL.Path, "/api/") && h.unary != nil {
		h.unary(w, r)
		return nil
	}
	if strings.HasPrefix(r.URL.Path, "/plugins/") {
		if r.URL.Path == "/plugins/events" {
			return h.servePluginsEvents(w, r)
		}
		if !servePlugins(w, r, h.responses) {
			http.Error(w, "not found", http.StatusNotFound)
		}
		return nil
	}
	cleaned := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
	if cleaned == "." || cleaned == "" {
		return h.serveIndex(w)
	}
	if strings.Contains(cleaned, "..") || filepath.IsAbs(cleaned) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	target := filepath.Join(h.dist, cleaned)
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		// A non-file path is the SPA route: the browser owns routing, the
		// server hands back the entry document.
		return h.serveIndex(w)
	}
	if !within(h.dist, target) {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	w.Header().Set("Content-Type", contentType(target))
	w.WriteHeader(http.StatusOK)
	_, err = w.Write(raw)
	return err
}

// servePlugins pushes the HMR SSE channel (official /plugins/events, client-hmr
// src/events.ts wire): one full graph frame on connect, keepalive comments
// after. The Go host never rebuilds bundles, so no rebuilt frames are ever
// emitted.
func (h *Host) servePluginsEvents(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(w)
	frame, err := json.Marshal(map[string]any{"type": "graph", "graph": h.graph})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", frame); err != nil {
		return nil
	}
	controller.Flush()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			controller.Flush()
		}
	}
}

// serveIndex renders the dist index.html with the client boot protocol rows
// ahead of the document (official renderIndexInjections head placement),
// then through the registry's injection table.
func (h *Host) serveIndex(w http.ResponseWriter) error {
	raw, err := os.ReadFile(filepath.Join(h.dist, "index.html"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	html := spliceAfterHead(string(raw), bootInjectionRows(h.graph))
	rendered, err := h.registry.RenderIndex(h.ctx, html)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, err = w.Write([]byte(rendered))
	return err
}

// spliceAfterHead inserts markup immediately after the opening head tag;
// headless fragments get it prepended (official renderIndexInjections).
func spliceAfterHead(html string, markup string) string {
	open := headOpenRe.FindStringIndex(html)
	if open == nil {
		return markup + html
	}
	at := open[1]
	return html[:at] + markup + html[at:]
}

var headOpenRe = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)

// contentType guesses a media type from the file extension, defaulting to
// application/octet-stream like the official static owner.
func contentType(path string) string {
	if t := mime.TypeByExtension(filepath.Ext(path)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// within reports whether path stays inside the dist root after evaluation.
func within(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

var _ fs.FS = os.DirFS(".")
