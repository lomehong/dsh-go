package anthropic

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"dshgo/llm"
)

// Translate the Anthropic Messages SSE stream into the harness StreamChunk
// protocol. The wire emits per-block events: content_block_start opens a
// text / thinking / tool_use block, content_block_delta streams its
// increments, content_block_stop closes it. message_delta carries the final
// stop reason and output usage; message_stop ends the stream. Blocks close
// at their own stop event (anthropic frames them; nothing defers to a
// [DONE] sentinel). An `error` event or a premature end surfaces as the
// stream's terminal failure.

// sseEvent is one parsed SSE frame: event name plus its JSON payload.
type sseEvent struct {
	event string
	data  json.RawMessage
}

// readEvents splits one SSE body into (event, data) frames. A read error
// mid-stream surfaces through the error channel after the frames already
// parsed.
func readEvents(reader *bufio.Reader, out chan<- sseEvent, errs chan<- error) {
	event := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if strings_hasContent(line) && strings_hasPrefix(line, "data:") {
				out <- sseEvent{event: event, data: json.RawMessage(trim(line[len("data:"):]))}
			}
			close(out)
			if err != io.EOF {
				errs <- err
			} else {
				close(errs)
			}
			return
		}
		trimmed := trim(line)
		switch {
		case trimmed == "":
			if event != "" {
				out <- sseEvent{event: event}
			}
			event = ""
		case hasPrefix(trimmed, "event:"):
			event = trim(trimmed[len("event:"):])
		case hasPrefix(trimmed, "data:"):
			out <- sseEvent{event: event, data: json.RawMessage(trim(trimmed[len("data:"):]))}
		}
	}
}

func hasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

func strings_hasContent(s string) bool        { return len(s) > 0 }
func strings_hasPrefix(s, prefix string) bool { return hasPrefix(s, prefix) }
func trim(s string) string                    { return trimBytes(s) }

