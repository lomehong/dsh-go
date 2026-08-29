package sessionquery

import (
	"encoding/json"
	"strings"

	"dshgo/llm"
	session "dshgo/session"
)

// eventTodoWrite is the first-party todo event consumed below (the todo
// package's append constant; imported by value shape, not by dependency).
const eventTodoWrite = "todo/write"

// ExtractSessionEventText extracts searchable semantic text from one
// first-party session event. Structural boundaries, raw stream chunks,
// request envelopes, and unknown declaration-merged events contribute no
// text.
func ExtractSessionEventText(event session.Event) string {
	switch event.Type {
	case session.EventUserMessage:
		message, err := session.DecodeUserMessage(event)
		if err != nil {
			return ""
		}
		return contentText(message.Content)
	case session.EventAssistantMsg:
		payload, err := session.DecodeAssistantMessage(event)
		if err != nil {
			return ""
		}
		return contentText(payload.Message.Content)
	case session.EventToolCall:
		var payload session.ToolCallData
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return ""
		}
		return joinText([]string{payload.Name, payload.Arguments})
	case session.EventToolResult:
		payload, err := session.DecodeToolResult(event)
		if err != nil {
			return ""
		}
		name, code := "", ""
		if payload.Error != nil {
			name, code = payload.Error.Name, payload.Error.Code
		}
		return joinText([]string{contentText(payload.Message.Content), name, code})
	case eventTodoWrite:
		var payload struct {
			Todos []struct {
				Status  string `json:"status"`
				Content string `json:"content"`
			} `json:"todos"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return ""
		}
		parts := make([]string, 0, len(payload.Todos)*2)
		for _, todo := range payload.Todos {
			parts = append(parts, todo.Status, todo.Content)
		}
		return joinText(parts)
	case session.EventTurnEnd:
		var payload session.TurnEndData
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return ""
		}
		return turnEndText(payload.Reason)
	default:
		// SessionEventMap is merge-extensible. Unknown events remain
		// non-searchable until a concrete first-party consumer defines
		// semantics.
		return ""
	}
}

func turnEndText(reason session.TurnEndReason) string {
	switch reason.Kind {
	case "error":
		message := ""
		if reason.Error != nil {
			message = reason.Error.Message
		}
		return joinText([]string{"error", message})
	case "aborted":
		return "aborted"
	case "max-tokens", "interrupted":
		return reason.Kind
	case "completed":
		return ""
	default:
		// TurnEndReasonMap is merge-extensible. Unknown outcomes stay out
		// until their owner defines which detail is semantic rather than
		// structural.
		return ""
	}
}

func contentText(content []llm.ContentBlock) string {
	parts := make([]string, 0, len(content))
	for _, block := range content {
		parts = append(parts, blockText(block)...)
	}
	return joinText(parts)
}

func blockText(block llm.ContentBlock) []string {
	switch block.Type {
	case llm.BlockText:
		return []string{block.Text}
	case llm.BlockReasoning:
		return nil
	case llm.BlockToolCall:
		return []string{block.Name, block.Arguments}
	case llm.BlockToolResult:
		text := contentText(block.Content)
		if text == "" {
			return nil
		}
		return []string{text}
	default:
		// ContentBlockMap is merge-extensible. Unknown blocks do not become
		// searchable merely because their payload happens to contain
		// strings.
		return nil
	}
}

func joinText(parts []string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, "\n")
}
