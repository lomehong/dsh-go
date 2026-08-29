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

	session "dshgo/session"
	"dshgo/session/projection"
)

func init() {
	if err := session.RegisterEventType("agent-preset/selected"); err != nil {
		panic(err)
	}
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

// decodeSelection decodes the event payload.
func decodeSelection(event session.Event) SelectionData {
	var data SelectionData
	_ = json.Unmarshal(event.Data, &data)
	return data
}

// AgentPresetProjectionKey is the projection key this unit owns.
const AgentPresetProjectionKey = "agentPreset"

// AgentPresetProjection is the current Session preset, initialized from its
// header and advanced by selection events. The state is the preset id
// string, or nil when the deployment composes none.
var AgentPresetProjection = projection.Definition{
	Key:          AgentPresetProjectionKey,
	StateVersion: 1,
	Init: func(header session.SessionHeader) any {
		if header.AgentPreset == "" {
			return nil
		}
		return header.AgentPreset
	},
	Apply: func(state any, event session.Event) any {
		if event.Type != EventSelected {
			// Every uninteresting event returns the same reference (the
			// change feed gates on reference identity).
			return state
		}
		selection := decodeSelection(event)
		if selection.AgentPreset == "" {
			return nil
		}
		return selection.AgentPreset
	},
	Wire: &projection.WireView{
		View: func(state any) any { return state },
	},
	DecodeState: func(raw json.RawMessage) (any, error) {
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		switch typed := value.(type) {
		case nil:
			return nil, nil
		case string:
			return typed, nil
		default:
			return nil, errors.New("agent-preset projection state must be a preset id or null")
		}
	},
}
