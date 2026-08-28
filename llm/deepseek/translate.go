// Translate DeepSeek SSE payloads into the harness StreamChunk protocol,
// with one stateful harness block per content, reasoning, or tool call
// index. An empty initial reasoning delta does not open a block. Finish
// reason and the latest usage are deferred until `[DONE]`, covering both
// finish-attached and trailing usage-only shapes while ensuring no chunk
// follows `finish`. Port of translate.ts.
package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"dshgo/llm"
)

// Done is the terminal payload DeepSeek (and OpenAI) send after the last
// chunk.
const Done = "[DONE]"

// openBlock is one open block under assembly.
type openBlock struct {
	index int
	kind  string // "text" | "reasoning" | "tool-call"
	text  string
	// tool-call only
	callID  string
	name    string
	nameSet bool
}

// mapFinishReason maps the wire finish_reason vocabulary to the harness
// FinishReason; unrecognized values (content_filter, …) become an error
// reason with the uppercased value as code.
func mapFinishReason(reason string) llm.FinishReason {
	switch reason {
	case "stop":
		return llm.FinishReason{Kind: llm.FinishStop}
	case "tool_calls":
		return llm.FinishReason{Kind: llm.FinishToolCalls}
	case "length":
		return llm.FinishReason{Kind: llm.FinishMaxTokens}
	default:
		// content_filter, insufficient_system_resource, future additions.
		return llm.FinishReason{Kind: llm.FinishError, Failure: &llm.LlmFailure{
			Message: fmt.Sprintf("model stopped: %s", reason),
			Code:    upperASCII(reason),
		}}
	}
}

// upperASCII uppercases ASCII letters, leaving other bytes untouched.
// blockPtr takes the address of a freshly built block value.
func blockPtr(block llm.ContentBlock) *llm.ContentBlock { return &block }

func upperASCII(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - ('a' - 'A')
		}
	}
	return string(out)
}

// mapUsage maps wire usage fields into disjoint harness counts. DeepSeek's
// prompt_tokens INCLUDES cache hits; the harness TokenUsage convention is
// DISJOINT counts, so cache reads are subtracted out of inputTokens. An
// exact total is present only when the aggregate counters are valid and
// agree with any wire total.
func mapUsage(usage *WireUsage) llm.TokenUsage {
	var cacheRead *int64
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens != nil {
		cacheRead = usage.PromptTokensDetails.CachedTokens
	} else {
		cacheRead = usage.PromptCacheHitTokens
	}
	reasoning := (*int64)(nil)
	if usage.CompletionTokensDetails != nil {
		reasoning = usage.CompletionTokensDetails.ReasoningTokens
	}
	combined := usage.PromptTokens + usage.CompletionTokens
	hasExactTotal := usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 &&
		(usage.TotalTokens == nil || *usage.TotalTokens == combined)
	out := llm.TokenUsage{
		InputTokens:  usage.PromptTokens - derefZero(cacheRead),
		OutputTokens: usage.CompletionTokens,
	}
	if hasExactTotal {
		out.TotalTokens = &combined
	}
	if cacheRead != nil {
		out.CacheReadTokens = cacheRead
	}
	if reasoning != nil {
		out.ReasoningTokens = reasoning
	}
	return out
}

func derefZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// closeBlock assembles the final ContentBlock for one open block.
func closeBlock(block *openBlock) llm.ContentBlock {
	switch block.kind {
	case "text":
		return llm.ContentBlock{Type: llm.BlockText, Text: block.text}
	case "reasoning":
		return llm.ContentBlock{Type: llm.BlockReasoning, Text: block.text}
	default:
		return llm.ContentBlock{
			Type:      llm.BlockToolCall,
			ID:        block.callID,
			Name:      block.name,
			Arguments: block.text,
		}
	}
}

// PayloadStream is the SSE data-payload source. Next returns io.EOF at a
// clean end of stream; any other error aborts the stream.
type PayloadStream interface {
	Next() (string, error)
}

// SlicePayloads adapts a payload slice to PayloadStream (tests and
// in-memory sources).
func SlicePayloads(payloads []string) PayloadStream {
	return &slicePayloads{payloads: payloads}
}

type slicePayloads struct {
	payloads []string
	pos      int
}

func (s *slicePayloads) Next() (string, error) {
	if s.pos >= len(s.payloads) {
		return "", io.EOF
	}
	payload := s.payloads[s.pos]
	s.pos++
	return payload, nil
}

