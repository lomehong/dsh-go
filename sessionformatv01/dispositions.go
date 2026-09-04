// Package sessionformatv01 re-implements the released-v0 -> released-v1
// Session format edge of @deepseek-ai/dsh-session-format-v0-to-v1 (official
// tag dsh-v0.1.3-alpha.1): the frozen released-v0 event inventory, strict
// source and target validation, the shared-layout physical codec, and the
// identity-shaped migration with bounded historical normalizations.
package sessionformatv01

import (
	"encoding/json"
	"sort"

	"dshgo/sessionformat"
)

// PayloadDisposition is the exact top-level payload member disposition
// frozen for every released-v0 event type (official
// ReleasedV0PayloadDisposition).
type PayloadDisposition struct {
	Required []string
	Optional []string
	// Opaque members are retained as lossless JSON without nested semantic
	// inspection.
	Opaque []string
}

type dispositionSpec struct {
	required []string
	optional []string
	opaque   []string
}

func disposition(required, optional, opaque []string) dispositionSpec {
	return dispositionSpec{required: required, optional: optional, opaque: opaque}
}

func opt(required []string) dispositionSpec {
	return dispositionSpec{required: required}
}

// releasedV0EventDispositions is the frozen released-v0 event and
// payload-member inventory. Every listed member is preserved by the identity
// edge; members in opaque remain lossless JSON without nested
// Session-sequence interpretation.
var releasedV0EventDispositions = map[string]dispositionSpec{
	"agent-preset/selected": opt([]string{"agentPreset"}),
	"agent/inbox/spliced":   disposition([]string{"target", "start", "inserted"}, []string{"removedCount", "outcome"}, nil),
	"approval/asked":        disposition([]string{"id", "toolName"}, []string{"callId", "reason"}, nil),
	"approval/decided":      opt([]string{"id", "outcome"}),
	"approval/policy":       disposition([]string{"policy"}, []string{"source"}, nil),
	"assistant/chunk":       opt([]string{"turn", "step", "chunk"}),
	"assistant/message":     disposition([]string{"turn", "step", "message"}, []string{"usage", "interrupted"}, nil),
	"command/done":          disposition([]string{"commandId", "kind"}, []string{"text", "sourceEventSeq"}, nil),
	"command/run":           disposition([]string{"commandId", "name", "source"}, []string{"args"}, nil),
	"compaction/end":        disposition([]string{"compactionId", "turn"}, []string{"sourceCommandId", "error"}, nil),
	"compaction/prune":      opt([]string{"shadowedRange", "shadowedSeqs", "shadowedTokenCount"}),
	"compaction/start":      disposition([]string{"compactionId", "turn"}, []string{"sourceCommandId"}, nil),
	"compaction/summary": disposition(
		[]string{"compactionId", "summary", "shadowedRange", "shadowedSeqs", "shadowedTokenCount", "provider", "model"},
		[]string{"sourceCommandId", "maxTokens", "usage", "rawOutput", "llmStreamCall"}, nil),
	"feedback/record": opt([]string{"text"}),
	"goal/change": disposition(
		[]string{"kind", "version", "operation"},
		[]string{"goal", "roundsStarted", "createdAt", "updatedAt", "cleared", "clearedAt"}, nil),
	"hook/invoked": disposition([]string{"turn", "point", "dialect", "handlerId"}, []string{"matcher"}, nil),
	"hook/result": disposition(
		[]string{"turn", "point", "handlerId", "decision", "durationMs"},
		[]string{"exitCode", "stderrSummary"}, nil),
	"llm/retry": disposition(
		[]string{"retryId", "turn", "step", "provider", "mode", "policyKey", "retry", "delayMs", "failure"},
		[]string{"maxRetries"}, nil),
	"llm/retry-started":                      opt([]string{"retryId", "turn", "step", "retry"}),
	"model/selection":                        disposition([]string{"provider", "model"}, []string{"reasoningEffort"}, nil),
	"permission/preset":                      opt([]string{"preset"}),
	"plan/mode":                              opt([]string{"active"}),
	"request/context":                        disposition([]string{"provider", "model"}, []string{"contextWindow"}, nil),
	"request/header":                         disposition([]string{"header", "reason"}, []string{"startsSeries"}, nil),
	"sandbox/mode":                           disposition([]string{"mode"}, []string{"source"}, nil),
	"schedule/change":                        disposition([]string{"version", "operation"}, []string{"schedule", "id", "acceptedAt"}, nil),
	"session-log-deepseek/delivery-accepted": opt([]string{"sessionId", "throughSeq"}),
	"session/end-seed":                       disposition(nil, nil, nil),
	"session/title":                          disposition([]string{"title", "messageSeqs", "source"}, nil, nil),
	"session/title-llm-request":              opt([]string{"titleProvider", "messageSeqs", "route", "system", "messages", "maxTokens"}),
	"step/end":                               opt([]string{"turn", "step"}),
	"step/start":                             opt([]string{"turn", "step"}),
	"subagent/descriptor": disposition(
		[]string{"mode", "version", "provider"},
		[]string{"label", "agentProvider", "agentModel", "agentReasoningEffort", "persona", "toolFilter"}, nil),
	"subagent/model-selection-policy": opt([]string{"allowedModels"}),
	"team/member":                     disposition([]string{"version", "teamId", "member"}, nil, nil),
	"team/message/delivered":          disposition([]string{"version", "teamId", "messageId", "targetId"}, nil, nil),
	"team/message/queued":             disposition([]string{"version", "teamId", "message"}, nil, nil),
	"team/task":                       disposition([]string{"version", "teamId", "task"}, nil, nil),
	"todo/write":                      opt([]string{"todos"}),
	"tool-workflow/agent-end":         disposition([]string{"runId", "seq", "outcome"}, nil, nil),
	"tool-workflow/agent-start":       disposition([]string{"runId", "seq", "label", "childId"}, []string{"phase"}, nil),
	"tool-workflow/run-end":           disposition([]string{"runId", "stopReason"}, nil, nil),
	"tool-workflow/run-start":         disposition([]string{"runId", "name"}, nil, nil),
	"tool/call":                       disposition([]string{"turn", "step", "callId", "name", "arguments"}, nil, nil),
	"tool/code-dispatch": disposition(
		[]string{"rootCallId", "parentCallId", "subCallId", "name", "arguments", "isError", "content"},
		nil, []string{"arguments"}),
	"tool/code-dispatch-start": disposition(
		[]string{"rootCallId", "parentCallId", "subCallId", "name", "arguments"},
		nil, []string{"arguments"}),
	"tool/result": disposition(
		[]string{"turn", "step", "message"}, []string{"error", "meta"}, []string{"meta"}),
	"turn/end":                        disposition([]string{"turn", "reason"}, nil, nil),
	"turn/start":                      opt([]string{"turn"}),
	"user/message":                    opt([]string{"role", "id", "content", "source"}),
	"web/deepseek-search-llm-request": opt([]string{"endpoint", "apiVersion", "body"}),
}

