package goal

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"dshgo/session"
)

// maxSafeInteger mirrors Number.MAX_SAFE_INTEGER: the safe-integer ceiling
// every goal counter must stay under (official Number.isSafeInteger gates).
const maxSafeInteger = int64(9007199254740991)

var snapshotOperations = map[GoalOperation]bool{
	OperationCreate: true, OperationEdit: true, OperationPause: true,
	OperationResume: true, OperationComplete: true, OperationBlock: true,
}

var phases = map[GoalPhase]bool{
	PhaseActive: true, PhasePaused: true, PhaseBlocked: true, PhaseComplete: true,
}

// kebabCodePattern is the stable lower-kebab-case block-reason code shape.
var kebabCodePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)

// FoldState is the mutable accumulator kept private to the pure fold.
type FoldState struct {
	Goal          *GoalSnapshot
	RoundsStarted int64
	CreatedAt     *int64
	UpdatedAt     *int64
	LastRef       *GoalRef
	seenGoalIDs   map[GoalID]struct{}
}

// EmptyFoldState builds an empty replay accumulator: no current goal or
// prior ref.
func EmptyFoldState() *FoldState {
	return &FoldState{seenGoalIDs: map[GoalID]struct{}{}}
}

// clone copies the independent fold for candidate validation.
func (s *FoldState) clone() *FoldState {
	seen := make(map[GoalID]struct{}, len(s.seenGoalIDs))
	for id := range s.seenGoalIDs {
		seen[id] = struct{}{}
	}
	return &FoldState{
		Goal: s.Goal, RoundsStarted: s.RoundsStarted,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, LastRef: s.LastRef,
		seenGoalIDs: seen,
	}
}

func isRecord(value any) bool {
	_, ok := value.(map[string]any)
	return ok
}

// asInteger narrows a decoded JSON number to a whole int64.
func asInteger(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number != float64(int64(number)) || number > float64(maxSafeInteger) || number < -float64(maxSafeInteger) {
		return 0, false
	}
	return int64(number), true
}

func positiveInteger(value any, field string) (int64, error) {
	whole, ok := asInteger(value)
	if !ok || whole < 1 {
		return 0, fmt.Errorf("goal change %s must be a positive safe integer", field)
	}
	return whole, nil
}

func nonNegativeInteger(value any, field string) (int64, error) {
	whole, ok := asInteger(value)
	if !ok || whole < 0 {
		return 0, fmt.Errorf("goal change %s must be a non-negative safe integer", field)
	}
	return whole, nil
}

// sortedKeys renders one record's exact-key fingerprint.
func sortedKeys(record map[string]any) string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// decodeBlockReason decodes one canonical blocker explanation.
func decodeBlockReason(value any) (*GoalBlockReason, error) {
	record, ok := value.(map[string]any)
	if !ok || sortedKeys(record) != "code,message" {
		return nil, fmt.Errorf("goal change goal.blockedReason must have exactly code and message fields")
	}
	code, ok := record["code"].(string)
	if !ok || !kebabCodePattern.MatchString(code) {
		return nil, fmt.Errorf("goal change goal.blockedReason.code must be lower-kebab-case")
	}
	message, ok := record["message"].(string)
	if !ok || strings.TrimSpace(message) == "" || message != strings.TrimSpace(message) {
		return nil, fmt.Errorf("goal change goal.blockedReason.message must be non-empty and normalized")
	}
	return &GoalBlockReason{Code: code, Message: message}, nil
}

