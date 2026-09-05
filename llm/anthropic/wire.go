package anthropic

import (
	"encoding/json"

	"dshgo/llm"
)

// Request/response wire shapes for the Anthropic Messages protocol.

// wireMessage is one request message; content blocks are role-dependent
// (assistant: text/thinking/tool_use; user: text/tool_result).
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type wireRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int64           `json:"max_tokens"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools,omitempty"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	Stop        []string        `json:"stop_sequences,omitempty"`
}

// buildRequest projects the harness GenerateOptions onto the anthropic
// messages wire: system rendered top-level, tool calls as tool_use blocks,
// tool results as tool_result blocks. Projection failures (unsupported
// content, partial tool-call JSON) fail the request — never a silent
// message drop.
func buildRequest(options llm.GenerateOptions, facts *Options) (wireRequest, error) {
	maxTokens := facts.MaxTokens
	if options.MaxTokens != nil && *options.MaxTokens > 0 {
		maxTokens = *options.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = maxTokensFloor
	}
	request := wireRequest{
		Model: options.Model, MaxTokens: maxTokens, Stream: true,
		Temperature: options.Temperature, Stop: options.Stop,
	}
	messages := make([]wireMessage, 0, len(options.Messages))
	for _, message := range options.Messages {
		// The harness system slot renders top-level (the wire's system
		// member), never as a message.
		if message.Role == llm.RoleSystem {
			continue
		}
		wire, err := projectMessage(message)
		if err != nil {
			return wireRequest{}, err
		}
		messages = append(messages, wire)
	}
	request.Messages = messages
	if options.System != "" {
		request.System = json.RawMessage(mustJSON(options.System))
	}
	if len(options.Tools) > 0 {
		tools := make([]wireTool, 0, len(options.Tools))
		for _, tool := range options.Tools {
			schema, _ := json.Marshal(tool.Parameters)
			tools = append(tools, wireTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		}
		request.Tools = tools
	}
	return request, nil
}
