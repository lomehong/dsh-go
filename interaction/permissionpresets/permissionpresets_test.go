package permissionpresets

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/commands"
	"dshgo/interaction/userapproval"
	"dshgo/session"
)

func newSession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	return sess
}

func testService(t *testing.T) *Service {
	t.Helper()
	service, err := NewService(Config{SandboxDefault: SandboxWorkspaceWrite})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service
}

func TestNewServiceFailsLoud(t *testing.T) {
	if _, err := NewService(Config{SandboxDefault: ""}); err == nil ||
		!strings.Contains(err.Error(), "the mounted bash executor does not confine (no sandboxMode)") {
		t.Fatalf("err = %v, want the unconfined rejection", err)
	}
	presets, names := DefaultPresets()
	presets[CustomPreset] = PresetSpec{Sandbox: SandboxReadOnly, Approval: userapproval.PolicyAsk}
	names = append(names, CustomPreset)
	if _, err := NewService(Config{Presets: presets, Names: names, SandboxDefault: SandboxWorkspaceWrite}); err == nil ||
		!strings.Contains(err.Error(), `"custom" is reserved for the derived not-a-preset state`) {
		t.Fatalf("err = %v, want the reserved-name rejection", err)
	}
	// Composed defaults matching no preset require an explicit default.
	if _, err := NewService(Config{
		Presets:         map[string]PresetSpec{"other": {Sandbox: SandboxReadOnly, Approval: userapproval.PolicyNever}},
		Names:           []string{"other"},
		SandboxDefault:  SandboxWorkspaceWrite,
		ApprovalDefault: userapproval.PolicyAsk,
	}); err == nil || !strings.Contains(err.Error(), "composed sandbox and approval defaults match no preset") {
		t.Fatalf("err = %v, want the no-match default rejection", err)
	}
	// An explicit unknown default fails through Resolve.
	if _, err := NewService(Config{SandboxDefault: SandboxWorkspaceWrite, DefaultPreset: "ghost"}); err == nil ||
		!strings.Contains(err.Error(), `unknown preset "ghost"`) {
		t.Fatalf("err = %v, want the unknown default rejection", err)
	}
}

