package webserver

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dshgo/cordis"
)

func okHandler(msg string) Handler {
	return func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(msg))
		return nil
	}
}

func serve(t *testing.T, rg *Registry, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	rg.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestExactRouteClaimsOwnPathOnly(t *testing.T) {
	rg := New(nil)
	if _, err := rg.Register(Route{Kind: KindExact, Path: "/api/status", Handler: okHandler("status")}); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	code, body := serve(t, rg, "/api/status")
	if code != http.StatusOK || body != "status" {
		t.Fatalf("exact route must answer its own path, got %d %q", code, body)
	}
	code, _ = serve(t, rg, "/api/status/extra")
	if code != http.StatusNotFound {
		t.Fatalf("exact route must not claim subpaths, got %d", code)
	}
}

func TestPrefixRouteClaimsSubtree(t *testing.T) {
	rg := New(nil)
	if _, err := rg.Register(Route{Kind: KindPrefix, Path: "/model-failover", Handler: okHandler("prefix")}); err != nil {
		t.Fatalf("register failed: %v", err)
	}
	for _, p := range []string{"/model-failover", "/model-failover/api/config", "/model-failover/a/b/c"} {
		if code, _ := serve(t, rg, p); code != http.StatusOK {
			t.Fatalf("prefix route must claim %q, got %d", p, code)
		}
	}
	if code, _ := serve(t, rg, "/model-failover-lookalike"); code != http.StatusNotFound {
		t.Fatal("prefix claims only its own path and its /-subtree (official match), not lookalike paths")
	}
}

func TestFallbackSeatClaimsUnclaimedPaths(t *testing.T) {
	rg := New(nil)
	_, errA := rg.Register(Route{Kind: KindExact, Path: "/known", Handler: okHandler("exact")})
	_, errF := rg.Register(Route{Kind: KindFallback, Handler: okHandler("fallback")})
	if errA != nil || errF != nil {
		t.Fatalf("register failed: %v %v", errA, errF)
	}
	if code, body := serve(t, rg, "/known"); code != http.StatusOK || body != "exact" {
		t.Fatalf("first claim must win over fallback, got %d %q", code, body)
	}
	if code, body := serve(t, rg, "/anything/else"); code != http.StatusOK || body != "fallback" {
		t.Fatalf("fallback must catch unclaimed paths, got %d %q", code, body)
	}
}

func TestNoFallbackAnswers404(t *testing.T) {
	rg := New(nil)
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/known", Handler: okHandler("exact")})
	if code, _ := serve(t, rg, "/other"); code != http.StatusNotFound {
		t.Fatalf("unclaimed path without fallback must 404, got %d", code)
	}
}

func TestDuplicateRegistrationRejected(t *testing.T) {
	rg := New(nil)
	if _, err := rg.Register(Route{Kind: KindExact, Path: "/a", Handler: okHandler("1")}); err != nil {
		t.Fatalf("first register failed: %v", err)
	}
	_, err := rg.Register(Route{Kind: KindExact, Path: "/a", Handler: okHandler("2")})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate exact route must be rejected, got %v", err)
	}
	// Same path under a different kind is a different claim and stays legal.
	if _, err := rg.Register(Route{Kind: KindPrefix, Path: "/a", Handler: okHandler("3")}); err != nil {
		t.Fatalf("same path under another kind must be accepted, got %v", err)
	}
}

