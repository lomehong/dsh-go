package anthropic

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
)

// Harness message → anthropic wire message projection. Assistant tool calls
// become tool_use blocks; user tool results become tool_result blocks; a
// trailing partially-streamed tool-call JSON (no complete arguments) cannot
// project and fails the request loudly.

// contentBlock is one wire content block; Type discriminates and the rest
// rides as the block's own JSON.
type contentBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// thinking
	Thinking string `json:"thinking,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   interface{} `json:"content,omitempty"`
	IsError   bool        `json:"is_error,omitempty"`
}

func textBlock(text string) contentBlock  { return contentBlock{Type: "text", Text: text} }
func thinkBlock(text string) contentBlock { return contentBlock{Type: "thinking", Thinking: text} }

func toolUseBlock(call llm.ContentBlock) (contentBlock, error) {
	input := map[string]any{}
	if strings_space(call.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
			return contentBlock{}, fmt.Errorf("tool_use arguments are not valid JSON: %w", err)
		}
	}
	return contentBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: mustJSON(input)}, nil
}

func toolResultBlock(result llm.ContentBlock) contentBlock {
	inner := make([]map[string]any, 0, len(result.Content))
	for _, block := range result.Content {
		if block.Type == llm.BlockText {
			inner = append(inner, map[string]any{"type": "text", "text": block.Text})
		}
	}
	var content any = inner
	if len(inner) == 1 {
		content = inner[0]["text"]
	}
	return contentBlock{
		Type: "tool_result", ToolUseID: result.ToolCallID,
		Content: content, IsError: result.IsError,
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("{}")
	}
	return encoded
}

func strings_space(value string) string {
	for len(value) > 0 && isSpaceByte(value[0]) {
		value = value[1:]
	}
	for len(value) > 0 && isSpaceByte(value[len(value)-1]) {
		value = value[:len(value)-1]
	}
	return value
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// projectMessage converts one harness message into the wire shape: content
// blocks per kind, role-carried. Assistant messages with only text collapse
// to a plain string (the wire accepts both forms).
func projectMessage(message llm.Message) (wireMessage, error) {
	blocks := make([]contentBlock, 0, len(message.Content))
	for _, block := range message.Content {
		switch block.Type {
		case llm.BlockText:
			if block.Text != "" {
				blocks = append(blocks, textBlock(block.Text))
			}
		case llm.BlockReasoning:
			// Thinking history is not replayed to the wire; anthropic
			// reconstructs reasoning per turn from its own state.
		case llm.BlockToolCall:
			projected, err := toolUseBlock(block)
			if err != nil {
				return wireMessage{}, err
			}
			blocks = append(blocks, projected)
		case llm.BlockToolResult:
			blocks = append(blocks, toolResultBlock(block))
		case llm.BlockImage:
			// Fail loud, never silently drop: a silent drop would replay a
			// history the model never saw while read_image keeps producing
			// durable image blocks (r139 audit finding over r137).
			return wireMessage{}, llm.NewLlmError(
				"The Anthropic messages adapter does not support image content yet; the multi-provider round landed the text/tool wire only.",
				"UNSUPPORTED_CONTENT", llm.LlmFailure{})
		case llm.BlockFile:
			// File blocks must project to text (ProjectFilesToText) before
			// adapter dispatch; reaching here means the projection seam was
			// bypassed — refuse rather than erase.
			return wireMessage{}, llm.NewLlmError(
				"The Anthropic messages adapter does not accept file blocks; file content must project to text before dispatch.",
				"UNSUPPORTED_CONTENT", llm.LlmFailure{})
		default:
			return wireMessage{}, llm.NewLlmError(
				fmt.Sprintf("The Anthropic messages adapter does not support content block type %q.", block.Type),
				"UNSUPPORTED_CONTENT", llm.LlmFailure{})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, textBlock(""))
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return wireMessage{}, err
	}
	return wireMessage{Role: message.Role, Content: encoded}, nil
}
