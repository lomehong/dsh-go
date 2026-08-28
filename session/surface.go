// Surface layer on top of the session event log: an ordered view of events
// that produce LLM messages. The append-only log remains the source of
// truth. Port of packages/core/session/src/surface.ts.
package session

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/llm"
)

// SurfaceFoldReplacement is one replacement operation observed while folding
// a session surface.
type SurfaceFoldReplacement struct {
	// Seq of the event that replaced the prior surface range.
	Seq int64
	// Start and End are the declared inclusive replaced range.
	Start, End int64
	// ShadowedSeqs are the surface entries actually removed, in order.
	ShadowedSeqs []int64
}

// SurfaceFoldResult is the complete result of replaying the surface
// operations in a session log.
type SurfaceFoldResult struct {
	// Nodes are the current surface event sequences in model-visible order.
	Nodes []int64
	// Replacements are the replacement operations in event order.
	Replacements []SurfaceFoldReplacement
}

// SurfaceFoldState is the mutable state shared by complete and incremental
// folds.
type SurfaceFoldState struct {
	Nodes             []int64
	ReplaceGeneration int64
}

type surfacePlanKind int

const (
	planNone surfacePlanKind = iota
	planAppend
	planReplace
)

// A validated surface transition that has not mutated fold state yet.
type surfacePlan struct {
	kind surfacePlanKind
	seq  int64
	// replace fields:
	start, end   int64
	startIdx     int64
	endIdx       int64
	shadowedSeqs []int64
}

func isEventSeq(value int64) bool { return value >= 0 }

// surfaceOpOf validates event-local surface eligibility and returns the
// operation. A nil result with nil error means a non-surface event.
func surfaceOpOf(event Event) (*SurfaceOp, error) {
	if !IsSurfaceEventType(event.Type) {
		if event.SurfaceOp != nil {
			return nil, fmt.Errorf("session event %q is not surface-eligible and cannot carry surfaceOp", event.Type)
		}
		if event.SourceEventSeqs != nil {
			return nil, fmt.Errorf("session event %q is not surface-eligible and cannot carry sourceEventSeqs", event.Type)
		}
		return nil, nil
	}
	if event.SurfaceOp == nil {
		return nil, fmt.Errorf("session event %q is surface-eligible and requires a surfaceOp marker", event.Type)
	}
	switch event.SurfaceOp.Kind {
	case SurfaceAppend:
		return event.SurfaceOp, nil
	case SurfaceReplace:
		return event.SurfaceOp, nil
	default:
		return nil, fmt.Errorf("session event %q carries an invalid surfaceOp", event.Type)
	}
}

