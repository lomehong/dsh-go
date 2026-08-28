// Serialize harness messages into DeepSeek chat completions (text-only
// requests; the image path is deferred — image content is rejected
// UNSUPPORTED_CONTENT before any text-flattening path can silently erase
// it). Port of serialize.ts.
package deepseek

import (
	"fmt"

	"dshgo/llm"
)

// RequestDefaults is the adapter-level request defaults (from plugin
// config).
type RequestDefaults struct {
	Thinking        string // "", "enabled", "disabled"
	ReasoningEffort string // "", "off", "low", "high", "max"
}

// resolvedThinking is one legal thinking/effort pair without exposing
// "off" as a wire effort.
type resolvedThinking struct {
	thinking        string // "", "enabled", "disabled"
	reasoningEffort string // "", "low", "high", "max"
}

// reasoningEffortWire validates the adapter-owned effort before resolving
// its DeepSeek wire fields.
func reasoningEffortWire(effort llm.ReasoningEffortID) (string, error) {
	switch effort {
	case "off", "low", "high", "max":
		return string(effort), nil
	}
	return "", llm.NewLlmError(
		fmt.Sprintf("DeepSeek does not support reasoning effort %q", string(effort)),
		"UNSUPPORTED_REASONING_EFFORT", llm.LlmFailure{})
}

// resolveThinking resolves one legal thinking/effort pair from the request
// and the adapter defaults.
func resolveThinking(purpose string, effort llm.ReasoningEffortID, defaults RequestDefaults) (resolvedThinking, error) {
	if purpose == llm.PurposeSessionTitle {
		return resolvedThinking{thinking: "disabled"}, nil
	}
	effective := ""
	if effort == "" {
		effective = defaults.ReasoningEffort
	} else {
		validated, err := reasoningEffortWire(effort)
		if err != nil {
			return resolvedThinking{}, err
		}
		effective = validated
	}
	if defaults.Thinking == "disabled" && effective != "" && effective != "off" {
		return resolvedThinking{}, llm.NewLlmError(
			fmt.Sprintf("DeepSeek deployment does not support reasoning effort %q", effective),
			"UNSUPPORTED_REASONING_EFFORT", llm.LlmFailure{})
	}
	if effective == "off" {
		return resolvedThinking{thinking: "disabled"}, nil
	}
	if effective == "low" || effective == "high" || effective == "max" {
		return resolvedThinking{thinking: "enabled", reasoningEffort: effective}, nil
	}
	if defaults.Thinking == "" {
		return resolvedThinking{}, nil
	}
	return resolvedThinking{thinking: defaults.Thinking}, nil
}

// flattenText joins the text blocks of a message (used for user and
// tool-result content).
func flattenText(blocks []llm.ContentBlock) string {
	out := ""
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			out += block.Text
		}
	}
	return out
}

// hasImageContent reports whether any block carries image input. The
// deferred image path rejects it everywhere.
func hasImageContent(blocks []llm.ContentBlock) bool {
	for _, block := range blocks {
		if block.Type == llm.BlockImage {
			return true
		}
		if block.Type == llm.BlockToolResult && hasImageContent(block.Content) {
			return true
		}
	}
	return false
}

// assertTextOnly rejects core image content before any text-flattening
// path can silently erase it.
func assertTextOnly(blocks []llm.ContentBlock) error {
	if hasImageContent(blocks) {
		return llm.NewLlmError(
			"The DeepSeek chat-completions adapter does not support image content.",
			"UNSUPPORTED_CONTENT", llm.LlmFailure{})
	}
	return nil
}

