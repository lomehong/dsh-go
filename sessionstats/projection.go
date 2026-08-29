package sessionstats

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// The sessionStats projection unit: a pure fold of step boundaries, stream
// chunks, tool pairs, and assembled assistant messages into whole-log
// counts and wall times.
//
// step/end — not assistant/message — is the counted step event because it is
// the step lifecycle authority: the loop appends exactly one per entered
// step, in a finally, so completed, failed, cancelled, and max-tokens steps
// all land one. Counting assembled assistant messages instead would
// overcount max-tokens usage-host messages (empty content, excluded from the
// surface) and undercount cancelled steps (aborted before the message
// assembles).
//
// The wall-time folds mirror the client window fold field by field: model
// time is step/start → assistant/message, first token is the first non-empty
// delta chunk and survives an in-step llm/retry, decode spans first token →
// assembled message on steps that also report output tokens, and tool time
// pairs tool/call → tool/result by callId. A cancelled step assembles no
// message, so its partial stream time stays uncounted in every time figure.

// openStep carries the in-flight step's boundary facts. Nil outside a step
// or after its message assembled.
type openStep struct {
	Turn           int64  `json:"turn"`
	Step           int64  `json:"step"`
	StartTime      int64  `json:"startTime"`
	FirstTokenTime *int64 `json:"firstTokenTime"`
}

// State is the fold state: the totals plus the in-flight boundaries they
// accrue from. Turn numbers are host-assigned and monotonic per session, so
// a single lastTurn slot decides "first closed step of a new turn"; the
// state is plain JSON per the unit contract (persisted-cache precondition).
type State struct {
	Turns        int64            `json:"turns"`
	Steps        int64            `json:"steps"`
	LlmMs        int64            `json:"llmMs"`
	ToolMs       int64            `json:"toolMs"`
	TtftMs       int64            `json:"ttftMs"`
	TtftSteps    int64            `json:"ttftSteps"`
	DecodeMs     int64            `json:"decodeMs"`
	DecodeTokens int64            `json:"decodeTokens"`
	LastTurn     *int64           `json:"lastTurn"`
	OpenStep     *openStep        `json:"openStep"`
	PendingCalls map[string]int64 `json:"pendingCalls"`
}

// SessionStatsProjection is the unit registered on a projection registry.
var SessionStatsProjection = projection.Unit[*State]{
	Key:          ProjectionKey,
	StateVersion: 1,
	Init: func(session.SessionHeader) *State {
		return &State{PendingCalls: map[string]int64{}}
	},
	Apply: func(current *State, event session.Event) (*State, bool) {
		// Every uninteresting event reports no change (zero downstream
		// work: the registry passes the previous state through).
		switch event.Type {
		case session.EventStepStart:
			start := decode[session.StepStartData](event)
			return &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs: current.LlmMs, ToolMs: current.ToolMs,
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				OpenStep:     &openStep{Turn: start.Turn, Step: start.Step, StartTime: event.Time},
				PendingCalls: copyPending(current.PendingCalls),
			}, true
		case session.EventAssistantChunk:
			chunked := decode[chunkEnvelope](event)
			open := current.OpenStep
			if open == nil || open.Turn != chunked.Turn || open.Step != chunked.Step {
				return current, false
			}
			if open.FirstTokenTime != nil || !isTokenDelta(chunked.Chunk) {
				return current, false
			}
			first := event.Time
			return &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs: current.LlmMs, ToolMs: current.ToolMs,
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				OpenStep:     &openStep{Turn: open.Turn, Step: open.Step, StartTime: open.StartTime, FirstTokenTime: &first},
				PendingCalls: copyPending(current.PendingCalls),
			}, true
		case session.EventAssistantMsg:
			message := decode[session.AssistantMessageData](event)
			open := current.OpenStep
			if open == nil || open.Turn != message.Turn || open.Step != message.Step {
				return current, false
			}
			// One assembled message per step: closing the boundary means a
			// defensive duplicate cannot accrue twice.
			next := &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs:        current.LlmMs + maxInt64(0, event.Time-open.StartTime),
				ToolMs:       current.ToolMs,
				TtftMs:       current.TtftMs,
				TtftSteps:    current.TtftSteps,
				DecodeMs:     current.DecodeMs,
				DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				PendingCalls: copyPending(current.PendingCalls),
			}
			if open.FirstTokenTime != nil {
				next.TtftMs += maxInt64(0, *open.FirstTokenTime-open.StartTime)
				next.TtftSteps++
				if outputTokens := usageOutputTokens(message.Usage); outputTokens != nil {
					next.DecodeMs += maxInt64(0, event.Time-*open.FirstTokenTime)
					next.DecodeTokens += *outputTokens
				}
			}
			return next, true
		case session.EventToolCall:
			call := decode[session.ToolCallData](event)
			pending := copyPending(current.PendingCalls)
			pending[string(call.CallID)] = event.Time
			return &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs: current.LlmMs, ToolMs: current.ToolMs,
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				OpenStep:     current.OpenStep,
				PendingCalls: pending,
			}, true
		case session.EventToolResult:
			result := decode[session.ToolResultData](event)
			callID := string(result.Message.Source.CallID)
			// Own-key check: callId is provider-minted (the model/tool JSON
			// boundary), so a result with no recorded call reads as
			// unmatched rather than poisoning toolMs.
			dispatched, matched := current.PendingCalls[callID]
			if !matched {
				return current, false
			}
			pending := copyPending(current.PendingCalls)
			delete(pending, callID)
			return &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs:  current.LlmMs,
				ToolMs: current.ToolMs + maxInt64(0, event.Time-dispatched),
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				OpenStep:     current.OpenStep,
				PendingCalls: pending,
			}, true
		case session.EventStepEnd:
			end := decode[session.StepEndData](event)
			next := &State{
				Turns: current.Turns, Steps: current.Steps + 1,
				LlmMs: current.LlmMs, ToolMs: current.ToolMs,
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     &end.Turn,
				PendingCalls: copyPending(current.PendingCalls),
			}
			if current.LastTurn != nil && *current.LastTurn == end.Turn {
				next.Turns = current.Turns
				next.LastTurn = current.LastTurn
			} else {
				next.Turns = current.Turns + 1
			}
			return next, true
		case session.EventTurnEnd:
			// A call whose result never landed belongs to a cancelled or
			// failed turn; results always land within their turn, so drop
			// the leftovers instead of growing persisted state forever.
			if len(current.PendingCalls) == 0 {
				return current, false
			}
			return &State{
				Turns: current.Turns, Steps: current.Steps,
				LlmMs: current.LlmMs, ToolMs: current.ToolMs,
				TtftMs: current.TtftMs, TtftSteps: current.TtftSteps,
				DecodeMs: current.DecodeMs, DecodeTokens: current.DecodeTokens,
				LastTurn:     current.LastTurn,
				OpenStep:     current.OpenStep,
				PendingCalls: map[string]int64{},
			}, true
		default:
			return current, false
		}
	},
	View: func(folded *State) any {
		return Projection{
			Turns: folded.Turns, Steps: folded.Steps,
			LlmMs: folded.LlmMs, ToolMs: folded.ToolMs,
			TtftMs: folded.TtftMs, TtftSteps: folded.TtftSteps,
			DecodeMs: folded.DecodeMs, DecodeTokens: folded.DecodeTokens,
		}
	},
	DecodeState: decodeState,
}

