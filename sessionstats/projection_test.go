package sessionstats

import (
	"encoding/json"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func payload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return raw
}

func statsEvent(eventType string, seq int64, at int64, data json.RawMessage) session.Event {
	return session.Event{Type: eventType, Seq: seq, Time: at, Data: data}
}

func foldEvents(t *testing.T, events []session.Event) *State {
	t.Helper()
	state := SessionStatsProjection.Init(session.SessionHeader{})
	for _, event := range events {
		state, _ = SessionStatsProjection.Apply(state, event)
	}
	return state
}

func TestSessionStatsHappyPathFold(t *testing.T) {
	state := foldEvents(t, []session.Event{
		statsEvent(session.EventTurnStart, 0, 90, payload(t, session.TurnStartData{Turn: 1})),
		statsEvent(session.EventStepStart, 1, 100, payload(t, session.StepStartData{Turn: 1, Step: 1})),
		statsEvent(session.EventAssistantChunk, 2, 120, payload(t, chunkEnvelope{Turn: 1, Step: 1, Chunk: llm.StreamChunk{Type: llm.ChunkTextDelta, Text: "hel"}})),
		// An empty delta is not a first token.
		statsEvent(session.EventAssistantChunk, 3, 121, payload(t, chunkEnvelope{Turn: 1, Step: 1, Chunk: llm.StreamChunk{Type: llm.ChunkTextDelta, Text: ""}})),
		statsEvent(session.EventAssistantChunk, 4, 122, payload(t, chunkEnvelope{Turn: 1, Step: 1, Chunk: llm.StreamChunk{Type: llm.ChunkToolCallDelta, Name: "echo"}})),
		statsEvent(session.EventAssistantMsg, 5, 200, payload(t, session.AssistantMessageData{Turn: 1, Step: 1, Usage: &llm.TokenUsage{OutputTokens: 50}})),
		statsEvent(session.EventToolCall, 6, 300, payload(t, session.ToolCallData{Turn: 1, Step: 1, CallID: "c1", Name: "echo"})),
		statsEvent(session.EventToolResult, 7, 350, payload(t, session.ToolResultData{Turn: 1, Step: 1, Message: llm.Message{Source: llm.MessageSource{CallID: "c1"}}})),
		statsEvent(session.EventStepEnd, 8, 360, payload(t, session.StepEndData{Turn: 1, Step: 1})),
	})
	view := SessionStatsProjection.View(state).(Projection)
	if view.Turns != 1 || view.Steps != 1 {
		t.Fatalf("counts = %+v", view)
	}
	if view.LlmMs != 100 || view.TtftMs != 20 || view.TtftSteps != 1 || view.DecodeMs != 80 || view.DecodeTokens != 50 {
		t.Fatalf("times = %+v", view)
	}
	if view.ToolMs != 50 {
		t.Fatalf("toolMs = %d", view.ToolMs)
	}
	if len(state.PendingCalls) != 0 || state.OpenStep != nil {
		t.Fatalf("leftovers = %+v", state)
	}
}

func TestSessionStatsUninterestingEventsPreserveReference(t *testing.T) {
	state := foldEvents(t, []session.Event{
		statsEvent(session.EventStepStart, 1, 100, payload(t, session.StepStartData{Turn: 1, Step: 1})),
	})
	foreign := []session.Event{
		statsEvent(session.EventUserMessage, 2, 110, payload(t, llm.Message{})),
		// Chunk for a different step.
		statsEvent(session.EventAssistantChunk, 3, 112, payload(t, chunkEnvelope{Turn: 1, Step: 9, Chunk: llm.StreamChunk{Type: llm.ChunkTextDelta, Text: "x"}})),
		// A usage chunk is not a token delta.
		statsEvent(session.EventAssistantChunk, 4, 113, payload(t, chunkEnvelope{Turn: 1, Step: 1, Chunk: llm.StreamChunk{Type: llm.ChunkUsage}})),
		statsEvent(session.EventToolResult, 5, 114, payload(t, session.ToolResultData{Message: llm.Message{Source: llm.MessageSource{CallID: "ghost"}}})),
		statsEvent(session.EventTurnEnd, 6, 115, payload(t, session.TurnEndData{Turn: 1})),
	}
	for _, event := range foreign {
		if _, changed := SessionStatsProjection.Apply(state, event); changed {
			t.Fatalf("%s must preserve the state reference", event.Type)
		}
	}
}

