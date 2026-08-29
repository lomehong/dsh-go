package permissionpresets

import (
	"fmt"
	"strings"

	"dshgo/commands"
	"dshgo/interaction/userapproval"
	"dshgo/session"
)

// Config is the service config: preset table and composition default.
type Config struct {
	// Presets is the preset table: name → knob bundle, with Names carrying
	// the declaration order (Go adaptation of JS object key order). The name
	// `custom` is reserved for the derived not-a-preset state.
	Presets map[string]PresetSpec
	// Names is the table's declaration order.
	Names []string
	// SandboxDefault is the composed sandbox default (`ctx.shell.sandboxMode`
	// in the source). Empty means the mounted executor does not confine —
	// presets bundle a sandbox mode, so composing this service over an
	// unconfined executor is a misconfiguration.
	SandboxDefault string
	// ApprovalDefault is the composed approval default
	// (`ctx.approval.config.policy`); empty resolves to ask.
	ApprovalDefault userapproval.ApprovalPolicy
	// DefaultPreset is the default for new sessions. When empty, the preset
	// matching the composed sandbox and approval defaults is used.
	DefaultPreset string
}

// Service owns the deployment's permission presets and their write path.
type Service struct {
	presets         map[string]PresetSpec
	names           []string
	sandboxDefault  string
	approvalDefault userapproval.ApprovalPolicy
	defaultPreset   string
}

// NewService validates the composition and builds the service.
func NewService(config Config) (*Service, error) {
	if config.Presets == nil || len(config.Names) == 0 {
		config.Presets, config.Names = DefaultPresets()
	}
	if _, reserved := config.Presets[CustomPreset]; reserved {
		return nil, fmt.Errorf("permission: %q is reserved for the derived not-a-preset state and cannot name a table entry", CustomPreset)
	}
	if config.SandboxDefault == "" {
		return nil, fmt.Errorf("permission: the mounted bash executor does not confine (no sandboxMode) — presets bundle a sandbox mode, so composing this plugin over an unconfined executor is a misconfiguration")
	}
	approvalDefault := config.ApprovalDefault
	if approvalDefault == "" {
		approvalDefault = userapproval.PolicyAsk
	}
	service := &Service{
		presets:         config.Presets,
		names:           config.Names,
		sandboxDefault:  config.SandboxDefault,
		approvalDefault: approvalDefault,
	}
	defaultPreset := config.DefaultPreset
	if defaultPreset == "" {
		defaultPreset = service.derive(EmptyKnobs())
	}
	if defaultPreset == CustomPreset {
		return nil, fmt.Errorf("permission: composed sandbox and approval defaults match no preset; configure defaultPreset explicitly")
	}
	if _, err := service.Resolve(defaultPreset); err != nil {
		return nil, err
	}
	service.defaultPreset = defaultPreset
	return service, nil
}

// Names returns the advertised preset names, in the preset table's
// declaration order.
func (s *Service) Names() []string { return s.names }

// OverrideOf reports the session's explicit sandbox-mode override (the
// `sandbox/mode` fold), or empty without one. It never falls back to the
// deployment default: delegation capture wants the parent's own choice
// only. Satisfies the subagent.SandboxOverrideService seam structurally.
func (s *Service) OverrideOf(sess *session.Session) string {
	mode, ok := EffectiveSandboxMode(sess.Events())
	if !ok {
		return ""
	}
	return mode
}

// DefaultPreset returns the preset currently selected as the default for
// future sessions.
func (s *Service) DefaultPreset() string { return s.defaultPreset }

// Current resolves the preset matching the effective knob values. A
// still-matching last selection wins shared-bundle ties; otherwise the
// first table match wins, or CustomPreset when no entry matches.
func (s *Service) Current(events []session.Event) string {
	return s.derive(foldKnobs(events))
}

// derive resolves the preset for one folded knob state (the shared
// mathematics of Current and the projection unit).
func (s *Service) derive(state KnobState) string {
	sandbox := s.sandboxDefault
	if state.Sandbox != nil {
		sandbox = *state.Sandbox
	}
	approval := s.approvalDefault
	if state.Approval != nil {
		approval = userapproval.ApprovalPolicy(*state.Approval)
	}
	matches := func(spec PresetSpec) bool { return spec.Sandbox == sandbox && spec.Approval == approval }
	if state.Preset != nil {
		if spec, known := s.presets[*state.Preset]; known && matches(spec) {
			return *state.Preset
		}
	}
	for _, name := range s.names {
		if matches(s.presets[name]) {
			return name
		}
	}
	return CustomPreset
}

// SelectFor builds the whole select value for one folded knob state: every
// table option in declaration order, `custom` appended exactly while
// derived.
func (s *Service) SelectFor(state KnobState) PermissionSelect {
	currentValue := s.derive(state)
	options := make([]PresetOption, 0, len(s.names)+1)
	for _, name := range s.names {
		options = append(options, s.OptionOf(name))
	}
	if currentValue == CustomPreset {
		options = append(options, s.OptionOf(CustomPreset))
	}
	return PermissionSelect{Options: options, CurrentValue: currentValue}
}

