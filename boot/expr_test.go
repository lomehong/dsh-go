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

func TestExprLogicalOrOperator(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	// The telemetry mode row uses `||` (not `??`): a set env wins, an empty
	// one falls back — the exact base-bundle expression.
	t.Setenv("DSH_TELEMETRY_MODE", "FULL")
	value, err := evalOne(t, `process.env.DSH_TELEMETRY_MODE || 'FEEDBACK_ONLY'`, root)
	if err != nil || value != "FULL" {
		t.Fatalf("set = %v %v", value, err)
	}
	t.Setenv("DSH_TELEMETRY_MODE", "")
	value, err = evalOne(t, `process.env.DSH_TELEMETRY_MODE || 'FEEDBACK_ONLY'`, root)
	if err != nil || value != "FEEDBACK_ONLY" {
		t.Fatalf("empty = %v %v", value, err)
	}
}

func TestExprWebStartupOpenBrowser(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	root.Provide("webStartup", map[string]any{"openBrowser": true})
	value, err := evalOne(t, `ctx.webStartup.openBrowser`, root)
	if err != nil || value != true {
		t.Fatalf("openBrowser = %v %v", value, err)
	}
	// Absent field reads undefined (nil).
	value, err = evalOne(t, `ctx.webStartup.openBrowser`, cordis.NewRoot(cordis.Discard{}))
	if err != nil || value != nil {
		t.Fatalf("absent = %v %v", value, err)
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

func TestParseWebStartup(t *testing.T) {
	values, err := parseWebStartup(nil)
	if err != nil || values["openBrowser"] != true {
		t.Fatalf("empty = %+v %v", values, err)
	}
	hosts, _ := values["trustedHosts"].([]any)
	if len(hosts) != 0 {
		t.Fatalf("empty trustedHosts = %v", hosts)
	}
	values, err = parseWebStartup([]string{"--host", "127.0.0.1", "--port", "8080", "--no-open", "--trusted-host", "a.internal", "--trusted-host", "b:443"})
	if err != nil {
		t.Fatal(err)
	}
	if values["host"] != "127.0.0.1" || values["port"] != float64(8080) || values["openBrowser"] != false {
		t.Fatalf("values = %+v", values)
	}
	hosts, _ = values["trustedHosts"].([]any)
	if len(hosts) != 2 || hosts[0] != "a.internal" || hosts[1] != "b:443" {
		t.Fatalf("trustedHosts = %v", hosts)
	}
	// openBrowser defaults true and --host is conditionally present.
	values, err = parseWebStartup([]string{"--port", "0"})
	if err != nil || values["openBrowser"] != true || values["port"] != float64(0) {
		t.Fatalf("defaults = %+v %v", values, err)
	}
	if _, ok := values["host"]; ok {
		t.Fatal("host must be absent when --host is not named")
	}
	if _, err := parseWebStartup([]string{"--port", "abc"}); err == nil ||
		!strings.Contains(err.Error(), "must be a number") {
		t.Fatalf("bad port err = %v", err)
	}
	// The safety rejection: 0.0.0.0 must be refused verbatim.
	if _, err := parseWebStartup([]string{"--host", "0.0.0.0"}); err == nil ||
		!strings.Contains(err.Error(), "is intentionally not supported yet for safety") {
		t.Fatalf("0.0.0.0 err = %v", err)
	}
}

func TestWebRuntimeConfig(t *testing.T) {
	// Defaults: openBrowser from webStartup, printUrl true, trusted from startup.
	settings, err := webRuntimeConfig(map[string]any{"openBrowser": true, "trustedHosts": []any{"a"}}, nil)
	if err != nil || !settings.openBrowser || !settings.printUrl || len(settings.trustedHosts) != 1 {
		t.Fatalf("defaults = %+v %v", settings, err)
	}
	// --no-open clears openBrowser; config printUrl:false wins.
	settings, err = webRuntimeConfig(map[string]any{"openBrowser": false}, map[string]any{"printUrl": false})
	if err != nil || settings.openBrowser || settings.printUrl {
		t.Fatalf("overrides = %+v %v", settings, err)
	}
}

func TestResolveLanTrust(t *testing.T) {
	// Loopback binds sample no LAN addresses.
	loopback := resolveLanTrust("127.0.0.1", []string{"explicit"})
	if len(loopback.lanAddresses) != 0 || len(loopback.trustedHosts) != 1 || loopback.trustedHosts[0] != "explicit" {
		t.Fatalf("loopback = %+v", loopback)
	}
	// All-interfaces binds collect non-internal IPv4 + explicit authorities.
	all := resolveLanTrust("0.0.0.0", []string{"explicit"})
	if len(all.trustedHosts) < 1 {
		t.Fatalf("all-interfaces = %+v", all)
	}
	for _, host := range all.lanAddresses {
		if host == "127.0.0.1" {
			t.Fatalf("loopback leaked into LAN: %v", all.lanAddresses)
		}
	}
}