func TestSecondFallbackRejected(t *testing.T) {
	rg := New(nil)
	if _, err := rg.Register(Route{Kind: KindFallback, Handler: okHandler("1")}); err != nil {
		t.Fatalf("first fallback failed: %v", err)
	}
	_, err := rg.Register(Route{Kind: KindFallback, Handler: okHandler("2")})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("second fallback seat must be rejected, got %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	rg := New(nil)
	cases := []struct {
		name  string
		route Route
		want  string
	}{
		{"unknown kind", Route{Kind: "regex", Path: "/a", Handler: okHandler("x")}, "unknown route kind"},
		{"exact without path", Route{Kind: KindExact, Handler: okHandler("x")}, "requires a path"},
		{"prefix without path", Route{Kind: KindPrefix, Handler: okHandler("x")}, "requires a path"},
		{"fallback with path", Route{Kind: KindFallback, Path: "/a", Handler: okHandler("x")}, "must not set a path"},
		{"no handler", Route{Kind: KindExact, Path: "/a"}, "has no handler"},
	}
	for _, tc := range cases {
		if _, err := rg.Register(tc.route); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestDisposerRemovesRoute(t *testing.T) {
	rg := New(nil)
	dispose, err := rg.Register(Route{Kind: KindExact, Path: "/gone", Handler: okHandler("x")})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if code, _ := serve(t, rg, "/gone"); code != http.StatusOK {
		t.Fatalf("route must serve before disposal, got %d", code)
	}
	dispose()
	if code, _ := serve(t, rg, "/gone"); code != http.StatusNotFound {
		t.Fatalf("disposed route must stop serving, got %d", code)
	}
}

func TestHandlerErrorDegradesTo400WithoutKillingServer(t *testing.T) {
	rg := New(nil)
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/boom", Handler: func(w http.ResponseWriter, r *http.Request) error {
		return errors.New("disk on fire")
	}})
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/healthy", Handler: okHandler("ok")})

	for i := 0; i < 2; i++ {
		if code, _ := serve(t, rg, "/boom"); code != http.StatusBadRequest {
			t.Fatalf("handler error must degrade to 400, got %d", code)
		}
	}
	if code, body := serve(t, rg, "/healthy"); code != http.StatusOK || body != "ok" {
		t.Fatalf("server must keep serving after a failed handler, got %d %q", code, body)
	}
}

func TestHandlerPanicDegradesTo400(t *testing.T) {
	rg := New(nil)
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/panic", Handler: func(w http.ResponseWriter, r *http.Request) error {
		panic("boom")
	}})
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/healthy", Handler: okHandler("ok")})

	if code, _ := serve(t, rg, "/panic"); code != http.StatusBadRequest {
		t.Fatalf("panicking handler must degrade to 400, got %d", code)
	}
	if code, _ := serve(t, rg, "/healthy"); code != http.StatusOK {
		t.Fatalf("server must keep serving after a panic, got %d", code)
	}
}

func TestErrorAfterCommittedResponseLeavesBodyIntact(t *testing.T) {
	rg := New(nil)
	_, _ = rg.Register(Route{Kind: KindExact, Path: "/late", Handler: func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		return errors.New("late failure")
	}})
	code, body := serve(t, rg, "/late")
	if code != http.StatusOK || body != "partial" {
		t.Fatalf("committed response must stay handler-owned, got %d %q", code, body)
	}
}

