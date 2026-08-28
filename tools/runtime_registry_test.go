// Registry behaviors: registration validation, scope shadowing, restriction
// intersection, monotonic guards, presentation declarations, schema
// projection, and change notifications. Ports of the ToolRuntime surface
// tests in packages/core/tools.
package tools

import (
	"context"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/llm"
)

func newTestRuntime(t *testing.T, config Config) *ToolRuntime {
	t.Helper()
	runtime, err := NewToolRuntime(cordis.Discard{}, config)
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	return runtime
}

// echoDefinition defines one string-in/string-out tool; the execute hook is
// optional so registry tests can register bodies they never dispatch.
func echoDefinition(t *testing.T, name string, execute func(args map[string]any, exec *ToolRunContext) (any, error)) *ToolDefinition {
	t.Helper()
	definition, err := DefineTool(DefineToolOptions{
		Name:        name,
		Description: "echo " + name,
		Parameters: map[string]PropSpec{
			"name": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: ToolOutput{
			Schema: &ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: value.(string)}}
			},
		},
		Execute: execute,
	})
	if err != nil {
		t.Fatalf("DefineTool(%s): %v", name, err)
	}
	return definition
}

func mustRegister(t *testing.T, runtime *ToolRuntime, scope ScopeKey, definition *ToolDefinition) func() {
	t.Helper()
	dispose, err := runtime.RegisterIn(scope, definition)
	if err != nil {
		t.Fatalf("RegisterIn(%s): %v", definition.Name, err)
	}
	return dispose
}

func TestRegisterRejectsDuplicatesReservedAndInvalidShapes(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	mustRegister(t, runtime, nil, echoDefinition(t, "echo", nil))

	_, err := runtime.Register(echoDefinition(t, "echo", nil))
	if err == nil || err.Error() != `tool "echo" is already registered (for a per-agent variant, register through that agent's scope instead)` {
		t.Fatalf("global dup err = %v", err)
	}
	scope := NewScopeKey(nil)
	mustRegister(t, runtime, scope, echoDefinition(t, "echo", nil))
	_, err = runtime.RegisterIn(scope, echoDefinition(t, "echo", nil))
	if err == nil || err.Error() != `tool "echo" is already registered in this scope` {
		t.Fatalf("scoped dup err = %v", err)
	}

	reserved := echoDefinition(t, ReservedRunCodeName, nil)
	_, err = runtime.Register(reserved)
	if err == nil || !strings.Contains(err.Error(), "is reserved for the PTC mode presentation transport") {
		t.Fatalf("reserved err = %v", err)
	}
	_, err = runtime.RegisterIn(scope, reserved)
	if err == nil || !strings.Contains(err.Error(), "is reserved") {
		t.Fatalf("reserved scoped err = %v", err)
	}

	noRender := echoDefinition(t, "no_render", nil)
	noRender.render = nil
	_, err = runtime.Register(noRender)
	if err == nil || err.Error() != `tool "no_render" must declare output { schema, render }` {
		t.Fatalf("render err = %v", err)
	}

	badSchema := echoDefinition(t, "bad_schema", nil)
	badSchema.OutputSchema = map[string]any{"type": "strung"}
	_, err = runtime.Register(badSchema)
	if err == nil || !strings.Contains(err.Error(), "schema.type must be one of") {
		t.Fatalf("output schema err = %v", err)
	}

	if _, err := runtime.Register(nil); err == nil || err.Error() != "tools: definition is required" {
		t.Fatalf("nil err = %v", err)
	}
}

func TestScopedRegistrationShadowsAndDisposes(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	mustRegister(t, runtime, nil, echoDefinition(t, "echo", nil))
	scope := NewScopeKey(nil)
	scoped := echoDefinition(t, "echo", nil)
	scoped.Description = "scoped variant"
	mustRegister(t, runtime, scope, scoped)
	child := NewScopeKey(scope)

	if got, _ := runtime.Get("echo", nil); got.Description != "echo echo" {
		t.Fatalf("global view = %q", got.Description)
	}
	if got, _ := runtime.Get("echo", scope); got.Description != "scoped variant" {
		t.Fatalf("scoped view = %q", got.Description)
	}
	if got, _ := runtime.Get("echo", child); got.Description != "scoped variant" {
		t.Fatalf("child inherits nearest scope = %q", got.Description)
	}
}

