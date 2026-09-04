package llm

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustPush(t *testing.T, acc *AssistantStreamAccumulator, time int64, chunk StreamChunk) TimedStreamChunk {
	t.Helper()
	timed, err := acc.Push(TimedStreamChunk{Time: time, Chunk: chunk})
	if err != nil {
		t.Fatalf("push %s at %d: %v", chunk.Type, time, err)
	}
	return timed
}

func TestAssistantStreamAccumulatesTextRun(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	mustPush(t, acc, 100, StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "he"})
	mustPush(t, acc, 105, StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "llo"})
	mustPush(t, acc, 120, StreamChunk{Type: ChunkTextDelta, Index: 1, Text: " world"})
	records := acc.Snapshot()
	if len(records) != 2 {
		t.Fatalf("expected 2 records (index change opens a new run), got %d", len(records))
	}
	first := records[0].(*TextRunRecord)
	if first.Type != RecordTextChunks || first.Time0 != 100 || first.Index != 0 {
		t.Fatalf("first run header: %+v", first)
	}
	if !reflect.DeepEqual(first.Dt, []int64{5}) || !reflect.DeepEqual(first.Texts, []string{"he", "llo"}) {
		t.Fatalf("first run members: %+v", first)
	}
	second := records[1].(*TextRunRecord)
	if second.Index != 1 || second.Time0 != 120 || len(second.Dt) != 0 || second.Texts[0] != " world" {
		t.Fatalf("second run members: %+v", second)
	}
	wire, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	// Single-member runs keep dt present as []; index and time0 are explicit.
	if string(wire) != `{"type":"text-chunks","time0":120,"index":1,"dt":[],"texts":[" world"]}` {
		t.Fatalf("run wire shape: %s", wire)
	}
}

func TestAssistantStreamAccumulatesReasoningAndToolRuns(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	mustPush(t, acc, 10, StreamChunk{Type: ChunkReasoningDelta, Index: 0, Text: "think"})
	mustPush(t, acc, 12, StreamChunk{Type: ChunkReasoningDelta, Index: 0, Text: " hard"})
	mustPush(t, acc, 20, StreamChunk{Type: ChunkToolCallDelta, Index: 1, ID: "call_1", Name: "read", ArgumentsDelta: `{"pa`})
	mustPush(t, acc, 24, StreamChunk{Type: ChunkToolCallDelta, Index: 1, ID: "call_1", Name: "read", ArgumentsDelta: `th":1}`})
	mustPush(t, acc, 30, StreamChunk{Type: ChunkToolCallDelta, Index: 2, ID: "call_2", ArgumentsDelta: "{}"})
	records := acc.Snapshot()
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}
	tool := records[1].(*ToolCallRunRecord)
	if tool.ID != "call_1" || tool.Name != "read" || !reflect.DeepEqual(tool.Args, []string{`{"pa`, `th":1}`}) {
		t.Fatalf("tool run: %+v", tool)
	}
	nameless := records[2].(*ToolCallRunRecord)
	if nameless.Name != "" {
		t.Fatalf("nameless run must omit name: %+v", nameless)
	}
	wire, _ := json.Marshal(nameless)
	if string(wire) != `{"type":"tool-call-chunks","time0":30,"index":2,"dt":[],"id":"call_2","args":["{}"]}` {
		t.Fatalf("nameless tool run wire: %s", wire)
	}
}

func TestAssistantStreamRawChunksStayRaw(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	mustPush(t, acc, 1, StreamChunk{Type: ChunkBlockStart, Index: 0, BlockType: BlockText})
	mustPush(t, acc, 2, StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "a"})
	mustPush(t, acc, 3, StreamChunk{Type: ChunkBlockEnd, Index: 0, Block: &ContentBlock{Type: BlockText, Text: "a"}})
	mustPush(t, acc, 4, StreamChunk{Type: ChunkUsage, Usage: &TokenUsage{InputTokens: 1, OutputTokens: 2}})
	mustPush(t, acc, 5, StreamChunk{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishStop}})
	records := acc.Snapshot()
	if len(records) != 5 {
		t.Fatalf("expected 4 raw records + 1 run, got %d", len(records))
	}
	raw := records[3].(*RawChunkRecord)
	if raw.Time != 4 {
		t.Fatalf("raw usage record time: %+v", raw)
	}
	var decoded StreamChunk
	if err := json.Unmarshal(raw.Chunk, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != ChunkUsage || decoded.Usage == nil || decoded.Usage.OutputTokens != 2 {
		t.Fatalf("raw usage chunk round-trip: %s", raw.Chunk)
	}
	finish := records[4].(*RawChunkRecord)
	var finishChunk StreamChunk
	if err := json.Unmarshal(finish.Chunk, &finishChunk); err != nil {
		t.Fatal(err)
	}
	if finishChunk.Reason == nil || finishChunk.Reason.Kind != FinishStop {
		t.Fatalf("raw finish round-trip: %s", finish.Chunk)
	}
	wire, _ := json.Marshal(raw)
	if string(wire) != `{"type":"chunk","time":4,"chunk":{"type":"usage","usage":{"inputTokens":1,"outputTokens":2}}}` {
		t.Fatalf("raw record wire: %s", wire)
	}
}

func TestAssistantStreamEmptyIDToolDeltaStaysRaw(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	mustPush(t, acc, 1, StreamChunk{Type: ChunkToolCallDelta, Index: 0, ArgumentsDelta: "{}"})
	records := acc.Snapshot()
	if len(records) != 1 {
		t.Fatalf("expected 1 raw record, got %d", len(records))
	}
	if _, ok := records[0].(*RawChunkRecord); !ok {
		t.Fatalf("empty-id tool delta must stay raw, got %T", records[0])
	}
}

