package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dshgo/llm"
)

func collectChunks(seq llm.Seq) []llm.StreamChunk {
	var out []llm.StreamChunk
	for chunk := range seq {
		out = append(out, chunk)
	}
	return out
}

// failCode extracts the LlmError code from a serialization failure.
func failCode(err error) string {
	var llmErr *llm.LlmError
	if errors.As(err, &llmErr) {
		return llmErr.Code()
	}
	return "<no-code>"
}

func int64Ptr3(v int64) *int64 { return &v }

// --- serialization -------------------------------------------------------

func testOptions() llm.GenerateOptions {
	return llm.GenerateOptions{
		Provider: "deepseek-official",
		Model:    "deepseek-v4-pro",
		Messages: []llm.Message{
			llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}, llm.MessageSource{Kind: llm.SourceUser}),
		},
	}
}

func TestSerializeRequestWireShape(t *testing.T) {
	options := testOptions()
	options.ReasoningEffort = "high"
	options.MaxTokens = int64Ptr3(4096)
	options.Stop = []string{"END"}
	options.System = "be brief"
	request, err := SerializeRequest(options, RequestDefaults{})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["model"] != "deepseek-v4-pro" {
		t.Fatalf("model = %v", wire["model"])
	}
	if wire["stream"] != true {
		t.Fatalf("stream = %v", wire["stream"])
	}
	streamOptions, ok := wire["stream_options"].(map[string]any)
	if !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %v", wire["stream_options"])
	}
	if wire["thinking"] == nil {
		t.Fatal("thinking missing for effort high")
	}
	if thinking, ok := wire["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v", wire["thinking"])
	}
	if wire["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", wire["reasoning_effort"])
	}
	if wire["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v", wire["max_tokens"])
	}
	if wire["temperature"] != nil {
		t.Fatalf("temperature should be omitted, got %v", wire["temperature"])
	}
	stop, ok := wire["stop"].([]any)
	if !ok || len(stop) != 1 || stop[0] != "END" {
		t.Fatalf("stop = %v", wire["stop"])
	}
	messages := wire["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	if messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] != "be brief" {
		t.Fatalf("system message = %v", messages[0])
	}
	if messages[1].(map[string]any)["role"] != "user" || messages[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("user message = %v", messages[1])
	}
}

func TestSerializeOmitsUnsetFields(t *testing.T) {
	request, err := SerializeRequest(testOptions(), RequestDefaults{})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	raw, _ := json.Marshal(request)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	for _, field := range []string{"thinking", "reasoning_effort", "tools", "temperature", "max_tokens", "stop"} {
		if _, present := wire[field]; present {
			t.Fatalf("%s should be omitted, got %v", field, wire[field])
		}
	}
}

func TestSerializeThinkingMatrix(t *testing.T) {
	cases := []struct {
		name       string
		effort     llm.ReasoningEffortID
		purpose    string
		defaults   RequestDefaults
		wantType   string
		wantEffort string
	}{
		{"no effort no defaults", "", "", RequestDefaults{}, "", ""},
		{"effort off", "off", "", RequestDefaults{}, "disabled", ""},
		{"effort low", "low", "", RequestDefaults{}, "enabled", "low"},
		{"effort max", "max", "", RequestDefaults{}, "enabled", "max"},
		{"default effort used", "", "", RequestDefaults{ReasoningEffort: "low"}, "enabled", "low"},
		{"default thinking only", "", "", RequestDefaults{Thinking: "enabled"}, "enabled", ""},
		{"default thinking disabled", "", "", RequestDefaults{Thinking: "disabled"}, "disabled", ""},
		{"session title forces disabled", "high", llm.PurposeSessionTitle, RequestDefaults{}, "disabled", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			options := testOptions()
			options.ReasoningEffort = tc.effort
			options.Purpose = tc.purpose
			request, err := SerializeRequest(options, tc.defaults)
			if err != nil {
				t.Fatalf("serialize: %v", err)
			}
			raw, _ := json.Marshal(request)
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			if tc.wantType == "" {
				if wire["thinking"] != nil {
					t.Fatalf("thinking = %v, want absent", wire["thinking"])
				}
			} else if thinking := wire["thinking"].(map[string]any); thinking["type"] != tc.wantType {
				t.Fatalf("thinking = %v, want %s", wire["thinking"], tc.wantType)
			}
			gotEffort := ""
			if wire["reasoning_effort"] != nil {
				gotEffort = wire["reasoning_effort"].(string)
			}
			if gotEffort != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q", gotEffort, tc.wantEffort)
			}
		})
	}
	// Disabling thinking with a real effort rejects.
	options := testOptions()
	options.ReasoningEffort = "high"
	_, err := SerializeRequest(options, RequestDefaults{Thinking: "disabled"})
	if err == nil || failCode(err) != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("disabled+high err = %v", err)
	}
	// Invalid effort vocabulary rejects before the wire.
	options = testOptions()
	options.ReasoningEffort = "turbo"
	_, err = SerializeRequest(options, RequestDefaults{})
	if err == nil || failCode(err) != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("turbo err = %v", err)
	}
}

