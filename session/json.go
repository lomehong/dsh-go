package session

import (
	"encoding/json"
	"errors"
	"fmt"
)

// IsJsonValue reports whether a Go value is a lossless JSON payload in the
// canonical shapes this codebase produces: nil, bool, string, integers,
// float64, []any, and map[string]any — recursively. A non-serializable
// payload is rejected at the source (Session.Append), so the durable log
// reproduces identical data on replay.
func IsJsonValue(value any) bool {
	switch typed := value.(type) {
	case nil, bool, string, int, int64, float64:
		return true
	case []any:
		for _, item := range typed {
			if !IsJsonValue(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range typed {
			if !IsJsonValue(item) {
				return false
			}
		}
		return true
	case json.RawMessage:
		return json.Valid(typed)
	case json.Number:
		return true
	default:
		// Structs, pointers, and other Go shapes are not the canonical log
		// vocabulary; callers marshal them first. Reject loudly at the
		// source instead of guessing an encoding.
		return false
	}
}

// ValidateEventJSON enforces the canonical-JSON rule on one event's payload
// before it enters the log.
func ValidateEventJSON(eventType string, data any) error {
	if !IsJsonValue(data) {
		return fmt.Errorf("session: event %s data is not JSON-serializable (%T); marshal it to the canonical shapes first",
			eventType, data)
	}
	encoded, err := json.Marshal(data)
	if err == nil && !json.Valid(encoded) {
		err = errors.New("encoded payload is not valid JSON")
	}
	if err != nil {
		return fmt.Errorf("session: event %s data failed to encode as JSON: %w", eventType, err)
	}
	return nil
}

// DeepCopyValue clones a canonical JSON value so callers can mutate their
// copy without aliasing logged state.
func DeepCopyValue(value any) any {
	if !IsJsonValue(value) {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return value
	}
	return decoded
}

// DeepCopyHeader clones one header (json round trip; the shape is frozen).
func DeepCopyHeader(header SessionHeader) SessionHeader {
	encoded, err := json.Marshal(header)
	if err != nil {
		panic(fmt.Sprintf("session: header must always marshal: %v", err))
	}
	var decoded SessionHeader
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		panic(fmt.Sprintf("session: header must always unmarshal: %v", err))
	}
	return decoded
}

// DeepCopyEvent clones one event envelope including its raw payload.
func DeepCopyEvent(event Event) Event {
	event.Data = json.RawMessage(append([]byte(nil), event.Data...))
	if event.SourceEventSeqs != nil {
		event.SourceEventSeqs = append([]int64(nil), event.SourceEventSeqs...)
	}
	if event.SurfaceOp != nil {
		op := *event.SurfaceOp
		event.SurfaceOp = &op
	}
	return event
}
