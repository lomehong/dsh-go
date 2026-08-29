// Package permissionpresets ports packages/interaction/permission-presets:
// user-facing permission presets over the independent sandbox-mode and
// approval-policy knobs. A switch records the selected preset, then writes
// changed knobs through their canonical setters. Execution, prompt
// narration, and replay keep reading their knob folds. The preset event
// preserves user intent when two presets share a bundle. The read side
// ships as the `permissions` session projection; the write side ships as
// the `/permission` command — both optional children over the same service.
package permissionpresets

import (
	"encoding/json"

	"dshgo/interaction/userapproval"
	"dshgo/session"
)

// EventPermissionPreset records the selected preset as durable, log-only
// user intent. The knob events follow in the same turn and control
// execution; this event stays out of the model transcript and lets
// EffectivePermissionPreset preserve a selection when bundles match.
const EventPermissionPreset = "permission/preset"

// EventSandboxMode is the `sandbox/mode` whole-value knob event. Go
// adaptation: these helpers are the dsh-sandbox-policy read/write face,
// hosted here until the shell/sandbox capability round — the projection and
// service need the exact event vocabulary, and no other Go consumer exists
// yet.
const EventSandboxMode = "sandbox/mode"

// CustomPreset is returned when effective knob values match no table entry.
// Clients may show it as the current value, but it is never a switch target
// or event payload.
const CustomPreset = "custom"

// PresetData is the `permission/preset` payload.
type PresetData struct {
	Preset string `json:"preset"`
}

// SandboxModeData is the `sandbox/mode` payload.
type SandboxModeData struct {
	Mode string `json:"mode"`
}

// Sandbox modes, mirroring the source's SANDBOX_MODES vocabulary.
const (
	SandboxReadOnly         = "read-only"
	SandboxWorkspaceWrite   = "workspace-write"
	SandboxDangerFullAccess = "danger-full-access"
)

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventPermissionPreset, EventSandboxMode)
}

// EffectivePermissionPreset folds the last selected preset from the durable
// log; replay needs no catch-up state. Other event types are ignored.
func EffectivePermissionPreset(events []session.Event) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != EventPermissionPreset {
			continue
		}
		var data PresetData
		if err := json.Unmarshal(events[index].Data, &data); err == nil {
			return data.Preset, true
		}
	}
	return "", false
}

// SetSandboxMode appends the `sandbox/mode` knob event.
func SetSandboxMode(sess *session.Session, mode string) error {
	_, err := sess.Append(EventSandboxMode, SandboxModeData{Mode: mode}, nil)
	return err
}

// EffectiveSandboxMode folds the last `sandbox/mode` from the log.
func EffectiveSandboxMode(events []session.Event) (string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != EventSandboxMode {
			continue
		}
		var data SandboxModeData
		if err := json.Unmarshal(events[index].Data, &data); err == nil {
			return data.Mode, true
		}
	}
	return "", false
}

// PresetOption is the select-option shape a presentation layer advertises
// for one preset (or for the derived `custom` state).
type PresetOption struct {
	// Value is the stable option value: the table key, or `custom`.
	Value string `json:"value"`
	// Name is the display label.
	Name string `json:"name"`
	// Description is one user-facing sentence on what the value means;
	// absent when not configured.
	Description string `json:"description,omitempty"`
}

// PermissionSelect is the whole `permissions` projection value: every
// switchable preset in table order (plus the derived current-only `custom`
// when the knobs match no entry) and the effective current value.
type PermissionSelect struct {
	// Options are the switchable presets, plus `custom` appended exactly
	// while it is current.
	Options []PresetOption `json:"options"`
	// CurrentValue is the effective current value: a preset table key, or
	// `custom`.
	CurrentValue string `json:"currentValue"`
}

// PresetSpec is one preset's sandbox/approval bundle and optional client
// presentation.
type PresetSpec struct {
	// Sandbox is the `sandbox/mode` value the preset writes through.
	Sandbox string
	// Approval is the `approval/policy` value the preset writes through.
	Approval userapproval.ApprovalPolicy
	// Name is the display label a client shows; the raw table key when
	// empty.
	Name string
	// Description is one user-facing sentence on what the preset means;
	// absent when empty.
	Description string
}

// DefaultPresets is the schema-defaulted preset table.
func DefaultPresets() (map[string]PresetSpec, []string) {
	table := map[string]PresetSpec{
		"workspace-write": {
			Sandbox:     SandboxWorkspaceWrite,
			Approval:    userapproval.PolicyAsk,
			Name:        "workspace-write",
			Description: "Write inside the workspace and permitted temporary directories; wider retries require approval.",
		},
		"danger-full-access": {
			Sandbox:     SandboxDangerFullAccess,
			Approval:    userapproval.PolicyNever,
			Name:        "danger-full-access",
			Description: "Full file access without approval prompts.",
		},
	}
	return table, []string{"workspace-write", "danger-full-access"}
}