func TestAssistantStreamSnapshotIsDetached(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	mustPush(t, acc, 1, StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "a"})
	first := acc.Snapshot()[0].(*TextRunRecord)
	first.Texts[0] = "mutated"
	again := acc.Snapshot()[0].(*TextRunRecord)
	if again.Texts[0] != "a" {
		t.Fatalf("snapshot must be detached from later mutation: %+v", again)
	}
}

func TestExpandAssistantStreamRoundTrip(t *testing.T) {
	acc := &AssistantStreamAccumulator{}
	input := []TimedStreamChunk{
		{Time: 100, Chunk: StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "he"}},
		{Time: 107, Chunk: StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "llo"}},
		{Time: 110, Chunk: StreamChunk{Type: ChunkToolCallDelta, Index: 1, ID: "c1", Name: "edit", ArgumentsDelta: "{"}},
		{Time: 113, Chunk: StreamChunk{Type: ChunkToolCallDelta, Index: 1, ID: "c1", Name: "edit", ArgumentsDelta: "}"}},
		{Time: 120, Chunk: StreamChunk{Type: ChunkUsage, Usage: &TokenUsage{InputTokens: 9, OutputTokens: 1}}},
	}
	for _, value := range input {
		mustPush(t, acc, value.Time, value.Chunk)
	}
	expanded, err := ExpandAssistantStream(acc.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != len(input) {
		t.Fatalf("expected %d timed chunks, got %d", len(input), len(expanded))
	}
	for i, want := range input {
		got := expanded[i]
		if got.Time != want.Time {
			t.Fatalf("chunk %d time: got %d want %d", i, got.Time, want.Time)
		}
		gotJSON, _ := json.Marshal(got.Chunk)
		wantJSON, _ := json.Marshal(want.Chunk)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("chunk %d: got %s want %s", i, gotJSON, wantJSON)
		}
	}
}

func TestParseAssistantStreamStrictValidation(t *testing.T) {
	valid := []AssistantStreamRecord{
		&TextRunRecord{Type: RecordTextChunks, Time0: 1, Index: 0, Dt: []int64{2}, Texts: []string{"a", "b"}},
		&RawChunkRecord{Type: RecordRawChunk, Time: 9, Chunk: json.RawMessage(`{"type":"finish","reason":{"kind":"stop"}}`)},
	}
	wire, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAssistantStream(wire)
	if err != nil {
		t.Fatalf("valid stream refused: %v", err)
	}
	expanded, err := ExpandAssistantStream(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 3 {
		t.Fatalf("expand produced %d chunks, want 3", len(expanded))
	}

	cases := map[string]string{
		"unknown record type":  `[{"type":"mystery"}]`,
		"extra key":            `[{"type":"text-chunks","time0":1,"index":0,"dt":[],"texts":["a"],"extra":1}]`,
		"missing key":          `[{"type":"text-chunks","time0":1,"index":0,"dt":[]}]`,
		"empty texts":          `[{"type":"text-chunks","time0":1,"index":0,"dt":[],"texts":[]}]`,
		"dt length mismatch":   `[{"type":"text-chunks","time0":1,"index":0,"dt":[1,2],"texts":["a","b"]}]`,
		"negative index":       `[{"type":"text-chunks","time0":1,"index":-1,"dt":[],"texts":["a"]}]`,
		"empty tool id":        `[{"type":"tool-call-chunks","time0":1,"index":0,"dt":[],"id":"","args":["{}"]}]`,
		"empty tool name":      `[{"type":"tool-call-chunks","time0":1,"index":0,"dt":[],"id":"c","name":"","args":["{}"]}]`,
		"empty args":           `[{"type":"tool-call-chunks","time0":1,"index":0,"dt":[],"id":"c","args":[]}]`,
		"raw chunk not object": `[{"type":"chunk","time":1,"chunk":"text"}]`,
		"raw chunk extra key":  `[{"type":"chunk","time":1,"chunk":{},"x":1}]`,
		"record not object":    `["text"]`,
		"stream not array":     `{"type":"text-chunks"}`,
	}
	for label, payload := range cases {
		if _, err := ParseAssistantStream(json.RawMessage(payload)); err == nil {
			t.Fatalf("%s: expected refusal, got none", label)
		}
	}
}

func TestStreamChunkWireVariants(t *testing.T) {
	cases := []struct {
		chunk StreamChunk
		want  string
	}{
		{StreamChunk{Type: ChunkBlockStart, Index: 0, BlockType: BlockText},
			`{"type":"block-start","index":0,"blockType":"text"}`},
		{StreamChunk{Type: ChunkTextDelta, Index: 0, Text: ""},
			`{"type":"text-delta","index":0,"text":""}`},
		{StreamChunk{Type: ChunkToolCallDelta, Index: 2, ID: "c", ArgumentsDelta: "{}"},
			`{"type":"tool-call-delta","index":2,"id":"c","argumentsDelta":"{}"}`},
		{StreamChunk{Type: ChunkToolCallDelta, Index: 2, ID: "c", Name: "edit", ArgumentsDelta: "{}"},
			`{"type":"tool-call-delta","index":2,"id":"c","name":"edit","argumentsDelta":"{}"}`},
		{StreamChunk{Type: ChunkUsage, Usage: &TokenUsage{InputTokens: 1}},
			`{"type":"usage","usage":{"inputTokens":1,"outputTokens":0}}`},
		{StreamChunk{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishStop}},
			`{"type":"finish","reason":{"kind":"stop"}}`},
	}
	for _, testCase := range cases {
		wire, err := json.Marshal(testCase.chunk)
		if err != nil {
			t.Fatal(err)
		}
		if string(wire) != testCase.want {
			t.Fatalf("%s wire: got %s want %s", testCase.chunk.Type, wire, testCase.want)
		}
	}
}