// chunkEnvelope is the assistant/chunk payload shape.
type chunkEnvelope struct {
	Turn  int64           `json:"turn"`
	Step  int64           `json:"step"`
	Chunk llm.StreamChunk `json:"chunk"`
}

// isTokenDelta reports whether a stream chunk carries a non-empty
// first-token delta.
func isTokenDelta(chunk llm.StreamChunk) bool {
	switch chunk.Type {
	case llm.ChunkTextDelta, llm.ChunkReasoningDelta:
		return chunk.Text != ""
	case llm.ChunkToolCallDelta:
		return chunk.ArgumentsDelta != "" || chunk.Name != ""
	default:
		return false
	}
}

// usageOutputTokens reads provider-reported completion tokens, guarded the
// way the window fold guards node usage: nil when unreported or invalid.
func usageOutputTokens(usage *llm.TokenUsage) *int64 {
	if usage == nil || usage.OutputTokens < 0 {
		return nil
	}
	tokens := usage.OutputTokens
	return &tokens
}

// decode decodes one known-type event payload. Known types are
// schema-gated at append and repair; a decode failure here means a
// corrupted log and fails closed.
func decode[T any](event session.Event) T {
	var decoded T
	if err := json.Unmarshal(event.Data, &decoded); err != nil {
		panic(fmt.Sprintf("sessionstats: event seq %d (%s) payload: %v", event.Seq, event.Type, err))
	}
	return decoded
}

// copyPending clones the pending-call table so folds stay immutable.
func copyPending(pending map[string]int64) map[string]int64 {
	cloned := make(map[string]int64, len(pending)+1)
	for callID, at := range pending {
		cloned[callID] = at
	}
	return cloned
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// decodeState validates and reifies a persisted state row after its
// version gate (strict: unknown fields reject, matching the official
// .strict() state schema).
func decodeState(raw json.RawMessage) (*State, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("trailing data after sessionStats state")
	}
	for _, violation := range state.violations() {
		return nil, fmt.Errorf("sessionStats state: %s", violation)
	}
	if state.PendingCalls == nil {
		state.PendingCalls = map[string]int64{}
	}
	return &state, nil
}

// violations names every shape violation in a decoded state.
func (s *State) violations() []string {
	var violations []string
	nonNegative := func(name string, value int64) {
		if value < 0 {
			violations = append(violations, fmt.Sprintf("%s must be a non-negative integer", name))
		}
	}
	nonNegative("turns", s.Turns)
	nonNegative("steps", s.Steps)
	nonNegative("llmMs", s.LlmMs)
	nonNegative("toolMs", s.ToolMs)
	nonNegative("ttftMs", s.TtftMs)
	nonNegative("ttftSteps", s.TtftSteps)
	nonNegative("decodeMs", s.DecodeMs)
	nonNegative("decodeTokens", s.DecodeTokens)
	if s.LastTurn != nil {
		nonNegative("lastTurn", *s.LastTurn)
	}
	if s.OpenStep != nil {
		nonNegative("openStep.turn", s.OpenStep.Turn)
		nonNegative("openStep.step", s.OpenStep.Step)
		nonNegative("openStep.startTime", s.OpenStep.StartTime)
		if s.OpenStep.FirstTokenTime != nil {
			nonNegative("openStep.firstTokenTime", *s.OpenStep.FirstTokenTime)
		}
	}
	for callID, at := range s.PendingCalls {
		if at < 0 {
			violations = append(violations, fmt.Sprintf("pendingCalls[%q] must be a non-negative integer", callID))
		}
	}
	return violations
}
