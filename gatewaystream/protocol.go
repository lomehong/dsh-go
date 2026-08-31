// Package gatewaystream ports packages/api/gateway/src/stream-protocol.ts:
// the wire messages and validation for Gateway-owned Remote streams and
// event-result RPCs. Pure lossless-JSON logic, portable verbatim.
package gatewaystream

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Wire constants (verbatim).
const (
	RemoteStreamMuxPath       = "/api/remote.mux"
	RemoteEventStreamEndpoint = "$events"
	RemoteEventResultEndpoint = "$events/result"
)

// RemoteEventHostInfo is the stable Host facts published with every
// established Client event generation.
type RemoteEventHostInfo struct {
	// Home is the Host account home used only to abbreviate displayed
	// filesystem paths.
	Home string `json:"home"`
}

// RemoteEventReadyFrame is the opening item binding later HTTP results to
// this active event stream.
type RemoteEventReadyFrame struct {
	Type     string              `json:"type"`
	ClientID string              `json:"clientId"`
	Host     RemoteEventHostInfo `json:"host"`
}

// RemoteEventEmitFrame is one Host notification delivered to a Client
// generation.
type RemoteEventEmitFrame struct {
	Type  string `json:"type"`
	Event string `json:"event"`
	Args  []any  `json:"args"`
}

// RemoteEventInvocationFrame is one pending Agent-scoped waterfall
// delivered to a Client generation.
type RemoteEventInvocationFrame struct {
	Type    string         `json:"type"`
	Event   string         `json:"event"`
	EventID string         `json:"eventId"`
	AgentID string         `json:"agentId"`
	Request map[string]any `json:"request"`
}

// RemoteEventCancellationFrame cancels a pending waterfall previously
// delivered under the same id.
type RemoteEventCancellationFrame struct {
	Type    string `json:"type"`
	EventID string `json:"eventId"`
}