// Resolve returns a preset's knob bundle; unknown names fail loud.
func (s *Service) Resolve(name string) (PresetSpec, error) {
	spec, known := s.presets[name]
	if !known {
		return PresetSpec{}, fmt.Errorf("permission: unknown preset %q (known: %s)", name, strings.Join(s.names, ", "))
	}
	return spec, nil
}

// OptionOf builds the client option for a table entry or CustomPreset. A
// missing label falls back to the table key.
func (s *Service) OptionOf(name string) PresetOption {
	if name == CustomPreset {
		return PresetOption{Value: CustomPreset, Name: "Custom", Description: "Current sandbox and approval settings do not match a preset."}
	}
	spec, err := s.Resolve(name)
	if err != nil {
		panic(err.Error())
	}
	label := spec.Name
	if label == "" {
		label = name
	}
	return PresetOption{Value: name, Name: label, Description: spec.Description}
}

// Set records a changed preset, then updates each changed knob through its
// own setter. Selecting the effective preset again appends nothing.
func (s *Service) Set(sess *session.Session, name string) error {
	spec, err := s.Resolve(name)
	if err != nil {
		return err
	}
	if s.Current(sess.Events()) != name {
		if _, err := sess.Append(EventPermissionPreset, PresetData{Preset: name}, nil); err != nil {
			return err
		}
	}
	events := sess.Events()
	if sandbox, ok := EffectiveSandboxMode(events); !ok || sandbox != spec.Sandbox {
		if err := SetSandboxMode(sess, spec.Sandbox); err != nil {
			return err
		}
	}
	if policy, ok := userapproval.EffectiveApprovalPolicy(events); !ok || policy != spec.Approval {
		if err := userapproval.SetApprovalPolicy(sess, spec.Approval); err != nil {
			return err
		}
	}
	return nil
}

// PinInitialPermission fills every missing permission fact before a session
// is published. A genuinely fresh session uses the current user default;
// seeded or partially initialized sessions preserve their effective knob
// values and only gain the missing durable facts.
func (s *Service) PinInitialPermission(sess *session.Session) error {
	events := sess.Events()
	selected, hasSelected := EffectivePermissionPreset(events)
	sandbox, hasSandbox := EffectiveSandboxMode(events)
	approval, hasApproval := userapproval.EffectiveApprovalPolicy(events)
	seeded := false
	for _, event := range events {
		if event.Type == "session/end-seed" {
			seeded = true
			break
		}
	}
	if !hasSelected && !hasSandbox && !hasApproval && !seeded {
		name := s.defaultPreset
		spec, err := s.Resolve(name)
		if err != nil {
			return err
		}
		if _, err := sess.Append(EventPermissionPreset, PresetData{Preset: name}, nil); err != nil {
			return err
		}
		if err := SetSandboxMode(sess, spec.Sandbox); err != nil {
			return err
		}
		return userapproval.SetApprovalPolicy(sess, spec.Approval)
	}
	state := KnobState{}
	if hasSelected {
		state.Preset = &selected
	}
	if hasSandbox {
		state.Sandbox = &sandbox
	}
	if hasApproval {
		policy := string(approval)
		state.Approval = &policy
	}
	effective := s.derive(state)
	if !hasSelected && effective != CustomPreset {
		if _, err := sess.Append(EventPermissionPreset, PresetData{Preset: effective}, nil); err != nil {
			return err
		}
	}
	if !hasSandbox {
		if err := SetSandboxMode(sess, s.sandboxDefault); err != nil {
			return err
		}
	}
	if !hasApproval {
		return userapproval.SetApprovalPolicy(sess, s.approvalDefault)
	}
	return nil
}

// CommandDefinition is the `/permission` command: the one write path a web
// client uses (the popup contribution submits the picked preset as this
// line). The composition registers it only when a command registry is
// composed.
func (s *Service) CommandDefinition() commands.CommandDefinition {
	return commands.CommandDefinition{
		Name:        "permission",
		Description: "Switch the permission preset (sandbox mode + approval policy)",
		Input:       &commands.CommandInputDescriptor{Hint: "<preset>"},
		// No settlement text labels its value with this command's own name:
		// a surface that renders `name · text` (the web command row) would
		// otherwise read `permission · Permission preset: workspace-write.`
		Handler: func(invocation commands.Invocation) (commands.CommandResult, error) {
			name := strings.TrimSpace(invocation.RawInput)
			if name == "" {
				return commands.CommandResult{Kind: commands.ResultSuccess, HasText: true,
					Text: fmt.Sprintf("current preset %s (available: %s)", s.Current(invocation.Session.Events()), strings.Join(s.names, ", "))}, nil
			}
			known := false
			for _, candidate := range s.names {
				if candidate == name {
					known = true
					break
				}
			}
			if !known {
				return commands.CommandResult{Kind: commands.ResultError, HasText: true,
					Text: fmt.Sprintf("unknown preset %q (available: %s)", name, strings.Join(s.names, ", "))}, nil
			}
			if err := s.Set(invocation.Session, name); err != nil {
				return commands.CommandResult{}, err
			}
			return commands.CommandResult{Kind: commands.ResultSuccess, HasText: true, Text: fmt.Sprintf("preset %s", name)}, nil
		},
	}
}
