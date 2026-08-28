package loader

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeEntryListPinsKnownFieldsAndPreservesJsNodes(t *testing.T) {
	data := `
- id: time-context-mock-llm
  name: './mock-llm.ts'

- id: agent-spine
  name: '@deepseek-ai/dsh-agent-spine-demo'
  inject: [settings, llm]
  provide: [spine]
  disabled: false
  config:
    agents:
      - id: main
        cwd: !!js process.cwd()
    persona: 'Test the time-context plugin.'
    workspaceContext: false
    retries: 3
`
	entries, err := DecodeEntryList([]byte(data))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].ID != "time-context-mock-llm" || entries[0].Name != "./mock-llm.ts" {
		t.Fatalf("first entry misdecoded: %+v", entries[0])
	}
	spine := entries[1]
	if strings.Join(spine.Inject, ",") != "settings,llm" || strings.Join(spine.Provide, ",") != "spine" {
		t.Fatalf("inject/provide misdecoded: %+v", spine)
	}
	if spine.Disabled != false {
		t.Fatalf("disabled must decode to a literal bool, got %v", spine.Disabled)
	}
	config, ok := spine.Config.(map[string]any)
	if !ok {
		t.Fatalf("config must decode to a map, got %T", spine.Config)
	}
	agents, ok := config["agents"].([]any)
	if !ok || len(agents) != 1 {
		t.Fatalf("nested config must decode, got %#v", config["agents"])
	}
	main := agents[0].(map[string]any)
	if expr, ok := main["cwd"].(RawExpression); !ok || expr.String() != "process.cwd()" {
		t.Fatalf("!!js node must survive as RawExpression, got %#v", main["cwd"])
	}
	if config["retries"] != int64(3) {
		t.Fatalf("ints must decode natively, got %#v", config["retries"])
	}
}

