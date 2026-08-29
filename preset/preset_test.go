package preset

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"testing"

	"dshgo/session"
	"dshgo/systemprompt"
)

// --- vocabulary ------------------------------------------------------------

func TestValidPresetID(t *testing.T) {
	valid := []string{"a", "abc123", "x-y", "0-0"}
	invalid := []string{"", "-a", "A", "a_b", "a/b", "..", "a b", "á"}
	for _, id := range valid {
		if !ValidPresetID(id) {
			t.Fatalf("id %q should be valid", id)
		}
	}
	for _, id := range invalid {
		if ValidPresetID(id) {
			t.Fatalf("id %q should be invalid", id)
		}
	}
}

func TestPresetErrorMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			"unknown with ids",
			&UnknownPresetError{PresetID: "x", Available: []string{"a", "b"}},
			`agent-presets: preset "x" not found (available: a, b)`,
		},
		{
			"unknown without ids",
			&UnknownPresetError{PresetID: "x"},
			`agent-presets: preset "x" not found (available: none)`,
		},
		{
			"locked",
			&PresetLockedError{SessionID: "s1", PresetID: "x"},
			`agent-presets: session "s1" has already started; its agent preset is fixed`,
		},
		{
			"mount",
			&PresetMountError{PresetID: "x", Reason: "it is broken"},
			`agent-presets: preset "x" failed to mount: it is broken`,
		},
		{
			"invalid id",
			&InvalidPresetIDError{PresetID: "../escape"},
			`agent-presets: preset id "../escape" must match /^[a-z0-9][a-z0-9-]*$/ — the id is a directory name, so anything else could escape the preset root`,
		},
		{
			"exists",
			&PresetExistsError{PresetID: "x"},
			`agent-presets: preset "x" already exists — a copy never overwrites; delete the existing preset first or choose another id`,
		},
		{
			"not writable",
			&PresetNotWritableError{PresetID: "x", Reason: "it ships with the deployment"},
			`agent-presets: preset "x" cannot be written: it ships with the deployment`,
		},
		{
			"no writable root",
			&PresetNotWritableError{PresetID: "", Reason: "this deployment configures no user-writable preset root"},
			`agent-presets: preset "" cannot be written: this deployment configures no user-writable preset root`,
		},
	}
	for _, testCase := range cases {
		if got := testCase.err.Error(); got != testCase.want {
			t.Fatalf("%s:\n got %q\nwant %q", testCase.name, got, testCase.want)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	if failure := ClassifyFailure(errors.New("plain"), "x"); failure != nil {
		t.Fatalf("plain error classified as %+v", failure)
	}
	notFound := ClassifyFailure(&UnknownPresetError{PresetID: "x", Available: []string{"a"}}, "x")
	if notFound.Code != CodePresetNotFound || notFound.Details["agentPreset"] != "x" {
		t.Fatalf("not-found = %+v", notFound)
	}
	mount := ClassifyFailure(&PresetMountError{PresetID: "x", Reason: "r"}, "x")
	if mount.Code != CodePresetInvalid || mount.Details["reason"] != "r" {
		t.Fatalf("invalid = %+v", mount)
	}
	readOnly := ClassifyFailure(&PresetNotWritableError{PresetID: "sys", Reason: "it ships"}, "sys")
	if readOnly.Code != CodePresetReadOnly || readOnly.Details["agentPreset"] != "sys" {
		t.Fatalf("read-only = %+v", readOnly)
	}
	locked := ClassifyFailure(&PresetLockedError{SessionID: "s1", PresetID: "x"}, "x")
	if locked.Code != CodePresetLocked || locked.Details["sessionId"] != "s1" {
		t.Fatalf("locked = %+v", locked)
	}
}

// --- specifier -------------------------------------------------------------

func TestClassifyRowSpecifier(t *testing.T) {
	cases := []struct{ name, kind string }{
		{"cordis:tools", SpecifierBuiltin},
		{"./helpers.js", SpecifierPreset},
		{"../shared/plugin.js", SpecifierPreset},
		{"file:///opt/preset/entry.js", SpecifierFile},
		{"@deepseek-ai/dsh-todo", SpecifierPackage},
		{"some-package/nested", SpecifierPackage},
	}
	for _, testCase := range cases {
		classified := ClassifyRowSpecifier(testCase.name)
		if classified.Kind != testCase.kind || classified.Specifier != testCase.name {
			t.Fatalf("%s = %+v, want kind %s", testCase.name, classified, testCase.kind)
		}
	}
	absolute := `C:\presets\entry.js`
	if runtime.GOOS == "windows" {
		if got := ClassifyRowSpecifier(absolute); got.Kind != SpecifierFile || got.Specifier != absolute {
			t.Fatalf("absolute path = %+v", got)
		}
		return
	}
	if got := ClassifyRowSpecifier("/opt/presets/entry.js"); got.Kind != SpecifierFile || got.Specifier != "/opt/presets/entry.js" {
		t.Fatalf("absolute path = %+v", got)
	}
}

// --- persona ---------------------------------------------------------------

func TestApplyPersonaShadowsDeployment(t *testing.T) {
	sp, err := systemprompt.NewSystemPrompt(systemprompt.Config{Persona: "Be terse."})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	scopeKey := systemprompt.NewScopeKey(nil)
	dispose, err := ApplyPersona(sp, scopeKey, PersonaConfig{Text: "You are a code reviewer."})
	if err != nil {
		t.Fatalf("apply persona: %v", err)
	}
	assembly, err := sp.Assemble(systemprompt.AssembleContext{Scope: scopeKey, Signal: context.Background()})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var persona []string
	for _, section := range assembly.Sections {
		if section.Name == systemprompt.PERSONA_SECTION {
			persona = append(persona, section.Text)
		}
	}
	if len(persona) != 1 || persona[0] != "You are a code reviewer." {
		t.Fatalf("persona sections = %v", persona)
	}
	dispose()
	assembly, err = sp.Assemble(systemprompt.AssembleContext{Scope: scopeKey, Signal: context.Background()})
	if err != nil {
		t.Fatalf("post-dispose assemble: %v", err)
	}
	for _, section := range assembly.Sections {
		if section.Name == systemprompt.PERSONA_SECTION && section.Text == "You are a code reviewer." {
			t.Fatalf("persona survived disposal: %+v", section)
		}
	}
}

func TestApplyPersonaRootScopeCollides(t *testing.T) {
	sp, err := systemprompt.NewSystemPrompt(systemprompt.Config{Persona: "Be terse."})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	// The registry registers the deployment persona at the root scope
	// unconditionally; a root-scope persona row collides and fails loud.
	if _, err := ApplyPersona(sp, nil, PersonaConfig{Text: "root"}); err == nil {
		t.Fatal("root-scope persona accepted")
	}
}

func TestApplyPersonaCompleteAndRuntimeContext(t *testing.T) {
	sp, err := systemprompt.NewSystemPrompt(systemprompt.Config{IncludeHarnessIdentity: new(bool)})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	if _, err := sp.Context(nil, systemprompt.PromptContext{Name: "todo", Order: 1, Text: "Do the thing."}); err != nil {
		t.Fatalf("context: %v", err)
	}
	scopeKey := systemprompt.NewScopeKey(nil)
	suppress := false
	dispose, err := ApplyPersona(sp, scopeKey, PersonaConfig{
		Text: "Only this.", Complete: true, IncludeRuntimeContext: &suppress,
	})
	if err != nil {
		t.Fatalf("apply persona: %v", err)
	}
	defer dispose()
	assembly, err := sp.Assemble(systemprompt.AssembleContext{Scope: scopeKey, Signal: context.Background()})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// Complete-section restoration: the persona row is the sole surviving
	// section, and the runtime context it suppressed stays suppressed.
	if len(assembly.Sections) != 1 ||
		assembly.Sections[0].Name != systemprompt.PERSONA_SECTION ||
		assembly.Sections[0].Text != "Only this." {
		t.Fatalf("complete assembly = %+v", assembly.Sections)
	}
	if len(assembly.Contexts) != 0 {
		t.Fatalf("runtime context not suppressed: %+v", assembly.Contexts)
	}
}

// --- session projection ----------------------------------------------------

func TestAgentPresetProjectionFold(t *testing.T) {
	definition := AgentPresetProjection
	if definition.Key != "agentPreset" || definition.StateVersion != 1 {
		t.Fatalf("definition header = %+v", definition)
	}
	if got := definition.Init(session.SessionHeader{AgentPreset: "standard"}); got != "standard" {
		t.Fatalf("init with header preset = %v", got)
	}
	if got := definition.Init(session.SessionHeader{}); got != nil {
		t.Fatalf("init without header preset = %v", got)
	}
	state := definition.Init(session.SessionHeader{})
	data, err := json.Marshal(SelectionData{AgentPreset: "ptc"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	advanced := definition.Apply(state, session.Event{Type: EventSelected, Data: data})
	if advanced != "ptc" {
		t.Fatalf("selection = %v", advanced)
	}
	// An uninteresting event returns the same reference (the change gate).
	if definition.Apply(advanced, session.Event{Type: "user/message"}) != advanced {
		t.Fatal("uninteresting event changed the state reference")
	}
	cleared := definition.Apply(advanced, session.Event{Type: EventSelected, Data: json.RawMessage(`{"agentPreset":""}`)})
	if cleared != nil {
		t.Fatalf("clear = %v", cleared)
	}
	// DecodeState accepts exactly string and null.
	if value, err := definition.DecodeState(json.RawMessage(`"ptc"`)); err != nil || value != "ptc" {
		t.Fatalf("decode string = %v %v", value, err)
	}
	if value, err := definition.DecodeState(json.RawMessage(`null`)); err != nil || value != nil {
		t.Fatalf("decode null = %v %v", value, err)
	}
	if _, err := definition.DecodeState(json.RawMessage(`7`)); err == nil {
		t.Fatal("decode number accepted")
	}
	if _, err := definition.DecodeState(json.RawMessage(`nope`)); err == nil {
		t.Fatal("decode garbage accepted")
	}
}

var _ = filepath.Join