func TestCordisSeamEndToEnd(t *testing.T) {
	ctx := cordis.NewRoot(cordis.Discard{})
	if err := ctx.Mount(AsPlugin(nil)); err != nil {
		t.Fatalf("mount failed: %v", err)
	}
	registry, ok := ContextService.From(ctx)
	if !ok || registry == nil {
		t.Fatal("webServer service must resolve from the mounted plugin")
	}
	dispose, err := registry.Register(Route{Kind: KindExact, Path: "/ping", Handler: okHandler("pong")})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if code, body := serve(t, registry, "/ping"); code != http.StatusOK || body != "pong" {
		t.Fatalf("registered route must serve through the cordis-provided registry, got %d %q", code, body)
	}
	dispose()
	if code, _ := serve(t, registry, "/ping"); code != http.StatusNotFound {
		t.Fatalf("disposed route must stop serving, got %d", code)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	rg := New(nil)
	_, errShort := rg.Register(Route{Kind: KindPrefix, Path: "/api", Handler: okHandler("short")})
	_, errLong := rg.Register(Route{Kind: KindPrefix, Path: "/api/v2", Handler: okHandler("long")})
	if errShort != nil || errLong != nil {
		t.Fatalf("register failed: %v %v", errShort, errLong)
	}
	if _, body := serve(t, rg, "/api/v2/items"); body != "long" {
		t.Fatalf("longest prefix must win, got %q", body)
	}
	if _, body := serve(t, rg, "/api/other"); body != "short" {
		t.Fatalf("sibling path must fall to the shorter prefix, got %q", body)
	}
	if code, _ := serve(t, rg, "/apx"); code != http.StatusNotFound {
		t.Fatalf("segment boundary must hold, got %d", code)
	}
}

func TestUpgradeDispatchHijacksAndCloses(t *testing.T) {
	rg := New(nil)
	started := make(chan struct{})
	dispose, err := rg.RegisterUpgrade(UpgradeRoute{Path: "/rt", Handler: func(conn net.Conn, rw *bufio.ReadWriter, r *http.Request) error {
		close(started)
		return nil
	}})
	if err != nil {
		t.Fatalf("register upgrade: %v", err)
	}
	server := httptest.NewServer(rg)
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	request := "GET /rt HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upgrade handler never ran")
	}
	rg.Close()
	// The registry owns the hijacked socket: Close must tear it down even
	// though the handler returned and the server keeps running.
	_ = dispose
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	buf := make([]byte, 1)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		t.Fatal("tracked upgrade socket must be closed by Registry.Close")
	}
}

func TestUpgradeUnknownPathAnswers404WithoutHijack(t *testing.T) {
	rg := New(nil)
	server := httptest.NewServer(rg)
	defer server.Close()
	resp, err := http.Get(server.URL + "/no-upgrade")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unclaimed upgrade path must 404, got %d", resp.StatusCode)
	}
}

func TestRenderIndexInjectsRowsAndTaps(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	rg := New(nil)
	inject := root.On("webserver/index-inject", func(value any, next func(any) any) any {
		if table, ok := value.(*[]IndexInjection); ok {
			*table = append(*table,
				IndexInjection{Kind: RowGlobal, Name: "INJ", Value: map[string]any{"a": "<script>"}},
				IndexInjection{Kind: RowScript, Placement: PlacementBody, Text: "boot()"},
			)
		}
		return next(value)
	})
	defer inject()
	tap, err := rg.TapIndex(func(html string) string { return strings.ReplaceAll(html, "</body>", "<b>tap</b></body>") })
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	defer tap()

	rendered, err := rg.RenderIndex(root, "<html><head></head><body><p>x</p></body></html>")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(rendered, `<script>globalThis["INJ"] = {"a":"\u003cscript\u003e"}</script>`) {
		t.Fatalf("global row missing or unescaped: %s", rendered)
	}
	if !strings.Contains(rendered, "<script>boot()</script>") {
		t.Fatalf("body script row missing: %s", rendered)
	}
	if !strings.Contains(rendered, "Promise.withResolvers") {
		t.Fatalf("boot-readiness tail missing: %s", rendered)
	}
	if !strings.Contains(rendered, "<b>tap</b>") {
		t.Fatalf("tap must run after row rendering: %s", rendered)
	}
	// Head rows before </head> … actually: after <head>; body rows after
	// <body> and before the paragraph.
	headAt := strings.Index(rendered, `globalThis["INJ"]`)
	bodyAt := strings.Index(rendered, "boot()")
	paraAt := strings.Index(rendered, "<p>x</p>")
	if !(headAt < bodyAt && bodyAt < paraAt) {
		t.Fatalf("row order must be head rows, body rows, then document content: %s", rendered)
	}
}

func TestRenderIndexUnknownRowKindFailsLoud(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	rg := New(nil)
	_ = root.On("webserver/index-inject", func(value any, next func(any) any) any {
		if table, ok := value.(*[]IndexInjection); ok {
			*table = append(*table, IndexInjection{Kind: "mystery"})
		}
		return next(value)
	})
	if _, err := rg.RenderIndex(root, "<html></html>"); err == nil {
		t.Fatal("unknown row kind must fail the render")
	}
}