// assertProvenance validates cited source-event seqs against prior log
// entries and the replacement range.
func assertProvenance(event Event, shadowedSeqs []int64) error {
	sources := map[int64]bool{}
	if event.SourceEventSeqs != nil {
		if len(event.SourceEventSeqs) == 0 && event.Type != EventAssistantMsg {
			return fmt.Errorf("sourceEventSeqs must not be empty except on assistant/message")
		}
		nonEarlier := false
		for _, source := range event.SourceEventSeqs {
			if !isEventSeq(source) {
				return fmt.Errorf("session event %q sourceEventSeqs must densely contain non-negative safe integers", event.Type)
			}
			if sources[source] {
				return fmt.Errorf("sourceEventSeqs must not contain duplicates")
			}
			sources[source] = true
			if source >= event.Seq {
				nonEarlier = true
			}
		}
		if nonEarlier {
			return fmt.Errorf("sourceEventSeqs must reference earlier events (>= current seq %d)", event.Seq)
		}
	}
	var missing []int64
	for _, seq := range shadowedSeqs {
		if !sources[seq] {
			missing = append(missing, seq)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("surface replace: sourceEventSeqs must include every shadowed surface node; missing %v", missing)
	}
	return nil
}

// replacementRange locates one replacement range without mutating fold
// state.
func replacementRange(state *SurfaceFoldState, op SurfaceOp) (startIdx, endIdx int64, shadowed []int64, err error) {
	startIdx = int64(-1)
	endIdx = int64(-1)
	for i, node := range state.Nodes {
		if node == op.Start && startIdx == -1 {
			startIdx = int64(i)
		}
		if node == op.End && endIdx == -1 {
			endIdx = int64(i)
		}
	}
	if startIdx == -1 {
		return 0, 0, nil, fmt.Errorf("surface replace: start seq %d not found in surface", op.Start)
	}
	if endIdx == -1 {
		return 0, 0, nil, fmt.Errorf("surface replace: end seq %d not found in surface", op.End)
	}
	if startIdx > endIdx {
		return 0, 0, nil, fmt.Errorf("surface replace: start seq %d (index %d) is after end seq %d (index %d)", op.Start, startIdx, op.End, endIdx)
	}
	shadowed = append([]int64(nil), state.Nodes[startIdx:endIdx+1]...)
	return startIdx, endIdx, shadowed, nil
}

// assertToolResultRewrite restricts a tool-result replacement to changing
// only one current result's content.
func assertToolResultRewrite(event Event, shadowedSeqs []int64, events []Event, baseSeq int64) error {
	if event.Type != EventToolResult {
		return nil
	}
	if len(shadowedSeqs) != 1 {
		return fmt.Errorf("tool/result surface replacement must rewrite exactly one current node")
	}
	for _, originalSeq := range shadowedSeqs {
		idx := originalSeq - baseSeq
		if idx < 0 || idx >= int64(len(events)) || events[idx].Type != EventToolResult {
			return fmt.Errorf("tool/result surface replacement must target a current tool/result")
		}
		same, err := toolResultSameExceptContent(events[idx].Data, event.Data)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("tool/result surface replacement may change only content")
		}
	}
	return nil
}

// toolResultSameExceptContent compares two tool/result payloads with every
// tool-result block's nested content nulled out, so only the content may
// differ.
func toolResultSameExceptContent(originalRaw, replacementRaw json.RawMessage) (bool, error) {
	strip := func(raw json.RawMessage) (json.RawMessage, error) {
		var payload ToolResultData
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, err
		}
		for i := range payload.Message.Content {
			if payload.Message.Content[i].Type == llm.BlockToolResult {
				payload.Message.Content[i].Content = nil
			}
		}
		return json.Marshal(payload)
	}
	strippedOriginal, err := strip(originalRaw)
	if err != nil {
		return false, fmt.Errorf("tool/result replacement original payload: %w", err)
	}
	strippedReplacement, err := strip(replacementRaw)
	if err != nil {
		return false, fmt.Errorf("tool/result replacement payload: %w", err)
	}
	return bytes.Equal(strippedOriginal, strippedReplacement), nil
}

// planSurfaceEvent validates one event at its replay boundary and prepares
// its atomic fold transition.
func planSurfaceEvent(state *SurfaceFoldState, event Event, expectedSeq int64, events []Event, baseSeq int64) (*surfacePlan, error) {
	if event.Seq != expectedSeq {
		return nil, fmt.Errorf("session event seq %d is not contiguous; expected %d", event.Seq, expectedSeq)
	}
	op, err := surfaceOpOf(event)
	if err != nil {
		return nil, err
	}
	if op == nil {
		return nil, nil
	}
	if op.Kind == SurfaceAppend {
		if err := assertProvenance(event, nil); err != nil {
			return nil, err
		}
		return &surfacePlan{kind: planAppend, seq: event.Seq}, nil
	}
	startIdx, endIdx, shadowed, err := replacementRange(state, *op)
	if err != nil {
		return nil, err
	}
	if err := assertProvenance(event, shadowed); err != nil {
		return nil, err
	}
	if err := assertToolResultRewrite(event, shadowed, events, baseSeq); err != nil {
		return nil, err
	}
	return &surfacePlan{
		kind: planReplace, seq: event.Seq,
		start: op.Start, end: op.End,
		startIdx: startIdx, endIdx: endIdx, shadowedSeqs: shadowed,
	}, nil
}

