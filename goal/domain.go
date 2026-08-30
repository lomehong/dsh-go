package goal

import (
	"time"

	"dshgo/agent"
)

// ChangeVersion is the version stamped on every durable goal change.
const ChangeVersion = 1

// EventChange is the session event type carrying every goal mutation.
const EventChange = "goal/change"

// EventChanged is the agent-scoped live notification emitted after one
// durable goal mutation commits.
const EventChanged = "goal/changed"

// GoalOperation is a goal state-changing verb recorded in the durable
// source change.
type GoalOperation string

// The goal operations.
const (
	OperationCreate   GoalOperation = "create"
	OperationEdit     GoalOperation = "edit"
	OperationPause    GoalOperation = "pause"
	OperationResume   GoalOperation = "resume"
	OperationComplete GoalOperation = "complete"
	OperationBlock    GoalOperation = "block"
	OperationClear    GoalOperation = "clear"
)

// ChangeMeta is the durable change union carried by the goal domain's own
// session event. One struct rather than a union interface because the wire
// shape must stay exact: snapshot changes carry goal/roundsStarted/
// createdAt/updatedAt (roundsStarted is legitimately zero on a create),
// clear tombstones carry cleared/clearedAt — pointer fields preserve the
// zero-valued members an omitempty value field would drop.
type ChangeMeta struct {
	Kind      string        `json:"kind"`
	Version   int           `json:"version"`
	Operation GoalOperation `json:"operation"`
	// Snapshot-change members.
	Goal          *GoalSnapshot `json:"goal,omitempty"`
	RoundsStarted *int64        `json:"roundsStarted,omitempty"`
	CreatedAt     *int64        `json:"createdAt,omitempty"`
	UpdatedAt     *int64        `json:"updatedAt,omitempty"`
	// Clear-tombstone members.
	Cleared   *GoalRef `json:"cleared,omitempty"`
	ClearedAt *int64   `json:"clearedAt,omitempty"`
}

// newSnapshotChange builds one full-snapshot mutation payload.
func newSnapshotChange(operation GoalOperation, goal GoalSnapshot, roundsStarted, createdAt, updatedAt int64) ChangeMeta {
	return ChangeMeta{
		Kind:          EventChange,
		Version:       ChangeVersion,
		Operation:     operation,
		Goal:          &goal,
		RoundsStarted: &roundsStarted,
		CreatedAt:     &createdAt,
		UpdatedAt:     &updatedAt,
	}
}

// newClearChange builds one clear tombstone payload.
func newClearChange(cleared GoalRef, clearedAt int64) ChangeMeta {
	return ChangeMeta{
		Kind:      EventChange,
		Version:   ChangeVersion,
		Operation: OperationClear,
		Cleared:   &cleared,
		ClearedAt: &clearedAt,
	}
}

// changeRef returns the revision identity carried by a snapshot or
// tombstone.
func changeRef(change ChangeMeta) GoalRef {
	if change.Operation == OperationClear {
		return *change.Cleared
	}
	return GoalRef{ID: change.Goal.ID, Revision: change.Goal.Revision}
}

// GoalChanged is the live notification after one durable goal mutation
// commits.
type GoalChanged struct {
	Operation GoalOperation `json:"operation"`
	Ref       GoalRef       `json:"ref"`
	// Absent for a clear tombstone.
	Goal *GoalView `json:"goal,omitempty"`
}

// ChangedPayload is the agent-scoped `goal/changed` dispatch payload.
type ChangedPayload struct {
	Agent  *agent.Agent
	Change GoalChanged
}

// GoalErrorCode names the stable error codes for rejected goal reads and
// mutations.
type GoalErrorCode = string

// The stable goal error codes.
const (
	CodeAgentNotLive       GoalErrorCode = "GOAL_AGENT_NOT_LIVE"
	CodeNotFound           GoalErrorCode = "GOAL_NOT_FOUND"
	CodeAlreadyExists      GoalErrorCode = "GOAL_ALREADY_EXISTS"
	CodeStaleRevision      GoalErrorCode = "GOAL_STALE_REVISION"
	CodeInvalidObjective   GoalErrorCode = "GOAL_INVALID_OBJECTIVE"
	CodeInvalidMaxRounds   GoalErrorCode = "GOAL_INVALID_MAX_ROUNDS"
	CodeInvalidBlockReason GoalErrorCode = "GOAL_INVALID_BLOCK_REASON"
	CodeInvalidEdit        GoalErrorCode = "GOAL_INVALID_EDIT"
	CodeInvalidTransition  GoalErrorCode = "GOAL_INVALID_TRANSITION"
)

func nowMilliseconds() int64 { return time.Now().UnixMilli() }