func TestSerializeAssistantReplay(t *testing.T) {
	options := testOptions()
	options.Messages = []llm.Message{
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "working"},
			{Type: llm.BlockReasoning, Text: "thought about it"},
			{Type: llm.BlockToolCall, ID: "call-1", Name: "read_file", Arguments: `{"path":"a.txt"}`},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolCallID: "call-1", Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "contents"}}},
		}},
	}
	request, err := SerializeRequest(options, RequestDefaults{})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if len(request.Messages) != 2 {
		t.Fatalf("messages = %d", len(request.Messages))
	}
	assistant := request.Messages[0]
	if assistant.Role != "assistant" {
		t.Fatalf("role = %s", assistant.Role)
	}
	if assistant.Content != "working" {
		t.Fatalf("content = %q", assistant.Content)
	}
	if assistant.ReasoningContent != "thought about it" {
		t.Fatalf("reasoning_content = %q", assistant.ReasoningContent)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call-1" ||
		assistant.ToolCalls[0].Function.Name != "read_file" ||
		assistant.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Fatalf("tool_calls = %+v", assistant.ToolCalls)
	}
	tool := request.Messages[1]
	if tool.Role != "tool" || tool.ToolCallID != "call-1" || tool.Content != "contents" {
		t.Fatalf("tool message = %+v", tool)
	}
}

func TestSerializeEmptyToolOutputPlaceholder(t *testing.T) {
	options := testOptions()
	options.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolCallID: "call-1", Content: nil},
		}},
	}
	request, err := SerializeRequest(options, RequestDefaults{})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if len(request.Messages) != 1 || request.Messages[0].Content != "(no output)" {
		t.Fatalf("messages = %+v", request.Messages)
	}
}

func TestSerializeRejectsImageContent(t *testing.T) {
	options := testOptions()
	options.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "look"},
			{Type: llm.BlockImage},
		}},
	}
	_, err := SerializeRequest(options, RequestDefaults{})
	if err == nil || failCode(err) != "UNSUPPORTED_CONTENT" {
		t.Fatalf("err = %v", err)
	}
	// Nested in a tool result also rejects.
	options.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolCallID: "c", Content: []llm.ContentBlock{{Type: llm.BlockImage}}},
		}},
	}
	_, err = SerializeRequest(options, RequestDefaults{})
	if err == nil || failCode(err) != "UNSUPPORTED_CONTENT" {
		t.Fatalf("nested err = %v", err)
	}
}