func TestCurrentSharedBundleTies(t *testing.T) {
	presets, _ := DefaultPresets()
	// Two presets sharing one bundle: the last explicit selection wins.
	presets["twin"] = PresetSpec{Sandbox: SandboxWorkspaceWrite, Approval: userapproval.PolicyAsk, Name: "twin"}
	service, err := NewService(Config{
		Presets:        presets,
		Names:          []string{"workspace-write", "danger-full-access", "twin"},
		SandboxDefault: SandboxWorkspaceWrite,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sess := newSession(t, "perm-ties")
	// The empty log resolves the first declaration-order match.
	if got := service.Current(sess.Events()); got != "workspace-write" {
		t.Fatalf("current = %s, want the first table match", got)
	}
	if err := service.Set(sess, "twin"); err != nil {
		t.Fatalf("set twin: %v", err)
	}
	if got := service.Current(sess.Events()); got != "twin" {
		t.Fatalf("current = %s, want the still-matching selection", got)
	}
	// After the knobs drift, the stale selection no longer wins.
	if err := service.Set(sess, "danger-full-access"); err != nil {
		t.Fatalf("set dfa: %v", err)
	}
	if got := service.Current(sess.Events()); got != "danger-full-access" {
		t.Fatalf("current = %s", got)
	}
}

func TestSetWritesOnlyChangedKnobs(t *testing.T) {
	service := testService(t)
	sess := newSession(t, "perm-set")
	if err := service.Set(sess, "workspace-write"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// First switch: the composed defaults already resolve to this preset, so
	// the current check skips the preset event; the knob facts are still
	// missing from the log and both land.
	if len(sess.Events()) != 2 {
		t.Fatalf("events = %d (%+v), want 2", len(sess.Events()), sess.Events())
	}
	// Re-selecting the effective preset appends nothing.
	if err := service.Set(sess, "workspace-write"); err != nil {
		t.Fatalf("re-set: %v", err)
	}
	if len(sess.Events()) != 2 {
		t.Fatalf("events = %d, want unchanged", len(sess.Events()))
	}
	// A switch whose bundle differs appends the preset event and both knobs
	// (approval ask→never changed too).
	if err := service.Set(sess, "danger-full-access"); err != nil {
		t.Fatalf("set dfa: %v", err)
	}
	if len(sess.Events()) != 5 {
		t.Fatalf("events = %d, want 5", len(sess.Events()))
	}
	types := []string{}
	for _, event := range sess.Events() {
		types = append(types, event.Type)
	}
	want := []string{EventSandboxMode, userapproval.EventApprovalPolicy, EventPermissionPreset, EventSandboxMode, userapproval.EventApprovalPolicy}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("types = %v, want %v", types, want)
		}
	}
	// A switch sharing the approval bundle writes only the sandbox knob.
	twin := map[string]PresetSpec{"workspace-write": {Sandbox: SandboxWorkspaceWrite, Approval: userapproval.PolicyAsk}, "twin": {Sandbox: SandboxWorkspaceWrite, Approval: userapproval.PolicyAsk}}
	sharing, err := NewService(Config{Presets: twin, Names: []string{"workspace-write", "twin"}, SandboxDefault: SandboxWorkspaceWrite})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	sess2 := newSession(t, "perm-set-sharing")
	if err := sharing.Set(sess2, "workspace-write"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := sharing.Set(sess2, "twin"); err != nil {
		t.Fatalf("set twin: %v", err)
	}
	if len(sess2.Events()) != 3 {
		t.Fatalf("sharing events = %d, want the first switch's two knobs + only the preset event", len(sess2.Events()))
	}
	if last := sess2.Events()[2]; last.Type != EventPermissionPreset {
		t.Fatalf("last event = %s, want the preset event (no knob rewrites)", last.Type)
	}
}

func TestSelectForDeclarationOrderAndCustom(t *testing.T) {
	service := testService(t)
	sess := newSession(t, "perm-select")
	selectValue := service.SelectFor(foldKnobs(sess.Events()))
	if len(selectValue.Options) != 2 || selectValue.Options[0].Value != "workspace-write" || selectValue.Options[1].Value != "danger-full-access" {
		t.Fatalf("select = %+v, want declaration order", selectValue)
	}
	if selectValue.CurrentValue != "workspace-write" {
		t.Fatalf("current = %s", selectValue.CurrentValue)
	}
	if selectValue.Options[0].Name != "workspace-write" ||
		selectValue.Options[0].Description != "Write inside the workspace and permitted temporary directories; wider retries require approval." {
		t.Fatalf("option = %+v", selectValue.Options[0])
	}
	// Drift the knobs off every entry: custom appears, exactly once, with
	// its verbatim copy.
	if err := SetSandboxMode(sess, SandboxReadOnly); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	selectValue = service.SelectFor(foldKnobs(sess.Events()))
	if selectValue.CurrentValue != CustomPreset || len(selectValue.Options) != 3 {
		t.Fatalf("select = %+v, want custom appended", selectValue)
	}
	custom := selectValue.Options[2]
	if custom.Value != CustomPreset || custom.Name != "Custom" ||
		custom.Description != "Current sandbox and approval settings do not match a preset." {
		t.Fatalf("custom option = %+v", custom)
	}
	// A matched state hides custom again.
	if err := service.Set(sess, "workspace-write"); err != nil {
		t.Fatalf("set: %v", err)
	}
	selectValue = service.SelectFor(foldKnobs(sess.Events()))
	if selectValue.CurrentValue == CustomPreset || len(selectValue.Options) != 2 {
		t.Fatalf("select = %+v, want custom hidden", selectValue)
	}
}

func TestEffectivePermissionPresetLastWins(t *testing.T) {
	sess := newSession(t, "perm-last")
	if _, ok := EffectivePermissionPreset(sess.Events()); ok {
		t.Fatal("an empty log has no selection")
	}
	if err := service_append(sess, "first"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := service_append(sess, "second"); err != nil {
		t.Fatalf("second: %v", err)
	}
	got, ok := EffectivePermissionPreset(sess.Events())
	if !ok || got != "second" {
		t.Fatalf("preset = %s %v, want last-wins", got, ok)
	}
}

func service_append(sess *session.Session, name string) error {
	_, err := sess.Append(EventPermissionPreset, PresetData{Preset: name}, nil)
	return err
}

func TestSetDefaultSourceDrivesFreshSessions(t *testing.T) {
	service := testService(t)
	// Without a source, the composition default pins fresh sessions.
	composition := newSession(t, "perm-source-none")
	if err := service.PinInitialPermission(composition); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if selected, _ := EffectivePermissionPreset(composition.Events()); selected != "workspace-write" {
		t.Fatalf("composition default = %s", selected)
	}

	// The live settings source wins for genuinely fresh sessions.
	service.SetDefaultSource(func() string { return "danger-full-access" })
	overridden := newSession(t, "perm-source-live")
	if err := service.PinInitialPermission(overridden); err != nil {
		t.Fatalf("pin overridden: %v", err)
	}
	if selected, _ := EffectivePermissionPreset(overridden.Events()); selected != "danger-full-access" {
		t.Fatalf("live default = %s", selected)
	}

	// A blank source result falls back to the composition default.
	service.SetDefaultSource(func() string { return "  " })
	fallback := newSession(t, "perm-source-blank")
	if err := service.PinInitialPermission(fallback); err != nil {
		t.Fatalf("pin fallback: %v", err)
	}
	if selected, _ := EffectivePermissionPreset(fallback.Events()); selected != "workspace-write" {
		t.Fatalf("fallback default = %s", selected)
	}

	// An unknown settings value fails loud at pin time, not silently.
	service.SetDefaultSource(func() string { return "bogus" })
	if err := service.PinInitialPermission(newSession(t, "perm-source-bogus")); err == nil {
		t.Fatal("unknown settings preset must fail the pin")
	}

	// Seeded sessions with their own knobs ignore the source entirely.
	service.SetDefaultSource(func() string { return "danger-full-access" })
	seeded := newSession(t, "perm-source-seeded")
	if err := service_append(seeded, "workspace-write"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := service.PinInitialPermission(seeded); err != nil {
		t.Fatalf("pin seeded: %v", err)
	}
	if selected, _ := EffectivePermissionPreset(seeded.Events()); selected != "workspace-write" {
		t.Fatalf("seeded must keep its own preset = %s", selected)
	}
	service.SetDefaultSource(nil)
}

func TestPinInitialPermission(t *testing.T) {
	service := testService(t)
	// A genuinely fresh session pins the user default and both knobs.
	fresh := newSession(t, "perm-fresh")
	if err := service.PinInitialPermission(fresh); err != nil {
		t.Fatalf("pin fresh: %v", err)
	}
	if len(fresh.Events()) != 3 {
		t.Fatalf("fresh events = %d, want preset + sandbox + approval", len(fresh.Events()))
	}
	if selected, _ := EffectivePermissionPreset(fresh.Events()); selected != "workspace-write" {
		t.Fatalf("selected = %s", selected)
	}

	// A seeded session preserves its effective knobs and only gains the
	// missing facts.
	seeded := newSession(t, "perm-seeded")
	if _, err := seeded.Append("session/end-seed", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := seeded.Append(EventSandboxMode, SandboxModeData{Mode: SandboxReadOnly}, nil); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	if err := service.PinInitialPermission(seeded); err != nil {
		t.Fatalf("pin seeded: %v", err)
	}
	// The drifted knobs (read-only + defaulting ask) match no entry, so the
	// derived state is custom — and custom is never an event payload: the
	// seeded session only gains its missing approval fact.
	if _, ok := EffectivePermissionPreset(seeded.Events()); ok {
		t.Fatal("a custom derivation must not be pinned as a preset event")
	}
	types := map[string]int{}
	for _, event := range seeded.Events() {
		types[event.Type]++
	}
	if types[EventSandboxMode] != 1 || types[userapproval.EventApprovalPolicy] != 1 {
		t.Fatalf("types = %+v, want the missing approval knob only", types)
	}

	// A seeded session whose knobs DO match an entry gains the derived name.
	matched := newSession(t, "perm-seeded-matched")
	if _, err := matched.Append("session/end-seed", json.RawMessage(`{}`), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := matched.Append(EventSandboxMode, SandboxModeData{Mode: SandboxDangerFullAccess}, nil); err != nil {
		t.Fatalf("sandbox: %v", err)
	}
	if _, err := matched.Append(userapproval.EventApprovalPolicy, mustJSON(t, userapproval.PolicyData{Policy: userapproval.PolicyNever}), nil); err != nil {
		t.Fatalf("approval: %v", err)
	}
	if err := service.PinInitialPermission(matched); err != nil {
		t.Fatalf("pin matched: %v", err)
	}
	if selected, ok := EffectivePermissionPreset(matched.Events()); !ok || selected != "danger-full-access" {
		t.Fatalf("selected = %s %v, want the derived match pinned", selected, ok)
	}
	if len(matched.Events()) != 4 {
		t.Fatalf("matched events = %d, want only the preset fact added", len(matched.Events()))
	}
}

func TestProjectionFoldAndWire(t *testing.T) {
	service := testService(t)
	definition := service.ProjectionDefinition()
	if definition.Key != "permissions" || definition.StateVersion != 1 {
		t.Fatalf("definition = %+v", definition)
	}
	state := definition.Init(session.SessionHeader{})
	state = definition.Apply(state, session.Event{Type: EventSandboxMode, Data: mustJSON(t, SandboxModeData{Mode: SandboxReadOnly})})
	view := definition.Wire.View(state).(PermissionSelect)
	if view.CurrentValue != CustomPreset || len(view.Options) != 3 {
		t.Fatalf("view = %+v", view)
	}
	state = definition.Apply(state, session.Event{Type: userapproval.EventApprovalPolicy, Data: mustJSON(t, userapproval.PolicyData{Policy: userapproval.PolicyNever})})
	view = definition.Wire.View(state).(PermissionSelect)
	if view.CurrentValue != CustomPreset {
		t.Fatalf("view = %+v, want custom for read-only+never", view)
	}
	state = definition.Apply(state, session.Event{Type: EventPermissionPreset, Data: mustJSON(t, PresetData{Preset: "danger-full-access"})})
	// The stale selection cannot win: the folded knobs (read-only + never)
	// do not match danger-full-access's bundle.
	view = definition.Wire.View(state).(PermissionSelect)
	if view.CurrentValue != CustomPreset || len(view.Options) != 3 {
		t.Fatalf("view = %+v, want custom while knobs mismatch the selection", view)
	}
	state = definition.Apply(state, session.Event{Type: EventSandboxMode, Data: mustJSON(t, SandboxModeData{Mode: SandboxDangerFullAccess})})
	view = definition.Wire.View(state).(PermissionSelect)
	if view.CurrentValue != "danger-full-access" || len(view.Options) != 2 {
		t.Fatalf("view = %+v", view)
	}
	// Uninterested events return the same reference.
	if definition.Apply(state, session.Event{Type: "turn/start", Data: json.RawMessage(`{}`)}) != state {
		t.Fatal("non-knob events must return the same state reference")
	}
	// Restore path: strict decode rejects unknown fields.
	if _, err := definition.DecodeState(json.RawMessage(`{"preset":null,"sandbox":null,"approval":null}`)); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := definition.DecodeState(json.RawMessage(`{"preset":null,"sandbox":null,"approval":null,"extra":1}`)); err == nil {
		t.Fatal("unknown fields must reject")
	}
}

func TestPermissionCommand(t *testing.T) {
	service := testService(t)
	definition := service.CommandDefinition()
	if definition.Name != "permission" || definition.Input.Hint != "<preset>" {
		t.Fatalf("definition = %+v", definition)
	}
	runtime := commands.NewCommandRuntime(nil)
	if _, err := runtime.Register(nil, definition); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "perm-cmd")

	// Bare line: report the current value and the available names.
	execution, err := runtime.Execute(context.Background(), nil, sess, "/permission", nil)
	if err != nil || execution == nil {
		t.Fatalf("bare = %v %v", execution, err)
	}
	if execution.Result.Text != "current preset workspace-write (available: workspace-write, danger-full-access)" {
		t.Fatalf("bare result = %+v", execution.Result)
	}

	// Unknown name: error result with the verbatim shape.
	execution, err = runtime.Execute(context.Background(), nil, sess, "/permission ghost", nil)
	if err != nil || execution == nil {
		t.Fatalf("ghost = %v %v", execution, err)
	}
	if execution.Result.Kind != commands.ResultError ||
		execution.Result.Text != `unknown preset "ghost" (available: workspace-write, danger-full-access)` {
		t.Fatalf("ghost result = %+v", execution.Result)
	}

	// A valid switch applies and reports the preset.
	execution, err = runtime.Execute(context.Background(), nil, sess, "/permission danger-full-access", nil)
	if err != nil || execution == nil {
		t.Fatalf("switch = %v %v", execution, err)
	}
	if execution.Result.Text != "preset danger-full-access" {
		t.Fatalf("switch result = %+v", execution.Result)
	}
	if got := service.Current(sess.Events()); got != "danger-full-access" {
		t.Fatalf("current = %s", got)
	}
	// The knob events landed with the lifecycle records.
	if got, _ := EffectiveSandboxMode(sess.Events()); got != SandboxDangerFullAccess {
		t.Fatalf("sandbox = %s", got)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}
