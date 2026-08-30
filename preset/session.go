// The session-log record of which preset a session actually runs.
//
// The creation header names the preset a session STARTED with. A session
// may still change preset while it is blank, and the effect of that change
// outlives the blank window: the first turn — and every turn after it —
// runs under the newly mounted composition. Recording the change is what
// keeps the log honest, and it is required outright by the
// model-visible ⟺ logged rule, since the preset decides the tool schemas
// and prompt sections the model sees.
//
// Reconstruction reads the agentPreset Session projection, never the header
// alone.
package preset

import (
	"encoding/json"
	"errors"
	"fmt"

	session "dshgo/session"
	"dshgo/session/projection"
)

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes("agent-preset/selected")
}

// EventSelected is the session-log event type: the session's agent preset
// was chosen after creation, while the session was still blank. Log-only:
// it records the composition later turns ran under, so a resumed or forked
// session rebuilds the same one instead of the header's creation-time
// value.
const EventSelected = "agent-preset/selected"

// SelectionData is the event payload.
type SelectionData struct {
	// AgentPreset is the preset the session committed to; empty means the
	// session explicitly cleared its preset (the official `null`).
	AgentPreset string `json:"agentPreset"`
}

// decodeSelection decodes the event payload. A corrupt persisted payload
// fails loud with seq attribution (the inbox replay convention): a malformed
// selection must not read as an explicit clear.
func decodeSelection(event session.Event) SelectionData {
	var data SelectionData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		panic(fmt.Errorf("preset: invalid persisted agent-preset/selected payload at seq %d: %w", event.Seq, err))
	}
	return data
}

// AgentPresetProjectionKey is the projection key this unit owns.
const AgentPresetProjectionKey = "agentPreset"

// AgentPresetUnit is the typed unit; AgentPresetProjection its erased
// runtime record for registry registration.
var AgentPresetUnit = projection.Unit[*string]{
	Key:          AgentPresetProjectionKey,
	StateVersion: 1,
	Init: func(header session.SessionHeader) *string {
		if header.AgentPreset == "" {
			return nil
		}
		selected := header.AgentPreset
		return &selected
	},
	Apply: func(state *string, event session.Event) (*string, bool) {
		if event.Type != EventSelected {
			return state, false
		}
		selection := decodeSelection(event)
		if selection.AgentPreset == "" {
			return nil, true
		}
		selected := selection.AgentPreset
		return &selected, true
	},
	// The client value stays the preset id string or null — the state
	// pointer never leaks into the view.
	View: func(state *string) any {
		if state == nil {
			return nil
		}
		return *state
	},
	DecodeState: func(raw json.RawMessage) (*string, error) {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		switch typed := value.(type) {
		case nil:
			return nil, nil
		case string:
			return &typed, nil
		default:
			return nil, errors.New("agent-preset projection state must be a preset id or null")
		}
	},
}

// AgentPresetProjection is the current Session preset: the preset id, or
// nil when the deployment composes none. Erased form for the registry.
var AgentPresetProjection = AgentPresetUnit.Definition()