func TestSerializeTools(t *testing.T) {
	options := testOptions()
	options.Tools = []llm.ToolSchema{{
		Name: "read_file", Description: "Read a file.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	request, err := SerializeRequest(options, RequestDefaults{})
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	raw, _ := json.Marshal(request)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	tools := wire["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", wire["tools"])
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read_file" || function["description"] != "Read a file." {
		t.Fatalf("function = %v", function)
	}
}

// --- translation ---------------------------------------------------------

func payloadChunk(delta string, finish string, usage *WireUsage) string {
	payload := map[string]any{
		"choices": []map[string]any{{
			"delta": map[string]any{"content": delta},
		}},
	}
	if finish != "" {
		payload["choices"].([]map[string]any)[0]["finish_reason"] = finish
	}
	if usage != nil {
		payload["usage"] = usage
	}
	raw, _ := json.Marshal(payload)
	return string(raw)
}

func TestTranslateTextStream(t *testing.T) {
	payloads := SlicePayloads([]string{
		payloadChunk("he", "", nil),
		payloadChunk("llo", "", nil),
		payloadChunk("", "stop", nil),
		Done,
	})
	chunks := collectChunks(Translate(payloads))
	var kinds []string
	for _, chunk := range chunks {
		kinds = append(kinds, chunk.Type)
	}
	// Deferred block-end and finish: deltas first, then end + finish at [DONE].
	want := []string{"block-start", "text-delta", "text-delta", "block-end", "finish"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v", kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds[%d] = %s, want %s (all: %v)", i, kinds[i], want[i], kinds)
		}
	}
	end := chunks[3]
	if end.Block == nil || end.Block.Text != "hello" || end.Block.Type != llm.BlockText {
		t.Fatalf("block-end = %+v", end.Block)
	}
	if chunks[4].Reason == nil || chunks[4].Reason.Kind != llm.FinishStop {
		t.Fatalf("finish = %+v", chunks[4].Reason)
	}
}

func TestTranslateReasoningAndTools(t *testing.T) {
	reasoning := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"reasoning_content": "thinking hard"}}}}
	emptyReasoning := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"reasoning_content": ""}}}}
	firstCall := map[string]any{"choices": []map[string]any{{"delta": map[string]any{
		"tool_calls": []map[string]any{{"index": 0, "id": "call-9", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":"}}},
	}}}}
	secondCall := map[string]any{"choices": []map[string]any{{"delta": map[string]any{
		"tool_calls": []map[string]any{{"index": 0, "function": map[string]any{"arguments": "\"ls\"}"}}},
	}}}}
	finish := map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}}
	encode := func(v any) string { raw, _ := json.Marshal(v); return string(raw) }
	payloads := SlicePayloads([]string{encode(reasoning), encode(emptyReasoning), encode(firstCall), encode(secondCall), encode(finish), Done})
	chunks := collectChunks(Translate(payloads))
	if chunks[0].Type != llm.ChunkBlockStart || chunks[0].BlockType != llm.BlockReasoning {
		t.Fatalf("first chunk = %+v", chunks[0])
	}
	sawToolStart, sawToolDelta := false, false
	ends := map[int]llm.ContentBlock{}
	for _, chunk := range chunks {
		switch chunk.Type {
		case llm.ChunkBlockStart:
			if chunk.BlockType == llm.BlockToolCall {
				sawToolStart = true
			}
		case llm.ChunkToolCallDelta:
			sawToolDelta = true
			if chunk.ID != "call-9" {
				t.Fatalf("delta id = %q", chunk.ID)
			}
			if chunk.Name != "run" {
				t.Fatalf("delta name = %q", chunk.Name)
			}
		case llm.ChunkBlockEnd:
			ends[chunk.Index] = *chunk.Block
		case llm.ChunkFinish:
			if chunk.Reason == nil || chunk.Reason.Kind != llm.FinishToolCalls {
				t.Fatalf("finish = %+v", chunk.Reason)
			}
		}
	}
	if !sawToolStart || !sawToolDelta {
		t.Fatalf("tool chunks missing (%v %v)", sawToolStart, sawToolDelta)
	}
	toolBlock, ok := ends[1]
	if !ok {
		t.Fatalf("block-ends = %v", ends)
	}
	if toolBlock.Arguments != `{"cmd":"ls"}` || toolBlock.Name != "run" || toolBlock.ID != "call-9" {
		t.Fatalf("tool block = %+v", toolBlock)
	}
}

