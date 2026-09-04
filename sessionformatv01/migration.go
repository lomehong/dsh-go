package sessionformatv01

import (
	"encoding/json"

	"dshgo/sessionformat"
)

// Identity format edge that promotes released v0 into released v1. Port of
// packages/session/session-format-v0-to-v1/src/migration.ts.

// MigrationName is the edge's stable identity.
const MigrationName = "@deepseek-ai/dsh-session-format-v0-to-v1"

// Migration is the compiled v0 -> v1 edge.
type Migration struct{}

// NewMigration returns the adjacent v0 -> v1 migration.
func NewMigration() Migration { return Migration{} }

// Name implements sessionformat.Migration.
func (Migration) Name() string { return MigrationName }

// FromVersion implements sessionformat.Migration.
func (Migration) FromVersion() int64 { return 0 }

// ToVersion implements sessionformat.Migration.
func (Migration) ToVersion() int64 { return 1 }

// MigrateHeader implements sessionformat.Migration.
func (Migration) MigrateHeader(header sessionformat.Header) (sessionformat.Header, error) {
	if err := assertHeaderVersion(header, 0); err != nil {
		return nil, err
	}
	next := header.Clone()
	next["version"] = mustDecodeNumber("1")
	return next, nil
}

// Migrate implements sessionformat.Migration.
func (m Migration) Migrate(source sessionformat.Artifact) (sessionformat.Artifact, error) {
	if err := AssertReleasedV0SourceArtifact(source); err != nil {
		return sessionformat.Artifact{}, err
	}
	events, err := normalizeReleasedV0Events(source.Events, headerID(source.Header))
	if err != nil {
		return sessionformat.Artifact{}, err
	}
	normalized := sessionformat.Artifact{Header: source.Header.Clone(), InheritedEventCount: source.InheritedEventCount, Events: events}
	if err := AssertNormalizedReleasedV0Artifact(normalized); err != nil {
		return sessionformat.Artifact{}, err
	}
	next := normalized
	next.Header["version"] = mustDecodeNumber("1")
	if _, err := sessionformat.SnapshotArtifact(next, "released v0-to-v1 target"); err != nil {
		return sessionformat.Artifact{}, err
	}
	if err := AssertReleasedV1Artifact(next); err != nil {
		return sessionformat.Artifact{}, err
	}
	return next, nil
}

// ValidateTarget implements sessionformat.Migration.
func (Migration) ValidateTarget(artifact sessionformat.Artifact) error {
	return AssertReleasedV1Artifact(artifact)
}

// ValidateTargetHeader implements sessionformat.Migration.
func (Migration) ValidateTargetHeader(header sessionformat.Header) error {
	return AssertReleasedV1Header(header)
}

func assertHeaderVersion(header sessionformat.Header, version int64) error {
	stored, err := sessionformat.HeaderVersion(header)
	if err != nil {
		return err
	}
	if stored != version {
		return sessionformat.FormatErrorf("expected format v%d header", version)
	}
	return nil
}

func headerID(header sessionformat.Header) string {
	id, _ := header["id"].(string)
	return id
}

func normalizeReleasedV0Events(events []sessionformat.Event, sessionId string) ([]sessionformat.Event, error) {
	messageIds := map[int64]string{}
	output := make([]sessionformat.Event, 0, len(events))
	for _, event := range events {
		if err := assertSupportedLegacyType(event, sessionId); err != nil {
			return nil, err
		}
		start, err := normalizeLegacyTurnStart(event, sessionId)
		if err != nil {
			return nil, err
		}
		end, err := normalizeLegacyTurnEnd(start, sessionId)
		if err != nil {
			return nil, err
		}
		header, err := normalizeLegacyRequestHeader(end, sessionId)
		if err != nil {
			return nil, err
		}
		steering, err := normalizeLegacySteering(header, sessionId)
		if err != nil {
			return nil, err
		}
		message, err := normalizeLegacyMessage(steering, sessionId, messageIds)
		if err != nil {
			return nil, err
		}
		if err := AssertReleasedEventPayload(message, 0); err != nil {
			return nil, err
		}
		output = append(output, message)
		if messageId, err := eventMessageId(message); err == nil && messageId != "" {
			messageIds[message.Seq] = messageId
		}
	}
	return output, nil
}

