package webhost

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/host/webserver"
)

// newTestHost builds a registry with one index-injection row and a Host over
// a fixture dist tree, bound on an OS-assigned port.
func newTestHost(t *testing.T) (*Host, *webserver.Registry, *cordis.Context, string) {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := webserver.New(cordis.Discard{})
	ctx := root.Child()
	ctx.On("webserver/index-inject", func(value any, next func(any) any) any {
		rows := value.(*[]webserver.IndexInjection)
		*rows = append(*rows, webserver.IndexInjection{
			Kind: webserver.RowScriptSrc, Placement: webserver.PlacementHead, Src: "/injected.js",
		})
		return next(rows)
	})

	dist := t.TempDir()
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("<html><head></head><body>app</body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "assets", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}

	host, err := Mount(registry, ctx, dist, cordis.Discard{})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Listen("127.0.0.1", "0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host, registry, ctx, dist
}

func TestMountFailsLoudWithoutDistIndex(t *testing.T) {
	registry := webserver.New(cordis.Discard{})
	root := cordis.NewRoot(cordis.Discard{})
	empty := t.TempDir()
	if _, err := Mount(registry, root.Child(), empty, cordis.Discard{}); err == nil ||
		!strings.Contains(err.Error(), "no index.html") {
		t.Fatalf("err = %v", err)
	}
}

func TestServeIndexRendersInjections(t *testing.T) {
	host, _, _, _ := newTestHost(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := host.serve(rec, req); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<script src="/injected.js">`) {
		t.Fatalf("injection missing: %q", body)
	}
	if !strings.Contains(body, "__DSH_BOOT_READY__") {
		t.Fatalf("boot-ready tail missing: %q", body)
	}
}

func TestServeStaticAssetWithMime(t *testing.T) {
	host, _, _, _ := newTestHost(t)
	req := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rec := httptest.NewRecorder()
	if err := host.serve(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Body.String() != "console.log(1)" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/javascript") {
		t.Fatalf("mime = %q", rec.Header().Get("Content-Type"))
	}
}

func TestSpaFallbackReturnsIndexForExtensionlessPaths(t *testing.T) {
	host, _, _, _ := newTestHost(t)
	req := httptest.NewRequest(http.MethodGet, "/some/route", nil)
	rec := httptest.NewRecorder()
	if err := host.serve(rec, req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "app") {
		t.Fatalf("spa fallback body = %q", rec.Body.String())
	}
}

func TestTraversalOutsideDistRejected(t *testing.T) {
	host, _, _, _ := newTestHost(t)
	req := httptest.NewRequest(http.MethodGet, "/..%2f..%2fetc%2fpasswd", nil)
	rec := httptest.NewRecorder()
	if err := host.serve(rec, req); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("traversal status = %d", rec.Code)
	}
}

func TestListenBindsAndServesHTTP(t *testing.T) {
	host, _, _, _ := newTestHost(t)
	if host.Addr() == nil {
		t.Fatal("no bound address")
	}
	resp, err := http.Get("http://" + host.Addr().String() + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "__DSH_BOOT_READY__") {
		t.Fatalf("status = %d body = %q", resp.StatusCode, body)
	}
}

func TestResolveFrontendDistWalksFromAnchor(t *testing.T) {
	base := t.TempDir()
	anchor := filepath.Join(base, "bin", "dsh", "package.json")
	dist := filepath.Join(base, "node_modules", "@deepseek-ai", "dsh-frontend", "dist")
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "index.html"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveFrontendDist(anchor)
	if err != nil {
		t.Fatal(err)
	}
	if got != dist {
		t.Fatalf("dist = %q want %q", got, dist)
	}
}

func TestResolveFrontendDistFailsLoudWhenMissing(t *testing.T) {
	base := t.TempDir()
	anchor := filepath.Join(base, "package.json")
	if _, err := ResolveFrontendDist(anchor); err == nil ||
		!strings.Contains(err.Error(), "frontend dist not built") {
		t.Fatalf("err = %v", err)
	}
}