func TestTranslateUnknownFinishReason(t *testing.T) {
	payloads := SlicePayloads([]string{payloadChunk("x", "content_filter", nil), Done})
	chunks := collectChunks(Translate(payloads))
	finish := chunks[len(chunks)-1]
	if finish.Reason == nil || finish.Reason.Kind != llm.FinishError {
		t.Fatalf("finish = %+v", finish.Reason)
	}
	if finish.Reason.Failure == nil || finish.Reason.Failure.Code != "CONTENT_FILTER" {
		t.Fatalf("failure = %+v", finish.Reason.Failure)
	}
}

func TestTranslateUsageDeferredAndLatestWins(t *testing.T) {
	first := &WireUsage{PromptTokens: 10, CompletionTokens: 5}
	second := &WireUsage{PromptTokens: 100, CompletionTokens: 50}
	payloads := SlicePayloads([]string{payloadChunk("x", "stop", first), payloadChunk("", "", second), Done})
	chunks := collectChunks(Translate(payloads))
	usageCount := 0
	for _, chunk := range chunks {
		if chunk.Type == llm.ChunkUsage {
			usageCount++
			if chunk.Usage.InputTokens != 100 || chunk.Usage.OutputTokens != 50 {
				t.Fatalf("usage = %+v", chunk.Usage)
			}
			if chunk.Usage.TotalTokens == nil || *chunk.Usage.TotalTokens != 150 {
				t.Fatalf("total = %+v", chunk.Usage.TotalTokens)
			}
		}
	}
	if usageCount != 1 {
		t.Fatalf("usage chunks = %d", usageCount)
	}
}

func TestTranslateMapUsageCacheSubtraction(t *testing.T) {
	cached := int64(40)
	usage := mapUsage(&WireUsage{PromptTokens: 100, CompletionTokens: 20, PromptTokensDetails: &struct {
		CachedTokens *int64 `json:"cached_tokens,omitempty"`
	}{CachedTokens: &cached}})
	if usage.InputTokens != 60 || usage.OutputTokens != 20 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.CacheReadTokens == nil || *usage.CacheReadTokens != 40 {
		t.Fatalf("cacheRead = %+v", usage.CacheReadTokens)
	}
	// Disagreeing wire total suppresses the exact total.
	disagreeing := int64(999)
	usage = mapUsage(&WireUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: &disagreeing})
	if usage.TotalTokens != nil {
		t.Fatalf("total = %+v", usage.TotalTokens)
	}
}

func TestTranslateEmptyResponse(t *testing.T) {
	payloads := SlicePayloads([]string{payloadChunk("", "stop", nil), Done})
	chunks := collectChunks(Translate(payloads))
	finish := chunks[len(chunks)-1]
	if finish.Reason == nil || finish.Reason.Kind != llm.FinishError ||
		finish.Reason.Failure == nil || finish.Reason.Failure.Code != llm.EmptyResponseCode {
		t.Fatalf("finish = %+v", finish.Reason)
	}
}