// RemoteEventRejection is the wire-safe error fields retained when a Client
// listener rejects a Host waterfall.
type RemoteEventRejection struct {
	Name    string `json:"name"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

// RemoteEventOutcome is one Client response outcome.
type RemoteEventOutcome struct {
	Kind  string                `json:"kind"` // next | result | rejected
	Value any                   `json:"value,omitempty"`
	Error *RemoteEventRejection `json:"error,omitempty"`
}

// RemoteEventResult is the Client response to one scoped Remote Event
// delivery.
type RemoteEventResult struct {
	ClientID string             `json:"clientId"`
	EventID  string             `json:"eventId"`
	Outcome  RemoteEventOutcome `json:"outcome"`
}

// ParseRemoteEventResult validates one untrusted result payload.
func ParseRemoteEventResult(value any) (RemoteEventResult, error) {
	record, ok := value.(map[string]any)
	if !ok || !exactKeys(record, "clientId", "eventId", "outcome") {
		return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
	}
	clientID, _ := record["clientId"].(string)
	eventID, _ := record["eventId"].(string)
	if clientID == "" || eventID == "" {
		return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
	}
	outcome, ok := record["outcome"].(map[string]any)
	if !ok {
		return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
	}
	kind, _ := outcome["kind"].(string)
	switch kind {
	case "next":
		if !exactKeys(outcome, "kind") {
			return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
		}
		return RemoteEventResult{ClientID: clientID, EventID: eventID, Outcome: RemoteEventOutcome{Kind: "next"}}, nil
	case "result":
		if !exactKeys(outcome, "kind") && !exactKeys(outcome, "kind", "value") {
			return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
		}
		value, hasValue := outcome["value"]
		if hasValue && !isRemoteJSONValue(value) {
			return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
		}
		return RemoteEventResult{ClientID: clientID, EventID: eventID, Outcome: RemoteEventOutcome{Kind: "result", Value: value}}, nil
	case "rejected":
		if !exactKeys(outcome, "kind", "error") {
			return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
		}
		rejection, err := ParseRemoteEventRejection(outcome["error"])
		if err != nil {
			return RemoteEventResult{}, err
		}
		return RemoteEventResult{ClientID: clientID, EventID: eventID, Outcome: RemoteEventOutcome{Kind: "rejected", Error: &rejection}}, nil
	default:
		return RemoteEventResult{}, fmt.Errorf("api gateway: invalid Remote event result")
	}
}

// ParseRemoteEventRejection validates the wire-safe error fields.
func ParseRemoteEventRejection(value any) (RemoteEventRejection, error) {
	record, ok := value.(map[string]any)
	if !ok || !exactKeys(record, "name", "message") &&
		!exactKeys(record, "name", "message", "code") &&
		!exactKeys(record, "name", "message", "code", "details") &&
		!exactKeys(record, "name", "message", "details") {
		return RemoteEventRejection{}, fmt.Errorf("api gateway: invalid Remote event rejection")
	}
	name, _ := record["name"].(string)
	message, _ := record["message"].(string)
	if name == "" || message == "" {
		return RemoteEventRejection{}, fmt.Errorf("api gateway: invalid Remote event rejection")
	}
	rejection := RemoteEventRejection{Name: name, Message: message}
	if code, ok := record["code"].(string); ok {
		rejection.Code = code
	}
	if details, ok := record["details"]; ok && isRemoteJSONValue(details) {
		rejection.Details = details
	}
	return rejection, nil
}

// ProjectRemoteEventRejection projects an arbitrary rejection to stable,
// JSON-safe error fields.
func ProjectRemoteEventRejection(reason any) RemoteEventRejection {
	record, isRecord := reason.(map[string]any)
	rejection := RemoteEventRejection{Name: "Error"}
	if isRecord {
		if name, ok := record["name"].(string); ok && name != "" {
			rejection.Name = name
		}
	}
	if isRecord {
		if message, ok := record["message"].(string); ok && message != "" {
			rejection.Message = message
		}
	}
	if rejection.Message == "" {
		rejection.Message = fmt.Sprintf("%v", reason)
	}
	if isRecord {
		if code, ok := record["code"].(string); ok {
			rejection.Code = code
		}
		if details, ok := record["details"]; ok && isRemoteJSONValue(details) {
			rejection.Details = details
		}
	}
	return rejection
}

// RestoreRemoteEventRejection recreates a Client rejection as an Error
// preserving the remote name, code, and JSON-safe details.
func RestoreRemoteEventRejection(rejection RemoteEventRejection) error {
	return &RemoteEventRestoredError{name: rejection.Name, message: rejection.Message, code: rejection.Code, details: rejection.Details}
}

// RemoteEventRestoredError is the restored Client rejection.
type RemoteEventRestoredError struct {
	name    string
	message string
	code    string
	details any
}

func (e *RemoteEventRestoredError) Error() string { return e.message }
func (e *RemoteEventRestoredError) Name() string  { return e.name }
func (e *RemoteEventRestoredError) Code() string  { return e.code }
func (e *RemoteEventRestoredError) Details() any  { return e.details }

// IsRemoteJSONValue reports whether a value crosses JSON transport without
// coercion or omission.
func isRemoteJSONValue(value any) bool {
	return visitJSONValue(value, map[uintptr]bool{})
}

func visitJSONValue(value any, seen map[uintptr]bool) bool {
	switch typed := value.(type) {
	case nil, bool, string:
		return true
	case float64, int, int64, uint64:
		return true
	case []any:
		ptr := reflect.ValueOf(value).Pointer()
		if seen[ptr] {
			return false
		}
		seen[ptr] = true
		for _, item := range typed {
			if !visitJSONValue(item, seen) {
				return false
			}
		}
		return true
	case map[string]any:
		ptr := reflect.ValueOf(value).Pointer()
		if seen[ptr] {
			return false
		}
		seen[ptr] = true
		for _, item := range typed {
			if !visitJSONValue(item, seen) {
				return false
			}
		}
		return true
	default:
		// Reject non-JSON shapes (structs, pointers, functions, channels).
		rv := reflect.ValueOf(value)
		if rv.IsValid() && (rv.Kind() == reflect.Ptr || rv.Kind() == reflect.Struct) {
			return false
		}
		// Types that round-trip through JSON (e.g. json.Number, time) are
		// rejected conservatively.
		return false
	}
}

// exactKeys reports whether a record has exactly the given keys.
func exactKeys(record map[string]any, keys ...string) bool {
	if len(record) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := record[key]; !ok {
			return false
		}
	}
	return true
}

// MarshalJSON is a helper for wire frame encoding.
func marshalFrame(frame any) ([]byte, error) { return json.Marshal(frame) }

var _ = strings.TrimSpace
