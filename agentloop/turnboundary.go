package agentloop

import (
	"encoding/json"
	"fmt"

	"dshgo/session"
	"dshgo/session/projection"
)

// TurnBoundaryProjection is the agent session's open/last turn and step
// boundary facts (the wire view equals the state: the whole value is the
// projection, exactly as upstream's SessionProjectionStateMap entry).
type TurnBoundaryProjection struct {
	// OpenTurnStartSeq is the seq of the open turn's turn/start, or nil
	// between turns.
	OpenTurnStartSeq *int64 `json:"openTurnStartSeq"`
	// LastStepStartSeq is the seq of the latest step/start event, or nil
	// before the first step.
	LastStepStartSeq *int64 `json:"lastStepStartSeq"`
	// LastStepBoundary is the latest step boundary (step/start or
	// step/end) and its seq, or nil before the first step boundary.
	LastStepBoundary *StepBoundary `json:"lastStepBoundary"`
	// LastTurn is the turn number of the latest turn/start; 0 before the
	// first turn.
	LastTurn int64 `json:"lastTurn"`
}

// StepBoundary names one step boundary event and its seq.
type StepBoundary struct {
	Kind string `json:"kind"`
	Seq  int64  `json:"seq"`
}

// TurnBoundaryProjectionKey is the projection registry key the loop owns.
const TurnBoundaryProjectionKey = "turnBoundary"

// TurnBoundaryProjectionDefinition builds the loop-owned turn/step boundary
// unit. Consumers (hooks bridges, agent factory) read turn numbers from this
// unit instead of scanning the event log — the loop is the authoritative
// driver of those boundaries.
func TurnBoundaryProjectionDefinition() projection.Definition {
	return projection.Unit[TurnBoundaryProjection]{
		Key:          TurnBoundaryProjectionKey,
		StateVersion: 2,
		Init: func(session.SessionHeader) TurnBoundaryProjection {
			return TurnBoundaryProjection{}
		},
		Apply: func(current TurnBoundaryProjection, event session.Event) (TurnBoundaryProjection, bool) {
			switch event.Type {
			case session.EventTurnStart:
				var data session.TurnStartData
				if err := json.Unmarshal(event.Data, &data); err != nil {
					return current, false
				}
				seq := event.Seq
				current.OpenTurnStartSeq = &seq
				current.LastTurn = data.Turn
				return current, true
			case session.EventTurnEnd:
				current.OpenTurnStartSeq = nil
				return current, true
			case session.EventStepStart:
				seq := event.Seq
				current.LastStepStartSeq = &seq
				current.LastStepBoundary = &StepBoundary{Kind: "start", Seq: event.Seq}
				return current, true
			case session.EventStepEnd:
				current.LastStepBoundary = &StepBoundary{Kind: "end", Seq: event.Seq}
				return current, true
			default:
				return current, false
			}
		},
		DecodeState: func(raw json.RawMessage) (TurnBoundaryProjection, error) {
			var decoded TurnBoundaryProjection
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return TurnBoundaryProjection{}, fmt.Errorf("turn boundary unit state: %w", err)
			}
			return decoded, nil
		},
	}.Definition()
}
