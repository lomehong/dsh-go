package session

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"dshgo/llm"
)

func chunkEvent(seq, time int64, turn, step, index int64, chunk map[string]any) Event {
	chunk["index"] = index
	data, err := json.Marshal(map[string]any{"turn": turn, "step": step, "chunk": chunk})
	if err != nil {
		panic(err)
	}
	return Event{Type: EventAssistantChunk, Seq: seq, Time: time, Data: data}
}

func TestSeqRangesRoundTrip(t *testing.T) {
	encoded := EncodeSeqRanges([]int64{5, 6, 7, 9, 11, 12})
	// Runs of three or more become pairs; [11,12] is only two, so it stays
	// verbatim.
	want := []EncodedSeq{[]int64{5, 7}, int64(9), int64(11), int64(12)}
	if !reflect.DeepEqual(encoded, want) {
		t.Fatalf("encode wrong: %#v", encoded)
	}
	decoded, err := DecodeSeqRanges(anySlice(encoded), 12)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !reflect.DeepEqual(decoded, []int64{5, 6, 7, 9, 11, 12}) {
		t.Fatalf("roundtrip wrong: %v", decoded)
	}
	// Non-increasing lists stay verbatim.
	verbatim := EncodeSeqRanges([]int64{9, 5, 5})
	if !reflect.DeepEqual(verbatim, []EncodedSeq{int64(9), int64(5), int64(5)}) {
		t.Fatalf("verbatim pass-through wrong: %#v", verbatim)
	}
	// A range may not cite the owning event or later seqs.
	if _, err := DecodeSeqRanges(anySlice([]EncodedSeq{[]int64{0, 5}}), 5); err == nil {
		t.Fatal("provenance must stay below its event seq")
	}
	if _, err := DecodeSeqRanges(anySlice([]EncodedSeq{[]int64{5, 0}}), 100); err == nil {
		t.Fatal("an inverted range must be refused")
	}
	if _, err := DecodeSeqRanges(anySlice([]EncodedSeq{[]int64{0}, 0}), 100); err == nil {
		t.Fatal("duplicates across entries must be refused")
	}
}

func anySlice(values []EncodedSeq) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

func TestPackChunkRunsPacksUniformDeltaRuns(t *testing.T) {
	textDelta := func(seq, time int64, text string) Event {
		return chunkEvent(seq, time, 1, 1, 0, map[string]any{"type": "text-delta", "text": text})
	}
	events := []Event{
		chunkEvent(0, 100, 1, 1, 0, map[string]any{"type": "block-start", "kind": "text"}),
		textDelta(1, 101, "He"),
		textDelta(2, 103, "llo"),
		textDelta(3, 106, "!"),
		textDelta(4, 110, "!"),
	}
	packed := PackChunkRuns(events)
	if len(packed) != 2 {
		t.Fatalf("block-start stays verbatim and the run packs into one row, got %d records", len(packed))
	}
	if packed[1]["type"] != RowTextChunks {
		t.Fatalf("row tag wrong: %#v", packed[1])
	}
	expanded, err := DecodeStorageRecord(packed[1])
	if err != nil {
		t.Fatalf("decode row failed: %v", err)
	}
	if !reflect.DeepEqual(expanded, events[1:]) {
		t.Fatalf("row expansion must restore exact events:\n got %#v\nwant %#v", expanded, events[1:])
	}
	// Below the minimum run length events stay verbatim.
	if packed := PackChunkRuns([]Event{textDelta(0, 1, "a"), textDelta(1, 2, "b")}); len(packed) != 2 {
		t.Fatalf("short runs must stay one event per line, got %d", len(packed))
	}
}

func TestPackChunkRunsToolCallIdentityAndBoundaries(t *testing.T) {
	call := func(seq int64, name string, withName bool) Event {
		chunk := map[string]any{"type": "tool-call-delta", "id": "call-1", "argumentsDelta": "{\"x\""}
		if withName {
			chunk["name"] = name
		}
		return chunkEvent(seq, 10*seq, 1, 1, 2, chunk)
	}
	// A mixed run (name present then absent) is not representable: it splits.
	packed := PackChunkRuns([]Event{call(0, "f", true), call(1, "f", true), call(2, "f", true), call(3, "f", false), call(4, "f", false), call(5, "f", false)})
	if len(packed) != 2 || packed[0]["type"] != RowToolCallChunks || packed[1]["type"] != RowToolCallChunks {
		t.Fatalf("mixed runs must split into two rows, got %#v", packed)
	}
	first := packed[0]["data"].(map[string]any)
	if _, has := first["name"]; !has {
		t.Fatal("a run whose members all carry name must keep it")
	}
	// A run whose members differ in name VALUE also splits; with six members
	// each side packs independently.
	packed = PackChunkRuns([]Event{
		call(0, "f", true), call(1, "f", true), call(2, "f", true),
		call(3, "g", true), call(4, "g", true), call(5, "g", true),
	})
	if len(packed) != 2 {
		t.Fatalf("differing name values must split the run, got %d records", len(packed))
	}
	firstRow := packed[0]["data"].(map[string]any)
	secondRow := packed[1]["data"].(map[string]any)
	if firstRow["name"] != "f" || secondRow["name"] != "g" {
		t.Fatalf("each row keeps its own uniform name: %#v vs %#v", firstRow["name"], secondRow["name"])
	}
}

