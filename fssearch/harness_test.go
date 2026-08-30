package fssearch

import (
	"dshgo/cordis"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// newHarness builds a tool runtime over a fresh composition root with the
// search tools registered and (when pinned) a resolvable rg path.
func newHarness(t testingTB, rgPath string) (*tools.ToolRuntime, *systemprompt.SystemPrompt, *cordis.Context) {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatal(err)
	}
	caps := DefaultCaps()
	caps.RGPath = rgPath
	undo, err := Register(runtime, prompt, root, caps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(undo)
	return runtime, prompt, root
}

// testingTB is the minimal face shared by *testing.T.
type testingTB interface {
	Helper()
	Fatal(...any)
	Fatalf(string, ...any)
	Cleanup(func())
}

func executeGlob(t testingTB, runtime *tools.ToolRuntime, pattern, path string) (string, error) {
	t.Helper()
	definition, ok := runtime.Get("glob", nil)
	if !ok {
		t.Fatal("glob must be registered")
	}
	result, err := definition.Execute(map[string]any{"pattern": pattern, "path": path}, &tools.ToolRunContext{})
	if err != nil {
		return "", err
	}
	value := result.(map[string]any)
	paths := value["paths"].([]any)
	lines := make([]string, 0, len(paths))
	for _, path := range paths {
		lines = append(lines, path.(string))
	}
	return RenderGlobPaths(lines, DefaultCaps(), value["root"].(string), nil), nil
}

func executeGrep(t testingTB, runtime *tools.ToolRuntime, pattern, path, include string) (string, error) {
	t.Helper()
	definition, ok := runtime.Get("grep", nil)
	if !ok {
		t.Fatal("grep must be registered")
	}
	args := map[string]any{"pattern": pattern, "path": path}
	if include != "" {
		args["include"] = include
	}
	result, err := definition.Execute(args, &tools.ToolRunContext{})
	if err != nil {
		return "", err
	}
	outcome := result.(map[string]any)
	kept, seen, truncated := retainFromValue(outcome, DefaultCaps())
	return FormatRetainedGrep(kept, seen, truncated, nil), nil
}