func TestRestrictIntersectsAndExemptsOwnRegistrations(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	for _, name := range []string{"alpha", "beta", "gamma"} {
		mustRegister(t, runtime, nil, echoDefinition(t, name, nil))
	}
	scope := NewScopeKey(nil)
	if _, err := runtime.RestrictIn(nil, []string{"alpha"}, nil); err == nil || !strings.Contains(err.Error(), "requires a scope") {
		t.Fatalf("global restrict err = %v", err)
	}
	if _, err := runtime.RestrictIn(scope, nil, nil); err == nil || !strings.Contains(err.Error(), "empty restriction is a no-op") {
		t.Fatalf("empty restrict err = %v", err)
	}
	if _, err := runtime.RestrictIn(scope, []string{ReservedRunCodeName}, nil); err == nil || !strings.Contains(err.Error(), "cannot name reserved") {
		t.Fatalf("reserved restrict err = %v", err)
	}
	if _, err := runtime.RestrictIn(scope, []string{"zeta"}, nil); err == nil ||
		!strings.Contains(err.Error(), `names unknown global tools [zeta]; known global tools: [alpha beta gamma]`) {
		t.Fatalf("unknown restrict err = %v", err)
	}

	disposeRestrict, err := runtime.RestrictIn(scope, []string{"alpha", "gamma"}, nil)
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	for _, name := range []string{"alpha", "gamma"} {
		if _, ok := runtime.Get(name, scope); !ok {
			t.Fatalf("%s should stay visible", name)
		}
	}
	if _, ok := runtime.Get("beta", scope); ok {
		t.Fatal("beta should be filtered")
	}

	// The scope's own registration survives its own restriction filter.
	own := echoDefinition(t, "delta", nil)
	mustRegister(t, runtime, scope, own)
	if _, ok := runtime.Get("delta", scope); !ok {
		t.Fatal("own-layer registration must be exempt from the filter")
	}

	// Restrictions intersect down the chain.
	grandchild := NewScopeKey(scope)
	if _, err := runtime.RestrictIn(grandchild, nil, []string{"gamma"}); err != nil {
		t.Fatalf("grandchild restrict: %v", err)
	}
	if _, ok := runtime.Get("gamma", grandchild); ok {
		t.Fatal("gamma should be denied by the intersection")
	}
	if _, ok := runtime.Get("alpha", grandchild); !ok {
		t.Fatal("alpha should survive both filters")
	}
	// delta is INHERITED by the grandchild (it lives in the ancestor's own
	// layer), so the ancestor's own restriction filters it there too: the
	// own-layer exemption belongs to the viewing scope alone.
	if _, ok := runtime.Get("delta", grandchild); ok {
		t.Fatal("the ancestor's own registration is not exempt from the ancestor's filter downstream")
	}

	disposeRestrict()
	if _, ok := runtime.Get("beta", scope); !ok {
		t.Fatal("dispose must restore the unrestricted view")
	}
}

func TestGuardDeniesMonotonicallyAfterWaterfall(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	mustRegister(t, runtime, nil, echoDefinition(t, "echo", func(args map[string]any, exec *ToolRunContext) (any, error) {
		return "ran:" + args["name"].(string), nil
	}))
	observed := false
	undoPre := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
		observed = true
		return next(exec)
	})
	defer undoPre()

	disposeGuard, err := runtime.Guard(func(execution *ToolExecution) (string, bool) {
		return "guard says no", true
	})
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	result := runtime.Execute(&ToolExecutionInput{
		CallID: "call-1", Name: "echo",
		Arguments: map[string]any{"name": "ada"}, Signal: context.Background(),
	})
	if !observed {
		t.Fatal("pre-execute must run before guards")
	}
	if !result.IsError || result.Content[0].Text != "Error: guard says no" {
		t.Fatalf("result = %+v", result)
	}
	if result.Error == nil || result.Error.Message != "guard says no" || result.Error.Info != nil {
		t.Fatalf("error = %+v", result.Error)
	}

	disposeGuard()
	result = runtime.Execute(&ToolExecutionInput{
		CallID: "call-2", Name: "echo",
		Arguments: map[string]any{"name": "ada"}, Signal: context.Background(),
	})
	if result.IsError || result.Value != "ran:ada" {
		t.Fatalf("post-dispose result = %+v", result)
	}
}

