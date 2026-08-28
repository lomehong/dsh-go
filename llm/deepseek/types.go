// DeepSeek chat-completions wire format (OpenAI-compatible). Types only;
// JSON field names are the wire vocabulary and must not change.
//
// Source of truth: the official API docs, cross-checked against live
// streams (packages/llm/llm-deepseek/src/types.ts @ dsh-v0.1.2-alpha.1).
package deepseek

// WireRequest is the request body for `POST {baseURL}/chat/completions`.
type WireRequest struct {
	Model         string        `json:"model"`
	Messages      []WireMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	// Thinking is the thinking-mode toggle (top level, NOT inside
	// extra_body on the wire).
	Thinking *struct {
		Type string `json:"type"`
	} `json:"thinking,omitempty"`
	// ReasoningEffort carries the official effort levels on the wire.
	ReasoningEffort string     `json:"reasoning_effort,omitempty"`
	Tools           []WireTool `json:"tools,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	MaxTokens       *int64     `json:"max_tokens,omitempty"`
	// Stop sequences halt generation as soon as the model produces any one
	// of these strings; the stop string itself is not included.
	Stop []string `json:"stop,omitempty"`
}

// WireSystemMessage is a system-role message: one string of instructions.
type WireSystemMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// WireTextContentPart is a text part inside a multimodal user message.
type WireTextContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// WireUserMessage is a user-role message. Text-only requests keep the
// compact string content form.
type WireUserMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string
}

// WireToolMessage is a tool-role message: one tool call's result, keyed by
// its call id.
type WireToolMessage struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// WireAssistantMessage is an assistant-role history message. Text-less
// turns replay `content: ""` (never null); reasoning carries the CoT
// passback required on tool-call turns in thinking mode.
type WireAssistantMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []WireToolCall `json:"tool_calls,omitempty"`
}

// WireMessage is one request `messages` entry, discriminated on `role` by
// which Go struct filled it in.
type WireMessage struct {
	// Role is "system", "user", "assistant", or "tool".
	Role string `json:"role"`
	// Content is the role's content (string for every supported role).
	Content string `json:"content"`
	// ReasoningContent is the assistant CoT passback.
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// ToolCallID keys tool-role messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolCalls replays completed calls on assistant messages.
	ToolCalls []WireToolCall `json:"tool_calls,omitempty"`
}

// WireToolCall is a completed tool call replayed on an assistant history
// message; Arguments is the raw JSON string.
type WireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// WireTool is one request `tools` entry; Parameters is a JSON Schema object.
type WireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// WireChunk is one parsed SSE `data:` payload (a chat.completion.chunk).
type WireChunk struct {
	Choices []WireChoice `json:"choices,omitempty"`
	// Usage arrives attached to the finish chunk and/or as a trailing
	// usage-only chunk.
	Usage *WireUsage `json:"usage,omitempty"`
}

// WireChoice is one streamed choice; FinishReason is non-null only on its
// terminal chunk.
type WireChoice struct {
	Delta        *WireDelta `json:"delta,omitempty"`
	FinishReason *string    `json:"finish_reason"`
}

// WireDelta is the incremental content of one streamed choice; any subset
// of fields may be present per chunk.
type WireDelta struct {
	Role string `json:"role,omitempty"`
	// Content is visible text; null/empty on reasoning/tool-call chunks.
	Content *string `json:"content"`
	// ReasoningContent is thinking-mode CoT. The FIRST chunk carries an
	// empty string (must not open a reasoning block); absent entirely in
	// non-thinking mode.
	ReasoningContent *string             `json:"reasoning_content"`
	ToolCalls        []WireToolCallDelta `json:"tool_calls,omitempty"`
}

// WireToolCallDelta is a streamed fragment of one tool call; fragments
// sharing an Index concatenate into one call.
type WireToolCallDelta struct {
	// Index disambiguates parallel tool calls; stable across a call's
	// deltas.
	Index    int64  `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function *struct {
		// Name is present on the first delta of each call only.
		Name string `json:"name,omitempty"`
		// Arguments is the argument JSON fragment (concatenate across
		// deltas).
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// WireUsage is the wire token accounting. PromptTokens INCLUDES cache hits
// (it equals PromptCacheHitTokens + PromptCacheMissTokens); MapUsage
// subtracts them for the harness convention of disjoint counts.
type WireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	// TotalTokens is the provider-reported aggregate across prompt and
	// completion tokens.
	TotalTokens           *int64 `json:"total_tokens,omitempty"`
	PromptCacheHitTokens  *int64 `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens *int64 `json:"prompt_cache_miss_tokens,omitempty"`
	PromptTokensDetails   *struct {
		CachedTokens *int64 `json:"cached_tokens,omitempty"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
	} `json:"completion_tokens_details,omitempty"`
}

// WireError is a non-2xx error body.
type WireError struct {
	Error *struct {
		Message string `json:"message,omitempty"`
		Type    string `json:"type,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
}
