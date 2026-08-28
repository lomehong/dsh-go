// Contract tests for the system-prompt registry: defaults, ordering,
// shadowing, interpolation, tool ordering, complete-section restoration,
// runtime-context suppression, scoped waterfall dispatch, and the assembly
// invariant.
package systemprompt

import (
	"context"
	"math"
	"strings"
	"testing"

	"dshgo/llm"
)

func newService(t *testing.T, config Config) *SystemPrompt {
	t.Helper()
	service, err := NewSystemPrompt(config)
	if err != nil {
		t.Fatalf("NewSystemPrompt: %v", err)
	}
	return service
}

func mustSection(t *testing.T, sp *SystemPrompt, scope ScopeKey, section PromptSection) func() {
	t.Helper()
	dispose, err := sp.Section(scope, section)
	if err != nil {
		t.Fatalf("Section(%s): %v", section.Name, err)
	}
	return dispose
}

func mustVariable(t *testing.T, sp *SystemPrompt, scope ScopeKey, name string, value string) func() {
	t.Helper()
	dispose, err := sp.Variable(scope, name, func(AssembleContext) (string, bool) { return value, true })
	if err != nil {
		t.Fatalf("Variable(%s): %v", name, err)
	}
	return dispose
}

func schema(name string) llm.ToolSchema {
	return llm.ToolSchema{Name: name, Description: "d:" + name, Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}

func TestDefaultsIncludeIdentityAndPersona(t *testing.T) {
	sp := newService(t, Config{Persona: "Be terse."})
	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	rendered, err := RenderPrompt(assembly)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	want := "You are an AI agent powered by DeepSeek Harness.\n\nBe terse."
	if rendered != want {
		t.Fatalf("rendered = %q", rendered)
	}
	if failures := ValidateAssembly(assembly); failures != nil {
		t.Fatalf("invariant failures = %v", failures)
	}
}

func TestConfigTogglesIdentityAndRuntimeContext(t *testing.T) {
	off := false
	sp := newService(t, Config{IncludeHarnessIdentity: &off, IncludeRuntimeContext: &off})
	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	rendered, _ := RenderPrompt(assembly)
	if strings.Contains(rendered, "DeepSeek Harness") {
		t.Fatalf("identity should be excluded: %q", rendered)
	}
	if len(assembly.Contexts) != 0 {
		t.Fatalf("contexts = %+v", assembly.Contexts)
	}
}

func TestConfigToolOrderValidation(t *testing.T) {
	if _, err := NewSystemPrompt(Config{ToolOrder: []string{"a", "a", TOOL_ORDER_REST}}); err == nil ||
		err.Error() != `toolOrder lists "a" more than once` {
		t.Fatalf("dup err = %v", err)
	}
	if _, err := NewSystemPrompt(Config{ToolOrder: []string{"a"}}); err == nil ||
		err.Error() != `toolOrder must contain the "<unlisted-tools>" rest entry (where unlisted tools are inserted)` {
		t.Fatalf("rest err = %v", err)
	}
}

func TestSectionOrderingTiebreakAndScopedShadowing(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	mustSection(t, sp, nil, PromptSection{Name: "zulu", Order: 10, Text: "z"})
	mustSection(t, sp, nil, PromptSection{Name: "alpha", Order: 10, Text: "a"})
	mustSection(t, sp, nil, PromptSection{Name: "early", Order: -5, Text: "e"})

	scope := NewScopeKey(nil)
	// The scoped definition replaces the whole section, including its order.
	mustSection(t, sp, scope, PromptSection{Name: "alpha", Order: 99, Text: "scoped alpha"})

	assembly, err := sp.Assemble(AssembleContext{Scope: scope, Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var texts []string
	for _, section := range assembly.Sections {
		texts = append(texts, section.Text)
	}
	// -5 early, then the always-registered persona slot at order 0 (empty
	// text renders away later), then order-10 "zulu" ("alpha" is shadowed
	// away), then the scoped "alpha" at its own order 99.
	want := []string{"e", "", "z", "scoped alpha"}
	if len(texts) != 4 || texts[0] != want[0] || texts[1] != want[1] || texts[2] != want[2] || texts[3] != want[3] {
		t.Fatalf("texts = %v", texts)
	}
	// The global view does not see the scoped shadow.
	globalAssembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("global Assemble: %v", err)
	}
	for _, section := range globalAssembly.Sections {
		if section.Name == "alpha" && section.Text != "a" {
			t.Fatalf("global alpha = %q", section.Text)
		}
	}
}

func TestDuplicateRegistrationMessages(t *testing.T) {
	sp := newService(t, Config{})
	mustSection(t, sp, nil, PromptSection{Name: "s", Order: 1, Text: "x"})
	_, err := sp.Section(nil, PromptSection{Name: "s", Order: 2, Text: "y"})
	if err == nil || err.Error() != `prompt section "s" is already registered (for a per-agent override, register through that agent's `+"`agent.ctx`"+` instead)` {
		t.Fatalf("global section err = %v", err)
	}
	scope := NewScopeKey(nil)
	_, err = sp.Section(scope, PromptSection{Name: "s", Order: 1, Text: "x"})
	if err != nil {
		t.Fatalf("scoped first: %v", err)
	}
	_, err = sp.Section(scope, PromptSection{Name: "s", Order: 2, Text: "y"})
	if err == nil || err.Error() != `prompt section "s" is already registered in this scope` {
		t.Fatalf("scoped section err = %v", err)
	}

	if _, err := sp.Context(nil, PromptContext{Name: "c", Order: 1, Text: "x"}); err != nil {
		t.Fatalf("context: %v", err)
	}
	_, err = sp.Context(nil, PromptContext{Name: "c", Order: 2, Text: "y"})
	if err == nil || !strings.Contains(err.Error(), `prompt context "c" is already registered`) {
		t.Fatalf("context err = %v", err)
	}

	if _, err := sp.Variable(nil, "v", func(AssembleContext) (string, bool) { return "x", true }); err != nil {
		t.Fatalf("variable: %v", err)
	}
	_, err = sp.Variable(nil, "v", func(AssembleContext) (string, bool) { return "y", true })
	if err == nil || !strings.Contains(err.Error(), `prompt variable "v" is already registered`) {
		t.Fatalf("variable err = %v", err)
	}
	_, err = sp.Variable(nil, "Bad-Name", func(AssembleContext) (string, bool) { return "x", true })
	if err == nil || err.Error() != `invalid prompt variable name "Bad-Name" (must match /^[a-z][a-z0-9_]*$/)` {
		t.Fatalf("name err = %v", err)
	}
}

func TestNonFiniteOrderRejected(t *testing.T) {
	sp := newService(t, Config{})
	if _, err := sp.Section(nil, PromptSection{Name: "x", Order: math.Inf(1), Text: "x"}); err == nil ||
		err.Error() != `prompt section "x" order must be a finite number` {
		t.Fatalf("section err = %v", err)
	}
	if _, err := sp.Context(nil, PromptContext{Name: "x", Order: math.NaN(), Text: "x"}); err == nil ||
		err.Error() != `prompt context "x" order must be a finite number` {
		t.Fatalf("context err = %v", err)
	}
}

func TestRenderPromptInterpolationContract(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	mustVariable(t, sp, nil, "topic", "cats")
	mustVariable(t, sp, nil, "empty_ok", "value")
	mustVariable(t, sp, nil, "loop", "{{topic}}!")
	mustSection(t, sp, nil, PromptSection{Name: "main", Order: 0, Text: "About {{topic}} and {{topic}} again."})
	mustSection(t, sp, nil, PromptSection{Name: "dropped", Order: 1, Text: ""})
	mustSection(t, sp, nil, PromptSection{Name: "notrescanned", Order: 2, Text: "Literal: {{loop}}"})

	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	rendered, err := RenderPrompt(assembly)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	want := "About cats and cats again.\n\nLiteral: {{topic}}!"
	if rendered != want {
		t.Fatalf("rendered = %q", rendered)
	}

	// Registered but undefined.
	if _, err := sp.Variable(nil, "missing_later", func(AssembleContext) (string, bool) { return "", false }); err != nil {
		t.Fatalf("undefined variable: %v", err)
	}
	mustSection(t, sp, nil, PromptSection{Name: "undef", Order: 3, Text: "Hi {{missing_later}}"})
	assembly, _ = sp.Assemble(AssembleContext{Signal: context.Background()})
	_, err = RenderPrompt(assembly)
	if err == nil || err.Error() != `prompt variable "{{missing_later}}" has no value for this assembly (section "undef")` {
		t.Fatalf("undefined err = %v", err)
	}

	// Each remaining diagnostics case runs on a fresh registry so render
	// order cannot mask one failure with an earlier section's.
	cases := []struct {
		name string
		text string
		want string
	}{
		{"unknown", "{{nope}}", `unknown prompt variable "{{nope}}" in section "unknown"; registered variables: topic, empty_ok, loop, missing_later`},
		{"invalid", "{{9lives}}", `malformed prompt variable reference "{{9lives}}" in section "invalid" (variable names match /^[a-z][a-z0-9_]*$/)`},
		{"malformed", "oops {{topic and {{other_ok}}", `malformed prompt variable reference at "{{topic and {{ot` + "\u2026" + `" in section "malformed" (references are complete simple {{name}} groups)`}, // ellipsis inside the quotes
	}
	for _, tc := range cases {
		one := newService(t, Config{IncludeHarnessIdentity: new(bool)})
		mustVariable(t, one, nil, "topic", "cats")
		mustVariable(t, one, nil, "empty_ok", "value")
		mustVariable(t, one, nil, "loop", "{{topic}}!")
		if _, err := one.Variable(nil, "missing_later", func(AssembleContext) (string, bool) { return "", false }); err != nil {
			t.Fatalf("undefined variable: %v", err)
		}
		mustSection(t, one, nil, PromptSection{Name: tc.name, Order: 0, Text: tc.text})
		fresh, err := one.Assemble(AssembleContext{Signal: context.Background()})
		if err != nil {
			t.Fatalf("%s assemble: %v", tc.name, err)
		}
		_, err = RenderPrompt(fresh)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s err = %v", tc.name, err)
		}
	}
}

func TestLoneOpenBraceIsLiteralProse(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	mustSection(t, sp, nil, PromptSection{Name: "prose", Order: 0, Text: "use { or {{ freely: 2 {{ 3"})
	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	rendered, err := RenderPrompt(assembly)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if rendered != "use { or {{ freely: 2 {{ 3" {
		t.Fatalf("rendered = %q", rendered)
	}
}

func TestRenderContextSnapshot(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	if _, err := sp.Context(nil, PromptContext{Name: "todo", Order: 1, Text: "Do the thing."}); err != nil {
		t.Fatalf("context: %v", err)
	}
	if _, err := sp.Context(nil, PromptContext{Name: "empty", Order: 2, Text: ""}); err != nil {
		t.Fatalf("context: %v", err)
	}
	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	snapshot, err := RenderContextSnapshot(assembly)
	if err != nil {
		t.Fatalf("RenderContextSnapshot: %v", err)
	}
	want := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nDo the thing."
	if snapshot != want {
		t.Fatalf("snapshot = %q", snapshot)
	}
	// Equal orders keep insertion order; no name tiebreak.
	if assembly.Contexts[0].Name != "todo" || assembly.Contexts[1].Name != "empty" {
		t.Fatalf("contexts = %+v", assembly.Contexts)
	}
	if JoinContextSections(nil) != "" {
		t.Fatal("empty join must render empty")
	}
}

func TestAssembleToolOrderingAndProviderContract(t *testing.T) {
	// "retired" is known but contributes no schema this assembly: listed
	// configured names may legitimately be restricted away.
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool), ToolOrder: []string{"retired", TOOL_ORDER_REST, "alpha"}})
	params := map[string]any{"type": "object", "properties": map[string]any{"x": true}}
	undo, err := sp.Tools(nil, func(AssembleContext) ToolProviderResult {
		return ToolProviderResult{Schemas: []llm.ToolSchema{
			{Name: "beta", Description: "b", Parameters: params},
			{Name: "alpha", Description: "a"},
		}}
	})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	defer undo()
	undo2, err := sp.Tools(nil, func(AssembleContext) ToolProviderResult {
		return ToolProviderResult{Schemas: []llm.ToolSchema{{Name: "gamma"}}, KnownNames: []string{"gamma", "retired"}}
	})
	if err != nil {
		t.Fatalf("Tools2: %v", err)
	}
	defer undo2()

	assembly, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var names []string
	for _, tool := range assembly.Tools {
		names = append(names, tool.Name)
	}
	// retired is known-but-restricted (no schema), the rest group is
	// [beta, gamma] lexicographic at the marker, alpha listed last.
	want := []string{"beta", "gamma", "alpha"}
	if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
		t.Fatalf("names = %v", names)
	}
	// Parameters are detached from the provider's map.
	tool := assembly.Tools[0]
	if tool.Name != "beta" {
		t.Fatalf("tool = %s", tool.Name)
	}
	tool.Parameters["properties"].(map[string]any)["x"] = false
	if params["properties"].(map[string]any)["x"] != true {
		t.Fatal("assembled parameters must be clones")
	}

	// Unknown configured name fails against the explicit knownNames union.
	_, err = sp.Assemble(AssembleContext{Signal: context.Background()})
	_ = err
	sp3 := newService(t, Config{IncludeHarnessIdentity: new(bool), ToolOrder: []string{"ghost", TOOL_ORDER_REST}})
	if _, err := sp3.Tools(nil, func(AssembleContext) ToolProviderResult { return ToolProviderResult{} }); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	_, err = sp3.Assemble(AssembleContext{Signal: context.Background()})
	if err == nil || err.Error() != `toolOrder lists unregistered tool "ghost"; known tools: (none)` {
		t.Fatalf("unknown err = %v", err)
	}
	sp4 := newService(t, Config{IncludeHarnessIdentity: new(bool), ToolOrder: []string{"ghost", "wraith", TOOL_ORDER_REST}})
	if _, err := sp4.Tools(nil, func(AssembleContext) ToolProviderResult {
		return ToolProviderResult{Schemas: []llm.ToolSchema{{Name: "ok"}}}
	}); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	_, err = sp4.Assemble(AssembleContext{Signal: context.Background()})
	if err == nil || err.Error() != `toolOrder lists unregistered tools "ghost", "wraith"; known tools: ok` {
		t.Fatalf("unknown plural err = %v", err)
	}

	// A provider cannot smuggle the reserved rest name.
	sp5 := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	if _, err := sp5.Tools(nil, func(AssembleContext) ToolProviderResult {
		return ToolProviderResult{Schemas: []llm.ToolSchema{{Name: TOOL_ORDER_REST}}}
	}); err != nil {
		t.Fatalf("Tools: %v", err)
	}
	_, err = sp5.Assemble(AssembleContext{Signal: context.Background()})
	if err == nil || err.Error() != `tool provider returned reserved tool name "<unlisted-tools>" (reserved for toolOrder's rest entry)` {
		t.Fatalf("reserved err = %v", err)
	}
}

