package boot

import (
	"runtime"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/cordis/loader"
)

func evalOne(t *testing.T, source string, root *cordis.Context) (any, error) {
	t.Helper()
	return evaluateExpression(evalContext{ctx: root}, source)
}

func TestExprDshHomePath(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	value, err := evalOne(t, `dshHomePath('sessions')`, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(value.(string), "sessions") {
		t.Fatalf("dshHomePath = %q", value)
	}
}

func TestExprEnvWithNullishFallback(t *testing.T) {
	t.Setenv("DSH_EXPR_PROBE", "probe-value")
	root := cordis.NewRoot(cordis.Discard{})
	value, err := evalOne(t, `process.env.DSH_EXPR_PROBE ?? 'fallback'`, root)
	if err != nil || value != "probe-value" {
		t.Fatalf("set env = %v %v", value, err)
	}
	value, err = evalOne(t, `process.env.DSH_EXPR_MISSING ?? 'fallback'`, root)
	if err != nil || value != "fallback" {
		t.Fatalf("unset env = %v %v", value, err)
	}
}

func TestExprProcessCwd(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	value, err := evalOne(t, `process.cwd()`, root)
	if err != nil || value == "" {
		t.Fatalf("cwd = %v %v", value, err)
	}
}

func TestExprPlatformPredicate(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	want := runtime.GOOS == "windows"
	value, err := evalOne(t, `process.platform === 'win32'`, root)
	if err != nil || value != want {
		t.Fatalf("win32 = %v %v, want %v", value, err, want)
	}
	negated, err := evalOne(t, `process.platform !== 'win32'`, root)
	if err != nil || negated != !want {
		t.Fatalf("!win32 = %v %v, want %v", negated, err, !want)
	}
}

func TestExprApprovalPolicyTernary(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	t.Setenv("DSH_PERMISSION_MODE", "read-only")
	value, err := evalOne(t, `(process.env.DSH_PERMISSION_MODE ?? 'workspace-write') === 'danger-full-access' ? 'never' : 'ask'`, root)
	if err != nil || value != "ask" {
		t.Fatalf("non-danger = %v %v", value, err)
	}
	t.Setenv("DSH_PERMISSION_MODE", "danger-full-access")
	value, err = evalOne(t, `(process.env.DSH_PERMISSION_MODE ?? 'workspace-write') === 'danger-full-access' ? 'never' : 'ask'`, root)
	if err != nil || value != "never" {
		t.Fatalf("danger = %v %v", value, err)
	}
	// Unset env: the nullish fallback feeds the comparison.
	t.Setenv("DSH_PERMISSION_MODE", "")
	value, err = evalOne(t, `(process.env.DSH_PERMISSION_MODE ?? 'workspace-write') === 'danger-full-access' ? 'never' : 'ask'`, root)
	if err != nil || value != "ask" {
		t.Fatalf("empty env = %v %v", value, err)
	}
}

func TestExprServiceReferenceWithNullish(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	root.Provide("webStartup", map[string]any{
		"host": "0.0.0.0", "port": float64(8080), "mode": "production",
		"trustedHosts": []any{"a.internal"},
	})
	host, err := evalOne(t, `ctx.webStartup.host ?? '127.0.0.1'`, root)
	if err != nil || host != "0.0.0.0" {
		t.Fatalf("host = %v %v", host, err)
	}
	port, err := evalOne(t, `ctx.webStartup.port ?? 3080`, root)
	if err != nil || port != float64(8080) {
		t.Fatalf("port = %v %v", port, err)
	}
	mode, err := evalOne(t, `ctx.webStartup.mode`, root)
	if err != nil || mode != "production" {
		t.Fatalf("mode = %v %v", mode, err)
	}
	trusted, err := evalOne(t, `ctx.webStartup.trustedHosts`, root)
	if err != nil {
		t.Fatal(err)
	}
	if list, ok := trusted.([]any); !ok || len(list) != 1 {
		t.Fatalf("trustedHosts = %v", trusted)
	}
	// A missing field or absent service reads as undefined; nullish wins.
	fallback, err := evalOne(t, `ctx.webStartup.missing ?? 'd'`, root)
	if err != nil || fallback != "d" {
		t.Fatalf("missing field = %v %v", fallback, err)
	}
	absent, err := evalOne(t, `ctx.otherService.host ?? 'x'`, root)
	if err != nil || absent != "x" {
		t.Fatalf("absent service = %v %v", absent, err)
	}
}

func TestExprUnknownRootFailsLoud(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	if _, err := evalOne(t, `require('node:fs')`, root); err == nil ||
		!strings.Contains(err.Error(), "not supported by the Go evaluator") {
		t.Fatalf("require err = %v", err)
	}
	if _, err := evalOne(t, `eval('process.exit()')`, root); err == nil ||
		!strings.Contains(err.Error(), "not supported by the Go evaluator") {
		t.Fatalf("eval err = %v", err)
	}
	if _, err := evalOne(t, `1 + 1`, root); err == nil {
		t.Fatal("arithmetic outside the supported grammar must fail loud")
	}
	if _, err := evalOne(t, `unknownRoot.x`, root); err == nil ||
		!strings.Contains(err.Error(), "unsupported root") {
		t.Fatalf("unknown root member err = %v", err)
	}
}

func TestEvaluateConfigWalksNestedMaps(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	t.Setenv("DSH_EXPR_PROBE", "nested")
	cfg := map[string]any{
		"outer": map[string]any{
			"root": loader.RawExpression(`dshHomePath('sessions')`),
			"list": []any{loader.RawExpression(`process.env.DSH_EXPR_PROBE`)},
		},
		"plain": "kept",
	}
	evaled, err := evaluateConfig(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	outer := evaled.(map[string]any)["outer"].(map[string]any)
	if !strings.HasSuffix(outer["root"].(string), "sessions") {
		t.Fatalf("nested root = %v", outer["root"])
	}
	if outer["list"].([]any)[0] != "nested" {
		t.Fatalf("nested list = %v", outer["list"])
	}
	if evaled.(map[string]any)["plain"] != "kept" {
		t.Fatalf("plain = %v", evaled.(map[string]any)["plain"])
	}
}