// setData replaces one event's payload tree.
func setData(event sessionformat.Event, data map[string]any) (sessionformat.Event, error) {
	raw, err := encodeTree(data)
	if err != nil {
		return sessionformat.Event{}, err
	}
	event.Data = raw
	return event, nil
}

// setEnvelopeExtra sets one extra envelope member.
func setEnvelopeExtra(event sessionformat.Event, key string, rawValue json.RawMessage) sessionformat.Event {
	if event.Extra == nil {
		event.Extra = map[string]json.RawMessage{}
	}
	event.Extra[key] = rawValue
	return event
}

func deleteEnvelopeExtra(event sessionformat.Event, key string) sessionformat.Event {
	if event.Extra != nil {
		delete(event.Extra, key)
	}
	return event
}

// treeOf decodes one event payload for manipulation.
func treeOf(event sessionformat.Event) (map[string]any, error) {
	tree, err := sessionformat.DecodeValue(event.Data)
	if err != nil {
		return nil, sessionformat.FormatErrorf("%s %d data must be a JSON object", event.Type, event.Seq)
	}
	return recordOf(tree, event.Type+" "+itoa(event.Seq)+" data")
}

func assertSupportedLegacyType(event sessionformat.Event, sessionId string) error {
	if event.Type == "request/header-delta" || event.Type == "mode/set" {
		return sessionformat.UnsupportedErrorf(
			"session %q contains unsupported legacy %s event at seq %d", sessionId, event.Type, event.Seq)
	}
	if event.Type == "request/header" {
		data, err := treeOf(event)
		if err != nil {
			return err
		}
		if data["reason"] == "fallback" {
			return sessionformat.UnsupportedErrorf(
				"session %q contains unsupported request/header reason \"fallback\" at seq %d", sessionId, event.Seq)
		}
	}
	return nil
}

func normalizeLegacyRequestHeader(event sessionformat.Event, sessionId string) (sessionformat.Event, error) {
	if event.Type != "request/header" {
		return event, nil
	}
	data, err := treeOf(event)
	if err != nil {
		return event, nil
	}
	header, ok := data["header"].(map[string]any)
	if !ok {
		header = map[string]any{}
	}
	if _, has := header["messagePrefix"]; !has {
		return event, nil
	}
	if _, isArray := header["messagePrefix"].([]any); !isArray {
		return event, sessionformat.FormatErrorf(
			"session %q contains malformed request/header messagePrefix at seq %d", sessionId, event.Seq)
	}
	delete(header, "messagePrefix")
	return setData(event, data)
}

func normalizeLegacySteering(event sessionformat.Event, sessionId string) (sessionformat.Event, error) {
	if event.Type != "steering/message" {
		return event, nil
	}
	data, err := treeOf(event)
	if err != nil {
		return event, err
	}
	wrapped, hasWrapped := data["message"]
	if hasWrapped {
		if err := assertKeys(data, []string{"turn", "message"}, nil, "steering/message "+itoa(event.Seq)+" data"); err != nil {
			return event, err
		}
		if _, err := countValue(data["turn"], "steering/message "+itoa(event.Seq)+" turn"); err != nil {
			return event, err
		}
		wrappedRecord, err := recordOf(wrapped, "steering/message "+itoa(event.Seq)+" message")
		if err != nil {
			return event, err
		}
		event.Type = "user/message"
		return setData(event, wrappedRecord)
	}
	if err := assertKeys(data, []string{"turn", "content", "source"}, nil, "steering/message "+itoa(event.Seq)+" data"); err != nil {
		return event, err
	}
	if _, err := countValue(data["turn"], "steering/message "+itoa(event.Seq)+" turn"); err != nil {
		return event, err
	}
	delete(data, "turn")
	data["id"] = legacyMessageId(sessionId, event.Seq)
	data["role"] = "user"
	event.Type = "user/message"
	return setData(event, data)
}