func TestEventLinesRangeEncodesProvenance(t *testing.T) {
	message := Event{
		Type: EventAssistantMsg, Seq: 9, Time: 50,
		Data:            json.RawMessage(`{"turn":1,"step":2,"message":{"id":"m1","role":"assistant","source":{"kind":"model","provider":"deepseek","model":"chat"},"content":[{"type":"text","text":"hi"}]}}`),
		SourceEventSeqs: []int64{2, 3, 4, 6},
		SurfaceOp:       &SurfaceOp{Kind: SurfaceAppend},
	}
	lines, err := EventLines([]Event{message}, true)
	if err != nil {
		t.Fatalf("serialize failed: %v", err)
	}
	var parsed map[string]any
	decoder := json.NewDecoder(bytes.NewReader(lines))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	provenance, ok := parsed["sourceEventSeqs"].([]any)
	if !ok || len(provenance) != 2 {
		t.Fatalf("provenance must range-encode to two entries, got %#v", parsed["sourceEventSeqs"])
	}
	if start, ok := provenance[0].([]any); !ok || start[0].(json.Number).String() != "2" || start[1].(json.Number).String() != "4" {
		t.Fatalf("first range wrong: %#v", provenance[0])
	}
}

func TestInterruptedTurnClosersRepairCrashTails(t *testing.T) {
	balanced := []Event{
		{Type: EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: EventTurnEnd, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	if closers := InterruptedTurnClosers(balanced); closers != nil {
		t.Fatalf("a balanced log needs no closers, got %#v", closers)
	}

	// A started call whose result never landed: unknown outcome, citing the
	// tool/call seq.
	assistant := AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{
			Type: llm.BlockToolCall, ToolCallID: "call-1", Name: "read",
			Arguments: "{}",
		}}, "deepseek", "chat", nil),
	}
	toolCall := Event{Type: EventToolCall, Seq: 3, Time: 5, Data: json.RawMessage(`{"turn":1,"step":1,"callId":"call-1","name":"read","arguments":"{}"}`)}
	assistantEvent := Event{Type: EventAssistantMsg, Seq: 2, Time: 4, Data: mustMarshalT(t, assistant)}
	open := []Event{
		{Type: EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: EventStepStart, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"step":1}`)},
		assistantEvent,
		toolCall,
	}
	closers := InterruptedTurnClosers(open)
	if len(closers) != 3 {
		t.Fatalf("started-call tail needs result + step/end + turn/end, got %d", len(closers))
	}
	result := closers[0]
	if result.Type != EventToolResult || result.Seq != 4 || result.Time != 5 {
		t.Fatalf("synthetic result wrong: %#v", result)
	}
	var resultData ToolResultData
	if err := json.Unmarshal(result.Data, &resultData); err != nil {
		t.Fatalf("synthetic result payload: %v", err)
	}
	if resultData.Error == nil || resultData.Error.Code != ToolOutcomeUnknown {
		t.Fatalf("started call must report %s, got %#v", ToolOutcomeUnknown, resultData.Error)
	}
	if !reflect.DeepEqual(result.SourceEventSeqs, []int64{3}) {
		t.Fatalf("synthetic result must cite the tool/call seq, got %v", result.SourceEventSeqs)
	}
	if closers[1].Type != EventStepEnd || closers[2].Type != EventTurnEnd {
		t.Fatalf("boundaries must close step before turn: %#v", closers[1:])
	}
	if closers[2].Seq != 6 {
		t.Fatalf("sequences must continue the log, got %d", closers[2].Seq)
	}

	// A call never recorded as started: TOOL_NOT_STARTED without provenance.
	unstarted := []Event{open[0], open[1], open[2]}
	closers = InterruptedTurnClosers(unstarted)
	var unstartedData ToolResultData
	_ = json.Unmarshal(closers[0].Data, &unstartedData)
	if unstartedData.Error == nil || unstartedData.Error.Code != ToolNotStarted {
		t.Fatalf("unstarted call must report %s, got %#v", ToolNotStarted, unstartedData.Error)
	}
	if unstartedData.Message.Content[0].Content[0].Text != toolNotStartedText {
		t.Fatalf("model-facing text must be the pinned NOT_STARTED string, got %q", unstartedData.Message.Content[0].Content[0].Text)
	}
	if closers[0].SourceEventSeqs != nil {
		t.Fatalf("an unstarted call cites no source events, got %v", closers[0].SourceEventSeqs)
	}
}

func mustMarshalT(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	return encoded
}