func TestTranslateMalformedPayload(t *testing.T) {
	payloads := SlicePayloads([]string{"{not json", Done})
	chunks := collectChunks(Translate(payloads))
	if len(chunks) != 1 || chunks[0].Type != llm.ChunkFinish ||
		chunks[0].Reason == nil || chunks[0].Reason.Failure == nil ||
		chunks[0].Reason.Failure.Code != "MALFORMED_RESPONSE" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestTranslateStreamClosedWithoutDone(t *testing.T) {
	payloads := SlicePayloads([]string{payloadChunk("x", "", nil)})
	chunks := collectChunks(Translate(payloads))
	last := chunks[len(chunks)-1]
	if last.Type != llm.ChunkFinish || last.Reason == nil || last.Reason.Failure == nil ||
		last.Reason.Failure.Code != "STREAM_CLOSED" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

// --- SSE framing ---------------------------------------------------------

func TestParseSseFraming(t *testing.T) {
	stream := "data: {\"a\":1}\r\n\r\ndata: {\"a\":2}\ndata: {\"a\":3}\n\n: keep-alive\nevent: ping\ndata: {\"a\":4}\n\ndata: [DONE]\n\n"
	var comments []string
	parser := ParseSse(strings.NewReader(stream), func(comment string) { comments = append(comments, comment) })
	var payloads []string
	for {
		payload, err := parser.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		payloads = append(payloads, payload)
	}
	want := []string{`{"a":1}`, `{"a":2}` + "\n" + `{"a":3}`, `{"a":4}`, Done}
	if len(payloads) != len(want) {
		t.Fatalf("payloads = %v", payloads)
	}
	for i := range want {
		if payloads[i] != want[i] {
			t.Fatalf("payloads[%d] = %q, want %q", i, payloads[i], want[i])
		}
	}
	if len(comments) != 1 || comments[0] != " keep-alive" {
		t.Fatalf("comments = %v", comments)
	}
}

func TestParseSseBOM(t *testing.T) {
	parser := ParseSse(strings.NewReader("\xEF\xBB\xBFdata: x\n\ndata: [DONE]\n"), nil)
	payload, err := parser.Next()
	if err != nil || payload != "x" {
		t.Fatalf("payload = %q, %v", payload, err)
	}
}

func TestParseSseTruncated(t *testing.T) {
	// Unterminated tail is truncation, never a flushable payload.
	parser := ParseSse(strings.NewReader("data: {\"a\":1}\n\ndata: partial-tail"), nil)
	if _, err := parser.Next(); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := parser.Next()
	if err == nil || failCode(err) != "STREAM_CLOSED" {
		t.Fatalf("err = %v", err)
	}
}

// --- options -------------------------------------------------------------

func TestResolveAdapterOptions(t *testing.T) {
	config := Config{BaseURL: "https://gw.internal", ReasoningEffort: "high"}
	connection, err := ResolveAdapterOptions(config, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if connection.BaseURL != "https://gw.internal" || connection.APIKeyEnv != DefaultAPIKeyEnv {
		t.Fatalf("connection = %+v", connection)
	}
	if connection.MaxTokens != DefaultMaxTokens || connection.DefaultContextWindow != DefaultContextWindow {
		t.Fatalf("defaults = %+v", connection)
	}
	if len(connection.Models) != 3 {
		t.Fatalf("models = %d", len(connection.Models))
	}
	if connection.RetryPolicy == nil || connection.RetryPolicy.Mode != llm.RetryModeNormal || connection.RetryPolicy.MaxRetries != 5 {
		t.Fatalf("retry policy = %+v", connection.RetryPolicy)
	}
	// Env fallback for the base URL.
	connection, err = ResolveAdapterOptions(Config{}, func(name string) (string, bool) {
		if name == BaseURLEnv {
			return "https://env-endpoint", true
		}
		return "", false
	})
	if err != nil || connection.BaseURL != "https://env-endpoint" {
		t.Fatalf("env baseURL = %+v, %v", connection, err)
	}
	// Public API default.
	connection, err = ResolveAdapterOptions(Config{}, nil)
	if err != nil || connection.BaseURL != PublicBaseURL {
		t.Fatalf("public baseURL = %+v, %v", connection, err)
	}
}

func TestResolveAdapterOptionsValidation(t *testing.T) {
	good := int64(100)
	cases := []struct {
		name   string
		config Config
		fails  string
	}{
		{"disabled thinking with effort", Config{Thinking: "disabled", ReasoningEffort: "high"}, "only reasoningEffort \"off\""},
		{"zero default context", Config{DefaultContextWindow: &good}, ""},
		{"bad model", Config{Models: []CatalogModel{{ID: ""}}}, "non-empty"},
		{"dup models", Config{Models: []CatalogModel{{ID: "m", Name: "M"}, {ID: "m", Name: "M"}}}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveAdapterOptions(tc.config, nil)
			if tc.fails == "" {
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.fails) {
				t.Fatalf("err = %v, want %q", err, tc.fails)
			}
		})
	}
	zero := int64(0)
	if _, err := ResolveAdapterOptions(Config{DefaultContextWindow: &zero}, nil); err == nil ||
		!strings.Contains(err.Error(), "defaultContextWindow") {
		t.Fatalf("zero context err = %v", err)
	}
}

// --- adapter over HTTP ---------------------------------------------------

func newTestAdapter(t *testing.T, connection *ConnectionOptions, client *http.Client) *Adapter {
	t.Helper()
	return NewAdapter(AdapterOptions{
		Options:       func() (*ConnectionOptions, error) { return connection, nil },
		ResolveAPIKey: func(*ConnectionOptions) (string, error) { return "test-key", nil },
		ResolveUserID: func() string { return "user-1" },
		HTTPClient:    client,
	})
}

func sseResponse(payloads ...string) string {
	body := ""
	for _, payload := range payloads {
		body += "data: " + payload + "\n\n"
	}
	return body
}

func TestAdapterStreamEndToEnd(t *testing.T) {
	var (
		gotAuth   string
		gotUA     string
		gotUserID string
		gotBody   map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotUserID = r.Header.Get("x-deepseek-harness-user-id")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sseResponse(payloadChunk("hi", "", nil), payloadChunk("", "stop", nil), Done))
	}))
	defer server.Close()

	connection := &ConnectionOptions{
		BaseURL: server.URL, APIKeyEnv: "TEST_KEY",
		Defaults:  RequestDefaults{Thinking: "enabled", ReasoningEffort: "high"},
		MaxTokens: DefaultMaxTokens, DefaultContextWindow: DefaultContextWindow,
		Models: defaultModels(), StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs,
	}
	connection.RetryPolicy, _ = llm.ResolveRetryPolicy(nil, "test")
	adapter := newTestAdapter(t, connection, server.Client())
	options := testOptions()
	chunks := collectChunks(adapter.Stream(options))
	if len(chunks) < 3 {
		t.Fatalf("chunks = %+v", chunks)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if !strings.HasPrefix(gotUA, "deepseek-harness/") {
		t.Fatalf("ua = %q", gotUA)
	}
	if gotUserID != "user-1" {
		t.Fatalf("user id = %q", gotUserID)
	}
	if gotBody["model"] != "deepseek-v4-pro" || gotBody["stream"] != true {
		t.Fatalf("body = %v", gotBody)
	}
	if thinking, ok := gotBody["thinking"].(map[string]any); !ok || thinking["type"] != "enabled" {
		t.Fatalf("body thinking = %v", gotBody["thinking"])
	}
	last := chunks[len(chunks)-1]
	if last.Type != llm.ChunkFinish || last.Reason == nil || last.Reason.Kind != llm.FinishStop {
		t.Fatalf("finish = %+v", last.Reason)
	}
}

func TestAdapterHTTPErrorFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("x-request-id", "req-42")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"throttled","code":"THROTTLE"}}`)
	}))
	defer server.Close()
	connection := &ConnectionOptions{BaseURL: server.URL, Models: defaultModels(), StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs, DefaultContextWindow: DefaultContextWindow, MaxTokens: DefaultMaxTokens}
	adapter := newTestAdapter(t, connection, server.Client())
	chunks := collectChunks(adapter.Stream(testOptions()))
	if len(chunks) != 1 || chunks[0].Type != llm.ChunkFinish {
		t.Fatalf("chunks = %+v", chunks)
	}
	failure := chunks[0].Reason.Failure
	if failure == nil || failure.Code != "RATE_LIMIT" || failure.Status != 429 ||
		failure.ProviderRetryAfterMs != 7000 || failure.RequestID != "req-42" || failure.Message != "rate limited" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestAdapterContextWindowClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"this model's maximum context length is 4096 tokens"}}`)
	}))
	defer server.Close()
	connection := &ConnectionOptions{BaseURL: server.URL, Models: defaultModels(), StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs, DefaultContextWindow: DefaultContextWindow, MaxTokens: DefaultMaxTokens}
	adapter := newTestAdapter(t, connection, server.Client())
	chunks := collectChunks(adapter.Stream(testOptions()))
	failure := chunks[0].Reason.Failure
	if failure == nil || failure.Code != llm.ContextWindowExceededCode {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestAdapterTruncatedSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer server.Close()
	connection := &ConnectionOptions{BaseURL: server.URL, Models: defaultModels(), StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs, DefaultContextWindow: DefaultContextWindow, MaxTokens: DefaultMaxTokens}
	adapter := newTestAdapter(t, connection, server.Client())
	chunks := collectChunks(adapter.Stream(testOptions()))
	last := chunks[len(chunks)-1]
	if last.Reason == nil || last.Reason.Failure == nil ||
		last.Reason.Failure.Code != "STREAM_CLOSED" {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func TestAdapterModelResolution(t *testing.T) {
	window := int64(131072)
	connection := &ConnectionOptions{
		BaseURL: "https://api.deepseek.com",
		Models: []CatalogModel{
			{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextWindow: &window},
		},
		DefaultContextWindow: DefaultContextWindow, MaxTokens: 4096,
		StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs,
	}
	adapter := newTestAdapter(t, connection, http.DefaultClient)
	resolved, err := adapter.ResolveModel("deepseek-official", "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ID != "deepseek-v4-pro" || resolved.Context == nil || resolved.Context.ContextWindow != 131072 {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.DefaultMaxTokens == nil || *resolved.DefaultMaxTokens != 4096 {
		t.Fatalf("defaultMaxTokens = %v", resolved.DefaultMaxTokens)
	}
	if resolved.Reasoning == nil || resolved.Reasoning.DefaultEffort != "high" || len(resolved.Reasoning.Efforts) != 4 {
		t.Fatalf("reasoning = %+v", resolved.Reasoning)
	}
	// Uncatalogued model: text-only fallback with the default context.
	resolved, err = adapter.ResolveModel("deepseek-official", "unknown-model")
	if err != nil {
		t.Fatalf("uncatalogued: %v", err)
	}
	if resolved.Name != "unknown-model" || resolved.Context.ContextWindow != DefaultContextWindow {
		t.Fatalf("uncatalogued = %+v", resolved)
	}
	// Thinking disabled collapses the effort list to off.
	connection.Defaults.Thinking = "disabled"
	resolved, _ = adapter.ResolveModel("deepseek-official", "deepseek-v4-pro")
	if resolved.Reasoning.DefaultEffort != "off" || len(resolved.Reasoning.Efforts) != 1 {
		t.Fatalf("off reasoning = %+v", resolved.Reasoning)
	}
}

func TestAdapterListModelsAndInfo(t *testing.T) {
	connection := &ConnectionOptions{BaseURL: "https://api.deepseek.com", Models: defaultModels(), StreamIdleTimeoutMs: DefaultStreamIdleTimeoutMs, DefaultContextWindow: DefaultContextWindow, MaxTokens: DefaultMaxTokens}
	connection.RetryPolicy, _ = llm.ResolveRetryPolicy(nil, "test")
	adapter := newTestAdapter(t, connection, http.DefaultClient)
	info := adapter.ProviderInfo("deepseek-official")
	if info.ID != "deepseek-official" || info.Name != "DeepSeek" {
		t.Fatalf("info = %+v", info)
	}
	models, err := adapter.ListModels("deepseek-official")
	if err != nil || len(models) != 3 {
		t.Fatalf("models = %v, %v", models, err)
	}
	if models[0].Name != "DeepSeek-V4-Flash" {
		t.Fatalf("models[0] = %+v", models[0])
	}
	policy := adapter.ProviderRetryPolicy("deepseek-official")
	if policy == nil || policy.Mode != llm.RetryModeNormal {
		t.Fatalf("policy = %+v", policy)
	}
}