func normalizeLegacyTurnStart(event sessionformat.Event, sessionId string) (sessionformat.Event, error) {
	if event.Type != "turn/start" {
		return event, nil
	}
	data, err := treeOf(event)
	if err != nil {
		return event, err
	}
	if _, has := data["trigger"]; !has {
		return event, nil
	}
	if err := assertKeys(data, []string{"turn", "trigger"}, nil, "turn/start "+itoa(event.Seq)+" data"); err != nil {
		return event, err
	}
	turn, err := countValue(data["turn"], "turn/start "+itoa(event.Seq)+" turn")
	if err != nil {
		return event, err
	}
	trigger, ok := data["trigger"].(map[string]any)
	if !ok || turn < 1 {
		return event, malformedLegacy(sessionId, "turn/start", event.Seq)
	}
	kind, isString := trigger["kind"].(string)
	if !isString || len(kind) == 0 {
		return event, malformedLegacy(sessionId, "turn/start", event.Seq)
	}
	delete(data, "trigger")
	return setData(event, data)
}

func normalizeLegacyTurnEnd(event sessionformat.Event, sessionId string) (sessionformat.Event, error) {
	if event.Type != "turn/end" {
		return event, nil
	}
	data, err := treeOf(event)
	if err != nil {
		return event, err
	}
	if err := assertKeys(data, []string{"turn", "reason"}, nil, "turn/end "+itoa(event.Seq)+" data"); err != nil {
		return event, err
	}
	turn, err := countValue(data["turn"], "turn/end "+itoa(event.Seq)+" turn")
	if err != nil {
		return event, err
	}
	if turn < 1 {
		return event, malformedLegacy(sessionId, "turn/end", event.Seq)
	}
	reason, err := recordOf(data["reason"], "turn/end "+itoa(event.Seq)+" reason")
	if err != nil {
		return event, err
	}
	kind, isString := reason["kind"].(string)
	if !isString {
		return event, malformedLegacy(sessionId, "turn/end", event.Seq)
	}
	var current map[string]any
	switch kind {
	case "completed", "blocked", "max-tokens", "interrupted":
		if err := assertKeys(reason, []string{"kind"}, nil, "turn/end "+itoa(event.Seq)+" reason"); err != nil {
			return event, err
		}
		return event, nil
	case "aborted":
		if _, has := reason["reason"]; has {
			return event, nil
		}
		if err := assertKeys(reason, []string{"kind"}, nil, "turn/end "+itoa(event.Seq)+" reason"); err != nil {
			return event, err
		}
		current = map[string]any{"kind": "aborted", "reason": map[string]any{"kind": "legacy"}}
	case "disposed":
		if err := assertKeys(reason, []string{"kind"}, nil, "turn/end "+itoa(event.Seq)+" reason"); err != nil {
			return event, err
		}
		current = map[string]any{"kind": "aborted", "reason": map[string]any{"kind": "disposed"}}
	case "error":
		if _, has := reason["error"]; has {
			return event, nil
		}
		normalized, err := normalizeLegacyErrorReason(reason, event.Seq, sessionId)
		if err != nil {
			return event, err
		}
		current = normalized
	default:
		return event, nil
	}
	data["reason"] = current
	return setData(event, data)
}

func normalizeLegacyErrorReason(reason map[string]any, seq int64, sessionId string) (map[string]any, error) {
	if _, err := countValue(reason["step"], "turn/end "+itoa(seq)+" error step"); err != nil {
		return nil, err
	}
	failure, hasFailure := reason["failure"]
	if hasFailure {
		if err := assertKeys(reason, []string{"kind", "step", "failure"}, nil, "turn/end "+itoa(seq)+" reason"); err != nil {
			return nil, err
		}
		record, err := recordOf(failure, "turn/end "+itoa(seq)+" failure")
		if err != nil {
			return nil, err
		}
		if err := assertKeys(record,
			[]string{"message", "code"},
			[]string{"status", "providerRetryAfterMs", "requestId"},
			"turn/end "+itoa(seq)+" failure"); err != nil {
			return nil, err
		}
		_, messageIsString := record["message"].(string)
		_, codeIsString := record["code"].(string)
		if !messageIsString || !codeIsString {
			return nil, malformedLegacy(sessionId, "turn/end", seq)
		}
		return map[string]any{"kind": "error", "error": record}, nil
	}
	if err := assertKeys(reason, []string{"kind", "step", "message"}, []string{"code"}, "turn/end "+itoa(seq)+" reason"); err != nil {
		return nil, err
	}
	message, messageIsString := reason["message"].(string)
	if !messageIsString {
		return nil, malformedLegacy(sessionId, "turn/end", seq)
	}
	code, ok := reason["code"]
	if ok && code != nil {
		if _, codeIsString := code.(string); !codeIsString {
			return nil, malformedLegacy(sessionId, "turn/end", seq)
		}
	}
	codeValue := any("UNKNOWN")
	if text, isString := reason["code"].(string); isString {
		codeValue = text
	}
	return map[string]any{
		"kind":  "error",
		"error": map[string]any{"message": message, "code": codeValue},
	}, nil
}

