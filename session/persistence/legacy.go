// Legacy stored-shape normalization: the coordinator upgrades the retired
// event shapes this build still reads, refuses the ones it does not, and
// rejects v0 vocabulary members that can no longer be replayed. Port of the
// coordinator's migration helpers (snapshotStoredEvents/adoptStoredEvents
// collapse into one pass: Go events are detached values by construction).
package persistence

import (
	"encoding/json"
	"fmt"

	"dshgo/session"
)

const legacySteeringType = "steering/message"
const legacyHeaderDeltaType = "request/header-delta"
const legacyModeSetType = "mode/set"

// assertSupportedEvents rejects events from an obsolete v0 vocabulary that
// this build cannot replay (write-side guard, shared by every append route).
func assertSupportedEvents(events []session.Event, id session.SessionID) error {
	for _, event := range events {
		switch event.Type {
		case legacyHeaderDeltaType:
			return fmt.Errorf("session %q contains unsupported legacy request/header-delta event at seq %d", id, event.Seq)
		case legacyModeSetType:
			return fmt.Errorf("session %q contains unsupported legacy mode/set event at seq %d", id, event.Seq)
		case session.EventRequestHeader:
			var data struct {
				Reason string `json:"reason"`
			}
			_ = json.Unmarshal(event.Data, &data)
			if data.Reason == "fallback" {
				return fmt.Errorf("session %q contains unsupported legacy request/header reason %q at seq %d", id, "fallback", event.Seq)
			}
		}
	}
	return nil
}

// asRecord decodes an event payload as an object record without widening
// arrays; nil means the payload is not an object.
func asRecord(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	record, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return record
}

// hasOnlyKeys reports whether a record contains every required key and no
// key outside the optional extension set.
func hasOnlyKeys(record map[string]any, required []string, optional ...string) bool {
	allowed := map[string]bool{}
	for _, key := range append(append([]string{}, required...), optional...) {
		allowed[key] = true
	}
	for key := range record {
		if !allowed[key] {
			return false
		}
	}
	for _, key := range required {
		if _, present := record[key]; !present {
			return false
		}
	}
	return true
}

// isSafeInteger reports whether a decoded value is a JSON integral number
// in the safe range.
func isSafeInteger(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number != float64(int64(number)) {
		return 0, false
	}
	return int64(number), true
}

// persistedMessageID mints the stable import identity for a message
// persisted before identities existed.
func legacyMessageID(id session.SessionID, seq int64) string {
	return fmt.Sprintf("legacy-message:%s:%d", id, seq)
}

// needsLegacyPrefix reports whether one suffix event needs facts available
// only from the preceding stored prefix.
func needsLegacyPrefix(event session.Event) bool {
	if event.Type == legacySteeringType {
		return true
	}
	data := asRecord(event.Data)
	if data == nil {
		return false
	}
	_, hasID := data["id"]
	_, hasContent := data["content"]
	_, hasMessage := data["message"]
	_, hasCallID := data["callId"]
	switch event.Type {
	case session.EventUserMessage:
		return !hasID && hasContent
	case session.EventAssistantMsg:
		return !hasMessage && hasContent
	case session.EventToolResult:
		return !hasMessage && hasCallID
	default:
		return false
	}
}

// migrateLegacySteeringEvent upgrades the removed steering surface event
// into its current user-message equivalent.
func migrateLegacySteeringEvent(event session.Event, id session.SessionID) (session.Event, error) {
	if event.Type != legacySteeringType {
		return event, nil
	}
	data := asRecord(event.Data)
	if data == nil {
		return event, fmt.Errorf("session %q contains malformed pre-react-loop steering/message at seq %d", id, event.Seq)
	}
	malformed := fmt.Errorf("session %q contains malformed pre-react-loop steering/message at seq %d", id, event.Seq)
	turn, turnOK := isSafeInteger(data["turn"])
	_ = turn
	if wrapped, ok := data["message"].(map[string]any); ok && turnOK && hasOnlyKeys(data, []string{"turn", "message"}) {
		return retypeEvent(event, session.EventUserMessage, wrapped)
	}
	_, hasContent := data["content"]
	_, hasSource := data["source"]
	if !turnOK || !hasContent || !hasSource || !hasOnlyKeys(data, []string{"turn", "content", "source"}) {
		return event, malformed
	}
	upgraded := map[string]any{
		"id":      legacyMessageID(id, event.Seq),
		"role":    "user",
		"content": data["content"],
		"source":  data["source"],
	}
	return retypeEvent(event, session.EventUserMessage, upgraded)
}