// Translate consumes SSE data payloads (ending with `[DONE]`) and yields
// StreamChunks. Malformed JSON payloads abort the stream with
// MALFORMED_RESPONSE; a payload source that ends without `[DONE]` is
// STREAM_CLOSED. Deltas arrive as they happen; `block-end`s, `usage`, and
// `finish` are all deferred to the `[DONE]` sentinel. A `stop` (or absent)
// finish with no opened blocks is a degenerate provider completion and maps
// to an EMPTY_RESPONSE error finish instead of a successful empty message.
func Translate(payloads PayloadStream) llm.Seq {
	return func(yield func(llm.StreamChunk) bool) {
		nextIndex := 0
		var textBlock, reasoningBlock *openBlock
		toolBlocks := map[int64]*openBlock{}
		var order []*openBlock
		var pendingFinish *llm.FinishReason
		var pendingUsage *llm.TokenUsage

		open := func(kind string) *openBlock {
			block := &openBlock{index: nextIndex, kind: kind}
			nextIndex++
			order = append(order, block)
			return block
		}

		for {
			payload, err := payloads.Next()
			if err != nil {
				// parseSse guarantees the [DONE] sentinel or a STREAM_CLOSED
				// failure; a bare EOF means the payload source violated that
				// contract.
				end := err
				if errors.Is(end, io.EOF) {
					end = llm.NewLlmError("SSE payload stream ended without [DONE]", "STREAM_CLOSED", llm.LlmFailure{})
				}
				yield(llm.TerminalFailureChunk(end, false))
				return
			}
			if payload == Done {
				for _, block := range order {
					if !yield(llm.StreamChunk{Type: llm.ChunkBlockEnd, Index: block.index, Block: blockPtr(closeBlock(block))}) {
						return
					}
				}
				if pendingUsage != nil {
					if !yield(llm.StreamChunk{Type: llm.ChunkUsage, Usage: pendingUsage}) {
						return
					}
				}
				reason := llm.FinishReason{Kind: llm.FinishStop}
				if pendingFinish != nil {
					reason = *pendingFinish
				}
				if reason.Kind == llm.FinishStop && len(order) == 0 {
					reason = llm.FinishReason{Kind: llm.FinishError, Failure: &llm.LlmFailure{
						Message: "model returned a completed response with no content",
						Code:    llm.EmptyResponseCode,
					}}
				}
				yield(llm.StreamChunk{Type: llm.ChunkFinish, Reason: &reason})
				return
			}

			var chunk WireChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				preview := payload
				if len(preview) > 120 {
					preview = preview[:120]
				}
				yield(llm.TerminalFailureChunk(
					llm.NewLlmError(fmt.Sprintf("malformed SSE payload: %s", preview), "MALFORMED_RESPONSE", llm.LlmFailure{}), false))
				return
			}

			stop := false
			for _, choice := range chunk.Choices {
				delta := choice.Delta
				if delta == nil {
					continue
				}
				// Reasoning first: thinking mode interleaves it before text.
				// The empty-string first chunk must not open a block.
				if reasoning := delta.ReasoningContent; reasoning != nil && *reasoning != "" {
					if reasoningBlock == nil {
						reasoningBlock = open("reasoning")
						if !yield(llm.StreamChunk{Type: llm.ChunkBlockStart, Index: reasoningBlock.index, BlockType: llm.BlockReasoning}) {
							return
						}
					}
					reasoningBlock.text += *reasoning
					if !yield(llm.StreamChunk{Type: llm.ChunkReasoningDelta, Index: reasoningBlock.index, Text: *reasoning}) {
						return
					}
				}
				if content := delta.Content; content != nil && *content != "" {
					if textBlock == nil {
						textBlock = open("text")
						if !yield(llm.StreamChunk{Type: llm.ChunkBlockStart, Index: textBlock.index, BlockType: llm.BlockText}) {
							return
						}
					}
					textBlock.text += *content
					if !yield(llm.StreamChunk{Type: llm.ChunkTextDelta, Index: textBlock.index, Text: *content}) {
						return
					}
				}
				for _, call := range delta.ToolCalls {
					block := toolBlocks[call.Index]
					if block == nil {
						block = open("tool-call")
						toolBlocks[call.Index] = block
						if !yield(llm.StreamChunk{Type: llm.ChunkBlockStart, Index: block.index, BlockType: llm.BlockToolCall}) {
							return
						}
					}
					if call.ID != "" {
						block.callID = call.ID
					}
					if call.Function != nil && call.Function.Name != "" {
						block.name = call.Function.Name
						block.nameSet = true
					}
					fragment := ""
					if call.Function != nil {
						fragment = call.Function.Arguments
					}
					block.text += fragment
					chunk := llm.StreamChunk{
						Type:           llm.ChunkToolCallDelta,
						Index:          block.index,
						ID:             block.callID,
						ArgumentsDelta: fragment,
					}
					if block.nameSet {
						chunk.Name = block.name
					}
					if !yield(chunk) {
						stop = true
						break
					}
				}
				if stop {
					break
				}
				if choice.FinishReason != nil && *choice.FinishReason != "" {
					mapped := mapFinishReason(*choice.FinishReason)
					pendingFinish = &mapped
				}
			}
			if stop {
				return
			}
			// Usage may arrive attached to the finish chunk or as a
			// trailing usage-only chunk — keep the latest.
			if chunk.Usage != nil {
				mapped := mapUsage(chunk.Usage)
				pendingUsage = &mapped
			}
		}
	}
}