func TestCompleteSectionRestoredAfterWaterfall(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	mustSection(t, sp, nil, PromptSection{Name: "normal", Order: 0, Text: "normal text"})
	scope := NewScopeKey(nil)
	mustSection(t, sp, scope, PromptSection{Name: "complete", Order: 1, Text: "the whole prompt", Complete: true})
	undo := sp.OnAssemble(nil, func(assembly *PromptAssembly, assembleContext AssembleContext, next func() *PromptAssembly) *PromptAssembly {
		assembled := next()
		assembled.Sections = append(assembled.Sections, AssembledSection{Name: "smuggled", Text: "nope"})
		assembled.Contexts = append(assembled.Contexts, AssembledContext{Name: "smuggled-ctx", Text: "nope"})
		return assembled
	})
	defer undo()

	assembly, err := sp.Assemble(AssembleContext{Scope: scope, Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(assembly.Sections) != 1 || assembly.Sections[0].Name != "complete" || assembly.Sections[0].Text != "the whole prompt" {
		t.Fatalf("sections = %+v", assembly.Sections)
	}
	// The scope without the complete section keeps waterfall results.
	other, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err != nil {
		t.Fatalf("other Assemble: %v", err)
	}
	found := false
	for _, section := range other.Sections {
		if section.Name == "smuggled" {
			found = true
		}
	}
	if !found {
		t.Fatal("a scope without a complete section keeps the waterfall result")
	}
}

func TestMultipleCompleteSectionsFail(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	mustSection(t, sp, nil, PromptSection{Name: "one", Order: 0, Text: "1", Complete: true})
	mustSection(t, sp, nil, PromptSection{Name: "two", Order: 1, Text: "2", Complete: true})
	_, err := sp.Assemble(AssembleContext{Signal: context.Background()})
	if err == nil || err.Error() != `multiple complete prompt sections are active: "one", "two"` {
		t.Fatalf("err = %v", err)
	}
}

func TestSuppressRuntimeContext(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	if _, err := sp.Context(nil, PromptContext{Name: "todo", Order: 1, Text: "Do the thing."}); err != nil {
		t.Fatalf("context: %v", err)
	}
	scope := NewScopeKey(nil)
	dispose, err := sp.SuppressRuntimeContext(scope)
	if err != nil {
		t.Fatalf("SuppressRuntimeContext: %v", err)
	}
	assembly, err := sp.Assemble(AssembleContext{Scope: scope, Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(assembly.Contexts) != 0 {
		t.Fatalf("contexts = %+v", assembly.Contexts)
	}
	dispose()
	assembly, err = sp.Assemble(AssembleContext{Scope: scope, Signal: context.Background()})
	if err != nil {
		t.Fatalf("post-dispose Assemble: %v", err)
	}
	if len(assembly.Contexts) != 1 {
		t.Fatalf("post-dispose contexts = %+v", assembly.Contexts)
	}
}

func TestAssembleWaterfallScopedDispatch(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	parent := NewScopeKey(nil)
	child := NewScopeKey(parent)
	seen := map[string]int{}
	undoParent := sp.OnAssemble(parent, func(assembly *PromptAssembly, assembleContext AssembleContext, next func() *PromptAssembly) *PromptAssembly {
		seen["parent"]++
		return next()
	})
	defer undoParent()
	undoChild := sp.OnAssemble(child, func(assembly *PromptAssembly, assembleContext AssembleContext, next func() *PromptAssembly) *PromptAssembly {
		seen["child"]++
		assembled := next()
		assembled.Contexts = append(assembled.Contexts, AssembledContext{Name: "child-ctx", Text: "hello"})
		return assembled
	})
	defer undoChild()

	if _, err := sp.Assemble(AssembleContext{Scope: child, Signal: context.Background()}); err != nil {
		t.Fatalf("child assemble: %v", err)
	}
	if seen["parent"] != 1 || seen["child"] != 1 {
		t.Fatalf("seen = %v (a listener tagged with an ancestor receives descendant dispatch)", seen)
	}
	if _, err := sp.Assemble(AssembleContext{Scope: NewScopeKey(nil), Signal: context.Background()}); err != nil {
		t.Fatalf("unrelated assemble: %v", err)
	}
	if seen["parent"] != 1 || seen["child"] != 1 {
		t.Fatalf("unrelated scope must not dispatch tagged listeners: %v", seen)
	}
	assembly, err := sp.Assemble(AssembleContext{Scope: child, Signal: context.Background()})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	found := false
	for _, assembled := range assembly.Contexts {
		if assembled.Name == "child-ctx" {
			found = true
		}
	}
	if !found {
		t.Fatal("the waterfall return value is authoritative")
	}
}

func TestOnChangeNotifications(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	changes := 0
	defer sp.OnChange(func() { changes++ })()
	disposeSection := mustSection(t, sp, nil, PromptSection{Name: "s", Order: 1, Text: "x"})
	if changes != 1 {
		t.Fatalf("after section = %d", changes)
	}
	disposeContext, err := sp.Context(nil, PromptContext{Name: "c", Order: 1, Text: "y"})
	if err != nil {
		t.Fatalf("context: %v", err)
	}
	if changes != 2 {
		t.Fatalf("after context = %d", changes)
	}
	disposeTools, err := sp.Tools(nil, func(AssembleContext) ToolProviderResult { return ToolProviderResult{} })
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	if changes != 3 {
		t.Fatalf("after tools = %d", changes)
	}
	disposeVariable := mustVariable(t, sp, nil, "v", "1")
	if changes != 4 {
		t.Fatalf("after variable = %d", changes)
	}
	disposeSuppress, err := sp.SuppressRuntimeContext(nil)
	if err != nil {
		t.Fatalf("suppress: %v", err)
	}
	if changes != 5 {
		t.Fatalf("after suppress = %d", changes)
	}
	disposeSection()
	disposeContext()
	disposeTools()
	disposeVariable()
	disposeSuppress()
	if changes != 10 {
		t.Fatalf("after disposals = %d", changes)
	}
}

func TestValidateAssemblyFailures(t *testing.T) {
	assembly := &PromptAssembly{
		Sections:  []AssembledSection{{Name: "s", Text: "x"}, {Name: "s", Text: "y"}},
		Contexts:  []AssembledContext{{Name: "", Text: "z"}},
		Variables: newVariableSet(),
	}
	failures := ValidateAssembly(assembly)
	if len(failures) != 2 ||
		failures[0] != `assembled section name "s" is duplicated` ||
		failures[1] != "assembled context names must be non-empty" {
		t.Fatalf("failures = %v", failures)
	}
}

func TestProviderEvaluationUsesAssemblyContext(t *testing.T) {
	sp := newService(t, Config{IncludeHarnessIdentity: new(bool)})
	scope := NewScopeKey(nil)
	evaluated := ""
	mustSection(t, sp, scope, PromptSection{Name: "dyn", Order: 0, TextProvider: func(ctx AssembleContext) string {
		if ctx.Scope != scope || ctx.Signal != context.Background() {
			return "wrong context"
		}
		evaluated = "provider ran"
		return evaluated
	}})
	assembly, err := sp.Assemble(AssembleContext{Scope: scope, Signal: context.Background()})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	var dynText string
	for _, section := range assembly.Sections {
		if section.Name == "dyn" {
			dynText = section.Text
		}
	}
	if dynText != "provider ran" || evaluated != "provider ran" {
		t.Fatalf("dyn = %q, evaluated = %q", dynText, evaluated)
	}
	if _, err := sp.Assemble(AssembleContext{Signal: context.Background()}); err != nil {
		t.Fatalf("global assemble: %v", err)
	}
}