func TestDecodeEntryListRejectsStructuralViolations(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"top-level mapping", "id: x", "top-level YAML array"},
		{"scalar row", "- just-a-string", "must be a mapping"},
		{"expression as id", "- id: !!js pick()\n  name: p", "plain string"},
	}
	for _, tc := range cases {
		if _, err := DecodeEntryList([]byte(tc.data)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestParsePatchFileAnchorsRelativeInsertNames(t *testing.T) {
	data := `
- insert:
    - id: child
      name: './plugins/child.ts'
    - id: absolute
      name: '@deepseek-ai/dsh-web'
`
	patches, err := ParsePatchFile([]byte(data), `D:\work\overlays`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(patches) != 1 || len(patches[0].Insert) != 2 {
		t.Fatalf("misdecoded patches: %+v", patches)
	}
	want := "file:///" + strings.TrimPrefix(filepath.ToSlash(filepath.Join(`D:\work\overlays`, "./plugins/child.ts")), "/")
	if patches[0].Insert[0].Name != want {
		t.Fatalf("relative insert name must anchor to the patch dir as %q, got %q", want, patches[0].Insert[0].Name)
	}
	if patches[0].Insert[1].Name != "@deepseek-ai/dsh-web" {
		t.Fatalf("package names must not be rewritten, got %q", patches[0].Insert[1].Name)
	}
}

func TestApplyEntryPatchesMatchesOfficialAlgorithm(t *testing.T) {
	base := []Entry{
		{ID: "llm", Name: "@deepseek-ai/dsh-llm", Config: map[string]any{"keep": "me"}},
		{ID: "group", Name: "bundle", Group: true, Config: []Entry{
			{ID: "inner", Name: "inner-plugin"},
		}},
	}

	patches := []Patch{
		// Wholesale config replacement (no merge — the official algorithm assigns).
		{ID: "llm", Overrides: []FieldOverride{{Key: "config", Value: map[string]any{"fresh": true}}}},
		// Append at top level.
		{Insert: []Entry{{ID: "appended", Name: "extra"}}},
		// Insert into a group's children.
		{ID: "group", Insert: []Entry{{ID: "adopted", Name: "adopted-plugin"}}},
		// Later patch targets a row an earlier patch inserted.
		{ID: "appended", Overrides: []FieldOverride{{Key: "inject", Value: []any{"webServer"}}}},
	}

	got, warnings := ApplyEntryPatches(base, patches)
	if len(warnings) != 0 {
		t.Fatalf("expected clean apply, got warnings %v", warnings)
	}

	var llm *Entry
	var appended *Entry
	for i := range got {
		switch got[i].ID {
		case "llm":
			llm = &got[i]
		case "appended":
			appended = &got[i]
		}
	}
	if llm == nil || llm.Config.(map[string]any)["fresh"] != true {
		t.Fatalf("config override must replace wholesale, got %#v", llm)
	}
	if _, still := llm.Config.(map[string]any)["keep"]; still {
		t.Fatal("config override must not merge with the old value")
	}
	if appended == nil || strings.Join(appended.Inject, ",") != "webServer" {
		t.Fatalf("later patch must reach a row an earlier patch inserted, got %+v", appended)
	}

	group := got[1]
	children, ok := group.Config.([]Entry)
	if !ok || len(children) != 2 || children[1].ID != "adopted" {
		t.Fatalf("group insert must append children, got %#v", group.Config)
	}
}

func TestApplyEntryPatchesReportsMissesAsWarnings(t *testing.T) {
	base := []Entry{{ID: "real", Name: "real-plugin", Config: "x"}}
	patches := []Patch{
		{ID: "ghost", Overrides: []FieldOverride{{Key: "config", Value: "y"}}},
		{ID: "real", Name: "wrong-name", Overrides: []FieldOverride{{Key: "config", Value: "y"}}},
		{Overrides: []FieldOverride{{Key: "config", Value: "y"}}},
		{ID: "real", Insert: []Entry{{ID: "c"}}},
		{ID: "ghost", Insert: []Entry{{ID: "c"}}},
	}
	got, warnings := ApplyEntryPatches(base, patches)
	if len(got) != 1 || got[0].Config != "x" {
		t.Fatalf("all five patches must be skipped, got %+v", got)
	}
	if len(warnings) != 5 {
		t.Fatalf("every skipped patch must warn, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "not found") ||
		!strings.Contains(warnings[1], "name mismatch") ||
		!strings.Contains(warnings[2], "id is required") ||
		!strings.Contains(warnings[3], "is not a group") {
		t.Fatalf("warning texts must mirror the official algorithm, got %v", warnings)
	}
}

func TestApplyEntryPatchesNeverMutatesInput(t *testing.T) {
	base := []Entry{{ID: "a", Name: "a", Inject: []string{"x"}, Config: map[string]any{"k": "v"}}}
	patches := []Patch{{ID: "a", Overrides: []FieldOverride{{Key: "config", Value: map[string]any{"k": "changed"}}}}}
	if _, _ = ApplyEntryPatches(base, patches); base[0].Config.(map[string]any)["k"] != "v" {
		t.Fatal("patches must work on a detached copy, never the caller's list")
	}
}

func TestGroupChildrenDecodeAndAnchorRecursively(t *testing.T) {
	data := `
- insert:
    - id: bundle
      name: bundle
      group: true
      config:
        - id: inner
          name: './inner.ts'
`
	patches, err := ParsePatchFile([]byte(data), `/srv/dsh`)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	inserted := patches[0].Insert
	if !inserted[0].Group {
		t.Fatal("group flag must decode")
	}
	children, ok := inserted[0].Config.([]Entry)
	if !ok || children[0].ID != "inner" {
		t.Fatalf("group config must decode to child entries, got %#v", inserted[0].Config)
	}
	if children[0].Name != "file:///srv/dsh/inner.ts" {
		t.Fatalf("nested insert names must anchor recursively, got %q", children[0].Name)
	}
}

func TestEvaluateFailsLoudlyWithoutAnEvaluator(t *testing.T) {
	if _, err := Evaluate(RawExpression("process.cwd()")); err == nil || !strings.Contains(err.Error(), "no JavaScript evaluator") {
		t.Fatalf("expression evaluation without an evaluator must fail loudly, got %v", err)
	}
}

func TestSetJSEvaluatorHookRoundTrip(t *testing.T) {
	restore := jsEvaluator
	defer func() { jsEvaluator = restore }()
	SetJSEvaluator(func(expr RawExpression) (any, error) {
		if expr.String() == "process.cwd()" {
			return `/work`, nil
		}
		return nil, errors.New("unsupported expression")
	})
	got, err := Evaluate(RawExpression("process.cwd()"))
	if err != nil || got != `/work` {
		t.Fatalf("registered evaluator must run, got %v %v", got, err)
	}
}

func TestIsDisabledResolvesBoolAndExpression(t *testing.T) {
	if disabled, err := IsDisabled(Entry{Disabled: true}); err != nil || !disabled {
		t.Fatalf("literal bool must pass through, got %v %v", disabled, err)
	}
	restore := jsEvaluator
	defer func() { jsEvaluator = restore }()
	SetJSEvaluator(func(expr RawExpression) (any, error) { return expr.String() == "yes", nil })
	if disabled, err := IsDisabled(Entry{Disabled: RawExpression("yes")}); err != nil || !disabled {
		t.Fatalf("expression must resolve through the evaluator, got %v %v", disabled, err)
	}
	if _, err := IsDisabled(Entry{Disabled: 42}); err == nil {
		t.Fatal("wrong disabled type must fail loudly")
	}
}
