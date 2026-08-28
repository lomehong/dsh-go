package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMessageJSONRoundTripMatchesOfficialWire(t *testing.T) {
	usage := int64(42)
	message := NewToolResultMessage("call-1", []ContentBlock{{Type: BlockText, Text: "ok"}}, false)
	event := map[string]any{
		"type": "tool/result",
		"seq":  7,
		"data": map[string]any{
			"turn":    1,
			"step":    2,
			"message": message,
			"usage":   TokenUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: &usage},
		},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`"toolCallId":"call-1"`, `"role":"user"`, `"kind":"tool"`, `"callId":"call-1"`,
		`"inputTokens":10`, `"outputTokens":5`, `"totalTokens":42`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("wire field %s missing from %s", want, text)
		}
	}

	var back Message
	if err := json.Unmarshal([]byte(`{"id":"m1","role":"assistant","content":[{"type":"tool-call","id":"c1","name":"run","arguments":"{}"}],"source":{"kind":"model","provider":"deepseek","model":"chat"}}`), &back); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if back.Content[0].Type != BlockToolCall || back.Content[0].Arguments != "{}" || back.Source.Provider != "deepseek" {
		t.Fatalf("round trip wrong: %#v", back)
	}
}

func TestStreamChunkJSONVocabulary(t *testing.T) {
	chunks := []StreamChunk{
		{Type: ChunkBlockStart, Index: 0, BlockType: BlockText},
		{Type: ChunkTextDelta, Index: 0, Text: "你好"},
		{Type: ChunkToolCallDelta, Index: 1, ID: "call_9", Name: "fs", ArgumentsDelta: `{"p"`},
		{Type: ChunkUsage, Usage: &TokenUsage{InputTokens: 1, OutputTokens: 2}},
		{Type: ChunkFinish, Reason: &FinishReason{Kind: FinishToolCalls}},
	}
	data, err := json.Marshal(chunks)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var back []StreamChunk
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if back[0].BlockType != BlockText || back[2].ID != "call_9" || back[4].Reason.Kind != FinishToolCalls {
		t.Fatalf("chunk vocabulary wrong: %s", data)
	}
}

func TestCallConfigEqualsIsFieldWise(t *testing.T) {
	temp := 0.7
	a := LlmCallConfig{Provider: "deepseek", Model: "chat", Temperature: &temp, Stop: []string{"\n\n"}}
	same := a
	if !CallConfigEquals(a, same) {
		t.Fatal("identical configs must be equal")
	}
	otherTemp := 0.8
	same.Temperature = &otherTemp
	if CallConfigEquals(a, same) {
		t.Fatal("changed temperature must break equality")
	}
	reordered := a
	reordered.Stop = []string{"\n", "\n"}
	if CallConfigEquals(a, reordered) {
		t.Fatal("stop lists must compare element-wise")
	}
	if CallConfigEquals(LlmCallConfig{Stop: nil}, LlmCallConfig{Stop: []string{}}) {
		t.Fatal("absent stop must differ from present-empty stop")
	}
}

func TestBoundContextSummary(t *testing.T) {
	if got := BoundContextSummary("short"); got != "short" {
		t.Fatalf("short summaries pass through, got %q", got)
	}
	long := strings.Repeat("好", CONTEXT_SUMMARY_MAX_CHARS+10)
	got := BoundContextSummary(long)
	if runeCount := len([]rune(got)); runeCount != CONTEXT_SUMMARY_MAX_CHARS {
		t.Fatalf("bound must hold in runes, got %d", runeCount)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatal("overflow must ellipsize")
	}
}

func TestContextWindowClassifier(t *testing.T) {
	for _, detail := range []string{
		"This model's maximum context length is 4096 tokens",
		"context_length_exceeded",
		"Request too large for the model context window",
		"your prompt exceeds the model's context window",
	} {
		if !IsContextWindowExceededError(detail) {
			t.Fatalf("must classify as context overflow: %q", detail)
		}
	}
	if IsContextWindowExceededError("connection refused by upstream window") {
		t.Fatal("ordinary failures must not classify as context overflow")
	}
}

func TestQuotaClassifier(t *testing.T) {
	for _, detail := range []string{
		"Insufficient Balance",
		"quota exceeded for this account",
		"usage limit reached",
	} {
		if !IsQuotaExceededError(detail) {
			t.Fatalf("must classify as quota: %q", detail)
		}
	}
	if IsQuotaExceededError("rate limit exceeded, retry later") {
		t.Fatal("rate limits are not quota exhaustion")
	}
}

func TestErrorChainRendersWrapperPrefixOnce(t *testing.T) {
	leaf := errors.New("dial tcp: connection refused")
	wrapped := fmt.Errorf("post failed: %w", leaf)
	if got := ErrorChain(wrapped); got != "post failed: dial tcp: connection refused" {
		t.Fatalf("chain wrong: %q", got)
	}
	hErr := NewError(CodeNoAdapter, "no adapter for provider deepseek", wrapped)
	got := ErrorChain(hErr)
	if !strings.Contains(got, "no adapter for provider deepseek: post failed") {
		t.Fatalf("harness error chain wrong: %q", got)
	}
	if hErr.Code() != CodeNoAdapter {
		t.Fatal("code must survive")
	}
	if !errors.Is(hErr, leaf) {
		t.Fatal("cause chaining must keep errors.Is working")
	}
}

func TestErrorChainRendersJoinedMembers(t *testing.T) {
	joined := errors.Join(errors.New("first"), errors.New("second"))
	got := ErrorChain(joined)
	if got != " [first; second]" {
		t.Fatalf("joined members wrong: %q", got)
	}
}

func TestMessageConstructorsPinIdentityAndRoles(t *testing.T) {
	user := NewUserMessage([]ContentBlock{{Type: BlockText, Text: "hi"}}, MessageSource{})
	if user.Role != RoleUser || user.Source.Kind != SourceUser || user.ID == "" {
		t.Fatalf("user message wrong: %#v", user)
	}
	assistant := NewAssistantMessage([]ContentBlock{{Type: BlockText, Text: "yo"}}, "deepseek", "chat", nil)
	if assistant.Role != RoleAssistant || assistant.Source.Kind != SourceModel ||
		assistant.Source.Provider != "deepseek" || assistant.Source.Model != "chat" {
		t.Fatalf("assistant message wrong: %#v", assistant)
	}
	result := NewToolResultMessage("call-2", nil, true)
	block := result.Content[0]
	if result.Role != RoleUser || block.Type != BlockToolResult || !block.IsError || block.ToolCallID != "call-2" {
		t.Fatalf("tool result wrong: %#v", result)
	}
	if NewMessageID() == NewMessageID() {
		t.Fatal("ids must be unique")
	}
}
