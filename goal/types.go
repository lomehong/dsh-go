// Package goal is the same-session goal domain (official
// @deepseek-ai/dsh-goal): event-sourced state over the owning session log,
// compare-and-set mutations, and process-local continuation activation.
// The durable source of truth is the `goal/change` session event stream;
// activation is process-local and never persisted.
package goal

// GoalID identifies one goal across its durable revisions.
type GoalID string

// GoalRef is the compare-and-set identity for one exact goal revision.
type GoalRef struct {
	// Stable goal identity.
	ID GoalID `json:"id"`
	// Positive revision; every durable mutation increments it.
	Revision int64 `json:"revision"`
}

// CreateGoalRequest is the create input whose omitted round cap is resolved
// by the service configuration.
type CreateGoalRequest struct {
	Objective string `json:"objective"`
	// Nil means absent: the deployment default applies.
	MaxGoalRounds *int64 `json:"maxGoalRounds,omitempty"`
}

// CreateGoalResult is the wire-safe acknowledgement of one created goal.
type CreateGoalResult struct {
	Ref GoalRef `json:"ref"`
}

// EditGoalRequest carries the fields changed by an edit; at least one must
// be present.
type EditGoalRequest struct {
	Objective     *string `json:"objective,omitempty"`
	MaxGoalRounds *int64  `json:"maxGoalRounds,omitempty"`
}

// GoalPhase is the durable continuation phase. Activation is process-local
// and separate.
type GoalPhase string

// The durable phases.
const (
	PhaseActive   GoalPhase = "active"
	PhasePaused   GoalPhase = "paused"
	PhaseBlocked  GoalPhase = "blocked"
	PhaseComplete GoalPhase = "complete"
)

// GoalBlockReason is the machine-routable and human-readable explanation
// for a blocked goal.
type GoalBlockReason struct {
	// Stable lower-kebab-case classification chosen by the blocking policy.
	Code string `json:"code"`
	// Non-empty explanation shown to humans and models.
	Message string `json:"message"`
}

// GoalSnapshot is the full durable state written by every non-clear goal
// mutation.
type GoalSnapshot struct {
	// Stable goal identity.
	ID GoalID `json:"id"`
	// Positive revision; every durable mutation increments it.
	Revision int64 `json:"revision"`
	// Human-requested completion objective.
	Objective string `json:"objective"`
	// Durable lifecycle phase.
	Phase GoalPhase `json:"phase"`
	// Present exactly while Phase is blocked.
	BlockedReason *GoalBlockReason `json:"blockedReason,omitempty"`
	// Total admitted goal-round cap.
	MaxGoalRounds int64 `json:"maxGoalRounds"`
}

// GoalActivation is whether this live process may automatically continue an
// active goal.
type GoalActivation string

// The activation states.
const (
	ActivationArmed    GoalActivation = "armed"
	ActivationDisarmed GoalActivation = "disarmed"
)

// GoalView is the current goal projection, including values derived from
// the session log.
type GoalView struct {
	GoalSnapshot
	// Highest admitted round number for this goal.
	RoundsStarted int64 `json:"roundsStarted"`
	// Epoch milliseconds of the create mutation.
	CreatedAt int64 `json:"createdAt"`
	// Epoch milliseconds of the latest mutation.
	UpdatedAt int64 `json:"updatedAt"`
	// Process-local continuation eligibility; never persisted.
	Activation GoalActivation `json:"activation"`
}

// GoalProjection is the `goal` projection value: the current durable goal
// with its replay counters, exactly as the latest `goal/change` event
// carried them. Activation is process-local (never persisted) and
// deliberately absent — the projection reflects durable phase only.
type GoalProjection struct {
	// Current durable goal snapshot (the CAS ref for mutations rides on it).
	Goal GoalSnapshot `json:"goal"`
	// Highest admitted round number for this goal.
	RoundsStarted int64 `json:"roundsStarted"`
	// Epoch milliseconds of the create mutation.
	CreatedAt int64 `json:"createdAt"`
	// Epoch milliseconds of the latest mutation.
	UpdatedAt int64 `json:"updatedAt"`
}

// FoldedGoal is the pure replay fold result of durable goal facts.
type FoldedGoal struct {
	// Current goal, absent after a clear or before the first create.
	Goal *GoalSnapshot
	// Highest admitted round for the current goal.
	RoundsStarted int64
	// Current goal creation time, absent without a current goal.
	CreatedAt *int64
	// Current goal mutation time, absent without a current goal.
	UpdatedAt *int64
	// Latest mutation ref, including a clear tombstone.
	LastRef *GoalRef
}