// decodeSnapshot decodes and validates one snapshot.
func decodeSnapshot(value any) (*GoalSnapshot, error) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("goal change goal must be a record")
	}
	id, ok := record["id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("goal change goal.id must be a non-empty string")
	}
	objective, ok := record["objective"].(string)
	if !ok || strings.TrimSpace(objective) == "" || objective != strings.TrimSpace(objective) {
		return nil, fmt.Errorf("goal change goal.objective must be non-empty and normalized")
	}
	rawPhase, ok := record["phase"].(string)
	if !ok || !phases[GoalPhase(rawPhase)] {
		return nil, fmt.Errorf("goal change goal.phase is invalid")
	}
	phase := GoalPhase(rawPhase)
	expectedKeys := "id,maxGoalRounds,objective,phase,revision"
	if phase == PhaseBlocked {
		expectedKeys = "blockedReason,id,maxGoalRounds,objective,phase,revision"
	}
	if sortedKeys(record) != expectedKeys {
		return nil, fmt.Errorf("goal change goal for phase %s must have exactly %s fields", phase, expectedKeys)
	}
	revision, err := positiveInteger(record["revision"], "goal.revision")
	if err != nil {
		return nil, err
	}
	maxGoalRounds, err := positiveInteger(record["maxGoalRounds"], "goal.maxGoalRounds")
	if err != nil {
		return nil, err
	}
	snapshot := &GoalSnapshot{
		ID: GoalID(id), Revision: revision, Objective: objective,
		Phase: phase, MaxGoalRounds: maxGoalRounds,
	}
	if phase == PhaseBlocked {
		reason, err := decodeBlockReason(record["blockedReason"])
		if err != nil {
			return nil, err
		}
		snapshot.BlockedReason = reason
	}
	return snapshot, nil
}

// decodeRef decodes and validates one tombstone ref.
func decodeRef(value any) (*GoalRef, error) {
	record, ok := value.(map[string]any)
	if !ok || sortedKeys(record) != "id,revision" {
		return nil, fmt.Errorf("goal clear tombstone must have exactly id and revision fields")
	}
	id, ok := record["id"].(string)
	if !ok || id == "" {
		return nil, fmt.Errorf("goal clear tombstone id must be a non-empty string")
	}
	revision, err := positiveInteger(record["revision"], "cleared.revision")
	if err != nil {
		return nil, err
	}
	return &GoalRef{ID: GoalID(id), Revision: revision}, nil
}

// DecodeGoalChange decodes a value that declares itself as a goal change.
// Unrelated values return nil without error; malformed goal changes fail
// replay loudly.
func DecodeGoalChange(raw json.RawMessage) (*ChangeMeta, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, nil //nolint:nilnil // undecodable data is another event's business
	}
	record, ok := value.(map[string]any)
	if !ok || record["kind"] != EventChange {
		return nil, nil //nolint:nilnil // not a goal change
	}
	version, ok := asInteger(record["version"])
	if !ok || version != ChangeVersion {
		return nil, fmt.Errorf("unsupported goal change version %v", record["version"])
	}
	if record["operation"] == string(OperationClear) {
		allowed := sortedKeys(map[string]any{"kind": 0, "version": 0, "operation": 0, "cleared": 0, "clearedAt": 0})
		if sortedKeys(record) != allowed {
			return nil, fmt.Errorf("goal clear change must have exactly %s fields", allowed)
		}
		cleared, err := decodeRef(record["cleared"])
		if err != nil {
			return nil, err
		}
		clearedAt, err := nonNegativeInteger(record["clearedAt"], "clearedAt")
		if err != nil {
			return nil, err
		}
		change := newClearChange(*cleared, clearedAt)
		return &change, nil
	}
	operation, ok := record["operation"].(string)
	if !ok || !snapshotOperations[GoalOperation(operation)] {
		return nil, fmt.Errorf("goal change operation is invalid")
	}
	allowed := sortedKeys(map[string]any{"kind": 0, "version": 0, "operation": 0, "goal": 0, "roundsStarted": 0, "createdAt": 0, "updatedAt": 0})
	if sortedKeys(record) != allowed {
		return nil, fmt.Errorf("goal snapshot change must have exactly %s fields", allowed)
	}
	createdAt, err := nonNegativeInteger(record["createdAt"], "createdAt")
	if err != nil {
		return nil, err
	}
	updatedAt, err := nonNegativeInteger(record["updatedAt"], "updatedAt")
	if err != nil {
		return nil, err
	}
	if updatedAt < createdAt {
		return nil, fmt.Errorf("goal change updatedAt cannot precede createdAt")
	}
	snapshot, err := decodeSnapshot(record["goal"])
	if err != nil {
		return nil, err
	}
	roundsStarted, err := nonNegativeInteger(record["roundsStarted"], "roundsStarted")
	if err != nil {
		return nil, err
	}
	change := newSnapshotChange(GoalOperation(operation), *snapshot, roundsStarted, createdAt, updatedAt)
	return &change, nil
}

