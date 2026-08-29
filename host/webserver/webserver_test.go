package webserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	if code, _ := serve(t, rg, "/model-failover-lookalike"); code != http.StatusOK {
		t.Fatal("prefix must be a plain string prefix (official behavior), so lookalike paths also claim")
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
