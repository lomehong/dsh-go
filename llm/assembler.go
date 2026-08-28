package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Incremental chunk-to-message assembler: the single canonical assembly
// algorithm the agent loop uses to build an assistant message from a chunk
// stream while logging the raw chunks for replay fidelity.
//
// Port of packages/llm/llm/src/assembler.ts. Go adaptations: partials live in
// insertion-ordered slices instead of a Map plus a key array; deep-freeze is
// moot (Go values are returned by value).
//
// Tolerant of delta-only protocols (no block-start/end); deltas arriving for
// an index already closed by block-end are ignored (malformed stream) so a
// misbehaving adapter cannot grow memory or corrupt a completed block.

// partialBlock is one streamed block's accumulated state.
type partialBlock struct {
	blockType         string
	text              string
	toolCallID        ToolCallID
	hasToolCallID     bool
	toolCallName      string
	toolCallArguments string
	// closed is set by block-end; it is authoritative and freezes the partial.
	block *ContentBlock
}

// BlockAssembler incrementally assembles raw StreamChunks into complete
// ContentBlocks and a final assistant Message.
//
// The agent loop feeds it while logging raw chunks for replay fidelity, then
// reads Blocks/Message/Usage/Finish once the stream ends, or
// InterruptedBlocks when cancellation cut the stream short.
type BlockAssembler struct {
	partials    map[int]*partialBlock
	order       []int
	usage       *TokenUsage
	finish      *FinishReason
	replayState *ReplayEnvelope
}

// NewBlockAssembler returns an empty assembler.
func NewBlockAssembler() *BlockAssembler {
	return &BlockAssembler{partials: map[int]*partialBlock{}}
}

// Push feeds one chunk into the assembly state, in stream order.
func (a *BlockAssembler) Push(chunk StreamChunk) {
	switch chunk.Type {
	case ChunkBlockStart:
		if _, ok := a.partials[chunk.Index]; !ok {
			a.partials[chunk.Index] = &partialBlock{blockType: chunk.BlockType}
			a.order = append(a.order, chunk.Index)
		}
	case ChunkTextDelta, ChunkReasoningDelta:
		blockType := BlockText
		if chunk.Type == ChunkReasoningDelta {
			blockType = BlockReasoning
		}
		partial := a.ensure(chunk.Index, blockType)
		if partial.block != nil {
			return // closed by block-end; ignore stragglers
		}
		partial.text += chunk.Text
	case ChunkToolCallDelta:
		partial := a.ensure(chunk.Index, BlockToolCall)
		if partial.block != nil {
			return // closed by block-end; ignore stragglers
		}
		partial.toolCallID = chunk.ID
		partial.hasToolCallID = chunk.ID != ""
		if chunk.Name != "" {
			partial.toolCallName = chunk.Name
		}
		partial.toolCallArguments += chunk.ArgumentsDelta
	case ChunkBlockEnd:
		partial := a.ensure(chunk.Index, chunk.Block.Type)
		// First close wins; ignoring re-close stragglers keeps streamed
		// output and the final assembled block in agreement.
		if partial.block != nil {
			return
		}
		block := *chunk.Block
		partial.block = &block
	case ChunkUsage:
		usage := *chunk.Usage
		a.usage = &usage
	case ChunkFinish:
		reason := *chunk.Reason
		a.finish = &reason
		a.replayState = chunk.ReplayState
	}
}

func (a *BlockAssembler) ensure(index int, blockType string) *partialBlock {
	partial, ok := a.partials[index]
	if !ok {
		partial = &partialBlock{blockType: blockType}
		a.partials[index] = partial
		a.order = append(a.order, index)
	}
	return partial
}

func (a *BlockAssembler) assemble(partial *partialBlock, index int) ContentBlock {
	if partial.block != nil {
		return *partial.block
	}
	switch partial.blockType {
	case BlockText:
		return ContentBlock{Type: BlockText, Text: partial.text}
	case BlockReasoning:
		return ContentBlock{Type: BlockReasoning, Text: partial.text}
	case BlockToolCall:
		id := partial.toolCallID
		if !partial.hasToolCallID {
			id = ToolCallID(fmt.Sprintf("call-%d", index))
		}
		return ContentBlock{Type: BlockToolCall, ID: string(id), Name: partial.toolCallName, Arguments: partial.toolCallArguments}
	default:
		panic(fmt.Sprintf("cannot assemble incomplete block of type %q", partial.blockType))
	}
}