// migrateLegacyTurnStartEvent removes the obsolete trigger after verifying
// the complete old turn-start envelope.
func migrateLegacyTurnStartEvent(event session.Event, id session.SessionID) (session.Event, error) {
	if event.Type != session.EventTurnStart {
		return event, nil
	}
	data := asRecord(event.Data)
	if data == nil {
		return event, nil
	}
	if _, hasTrigger := data["trigger"]; !hasTrigger {
		return event, nil
	}
	turn, turnOK := isSafeInteger(data["turn"])
	trigger, triggerIsRecord := data["trigger"].(map[string]any)
	kind, _ := trigger["kind"].(string)
	if !turnOK || turn < 1 || !hasOnlyKeys(data, []string{"turn", "trigger"}) ||
		!triggerIsRecord || kind == "" {
		return event, fmt.Errorf("session %q contains malformed pre-react-loop turn/start at seq %d", id, event.Seq)
	}
	return retypeEvent(event, event.Type, map[string]any{"turn": float64(turn)})
}

// migrateLegacyTurnEndEvent upgrades an obsolete turn ending while
// preserving the latest-master envelope.
func migrateLegacyTurnEndEvent(event session.Event, id session.SessionID) (session.Event, error) {
	if event.Type != session.EventTurnEnd {
		return event, nil
	}
	data := asRecord(event.Data)
	if data == nil {
		return event, nil
	}
	malformed := fmt.Errorf("session %q contains malformed pre-react-loop turn/end at seq %d", id, event.Seq)
	turn, turnOK := isSafeInteger(data["turn"])
	reason, reasonIsRecord := data["reason"].(map[string]any)
	if !turnOK || turn < 1 || !hasOnlyKeys(data, []string{"turn", "reason"}) || !reasonIsRecord {
		return event, malformed
	}
	reasonKind, _ := reason["kind"].(string)
	var currentReason map[string]any
	switch reasonKind {
	case session.TurnEndCompleted, session.TurnEndBlocked, session.TurnEndMaxTokens, session.TurnEndInterrupted:
		if !hasOnlyKeys(reason, []string{"kind"}) {
			return event, malformed
		}
		return event, nil
	case session.TurnEndAborted:
		if _, hasReason := reason["reason"]; hasReason {
			return event, nil
		}
		if !hasOnlyKeys(reason, []string{"kind"}) {
			return event, malformed
		}
		currentReason = map[string]any{"kind": session.TurnEndAborted, "reason": map[string]any{"kind": session.CancelLegacy}}
	case "disposed":
		if !hasOnlyKeys(reason, []string{"kind"}) {
			return event, malformed
		}
		currentReason = map[string]any{"kind": session.TurnEndAborted, "reason": map[string]any{"kind": session.CancelDisposed}}
	case session.TurnEndError:
		if _, hasError := reason["error"]; hasError {
			return event, nil
		}
		step, stepOK := isSafeInteger(reason["step"])
		failure, failureIsRecord := reason["failure"].(map[string]any)
		if stepOK && step >= 0 && failureIsRecord &&
			hasOnlyKeys(reason, []string{"kind", "step", "failure"}) &&
			hasOnlyKeys(failure, []string{"message", "code"}, "status", "providerRetryAfterMs", "requestId") &&
			stringValue(failure["message"]) != nil && stringValue(failure["code"]) != nil {
			currentReason = map[string]any{"kind": session.TurnEndError, "error": failure}
			break
		}
		keys := []string{"kind", "step", "message"}
		if _, hasCode := reason["code"]; hasCode {
			keys = append(keys, "code")
		}
		if !hasOnlyKeys(reason, keys) || !stepOK || step < 0 || stringValue(reason["message"]) == nil {
			return event, malformed
		}
		code := "UNKNOWN"
		if rawCode := stringValue(reason["code"]); rawCode != nil {
			code = *rawCode
		}
		currentReason = map[string]any{
			"kind":  session.TurnEndError,
			"error": map[string]any{"message": *stringValue(reason["message"]), "code": code},
		}
	default:
		return event, nil
	}
	upgraded := map[string]any{"turn": float64(turn), "reason": currentReason}
	return retypeEvent(event, event.Type, upgraded)
}

// stringValue narrows a decoded value to a string.
func stringValue(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	return &text
}

// retypeEvent replaces one event's payload (and optionally its type) while
// preserving the envelope.
func retypeEvent(event session.Event, eventType string, data map[string]any) (session.Event, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return event, fmt.Errorf("session event at seq %d failed migration re-encode: %w", event.Seq, err)
	}
	event.Type = eventType
	event.Data = encoded
	return event, nil
}