func trimBytes(s string) string {
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

// openBlock is one open content block under assembly.
type openBlock struct {
	kind    string // text | thinking | tool-call
	text    string
	callID  string
	name    string
	partial string
}

// Translate consumes the SSE event stream and yields harness chunks. Blocks
// close at their own stop; usage and stop reason (from message_delta) ride
// the finish.
func translateSse(reader *bufio.Reader, yield func(llm.StreamChunk) bool) {
	frames := make(chan sseEvent, 8)
	errs := make(chan error, 1)
	go readEvents(reader, frames, errs)

	openBlocks := map[int]*openBlock{}
	var order []int
	var pendingUsage *llm.TokenUsage
	var stopReason string

	indexOf := func(index int) *openBlock { return openBlocks[index] }
	open := func(index int, kind string) *openBlock {
		block := &openBlock{kind: kind}
		openBlocks[index] = block
		order = append(order, index)
		return block
	}
	closeBlock := func(index int) *llm.ContentBlock {
		block := openBlocks[index]
		delete(openBlocks, index)
		switch block.kind {
		case "text":
			return &llm.ContentBlock{Type: llm.BlockText, Text: block.text}
		case "thinking":
			return &llm.ContentBlock{Type: llm.BlockReasoning, Text: block.text}
		default:
			return &llm.ContentBlock{Type: llm.BlockToolCall, ID: block.callID, Name: block.name, Arguments: block.partial}
		}
	}

	for {
		select {
		case err := <-errs:
			if err != nil {
				yield(llm.TerminalFailureChunk(err, false))
				return
			}
		case frame, ok := <-frames:
			if !ok {
				// Stream ended without message_stop: refuse as truncated.
				yield(llm.TerminalFailureChunk(llm.NewLlmError(
					"anthropic SSE stream ended without message_stop", "STREAM_CLOSED", llm.LlmFailure{}), false))
				return
			}
			switch frame.event {
			case "content_block_start":
				var payload struct {
					Index        int `json:"index"`
					ContentBlock struct {
						Type string `json:"type"`
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"content_block"`
				}
				if err := json.Unmarshal(frame.data, &payload); err != nil {
					continue
				}
				kind := "text"
				switch payload.ContentBlock.Type {
				case "tool_use":
					kind = "tool-call"
				case "thinking":
					kind = "thinking"
				}
				block := open(payload.Index, kind)
				block.callID = payload.ContentBlock.ID
				block.name = payload.ContentBlock.Name
				yield(llm.StreamChunk{
					Type: llm.ChunkBlockStart, Index: payload.Index,
					BlockType: blockKindToBlockType(kind),
				})
			case "content_block_delta":
				var payload struct {
					Index int `json:"index"`
					Delta struct {
						Type     string `json:"type"`
						Text     string `json:"text"`
						Thinking string `json:"thinking"`
						Partial  string `json:"partial_json"`
					} `json:"delta"`
				}
				if err := json.Unmarshal(frame.data, &payload); err != nil {
					continue
				}
				block := indexOf(payload.Index)
				switch payload.Delta.Type {
				case "text_delta":
					if block == nil {
						block = open(payload.Index, "text")
						yield(llm.StreamChunk{Type: llm.ChunkBlockStart, Index: payload.Index, BlockType: llm.BlockText})
					}
					block.text += payload.Delta.Text
					yield(llm.StreamChunk{Type: llm.ChunkTextDelta, Index: payload.Index, Text: payload.Delta.Text})
				case "thinking_delta":
					if block == nil {
						block = open(payload.Index, "thinking")
						yield(llm.StreamChunk{Type: llm.ChunkBlockStart, Index: payload.Index, BlockType: llm.BlockReasoning})
					}
					block.text += payload.Delta.Thinking
					yield(llm.StreamChunk{Type: llm.ChunkReasoningDelta, Index: payload.Index, Text: payload.Delta.Thinking})
				case "input_json_delta":
					if block == nil {
						continue
					}
					block.partial += payload.Delta.Partial
					yield(llm.StreamChunk{Type: llm.ChunkToolCallDelta, Index: payload.Index,
						ID: block.callID, Name: block.name, ArgumentsDelta: payload.Delta.Partial})
				}
			case "content_block_stop":
				var payload struct {
					Index int `json:"index"`
				}
				if err := json.Unmarshal(frame.data, &payload); err != nil {
					continue
				}
				if block := indexOf(payload.Index); block != nil {
					yield(llm.StreamChunk{
						Type: llm.ChunkBlockEnd, Index: payload.Index,
						Block: closeBlock(payload.Index),
					})
				}
			case "message_delta":
				var payload struct {
					Delta struct {
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage struct {
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(frame.data, &payload); err != nil {
					continue
				}
				stopReason = payload.Delta.StopReason
				if payload.Usage.OutputTokens > 0 {
					pendingUsage = &llm.TokenUsage{OutputTokens: payload.Usage.OutputTokens}
				}
			case "message_stop":
				for _, index := range order {
					if _, stillOpen := openBlocks[index]; stillOpen {
						yield(llm.StreamChunk{Type: llm.ChunkBlockEnd, Index: index, Block: closeBlock(index)})
					}
				}
				if pendingUsage != nil {
					yield(llm.StreamChunk{Type: llm.ChunkUsage, Usage: pendingUsage})
				}
				yield(llm.StreamChunk{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: mapStop(stopReason)}})
				return
			case "error":
				var payload struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				_ = json.Unmarshal(frame.data, &payload)
				message := payload.Error.Message
				if message == "" {
					message = "anthropic SSE error event"
				}
				yield(llm.TerminalFailureChunk(llm.NewLlmError(message, "PROVIDER_ERROR", llm.LlmFailure{}), false))
				return
			case "ping":
			default:
			}
		}
	}
}

// mapStop maps the wire stop_reason to the harness finish vocabulary.
func mapStop(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return llm.FinishStop
	case "tool_use":
		return llm.FinishToolCalls
	case "max_tokens":
		return llm.FinishMaxTokens
	case "refusal":
		return llm.FinishStop
	default:
		return llm.FinishStop
	}
}

// blockKindToBlockType maps the internal block kind to the harness block
// type vocabulary.
func blockKindToBlockType(kind string) string {
	switch kind {
	case "thinking":
		return llm.BlockReasoning
	case "tool-call":
		return llm.BlockToolCall
	default:
		return llm.BlockText
	}
}

var _ = fmt.Sprintf
