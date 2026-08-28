package permissionpresets

import (
	"bytes"
	"encoding/json"

	"dshgo/interaction/userapproval"
	"dshgo/session"
	"dshgo/session/projection"
)

// KnobState is the projection unit's state: the last seen value of each
// knob event, nil before an override (composition defaults apply at view
// time). Plain JSON (persisted-cache precondition).
type KnobState struct {
	// Preset is the last `permission/preset` payload, or nil.
	Preset *string `json:"preset"`
	// Sandbox is the last `sandbox/mode` payload, or nil.
	Sandbox *string `json:"sandbox"`
	// Approval is the last `approval/policy` payload, or nil.
	Approval *string `json:"approval"`
}

// EmptyKnobs is the state for the empty log: every knob at its composition
// default.
func EmptyKnobs() KnobState {
	return KnobState{}
}

// ApplyKnobEvent is the one-event knob transition (the projection unit's
// apply). Uninterested events return the same value — the registry's change
// gate.
func ApplyKnobEvent(state KnobState, event session.Event) KnobState {
	switch event.Type {
	case EventPermissionPreset:
		var data PresetData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return state
		}
		return KnobState{Preset: &data.Preset, Sandbox: state.Sandbox, Approval: state.Approval}
	case EventSandboxMode:
		var data SandboxModeData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return state
		}
		return KnobState{Preset: state.Preset, Sandbox: &data.Mode, Approval: state.Approval}
	case userapproval.EventApprovalPolicy:
		var data userapproval.PolicyData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return state
		}
		policy := string(data.Policy)
		return KnobState{Preset: state.Preset, Sandbox: state.Sandbox, Approval: &policy}
	default:
		return state
	}
}

// foldKnobs is the whole-log knob fold (the cold-read parallel of
// ApplyKnobEvent).
func foldKnobs(events []session.Event) KnobState {
	state := EmptyKnobs()
	for _, event := range events {
		state = ApplyKnobEvent(state, event)
	}
	return state
}

// ProjectionDefinition builds the `permissions` projection unit: fold the
// three whole-value knob events; the view derives the select over the
// composition defaults the service owns. Key absence means no permission
// service is composed — clients hide the control.
func (s *Service) ProjectionDefinition() projection.Definition {
	return projection.Definition{
		Key:          "permissions",
		StateVersion: 1,
		Init: func(session.SessionHeader) any {
			return EmptyKnobs()
		},
		Apply: func(state any, event session.Event) any {
			current, ok := state.(KnobState)
			if !ok {
				return state
			}
			return ApplyKnobEvent(current, event)
		},
		Wire: &projection.WireView{
			View: func(state any) any {
				current, ok := state.(KnobState)
				if !ok {
					return s.SelectFor(EmptyKnobs())
				}
				return s.SelectFor(current)
			},
		},
		DecodeState: func(raw json.RawMessage) (any, error) {
			// The persisted state schema is strict: unknown fields reject
			// (the zod `.strict()` parallel).
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			var decoded KnobState
			if err := decoder.Decode(&decoded); err != nil {
				return nil, err
			}
			return decoded, nil
		},
	}
}