// applySurfacePlan commits one previously validated transition and reports
// replacement metadata when one occurred. A nil plan is a non-surface
// event: the fold state is untouched.
func applySurfacePlan(state *SurfaceFoldState, plan *surfacePlan) *SurfaceFoldReplacement {
	if plan == nil || plan.kind == planNone {
		return nil
	}
	switch plan.kind {
	case planAppend:
		state.Nodes = append(state.Nodes, plan.seq)
	case planReplace:
		state.Nodes = append(state.Nodes[:plan.startIdx],
			append([]int64{plan.seq}, state.Nodes[plan.endIdx+1:]...)...)
		state.ReplaceGeneration++
	default:
		return nil
	}
	if plan.kind != planReplace {
		return nil
	}
	return &SurfaceFoldReplacement{Seq: plan.seq, Start: plan.start, End: plan.end, ShadowedSeqs: plan.shadowedSeqs}
}

// FoldSurface replays a complete session log through the canonical surface
// fold. An error names the event that violated surface metadata,
// source-event references, range, or tool-result rewrite rules.
func FoldSurface(events []Event) (SurfaceFoldResult, error) {
	state := &SurfaceFoldState{}
	var replacements []SurfaceFoldReplacement
	for index, event := range events {
		plan, err := planSurfaceEvent(state, event, int64(index), events, 0)
		if err != nil {
			return SurfaceFoldResult{}, err
		}
		if replacement := applySurfacePlan(state, plan); replacement != nil {
			replacements = append(replacements, *replacement)
		}
	}
	return SurfaceFoldResult{Nodes: append([]int64(nil), state.Nodes...), Replacements: replacements}, nil
}

// SurfaceManager is the incremental ordered surface view and
// append-boundary validator. It reads the live log through a pointer, so
// appends the session commits are observed lazily.
type SurfaceManager struct {
	log *[]Event

	state         *SurfaceFoldState
	lastProcessed int64
	pendingEvent  *Event
	pendingPlan   *surfacePlan
}

// NewSurfaceManager builds a manager over the given log pointer.
func NewSurfaceManager(log *[]Event) *SurfaceManager {
	return &SurfaceManager{log: log, state: &SurfaceFoldState{}, lastProcessed: -1}
}

// ValidateNext validates the next candidate without mutating the committed
// surface. The validation error surfaces here and, on success, the planned
// transition is consumed by the next fold delta.
func (m *SurfaceManager) ValidateNext(event Event) error {
	m.processDelta()
	expected := int64(len(*m.log))
	plan, err := planSurfaceEvent(m.state, event, expected, *m.log, 0)
	if err != nil {
		return err
	}
	candidate := event
	m.pendingEvent, m.pendingPlan = &candidate, plan
	return nil
}

// ReplaceGeneration is the monotonic count of folded positional
// replacements.
func (m *SurfaceManager) ReplaceGeneration() int64 {
	m.processDelta()
	return m.state.ReplaceGeneration
}

// Nodes returns the surface event sequences in model-visible order.
func (m *SurfaceManager) Nodes() []int64 {
	m.processDelta()
	return m.state.Nodes
}

// processDelta folds events appended since the previous access.
func (m *SurfaceManager) processDelta() {
	tail := int64(len(*m.log)) - 1
	for seq := m.lastProcessed + 1; seq <= tail; seq++ {
		event := (*m.log)[seq]
		if m.pendingEvent != nil && m.pendingEvent.Seq == event.Seq && m.pendingPlan != nil {
			applySurfacePlan(m.state, m.pendingPlan)
		} else {
			plan, err := planSurfaceEvent(m.state, event, seq, *m.log, 0)
			if err != nil {
				// The committed log already validated this event at append
				// (or seed) time; a fold failure here is an invariant break.
				panic(fmt.Sprintf("session: committed log fails surface fold at seq %d: %v", seq, err))
			}
			applySurfacePlan(m.state, plan)
		}
		m.pendingEvent, m.pendingPlan = nil, nil
		m.lastProcessed = seq
	}
}
