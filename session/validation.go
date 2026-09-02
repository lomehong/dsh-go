// Event and header validation: the load/seed boundary checks the official
// Session class applies, enforced identically on the Go append path (Go has
// no per-type compiler face, so runtime validation is the boundary).
package session

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"dshgo/llm"
)

// validateSessionHeader checks storage metadata against the session it
// names. The header is detached immutable metadata: values are validated
// once here, never re-derived.
func validateSessionHeader(id SessionID, header SessionHeader) error {
	if header.Version != SESSION_FORMAT_VERSION {
		return fmt.Errorf("session header for %q has format version %d; this build stores %d only (no migration)", id, header.Version, SESSION_FORMAT_VERSION)
	}
	if header.ID != id {
		return fmt.Errorf("session header id %q does not match requested id %q", header.ID, id)
	}
	if header.CreatedAt < 0 {
		return fmt.Errorf("session header for %q has negative createdAt %d", id, header.CreatedAt)
	}
	if header.CWD != "" && !filepath.IsAbs(header.CWD) {
		return fmt.Errorf("session header for %q has a non-absolute cwd %q", id, header.CWD)
	}
	if header.InheritedEventCount < 0 {
		return fmt.Errorf("session header for %q has negative inheritedEventCount %d", id, header.InheritedEventCount)
	}
	if !header.IsSeeded && header.InheritedEventCount != 0 {
		return fmt.Errorf("session header for %q is unseeded but has inheritedEventCount %d (must be 0)", id, header.InheritedEventCount)
	}
	if header.Origin != "" && header.Origin != "subagent" {
		return fmt.Errorf("session header for %q has an unknown origin %q", id, header.Origin)
	}
	if header.DelegationDepth != nil && *header.DelegationDepth < 0 {
		return fmt.Errorf("session header for %q has negative delegationDepth %d", id, *header.DelegationDepth)
	}
	return nil
}

// validateSeedEvent validates one borrowed event entering at seed
// construction.
func validateSeedEvent(event Event, index int) error {
	if event.Type == "" {
		return fmt.Errorf("seed event at index %d is missing its type", index)
	}
	if event.Seq < 0 || event.Time < 0 {
		return fmt.Errorf("seed event at index %d (%s) carries negative seq or time", index, event.Type)
	}
	if event.Data == nil || !json.Valid(event.Data) {
		return fmt.Errorf("seed event at index %d (%s) is missing valid data", index, event.Type)
	}
	if err := assertSupportedRequestHeader(event.Type, event.Data); err != nil {
		return err
	}
	return assertMessageEventShape(event)
}

// assertSupportedRequestHeader rejects request-header vocabulary removed
// with the legacy delta codec.
func assertSupportedRequestHeader(eventType string, data json.RawMessage) error {
	if eventType == "request/header-delta" {
		return fmt.Errorf("event uses unsupported legacy request/header-delta format")
	}
	if eventType == EventRequestHeader {
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err == nil {
			if reason, _ := decoded["reason"].(string); reason == "fallback" {
				return fmt.Errorf("event uses unsupported legacy request/header reason \"fallback\"")
			}
		}
	}
	return nil
}

// assertMessageEventShape enforces the message-carrying payloads' identity
// and role invariants.
func assertMessageEventShape(event Event) error {
	switch event.Type {
	case EventUserMessage:
		message, err := DecodeUserMessage(event)
		if err != nil {
			return err
		}
		if message.ID == "" || message.Role != llm.RoleUser || message.Source.Kind == "" || message.Content == nil {
			return fmt.Errorf("user/message event must carry an identified user message")
		}
		return nil
	case EventAssistantMsg:
		data, err := DecodeAssistantMessage(event)
		if err != nil {
			return err
		}
		if data.Message.ID == "" || data.Message.Role != llm.RoleAssistant ||
			data.Message.Source.Kind != llm.SourceModel ||
			data.Message.Source.Provider == "" || data.Message.Source.Model == "" ||
			data.Message.Content == nil {
			return fmt.Errorf("assistant/message event must carry a model-sourced assistant message")
		}
		return nil
	case EventToolResult:
		data, err := DecodeToolResult(event)
		if err != nil {
			return err
		}
		message := data.Message
		if message.ID == "" || message.Role != llm.RoleUser || message.Source.Kind != llm.SourceTool ||
			message.Source.CallID == "" {
			return fmt.Errorf("tool/result event must carry a tool-sourced user message")
		}
		if len(message.Content) != 1 || message.Content[0].Type != llm.BlockToolResult {
			return fmt.Errorf("tool/result event must contain one tool-result block")
		}
		if message.Content[0].ToolCallID != message.Source.CallID {
			return fmt.Errorf("tool/result event has mismatched tool call ids")
		}
		return nil
	default:
		return nil
	}
}