// serializeAssistant serializes one assistant message (text + reasoning +
// tool calls).
func serializeAssistant(message llm.Message) WireMessage {
	text := flattenText(message.Content)
	reasoning := ""
	for _, block := range message.Content {
		if block.Type == llm.BlockReasoning {
			reasoning += block.Text
		}
	}
	wire := WireMessage{Role: "assistant", Content: text}
	// CoT passback on every reasoning-carrying turn: the official rule
	// requires it on tool-call turns and ignores it elsewhere; a gateway
	// re-encoding the conversation for another vendor recovers that turn's
	// upstream thinking signature by hashing this exact text.
	if reasoning != "" {
		wire.ReasoningContent = reasoning
	}
	for _, block := range message.Content {
		if block.Type != llm.BlockToolCall {
			continue
		}
		call := WireToolCall{ID: block.ID, Type: "function"}
		call.Function.Name = block.Name
		call.Function.Arguments = block.Arguments
		wire.ToolCalls = append(wire.ToolCalls, call)
	}
	return wire
}

// serializeMessages serializes the conversation. `tool-result` blocks
// become standalone `{role: "tool"}` messages; the harness puts each tool
// result in its own user-role message, so a mixed user message contributes
// its text first and its tool results as separate wire messages after.
func serializeMessages(messages []llm.Message) ([]WireMessage, error) {
	wire := []WireMessage{}
	for _, message := range messages {
		if err := assertTextOnly(message.Content); err != nil {
			return nil, err
		}
		switch message.Role {
		case llm.RoleSystem:
			wire = append(wire, WireMessage{Role: "system", Content: flattenText(message.Content)})
			continue
		case llm.RoleAssistant:
			wire = append(wire, serializeAssistant(message))
			continue
		}
		// user role: tool results ride in user messages in the harness
		// vocabulary, but DeepSeek wants them as role:'tool' messages.
		text := flattenText(message.Content)
		toolResults := 0
		for _, block := range message.Content {
			if block.Type == llm.BlockToolResult {
				toolResults++
			}
		}
		if text != "" || toolResults == 0 {
			wire = append(wire, WireMessage{Role: "user", Content: text})
		}
		for _, block := range message.Content {
			if block.Type != llm.BlockToolResult {
				continue
			}
			wire = append(wire, WireMessage{
				Role:       "tool",
				ToolCallID: string(block.ToolCallID),
				// Empty tool output still needs SOME content on the wire.
				Content: orNoOutput(flattenText(block.Content)),
			})
		}
	}
	return wire, nil
}

// orNoOutput substitutes the fixed placeholder for empty tool output.
func orNoOutput(text string) string {
	if text == "" {
		return "(no output)"
	}
	return text
}

// requestWithMessages assembles request fields shared by text-only and
// image-capable conversion.
func requestWithMessages(options llm.GenerateOptions, messages []WireMessage, defaults RequestDefaults) (*WireRequest, error) {
	var tools []WireTool
	for _, tool := range options.Tools {
		entry := WireTool{Type: "function"}
		entry.Function.Name = tool.Name
		entry.Function.Description = tool.Description
		entry.Function.Parameters = tool.Parameters
		tools = append(tools, entry)
	}
	resolved, err := resolveThinking(options.Purpose, options.ReasoningEffort, defaults)
	if err != nil {
		return nil, err
	}
	request := &WireRequest{
		Model:    options.Model,
		Messages: messages,
		Stream:   true,
	}
	request.StreamOptions = &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	if resolved.thinking != "" {
		request.Thinking = &struct {
			Type string `json:"type"`
		}{Type: resolved.thinking}
	}
	if resolved.reasoningEffort != "" {
		request.ReasoningEffort = resolved.reasoningEffort
	}
	if len(tools) > 0 {
		request.Tools = tools
	}
	request.Temperature = options.Temperature
	request.MaxTokens = options.MaxTokens
	request.Stop = options.Stop
	return request, nil
}

// SerializeRequest builds the full wire request. Always streaming
// (`stream: true`, usage reporting on); optional fields are omitted rather
// than sent as null, so provider defaults apply. The system prompt rides
// first as its own message.
func SerializeRequest(options llm.GenerateOptions, defaults RequestDefaults) (*WireRequest, error) {
	messages := []WireMessage{}
	if options.System != "" {
		messages = append(messages, WireMessage{Role: "system", Content: options.System})
	}
	serialized, err := serializeMessages(options.Messages)
	if err != nil {
		return nil, err
	}
	messages = append(messages, serialized...)
	return requestWithMessages(options, messages, defaults)
}
