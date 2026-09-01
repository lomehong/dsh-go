package boot

import (
	"runtime"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/cordis/loader"
)

func TestPlatformEvaluatorWin32Predicate(t *testing.T) {
	RegisterPlatformEvaluator()
	want := runtime.GOOS == "windows"
	got, err := loader.Evaluate(loader.RawExpression(`process.platform === 'win32'`))
	if err != nil {
		t.Fatalf("evaluate win32: %v", err)
	}
	if got != want {
		t.Fatalf("win32 predicate = %v, want %v (GOOS=%s)", got, want, runtime.GOOS)
	}
	negated, err := loader.Evaluate(loader.RawExpression(`process.platform !== 'win32'`))
	if err != nil {
		t.Fatalf("evaluate !win32: %v", err)
	}
	if negated != !want {
		t.Fatalf("!win32 predicate = %v, want %v", negated, !want)
	}
}

func TestPlatformEvaluatorUnknownExpressionFailsLoud(t *testing.T) {
	RegisterPlatformEvaluator()
	if _, err := loader.Evaluate(loader.RawExpression(`process.env.DSH_TOOLS_MODE`)); err == nil ||
		!strings.Contains(err.Error(), "not a supported platform predicate") {
		t.Fatalf("unknown expression err = %v", err)
	}
}

func TestAssembleSkipsPlatformDisabledEntry(t *testing.T) {
	// A disabled entry with the win32 platform predicate is evaluated (not
	// treated as literal) and skipped when the host matches.
	entries := []loader.Entry{{
		ID:       "platform-row",
		Name:     "dsh/echo",
		Disabled: loader.RawExpression(`process.platform === 'win32'`),
	}}
	resolved := false
	app, err := Assemble(cordis.NewRoot(cordis.Discard{}), entries, func(name string) (PluginSpec, error) {
		resolved = true
		return PluginSpec{Apply: func(*cordis.Context, any) error { return nil }}, nil
	})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer app.Shutdown()
	if runtime.GOOS == "windows" && resolved {
		t.Fatal("win32 host must skip the win32-disabled entry")
	}
	if runtime.GOOS != "windows" && !resolved {
		t.Fatal("non-win32 host must mount the win32-disabled entry")
	}
}
