package anthropic

import (
	"errors"
	"strings"
	"testing"

	"dshgo/llm"
)

// options returns the minimal connection facts for buildRequest.
func options() *Options {
	return &Options{BaseURL: "https://example.test", MaxTokens: 4096}
}

// unsupported asserts err is an UNSUPPORTED_CONTENT LlmError.
func unsupported(t *testing.T, err error) {
	t.Helper()
	var llmErr *llm.LlmError
	if !errors.As(err, &llmErr) || llmErr.Code() != "UNSUPPORTED_CONTENT" {
		t.Fatalf("err = %v, want UNSUPPORTED_CONTENT LlmError", err)
	}
}

// The r137 multi-provider round landed the text/tool wire only; image
// blocks must fail the request with UNSUPPORTED_CONTENT — a silent drop
// would replay a history the model never saw while read_image keeps
// producing durable image blocks (r139 audit finding).
func TestProjectMessageRejectsImageBlocks(t *testing.T) {
	message := llm.Message{
		Role: llm.RoleUser,
		Content: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "look at this"},
			{Type: llm.BlockImage, Attachment: map[string]any{"mediaType": "image/png", "ref": "att-1"}},
		},
	}
	_, err := projectMessage(message)
	if err == nil {
		t.Fatal("image block projected without error")
	}
	unsupported(t, err)
}

// File blocks never reach an adapter (ProjectFilesToText projects them to
// handle text before dispatch); reaching the projection is a composition
// bug and must refuse, not erase.
func TestProjectMessageRejectsFileBlocks(t *testing.T) {
	message := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: llm.BlockFile}},
	}
	_, err := projectMessage(message)
	unsupported(t, err)
}

// Unknown block types fail loud with the offending type named — never the
// silent switch fall-through that erases them.
func TestProjectMessageRejectsUnknownBlockTypes(t *testing.T) {
	message := llm.Message{
		Role:    llm.RoleUser,
		Content: []llm.ContentBlock{{Type: "hologram"}},
	}
	_, err := projectMessage(message)
	unsupported(t, err)
	if !strings.Contains(err.Error(), "hologram") {
		t.Fatalf("err = %v, want the offending block type named", err)
	}
}

// buildRequest propagates the first projection failure; the zero request
// must never reach serialization.
func TestBuildRequestPropagatesProjectionFailure(t *testing.T) {
	_, err := buildRequest(llm.GenerateOptions{
		Model: "claude-test",
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockImage}}},
		},
	}, options())
	unsupported(t, err)
}

// The text/tool wire keeps projecting: system renders top-level, tool
// calls become tool_use, tool results become tool_result.
func TestBuildRequestProjectsTextAndToolWire(t *testing.T) {
	request, err := buildRequest(llm.GenerateOptions{
		Model: "claude-test",
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "sys"}}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "run it"}}},
			{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockToolCall, ID: "call-1", Name: "read", Arguments: "{}"},
			}},
			{Role: llm.RoleUser, Content: []llm.ContentBlock{
				{Type: llm.BlockToolResult, ToolCallID: "call-1",
					Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "done"}}},
			}},
		},
	}, options())
	if err != nil {
		t.Fatalf("buildRequest: %v", err)
	}
	if len(request.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (system renders top-level)", len(request.Messages))
	}
	if request.Messages[1].Role != "assistant" || !strings.Contains(string(request.Messages[1].Content), `"tool_use"`) {
		t.Fatalf("assistant wire = %s/%s", request.Messages[1].Role, request.Messages[1].Content)
	}
	if !strings.Contains(string(request.Messages[2].Content), `"tool_result"`) {
		t.Fatalf("tool result wire = %s", request.Messages[2].Content)
	}
}
