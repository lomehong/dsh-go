package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssemblerTextDeltasWithoutBoundaries(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "hel"})
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "lo"})

	blocks := a.Blocks()
	if len(blocks) != 1 || blocks[0].Type != BlockText || blocks[0].Text != "hello" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if a.Finish().Kind != FinishStop {
		t.Fatalf("default finish = %+v", a.Finish())
	}
}

func TestAssemblerBlockEndIsAuthoritativeAndFrozen(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "stale partial"})
	a.Push(StreamChunk{Type: ChunkBlockEnd, Index: 0, Block: &ContentBlock{Type: BlockText, Text: "final"}})
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: " straggler"})

	blocks := a.Blocks()
	if len(blocks) != 1 || blocks[0].Text != "final" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestAssemblerToolCallIDFallback(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkToolCallDelta, Index: 2, Name: "echo", ArgumentsDelta: `{"x"`})
	a.Push(StreamChunk{Type: ChunkToolCallDelta, Index: 2, ArgumentsDelta: `:1}`})

	blocks := a.Blocks()
	if len(blocks) != 1 || blocks[0].Type != BlockToolCall {
		t.Fatalf("blocks = %+v", blocks)
	}
	if blocks[0].ID != "call-2" || blocks[0].Name != "echo" || blocks[0].Arguments != `{"x":1}` {
		t.Fatalf("tool call block = %+v", blocks[0])
	}
}

func TestAssemblerOrderedBlocks(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkReasoningDelta, Index: 0, Text: "thinking"})
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 1, Text: "answer"})
	a.Push(StreamChunk{Type: ChunkBlockStart, Index: 2, BlockType: BlockToolCall})
	a.Push(StreamChunk{Type: ChunkToolCallDelta, Index: 2, ID: "c1", Name: "t", ArgumentsDelta: "{}"})
	a.Push(StreamChunk{Type: ChunkBlockEnd, Index: 2, Block: &ContentBlock{Type: BlockToolCall, ID: "c1", Name: "t", Arguments: "{}"}})

	blocks := a.Blocks()
	wantTypes := []string{BlockReasoning, BlockText, BlockToolCall}
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v", blocks)
	}
	for index, block := range blocks {
		if block.Type != wantTypes[index] {
			t.Fatalf("block %d type = %q, want %q", index, block.Type, wantTypes[index])
		}
	}
}

func TestAssemblerUsageAndFinishAndReplay(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "hi"})
	a.Push(StreamChunk{Type: ChunkUsage, Usage: &TokenUsage{InputTokens: 3, OutputTokens: 5}})
	envelope := &ReplayEnvelope{Blocks: []json.RawMessage{json.RawMessage(`{"t":1}`)}}
	a.Push(StreamChunk{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishStop}, ReplayState: envelope})

	if usage := a.Usage(); usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	message := a.Message("deepseek", "deepseek-chat")
	if message.Source.Provider != "deepseek" || message.Source.Model != "deepseek-chat" || message.Source.Kind != SourceModel {
		t.Fatalf("source = %+v", message.Source)
	}
	if message.Source.ReplayState == nil {
		t.Fatalf("replay state missing from source")
	}
	if a.ReplayState() == nil {
		t.Fatalf("ReplayState missing")
	}
}

func TestAssemblerMaxTokensDropsToolCallsAndPrunesReplay(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "partial "})
	a.Push(StreamChunk{Type: ChunkBlockStart, Index: 1, BlockType: BlockToolCall})
	a.Push(StreamChunk{Type: ChunkToolCallDelta, Index: 1, ID: "c1", Name: "t", ArgumentsDelta: "{}"})
	a.Push(StreamChunk{Type: ChunkUsage, Usage: &TokenUsage{}})
	a.Push(StreamChunk{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishMaxTokens}, ReplayState: &ReplayEnvelope{
		Blocks: []json.RawMessage{json.RawMessage(`{"b":0}`), json.RawMessage(`{"b":1}`)},
	}})

	blocks := a.Blocks()
	if len(blocks) != 1 || blocks[0].Type != BlockText {
		t.Fatalf("blocks = %+v, want text only", blocks)
	}
	replay := a.ReplayState()
	if replay == nil || len(replay.Blocks) != 1 || string(replay.Blocks[0]) != `{"b":0}` {
		t.Fatalf("replay = %+v", replay)
	}
	message := a.Message("p", "m")
	if len(message.Content) != 1 {
		t.Fatalf("message content = %+v", message.Content)
	}
	if message.Source.ReplayState == nil {
		t.Fatalf("pruned replay envelope missing from source")
	}
}

func TestAssemblerReplayLengthMismatchDropsEnvelope(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "x"})
	a.Push(StreamChunk{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishStop}, ReplayState: &ReplayEnvelope{
		Blocks: []json.RawMessage{json.RawMessage(`{"b":0}`), json.RawMessage(`{"b":1}`)},
	}})
	if replay := a.ReplayState(); replay != nil {
		t.Fatalf("misaligned envelope kept: %+v", replay)
	}
}

func TestAssemblerInterruptedBlocks(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 0, Text: "kept words"})
	a.Push(StreamChunk{Type: ChunkTextDelta, Index: 1, Text: "   "})
	a.Push(StreamChunk{Type: ChunkBlockStart, Index: 2, BlockType: BlockToolCall})
	a.Push(StreamChunk{Type: ChunkToolCallDelta, Index: 2, ID: "c1", Name: "t", ArgumentsDelta: "{}"})

	interrupted := a.InterruptedBlocks()
	if len(interrupted) != 1 || interrupted[0].Text != "kept words" {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	if !strings.Contains(interrupted[0].Text, "kept") {
		t.Fatalf("unexpected interrupted content")
	}

	// Blocks() still assembles everything (including the open tool call) for
	// the non-interrupted path.
	if len(a.Blocks()) != 3 {
		t.Fatalf("blocks = %+v", a.Blocks())
	}
}

func TestAssemblerUnknownOpenBlockInterruptedOmitted(t *testing.T) {
	a := NewBlockAssembler()
	a.Push(StreamChunk{Type: ChunkBlockStart, Index: 0, BlockType: "image"})
	// InterruptedBlocks skips unknown open blocks instead of panicking.
	if blocks := a.InterruptedBlocks(); len(blocks) != 0 {
		t.Fatalf("interrupted = %+v", blocks)
	}
}