// mustGet asserts the assembler invariant: every index in order has a partial.
func (a *BlockAssembler) mustGet(index int) *partialBlock {
	partial, ok := a.partials[index]
	if !ok {
		panic(fmt.Sprintf("BlockAssembler invariant violated: no partial for index %d", index))
	}
	return partial
}

// assembled is the one shared keep/drop decision over all seen blocks:
// max-token truncation drops tool calls that cannot be executed safely.
// Emitted blocks and replay metadata both derive from this result, so they
// cannot disagree.
func (a *BlockAssembler) assembled() ([]ContentBlock, *ReplayEnvelope) {
	all := make([]ContentBlock, 0, len(a.order))
	for _, index := range a.order {
		all = append(all, a.assemble(a.mustGet(index), index))
	}
	truncating := a.Finish().Kind == FinishMaxTokens
	blocks := all
	if truncating {
		kept := all[:0:0]
		for _, block := range all {
			if block.Type != BlockToolCall {
				kept = append(kept, block)
			}
		}
		blocks = kept
	}
	envelope := a.replayState
	if envelope == nil || envelope.Blocks == nil {
		return blocks, envelope
	}
	if len(envelope.Blocks) != len(all) {
		return blocks, nil
	}
	if !truncating || len(blocks) == len(all) {
		return blocks, envelope
	}
	pruned := make([]json.RawMessage, 0, len(blocks))
	for position := range all {
		if all[position].Type != BlockToolCall {
			pruned = append(pruned, envelope.Blocks[position])
		}
	}
	filtered := *envelope
	filtered.Blocks = pruned
	return blocks, &filtered
}

// Blocks assembles all blocks seen so far, in stream order: one block per seen
// index, except that max-token truncation drops tool calls that cannot be
// executed safely. An open block assembles from its accumulated deltas (an
// unknown block type never closed by block-end panics).
func (a *BlockAssembler) Blocks() []ContentBlock {
	blocks, _ := a.assembled()
	return blocks
}

// InterruptedBlocks assembles the prefix an interrupted stream can safely
// finalize: closed and open text/reasoning blocks with non-whitespace content,
// in stream order. Tool calls are omitted because interruption precedes
// dispatch; retaining one would require a fabricated result. Open unknown
// blocks are also omitted. Empty when nothing streamed before the interruption.
func (a *BlockAssembler) InterruptedBlocks() []ContentBlock {
	blocks := []ContentBlock{}
	for _, index := range a.order {
		partial := a.mustGet(index)
		blockType := partial.blockType
		if partial.block != nil {
			blockType = partial.block.Type
		}
		if blockType != BlockText && blockType != BlockReasoning {
			continue
		}
		assembled := a.assemble(partial, index)
		if strings.TrimSpace(assembled.Text) == "" {
			continue
		}
		blocks = append(blocks, assembled)
	}
	return blocks
}

// Usage returns the usage from the usage chunk, or nil until one arrives.
func (a *BlockAssembler) Usage() *TokenUsage { return a.usage }

// Finish returns the finish reason from the finish chunk, or kind stop when
// the stream ended without one.
func (a *BlockAssembler) Finish() FinishReason {
	if a.finish == nil {
		return FinishReason{Kind: FinishStop}
	}
	return *a.finish
}

// ReplayState returns the replay metadata from the terminal finish chunk, if
// any, with per-block entries pruned in step with Blocks. Nil when the
// envelope's entries do not align with the emitted blocks.
func (a *BlockAssembler) ReplayState() *ReplayEnvelope {
	_, replay := a.assembled()
	return replay
}

// Message assembles the assistant message over Blocks (same open-block
// assembly rules) with the model source: provider, model, and the pruned
// replay envelope when one survived assembly.
func (a *BlockAssembler) Message(provider, model string) Message {
	blocks, replay := a.assembled()
	source := MessageSource{Kind: SourceModel, Provider: provider, Model: model}
	if replay != nil {
		encoded, err := json.Marshal(replay)
		if err != nil {
			// ReplayState is lossless JSON by construction; a marshal failure
			// means a caller smuggled unencodable state, so drop the envelope
			// rather than corrupt the source.
			encoded = nil
		}
		source.ReplayState = encoded
	}
	message := Message{ID: NewMessageID(), Role: RoleAssistant, Content: blocks, Source: source}
	return message
}