func TestPresentAsValidationAndModeChain(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	scope := NewScopeKey(nil)

	if _, err := runtime.PresentAs(nil, ModePtc); err == nil || !strings.Contains(err.Error(), `requires a scope`) {
		t.Fatalf("nil scope err = %v", err)
	}
	if _, err := runtime.PresentAs(scope, "sideways"); err == nil || err.Error() != `tools: unknown presentation mode "sideways"` {
		t.Fatalf("mode err = %v", err)
	}
	dispose, err := runtime.PresentAs(scope, ModePtc)
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if _, err := runtime.PresentAs(scope, ModeBoth); err == nil ||
		!strings.Contains(err.Error(), `PresentAs("both") conflicts with "ptc" already declared`) {
		t.Fatalf("conflict err = %v", err)
	}
	child := NewScopeKey(scope)
	if got := runtime.modeFor(child); got != ModePtc {
		t.Fatalf("child mode = %q", got)
	}
	dispose()
	if got := runtime.modeFor(child); got != ModeNative {
		t.Fatalf("post-dispose child mode = %q", got)
	}
}

func TestSchemasAreSortedAndCloned(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	mustRegister(t, runtime, nil, echoDefinition(t, "beta", nil))
	mustRegister(t, runtime, nil, echoDefinition(t, "alpha", nil))

	schemas := runtime.Schemas(nil)
	if len(schemas) != 2 || schemas[0].Name != "alpha" || schemas[1].Name != "beta" {
		t.Fatalf("schemas = %+v", schemas)
	}
	schemas[0].Parameters["injected"] = map[string]any{"evil": true}
	again := runtime.Schemas(nil)
	if _, ok := again[0].Parameters["injected"]; ok {
		t.Fatal("returned schemas must be detached clones")
	}
}

func TestOnChangeFiresOnSubjectChangesNotGuards(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	changes := 0
	defer runtime.OnChange(func() { changes++ })()

	mustRegister(t, runtime, nil, echoDefinition(t, "echo", nil))
	if changes != 1 {
		t.Fatalf("register changes = %d", changes)
	}
	scope := NewScopeKey(nil)
	disposeRestrict, err := runtime.RestrictIn(scope, []string{"echo"}, nil)
	if err != nil {
		t.Fatalf("restrict: %v", err)
	}
	if changes != 2 {
		t.Fatalf("restrict changes = %d", changes)
	}
	disposePresent, err := runtime.PresentAs(scope, ModeBoth)
	if err != nil {
		t.Fatalf("present: %v", err)
	}
	if changes != 3 {
		t.Fatalf("present changes = %d", changes)
	}
	disposeGuard, err := runtime.Guard(func(*ToolExecution) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if changes != 3 {
		t.Fatalf("guard must not notify; changes = %d", changes)
	}
	disposeGuard()
	disposePresent()
	disposeRestrict()
	if changes != 6 {
		t.Fatalf("dispose changes = %d", changes)
	}
}

func TestExecutionModeAndWireSchemas(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	safe := echoDefinition(t, "safe", nil)
	safe.isConcurrencySafe = func(map[string]any) bool { return true }
	mustRegister(t, runtime, nil, safe)
	mustRegister(t, runtime, nil, echoDefinition(t, "unsafe", nil))

	parallel := runtime.ExecutionMode(&ToolExecutionInput{
		Name: "safe", Arguments: map[string]any{"name": "x"}, Signal: context.Background(),
	})
	if parallel != ModeParallel {
		t.Fatalf("safe mode = %q", parallel)
	}
	exclusive := runtime.ExecutionMode(&ToolExecutionInput{
		Name: "unsafe", Arguments: map[string]any{"name": "x"}, Signal: context.Background(),
	})
	if exclusive != ModeExclusive {
		t.Fatalf("unsafe mode = %q", exclusive)
	}
	unknown := runtime.ExecutionMode(&ToolExecutionInput{Name: "ghost", Signal: context.Background()})
	if unknown != ModeExclusive {
		t.Fatalf("unknown mode = %q", unknown)
	}

	wire, err := runtime.WireSchemas(nil)
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	if len(wire.KnownNames) != 2 || wire.KnownNames[0] != "safe" || wire.KnownNames[1] != "unsafe" {
		t.Fatalf("known = %v", wire.KnownNames)
	}

	ptc := newTestRuntime(t, Config{Mode: ModePtc})
	if _, err := ptc.WireSchemas(nil); err == nil || !strings.Contains(err.Error(), `mode "ptc" requires a code runtime`) {
		t.Fatalf("ptc wire err = %v", err)
	}
}