func normalizeLegacyMessage(event sessionformat.Event, sessionId string, messageIds map[int64]string) (sessionformat.Event, error) {
	data, err := treeOf(event)
	if err != nil {
		return event, err
	}
	switch event.Type {
	case "user/message":
		_, hasID := data["id"]
		_, hasRole := data["role"]
		_, hasMessage := data["message"]
		_, hasContent := data["content"]
		_, hasSource := data["source"]
		if hasID || hasRole || hasMessage || !hasContent || !hasSource {
			return event, nil
		}
		data["id"] = legacyMessageId(sessionId, event.Seq)
		data["role"] = "user"
		return setData(event, data)
	case "assistant/message":
		if _, hasMessage := data["message"]; hasMessage {
			return event, nil
		}
		content, hasContent := data["content"]
		provenance, hasProvenance := data["provenance"]
		if !hasContent || !hasProvenance {
			return event, nil
		}
		source, err := recordOf(provenance, "assistant/message "+itoa(event.Seq)+" provenance")
		if err != nil {
			return event, err
		}
		source["kind"] = "model"
		delete(data, "content")
		delete(data, "provenance")
		data["message"] = map[string]any{
			"id":      legacyMessageId(sessionId, event.Seq),
			"role":    "assistant",
			"content": content,
			"source":  source,
		}
		return setData(event, data)
	case "tool/result":
		if _, hasMessage := data["message"]; hasMessage {
			return event, nil
		}
		callId, hasCallId := data["callId"].(string)
		content, hasContent := data["content"]
		isError, hasIsError := data["isError"].(bool)
		if !hasCallId || !hasContent || !hasIsError {
			return event, nil
		}
		delete(data, "callId")
		delete(data, "content")
		delete(data, "isError")
		inheritedId, err := replacementStart(event)
		if err != nil {
			return event, err
		}
		var messageId string
		if inheritedId == nil {
			messageId = legacyMessageId(sessionId, event.Seq)
		} else {
			messageId = messageIds[*inheritedId]
		}
		if messageId == "" {
			return event, sessionformat.FormatErrorf(
				"tool/result %d replacement cites a message without identity", event.Seq)
		}
		data["message"] = map[string]any{
			"id":   messageId,
			"role": "user",
			"content": []any{map[string]any{
				"type": "tool-result", "toolCallId": callId, "content": content, "isError": isError,
			}},
			"source": map[string]any{"kind": "tool", "callId": callId},
		}
		return setData(event, data)
	default:
		return event, nil
	}
}

// replacementStart reads the replace surfaceOp's start coordinate.
func replacementStart(event sessionformat.Event) (*int64, error) {
	raw, ok := event.ExtraField("surfaceOp")
	if !ok {
		return nil, nil
	}
	tree, err := sessionformat.DecodeValue(raw)
	if err != nil {
		return nil, err
	}
	operation, ok := tree.(map[string]any)
	if !ok || operation["op"] != "replace" {
		return nil, nil
	}
	start, err := sessionformat.Count(operation["start"], "surface replace start")
	if err != nil {
		return nil, err
	}
	return &start, nil
}

// eventMessageId extracts the normalized message identity.
func eventMessageId(event sessionformat.Event) (string, error) {
	data, err := treeOf(event)
	if err != nil {
		return "", err
	}
	var message map[string]any
	if event.Type == "user/message" {
		message = data
	} else if record, ok := data["message"].(map[string]any); ok {
		message = record
	}
	if message == nil {
		return "", nil
	}
	id, _ := message["id"].(string)
	return id, nil
}

func legacyMessageId(sessionId string, seq int64) string {
	return "legacy-message:" + sessionId + ":" + itoa(seq)
}

func malformedLegacy(sessionId, eventType string, seq int64) error {
	return sessionformat.FormatErrorf(
		"session %q contains malformed pre-react-loop %s at seq %d", sessionId, eventType, seq)
}