func TestSessionStatsCancelledStepUncounted(t *testing.T) {
	state := foldEvents(t, []session.Event{
		statsEvent(session.EventStepStart, 1, 100, payload(t, session.StepStartData{Turn: 3, Step: 1})),
		statsEvent(session.EventAssistantChunk, 2, 120, payload(t, chunkEnvelope{Turn: 3, Step: 1, Chunk: llm.StreamChunk{Type: llm.ChunkReasoningDelta, Text: "thinking"}})),
		// The step never assembles a message; step/end still lands.
		statsEvent(session.EventStepEnd, 3, 200, payload(t, session.StepEndData{Turn: 3, Step: 1})),
	})
	view := SessionStatsProjection.View(state).(Projection)
	if view.Turns != 1 || view.Steps != 1 {
		t.Fatalf("counts = %+v", view)
	}
	if view.LlmMs != 0 || view.TtftMs != 0 || view.TtftSteps != 0 {
		t.Fatalf("cancelled step streamed time into the fold: %+v", view)
	}
}

func TestSessionStatsTurnDedupAndPendingDrop(t *testing.T) {
	state := foldEvents(t, []session.Event{
		statsEvent(session.EventStepStart, 1, 100, payload(t, session.StepStartData{Turn: 5, Step: 1})),
		statsEvent(session.EventStepEnd, 2, 110, payload(t, session.StepEndData{Turn: 5, Step: 1})),
		statsEvent(session.EventStepStart, 3, 120, payload(t, session.StepStartData{Turn: 5, Step: 2})),
		statsEvent(session.EventStepEnd, 4, 130, payload(t, session.StepEndData{Turn: 5, Step: 2})),
		// A call whose result never landed is dropped at turn/end.
		statsEvent(session.EventToolCall, 5, 140, payload(t, session.ToolCallData{Turn: 5, Step: 2, CallID: "leftover"})),
		statsEvent(session.EventTurnEnd, 6, 150, payload(t, session.TurnEndData{Turn: 5})),
	})
	if state.Turns != 1 || state.Steps != 2 {
		t.Fatalf("turn dedup = %+v", state)
	}
	if len(state.PendingCalls) != 0 {
		t.Fatalf("leftover calls survived turn/end: %+v", state.PendingCalls)
	}
	// A wall-time guard: a message timestamped before its step start adds
	// nothing.
	state = foldEvents(t, []session.Event{
		statsEvent(session.EventStepStart, 1, 500, payload(t, session.StepStartData{Turn: 1, Step: 1})),
		statsEvent(session.EventAssistantMsg, 2, 400, payload(t, session.AssistantMessageData{Turn: 1, Step: 1})),
	})
	if state.LlmMs != 0 {
		t.Fatalf("negative window clamped to %d", state.LlmMs)
	}
}

func TestSessionStatsDecodeState(t *testing.T) {
	if _, err := DecodeStateForTest(t, `{"turns":1,"steps":2,"llmMs":30,"toolMs":0,"ttftMs":10,"ttftSteps":1,"decodeMs":20,"decodeTokens":9,"lastTurn":3,"openStep":null,"pendingCalls":{}}`); err != nil {
		t.Fatalf("valid row rejected: %v", err)
	}
	if _, err := DecodeStateForTest(t, `{"turns":1,"steps":2,"llmMs":30,"toolMs":0,"ttftMs":10,"ttftSteps":1,"decodeMs":20,"decodeTokens":9,"lastTurn":3,"openStep":null,"pendingCalls":{},"extra":1}`); err == nil {
		t.Fatal("unknown fields must reject")
	}
	if _, err := DecodeStateForTest(t, `{"turns":-1,"steps":0,"llmMs":0,"toolMs":0,"ttftMs":0,"ttftSteps":0,"decodeMs":0,"decodeTokens":0,"lastTurn":null,"openStep":null,"pendingCalls":{}}`); err == nil {
		t.Fatal("negative totals must reject")
	}
	decoded, err := DecodeStateForTest(t, `{"turns":0,"steps":0,"llmMs":0,"toolMs":0,"ttftMs":0,"ttftSteps":0,"decodeMs":0,"decodeTokens":0,"lastTurn":null,"openStep":{"turn":1,"step":2,"startTime":30,"firstTokenTime":null},"pendingCalls":{}}`)
	if err != nil {
		t.Fatalf("openStep row rejected: %v", err)
	}
	if decoded.OpenStep == nil || decoded.OpenStep.Step != 2 {
		t.Fatalf("openStep = %+v", decoded.OpenStep)
	}
	// A persisted row without a pendingCalls table reifies the empty map.
	state := foldEvents(t, nil)
	if state.PendingCalls == nil {
		t.Fatal("init must seed the pending table")
	}
}
