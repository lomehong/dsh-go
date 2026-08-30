package goal

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/session"
	"dshgo/session/projection"
)

// projectionStateVersion guards persisted goal-projection rows (official
// stateVersion 4).
const projectionStateVersion = 4

// ProjectionKey is the session-projection key this domain owns.
const ProjectionKey = "goal"

// ApplyGoalProjection is the light last-wins fold of the `goal` projection
// unit. Unlike the strict replay fold (fold.go: transition validation,
// fail-loud on malformed changes), this transition is projection-grade:
// the state is plain JSON (persisted-cache precondition), any non-goal or
// malformed event returns the same reference (the registry's identity gate
// — the title/todos posture), and correctness of the written change is the
// write side's job (the service validated it before appending; the package
// invariant rejects a violating stream fail-loud where it is installed).
func ApplyGoalProjection(state *GoalProjection, event session.Event) (*GoalProjection, bool) {
	if event.Type != EventChange {
		return state, false
	}
	change, err := DecodeGoalChange(event.Data)
	if err != nil || change == nil {
		return state, false
	}
	if change.Operation == OperationClear {
		// Object.is(nil, nil) is true: a clear over an already-null
		// projection publishes nothing new.
		return nil, state != nil
	}
	return &GoalProjection{
		Goal:          *change.Goal,
		RoundsStarted: *change.RoundsStarted,
		CreatedAt:     *change.CreatedAt,
		UpdatedAt:     *change.UpdatedAt,
	}, true
}

// GoalUnit is the `goal` projection unit definition: last-wins fold of
// goal/change whole values. The unit child activates only when a
// projection registry is composed (headless assemblies stay unaffected).
func GoalUnit() projection.Unit[*GoalProjection] {
	return projection.Unit[*GoalProjection]{
		Key:          ProjectionKey,
		StateVersion: projectionStateVersion,
		Init:         func(session.SessionHeader) *GoalProjection { return nil },
		Apply:        ApplyGoalProjection,
		View: func(state *GoalProjection) any {
			if state == nil {
				// Untyped null: a typed nil pointer would survive an any
				// boxing and read as present.
				return nil
			}
			return state
		},
		DecodeState: decodeGoalProjection,
	}
}

// decodeGoalProjection validates and reifies one persisted row value (the
// official zod stateSchema/viewSchema role): whole current goal or
// pre-create/cleared null.
func decodeGoalProjection(raw json.RawMessage) (*GoalProjection, error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil //nolint:nilnil // null IS the absent-value row
	}
	var record map[string]any
	if err := json.Unmarshal(trimmed, &record); err != nil {
		return nil, fmt.Errorf("goal projection row is not a record: %w", err)
	}
	goalRaw, ok := record["goal"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("goal projection row lacks a goal record")
	}
	id, ok := goalRaw["id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("goal projection goal.id must be a non-empty string")
	}
	revision, err := positiveInteger(goalRaw["revision"], "goal.revision")
	if err != nil {
		return nil, fmt.Errorf("goal projection goal.revision must be a positive integer")
	}
	objective, ok := goalRaw["objective"].(string)
	if !ok || objective == "" {
		return nil, fmt.Errorf("goal projection goal.objective must be a non-empty string")
	}
	rawPhase, ok := goalRaw["phase"].(string)
	if !ok || !phases[GoalPhase(rawPhase)] {
		return nil, fmt.Errorf("goal projection goal.phase is invalid")
	}
	maxGoalRounds, err := positiveInteger(goalRaw["maxGoalRounds"], "goal.maxGoalRounds")
	if err != nil {
		return nil, fmt.Errorf("goal projection goal.maxGoalRounds must be a positive integer")
	}
	snapshot := GoalSnapshot{
		ID: GoalID(id), Revision: revision, Objective: objective,
		Phase: GoalPhase(rawPhase), MaxGoalRounds: maxGoalRounds,
	}
	if rawReason, present := goalRaw["blockedReason"]; present && rawReason != nil {
		reason, ok := rawReason.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("goal projection goal.blockedReason must be a record")
		}
		code, codeOK := reason["code"].(string)
		message, messageOK := reason["message"].(string)
		if !codeOK || !messageOK {
			return nil, fmt.Errorf("goal projection goal.blockedReason must carry code and message strings")
		}
		snapshot.BlockedReason = &GoalBlockReason{Code: code, Message: message}
	}
	roundsStarted, ok := asInteger(record["roundsStarted"])
	if !ok || roundsStarted < 0 {
		return nil, fmt.Errorf("goal projection roundsStarted must be a non-negative integer")
	}
	createdAt, ok := record["createdAt"].(float64)
	if !ok {
		return nil, fmt.Errorf("goal projection createdAt must be a number")
	}
	updatedAt, ok := record["updatedAt"].(float64)
	if !ok {
		return nil, fmt.Errorf("goal projection updatedAt must be a number")
	}
	return &GoalProjection{
		Goal:          snapshot,
		RoundsStarted: roundsStarted,
		CreatedAt:     int64(createdAt),
		UpdatedAt:     int64(updatedAt),
	}, nil
}