// goalSource narrows one decoded user-message source to a valid goal source.
func goalSource(raw json.RawMessage) (*goalMessageSource, error, bool) {
	var envelope struct {
		Source struct {
			Kind     string `json:"kind"`
			GoalID   string `json:"goalId"`
			Revision any    `json:"revision"`
			Round    any    `json:"round"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, nil, false
	}
	source := envelope.Source
	if source.Kind != "goal" {
		return nil, nil, false
	}
	revision, revisionOK := asInteger(source.Revision)
	round, roundOK := asInteger(source.Round)
	if source.GoalID == "" || !revisionOK || revision < 1 || !roundOK || round < 1 {
		return nil, fmt.Errorf("goal message source is invalid"), false
	}
	return &goalMessageSource{goalID: GoalID(source.GoalID), revision: revision, round: round}, nil, true
}

// goalMessageSource is one admitted continuation round attribution.
type goalMessageSource struct {
	goalID   GoalID
	revision int64
	round    int64
}

// requireSameDefinition requires two snapshots to retain fields that only
// `edit` may replace.
func requireSameDefinition(current, next *GoalSnapshot, operation GoalOperation) error {
	if next.Objective != current.Objective || next.MaxGoalRounds != current.MaxGoalRounds {
		return fmt.Errorf("goal %s cannot change objective or maxGoalRounds", operation)
	}
	return nil
}

// requireNextRevision requires one exact next revision of the current goal.
func requireNextRevision(current *GoalSnapshot, next GoalRef, operation GoalOperation) error {
	if next.ID != current.ID || next.Revision != current.Revision+1 {
		return fmt.Errorf("goal %s must advance the current goal by one revision", operation)
	}
	return nil
}

// validateSnapshotTransition validates one non-create snapshot operation
// against the preceding projection.
func validateSnapshotTransition(state *FoldState, change *ChangeMeta, current *GoalSnapshot) error {
	next := change.Goal
	if err := requireNextRevision(current, GoalRef{ID: next.ID, Revision: next.Revision}, change.Operation); err != nil {
		return err
	}
	if state.UpdatedAt == nil {
		return fmt.Errorf("current goal fold lacks updatedAt")
	}
	if *change.CreatedAt != *state.CreatedAt ||
		*change.UpdatedAt < *state.UpdatedAt ||
		*change.RoundsStarted != state.RoundsStarted {
		return fmt.Errorf("goal %s does not preserve the current counters and timestamps", change.Operation)
	}
	switch change.Operation {
	case OperationEdit:
		if next.Phase != current.Phase || !sameBlockReason(next.BlockedReason, current.BlockedReason) {
			return fmt.Errorf("goal edit cannot change phase or blocked reason")
		}
	case OperationPause:
		if err := requireSameDefinition(current, next, change.Operation); err != nil {
			return err
		}
		if current.Phase != PhaseActive || next.Phase != PhasePaused {
			return fmt.Errorf("goal pause has an invalid phase transition")
		}
	case OperationResume:
		if err := requireSameDefinition(current, next, change.Operation); err != nil {
			return err
		}
		resumable := current.Phase == PhaseActive || current.Phase == PhasePaused || current.Phase == PhaseBlocked
		if !resumable || next.Phase != PhaseActive || state.RoundsStarted >= next.MaxGoalRounds {
			return fmt.Errorf("goal resume has an invalid phase transition or exhausted round budget")
		}
	case OperationComplete:
		if err := requireSameDefinition(current, next, change.Operation); err != nil {
			return err
		}
		if current.Phase == PhaseComplete || next.Phase != PhaseComplete {
			return fmt.Errorf("goal complete has an invalid phase transition")
		}
	case OperationBlock:
		if err := requireSameDefinition(current, next, change.Operation); err != nil {
			return err
		}
		if current.Phase != PhaseActive || next.Phase != PhaseBlocked {
			return fmt.Errorf("goal block has an invalid phase transition")
		}
	case OperationCreate:
		// The caller excludes create; this arm retains fail-loud
		// exhaustiveness (the official satisfies-never guard).
		return fmt.Errorf("goal create cannot be validated as a current-goal transition")
	default:
		return fmt.Errorf("unknown goal snapshot operation")
	}
	return nil
}

// sameBlockReason is the structural blocked-reason equality (the official
// JSON.stringify comparison).
func sameBlockReason(next, current *GoalBlockReason) bool {
	if next == nil || current == nil {
		return next == current
	}
	return *next == *current
}

// ApplyGoalChange validates and applies one decoded change to a mutable
// accumulator.
func ApplyGoalChange(state *FoldState, change *ChangeMeta) error {
	ref := changeRef(*change)
	if change.Operation == OperationClear {
		current := state.Goal
		if current == nil {
			return fmt.Errorf("goal clear requires a current goal")
		}
		if err := requireNextRevision(current, ref, change.Operation); err != nil {
			return err
		}
		if state.UpdatedAt == nil {
			return fmt.Errorf("current goal fold lacks updatedAt")
		}
		if *change.ClearedAt < *state.UpdatedAt {
			return fmt.Errorf("goal clear timestamp cannot precede the current goal update")
		}
		state.Goal = nil
		state.RoundsStarted = 0
		state.CreatedAt = nil
		state.UpdatedAt = nil
		state.LastRef = &ref
		return nil
	}
	if change.Operation == OperationCreate {
		_, seen := state.seenGoalIDs[change.Goal.ID]
		if change.Goal.Revision != 1 || change.Goal.Phase != PhaseActive || *change.RoundsStarted != 0 ||
			(state.Goal != nil && state.Goal.Phase != PhaseComplete) || seen {
			return fmt.Errorf("goal create requires a fresh active revision-one goal with zero rounds")
		}
		state.seenGoalIDs[change.Goal.ID] = struct{}{}
	} else {
		current := state.Goal
		if current == nil {
			return fmt.Errorf("goal %s requires a current goal", change.Operation)
		}
		if err := validateSnapshotTransition(state, change, current); err != nil {
			return err
		}
	}
	goal := *change.Goal
	createdAt := *change.CreatedAt
	updatedAt := *change.UpdatedAt
	state.Goal = &goal
	state.RoundsStarted = *change.RoundsStarted
	state.CreatedAt = &createdAt
	state.UpdatedAt = &updatedAt
	state.LastRef = &ref
	return nil
}

// ApplyGoalEvent applies one session event to the strict durable goal fold.
func ApplyGoalEvent(state *FoldState, event session.Event) error {
	switch event.Type {
	case EventChange:
		change, err := DecodeGoalChange(event.Data)
		if err != nil {
			return err
		}
		if change == nil {
			// The event's declared payload always identifies itself as a
			// goal change.
			return fmt.Errorf("goal change at session event %d has an invalid kind", event.Seq)
		}
		return ApplyGoalChange(state, change)
	case session.EventUserMessage:
		source, err, matched := goalSource(event.Data)
		if err != nil {
			return err
		}
		if !matched {
			return nil
		}
		current := state.Goal
		if current == nil || current.Phase != PhaseActive || source.goalID != current.ID ||
			source.revision != current.Revision || source.round != state.RoundsStarted+1 ||
			source.round > current.MaxGoalRounds {
			return fmt.Errorf("goal round at session event %d is not the next admitted round of the active goal", event.Seq)
		}
		state.RoundsStarted = source.round
	}
	return nil
}

// FoldGoal folds current goal state from a contiguous session event log.
// The result is a fresh durable projection; activation is deliberately
// absent.
func FoldGoal(events []session.Event) (FoldedGoal, error) {
	state := EmptyFoldState()
	for _, event := range events {
		if err := ApplyGoalEvent(state, event); err != nil {
			return FoldedGoal{}, err
		}
	}
	folded := FoldedGoal{RoundsStarted: state.RoundsStarted}
	if state.Goal != nil {
		goal := *state.Goal
		folded.Goal = &goal
	}
	folded.CreatedAt = state.CreatedAt
	folded.UpdatedAt = state.UpdatedAt
	folded.LastRef = state.LastRef
	return folded, nil
}