// migrateLegacyMessageEvent upgrades one pre-identity message event into
// the current wrapper shape. Current-looking malformed events remain
// untouched so validation rejects them instead of disguising corruption as
// legacy data.
func migrateLegacyMessageEvent(event session.Event, id session.SessionID, messageIDs map[int64]string) (session.Event, error) {
	data := asRecord(event.Data)
	if data == nil {
		return event, nil
	}
	_, hasID := data["id"]
	_, hasRole := data["role"]
	_, hasMessage := data["message"]
	_, hasContent := data["content"]
	_, hasSource := data["source"]
	_, hasProvenance := data["provenance"]
	_, hasCallID := data["callId"]
	switch event.Type {
	case session.EventUserMessage:
		if hasID || hasRole || hasMessage || !hasContent || !hasSource {
			return event, nil
		}
		upgraded := map[string]any{
			"id":      legacyMessageID(id, event.Seq),
			"role":    "user",
			"content": data["content"],
			"source":  data["source"],
		}
		return retypeEvent(event, event.Type, upgraded)
	case session.EventAssistantMsg:
		if hasMessage || !hasContent || !hasProvenance {
			return event, nil
		}
		provenance, _ := data["provenance"].(map[string]any)
		if provenance == nil {
			provenance = map[string]any{}
		}
		provenance["kind"] = "model"
		upgraded := map[string]any{
			"message": map[string]any{
				"id":      legacyMessageID(id, event.Seq),
				"role":    "assistant",
				"content": data["content"],
				"source":  provenance,
			},
		}
		return retypeEvent(event, event.Type, upgraded)
	case session.EventToolResult:
		if hasMessage || !hasCallID {
			return event, nil
		}
		callID, _ := data["callId"].(string)
		content := data["content"]
		isError := data["isError"]
		resultBlock := map[string]any{
			"type":       "tool-result",
			"toolCallId": callID,
			"content":    content,
		}
		if isError != nil {
			resultBlock["isError"] = isError
		}
		var messageID any
		if start, ok := surfaceReplaceStart(event); ok {
			if inherited, known := messageIDs[int64(start)]; known {
				messageID = inherited
			}
		}
		if messageID == nil {
			messageID = legacyMessageID(id, event.Seq)
		}
		upgraded := map[string]any{
			"message": map[string]any{
				"id":      messageID,
				"role":    "user",
				"content": []any{resultBlock},
				"source":  map[string]any{"kind": "tool", "callId": callID},
			},
		}
		return retypeEvent(event, event.Type, upgraded)
	default:
		return event, nil
	}
}

// surfaceReplaceStart reads the replacement start from a tool-result's
// surface op.
func surfaceReplaceStart(event session.Event) (float64, bool) {
	if event.SurfaceOp == nil || event.SurfaceOp.Kind != session.SurfaceReplace {
		return 0, false
	}
	return float64(event.SurfaceOp.Start), true
}

// eventMessageID reads the identified message carried by one validated
// current event.
func eventMessageID(event session.Event) (string, bool) {
	data := asRecord(event.Data)
	if data == nil {
		return "", false
	}
	var message map[string]any
	if event.Type == session.EventUserMessage {
		message = data
	} else {
		message, _ = data["message"].(map[string]any)
	}
	if message == nil {
		return "", false
	}
	id, ok := message["id"].(string)
	return id, ok
}

// normalizeStoredEvents materializes stored events as upgraded, validated
// events with immutable messages (official snapshotStoredEvents and
// adoptStoredEvents collapse: Go events are detached values, so one pass
// freezes by construction).
func normalizeStoredEvents(events []session.Event, id session.SessionID) ([]session.Event, error) {
	if err := assertSupportedEvents(events, id); err != nil {
		return nil, err
	}
	messageIDs := map[int64]string{}
	normalized := make([]session.Event, 0, len(events))
	for _, event := range events {
		var err error
		event, err = migrateLegacyTurnStartEvent(event, id)
		if err == nil {
			event, err = migrateLegacyTurnEndEvent(event, id)
		}
		if err == nil {
			event, err = migrateLegacySteeringEvent(event, id)
		}
		if err == nil {
			event, err = migrateLegacyMessageEvent(event, id, messageIDs)
		}
		if err != nil {
			return nil, err
		}
		// Session validator: fail loud on a payload this build cannot read.
		var payload any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("session %q event %q at seq %d has undecodable data: %w", id, event.Type, event.Seq, err)
		}
		if err := session.ValidateEventJSON(event.Type, payload); err != nil {
			return nil, err
		}
		if messageID, ok := eventMessageID(event); ok {
			messageIDs[event.Seq] = messageID
		}
		normalized = append(normalized, event)
	}
	return normalized, nil
}