// ReleasedV0EventTypes is the stable sorted released-v0 event inventory.
var ReleasedV0EventTypes = sortedInventory()

func sortedInventory() []string {
	names := make([]string, 0, len(releasedV0EventDispositions))
	for name := range releasedV0EventDispositions {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// dispositionFor returns the frozen disposition for one event type.
func dispositionFor(eventType string) (dispositionSpec, bool) {
	spec, ok := releasedV0EventDispositions[eventType]
	return spec, ok
}

// recordOf requires one plain JSON object (official releasedV0Record).
func recordOf(value any, label string) (map[string]any, error) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("%s must be a JSON object", label)
	}
	return record, nil
}

// assertKeys requires every named member and no member outside the optional
// list (official assertReleasedV0Keys).
func assertKeys(record map[string]any, required, optional []string, label string) error {
	allowed := map[string]bool{}
	for _, key := range required {
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range record {
		if !allowed[key] {
			return sessionformat.FormatErrorf("%s has unexpected member %q", label, key)
		}
	}
	for _, key := range required {
		if _, ok := record[key]; !ok {
			return sessionformat.FormatErrorf("%s lacks required member %q", label, key)
		}
	}
	return nil
}

// decodeData decodes one event payload into a lossless tree.
func decodeData(event sessionformat.Event, label string) (map[string]any, error) {
	tree, err := sessionformat.DecodeValue(event.Data)
	if err != nil {
		return nil, sessionformat.FormatErrorf("%s data must be a JSON object", label)
	}
	return recordOf(tree, label)
}

// encodeTree renders a manipulated payload tree back to raw JSON.
func encodeTree(value any) (json.RawMessage, error) {
	return sessionformat.EncodeValue(value)
}
