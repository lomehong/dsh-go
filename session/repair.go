// Crash-recovery repair for an interrupted session log: preserves a fully
// written final turn and supplies the missing tool, step, and turn
// boundaries needed to resume with a provider-valid transcript. Port of
// packages/core/session/src/repair.ts.
package session

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
)

// Recovery codes for synthetic closers.
const (
	// ToolNotStarted marks an assistant tool request that never reached a
	// recorded call start.
	ToolNotStarted = "TOOL_NOT_STARTED"
	// ToolOutcomeUnknown marks a recorded tool call whose completed outcome
	// was not durably recorded.
	ToolOutcomeUnknown = "TOOL_OUTCOME_UNKNOWN"
)

// Synthetic tool-result texts, pinned verbatim: the model decides whether to
// retry from the tool's own semantics.
const (
	toolOutcomeUnknownText = "The tool call was interrupted after it was recorded, but no result was durably recorded. Its outcome is unknown. Decide whether to retry from the tool semantics: retry only if the operation is read-only or idempotent; if it may have side effects, first verify external state or ask the user. Do not retry blindly."
	toolNotStartedText     = "The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."
)

type pendingToolCall struct {
	callID  string
	step    int64
	callSeq int64
	started bool
}

// InterruptedTurnClosers returns the deterministic synthetic events that
// close an open tail turn: unmatched calls receive error results first,
// followed by an open step/end and an interrupted turn/end; sequences
// continue the log and timestamps reuse the last real event (deterministic,
// never a "future" time). A balanced or empty log yields no events.
func InterruptedTurnClosers(events []Event) []Event {
	var openTurn, openStep int64 = -1, -1
	// Reset at each turn boundary so earlier calls cannot leak into tail
	// repair. Assistant blocks register calls; later tool/call events mark
	// them started.
	var pending []*pendingToolCall
	pendingIndex := map[string]*pendingToolCall{}
	for _, event := range events {
		switch event.Type {
		case EventTurnStart:
			var data TurnStartData
			_ = json.Unmarshal(event.Data, &data)
			openTurn, openStep = data.Turn, -1
			pending, pendingIndex = nil, map[string]*pendingToolCall{}
		case EventTurnEnd:
			openTurn, openStep = -1, -1
			pending, pendingIndex = nil, map[string]*pendingToolCall{}
		case EventStepStart:
			var data StepStartData
			_ = json.Unmarshal(event.Data, &data)
			openStep = data.Step
		case EventStepEnd:
			pending, pendingIndex = nil, map[string]*pendingToolCall{}
			openStep = -1
		case EventAssistantMsg:
			if data, err := DecodeAssistantMessage(event); err == nil {
				for _, block := range data.Message.Content {
					if block.Type == llm.BlockToolCall {
						entry := &pendingToolCall{callID: block.ToolCallID, step: data.Step}
						pending = append(pending, entry)
						pendingIndex[block.ToolCallID] = entry
					}
				}
			}
		case EventToolCall:
			var data ToolCallData
			if err := json.Unmarshal(event.Data, &data); err == nil {
				if entry, ok := pendingIndex[data.CallID]; ok {
					entry.callSeq = event.Seq
					entry.started = true
				}
			}
		case EventToolResult:
			if data, err := DecodeToolResult(event); err == nil {
				delete(pendingIndex, data.Message.Source.CallID)
			}
		}
		// Other event types do not move the turn/step boundary cursor.
	}
	// Balanced log (no crash mid-turn): nothing to close. An open turn
	// implies events is non-empty (its turn/start was logged).
	if openTurn < 0 || len(events) == 0 {
		return nil
	}
	last := events[len(events)-1]
	seq := last.Seq + 1
	time := last.Time
	var closers []Event
	// Close calls before their step: providers reject dangling assistant
	// calls, and registration order preserves their transcript order.
	for _, call := range pending {
		text := toolNotStartedText
		name := "ToolNotStartedError"
		code := ToolNotStarted
		intent := &SurfaceIntent{SurfaceOp: SurfaceOp{Kind: SurfaceAppend}}
		if call.started {
			text = toolOutcomeUnknownText
			name = "ToolOutcomeUnknownError"
			code = ToolOutcomeUnknown
			seqs := []int64{call.callSeq}
			intent.SourceEventSeqs = seqs
		}
		message := llm.NewToolResultMessage(llm.ToolCallID(call.callID),
			[]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, true)
		message.ID = llm.MessageID(fmt.Sprintf("interrupted-tool-result-%s-%d", call.callID, seq))
		closers = append(closers, Event{
			Type: EventToolResult, Seq: seq, Time: time,
			Data: mustCloseData(ToolResultData{
				Turn: openTurn, Step: call.step, Message: message,
				Error: &ToolResultError{Name: name, Code: code},
			}),
			SurfaceOp:       &intent.SurfaceOp,
			SourceEventSeqs: intent.SourceEventSeqs,
		})
		seq++
	}
	// Close an open step next — a turn/end while a step is open is an
	// invariant violation, so the step's boundary is synthesized first.
	if openStep >= 0 {
		closers = append(closers, Event{
			Type: EventStepEnd, Seq: seq, Time: time,
			Data: mustCloseData(StepEndData{Turn: openTurn, Step: openStep}),
		})
		seq++
	}
	cause := TurnEndInterrupted
	closers = append(closers, Event{
		Type: EventTurnEnd, Seq: seq, Time: time,
		Data: mustCloseData(TurnEndData{Turn: openTurn, Reason: TurnEndReason{Kind: cause}}),
	})
	return closers
}

func mustCloseData(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("session: repair closer failed to marshal: %v", err))
	}
	return encoded
}
