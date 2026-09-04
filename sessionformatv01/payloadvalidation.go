package sessionformatv01

import (
	"encoding/json"
	"strings"

	"dshgo/sessionformat"
)

// Nested released payload semantics for one known event. Port of
// packages/session/session-format-v0-to-v1/src/payload-validation.ts.

func assertReleasedPayloadSemantics(event sessionformat.Event, version int64, data map[string]any, label string) error {
	switch event.Type {
	case "agent-preset/selected":
		return stringValue(data["agentPreset"], label+" agentPreset")
	case "agent/inbox/spliced":
		if err := literalValue(data["target"], []string{"next-turn", "next-step"}, label+" target"); err != nil {
			return err
		}
		if _, err := countValue(data["start"], label+" start"); err != nil {
			return err
		}
		if value, ok := data["removedCount"]; ok {
			if _, err := countValue(value, label+" removedCount"); err != nil {
				return err
			}
		}
		err := arrayValue(data["inserted"], label+" inserted", func(member any, memberLabel string) error {
			return messageValue(member, memberLabel, version, "user")
		})
		return err
	case "approval/asked":
		if err := nonEmptyString(data["id"], label+" id"); err != nil {
			return err
		}
		if err := nonEmptyString(data["toolName"], label+" toolName"); err != nil {
			return err
		}
		if value, ok := data["callId"]; ok {
			if err := nonEmptyString(value, label+" callId"); err != nil {
				return err
			}
		}
		if value, ok := data["reason"]; ok {
			return stringValue(value, label+" reason")
		}
		return nil
	case "approval/decided":
		if err := nonEmptyString(data["id"], label+" id"); err != nil {
			return err
		}
		return literalValue(data["outcome"], []string{"allowed-once", "rejected", "cancelled", "unavailable"}, label+" outcome")
	case "approval/policy":
		if err := literalValue(data["policy"], []string{"ask", "never"}, label+" policy"); err != nil {
			return err
		}
		if value, ok := data["source"]; ok {
			return literalValue(value, []string{"delegation"}, label+" source")
		}
		return nil
	case "assistant/chunk":
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		return streamChunkValue(data["chunk"], label+" chunk")
	case "assistant/message":
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		if err := messageValue(data["message"], label+" message", version, "assistant"); err != nil {
			return err
		}
		if value, ok := data["usage"]; ok {
			if err := tokenUsageValue(value, label+" usage"); err != nil {
				return err
			}
		}
		if value, ok := data["interrupted"]; ok {
			return literalBool(value, true, label+" interrupted")
		}
		return nil
	case "command/done":
		if err := nonEmptyString(data["commandId"], label+" commandId"); err != nil {
			return err
		}
		if err := literalValue(data["kind"], []string{"success", "error"}, label+" kind"); err != nil {
			return err
		}
		if value, ok := data["text"]; ok {
			if err := stringValue(value, label+" text"); err != nil {
				return err
			}
		}
		if value, ok := data["sourceEventSeq"]; ok {
			_, err := earlierSeq(value, event.Seq, label+" sourceEventSeq")
			return err
		}
		return nil
	case "command/run":
		if err := nonEmptyString(data["commandId"], label+" commandId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["name"], label+" name"); err != nil {
			return err
		}
		if value, ok := data["args"]; ok {
			if err := stringValue(value, label+" args"); err != nil {
				return err
			}
		}
		source, err := exactRecord(data["source"], label+" source", []string{"kind"}, nil)
		if err != nil {
			return err
		}
		return literalValue(source["kind"], []string{"user"}, label+" source kind")
	case "compaction/start", "compaction/end":
		if err := nonEmptyString(data["compactionId"], label+" compactionId"); err != nil {
			return err
		}
		if value, ok := data["sourceCommandId"]; ok {
			if err := nonEmptyString(value, label+" sourceCommandId"); err != nil {
				return err
			}
		}
		if value, ok := data["turn"]; ok && value != nil {
			if _, err := countValue(value, label+" turn"); err != nil {
				return err
			}
		}
		if value, ok := data["error"]; ok {
			return stringValue(value, label+" error")
		}
		return nil
	case "compaction/prune":
		return shadowedValue(data, event.Seq, label)
	case "compaction/summary":
		if streamCall, ok := data["llmStreamCall"].(bool); ok && streamCall {
			if _, hasRaw := data["rawOutput"]; !hasRaw {
				return sessionformat.FormatErrorf("%s llmStreamCall requires rawOutput", label)
			}
		}
		if err := nonEmptyString(data["compactionId"], label+" compactionId"); err != nil {
			return err
		}
		if value, ok := data["sourceCommandId"]; ok {
			if err := nonEmptyString(value, label+" sourceCommandId"); err != nil {
				return err
			}
		}
		if err := contentBlocksValue(data["summary"], label+" summary", version); err != nil {
			return err
		}
		if err := shadowedValue(data, event.Seq, label); err != nil {
			return err
		}
		if err := nonEmptyString(data["provider"], label+" provider"); err != nil {
			return err
		}
		if err := nonEmptyString(data["model"], label+" model"); err != nil {
			return err
		}
		if value, ok := data["maxTokens"]; ok {
			if _, err := countValue(value, label+" maxTokens"); err != nil {
				return err
			}
		}
		if value, ok := data["usage"]; ok {
			if err := tokenUsageValue(value, label+" usage"); err != nil {
				return err
			}
		}
		if value, ok := data["rawOutput"]; ok {
			if err := contentBlocksValue(value, label+" rawOutput", version); err != nil {
				return err
			}
		}
		if value, ok := data["llmStreamCall"]; ok {
			return literalBool(value, true, label+" llmStreamCall")
		}
		return nil
	case "feedback/record":
		return nonEmptyString(data["text"], label+" text")
	case "goal/change":
		return goalChangeValue(data, label)
	case "hook/invoked":
		if _, err := countValue(data["turn"], label+" turn"); err != nil {
			return err
		}
		if err := nonEmptyString(data["point"], label+" point"); err != nil {
			return err
		}
		if err := literalValue(data["dialect"], []string{"claude-code", "codex"}, label+" dialect"); err != nil {
			return err
		}
		if value, ok := data["matcher"]; ok {
			if err := stringValue(value, label+" matcher"); err != nil {
				return err
			}
		}
		return nonEmptyString(data["handlerId"], label+" handlerId")
	case "hook/result":
		if _, err := countValue(data["turn"], label+" turn"); err != nil {
			return err
		}
		if err := nonEmptyString(data["point"], label+" point"); err != nil {
			return err
		}
		if err := nonEmptyString(data["handlerId"], label+" handlerId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["decision"], label+" decision"); err != nil {
			return err
		}
		if value, ok := data["exitCode"]; ok {
			if _, err := safeIntegerValue(value, label+" exitCode"); err != nil {
				return err
			}
		}
		if value, ok := data["stderrSummary"]; ok {
			if err := stringValue(value, label+" stderrSummary"); err != nil {
				return err
			}
		}
		duration, err := finiteNumberValue(data["durationMs"], label+" durationMs")
		if err != nil {
			return err
		}
		if duration < 0 {
			return sessionformat.FormatErrorf("%s durationMs must be non-negative", label)
		}
		return nil
	case "llm/retry":
		if err := nonEmptyString(data["retryId"], label+" retryId"); err != nil {
			return err
		}
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		if err := nonEmptyString(data["provider"], label+" provider"); err != nil {
			return err
		}
		mode, ok := data["mode"].(string)
		if !ok || (mode != "normal" && mode != "always") {
			return sessionformat.FormatErrorf("%s mode must be one of normal, always", label)
		}
		if err := nonEmptyString(data["policyKey"], label+" policyKey"); err != nil {
			return err
		}
		retry, err := positiveIntegerValue(data["retry"], label+" retry")
		if err != nil {
			return err
		}
		if mode == "normal" {
			maxRetries, err := positiveIntegerValue(data["maxRetries"], label+" maxRetries")
			if err != nil {
				return err
			}
			if retry > maxRetries {
				return sessionformat.FormatErrorf("%s retry exceeds maxRetries", label)
			}
		} else if _, hasMax := data["maxRetries"]; hasMax {
			return sessionformat.FormatErrorf("%s always mode must omit maxRetries", label)
		}
		delayMs, err := finiteNumberValue(data["delayMs"], label+" delayMs")
		if err != nil {
			return err
		}
		if delayMs < 0 {
			return sessionformat.FormatErrorf("%s delayMs must be non-negative", label)
		}
		if delayMs > 2_147_483_647 {
			return sessionformat.FormatErrorf("%s delayMs exceeds the timer range", label)
		}
		return llmFailureValue(data["failure"], label+" failure")
	case "llm/retry-started":
		if err := nonEmptyString(data["retryId"], label+" retryId"); err != nil {
			return err
		}
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		_, err := positiveIntegerValue(data["retry"], label+" retry")
		return err
	case "model/selection":
		if err := nonEmptyString(data["provider"], label+" provider"); err != nil {
			return err
		}
		if err := nonEmptyString(data["model"], label+" model"); err != nil {
			return err
		}
		if value, ok := data["reasoningEffort"]; ok {
			return nonEmptyString(value, label+" reasoningEffort")
		}
		return nil
	case "permission/preset":
		return nonEmptyString(data["preset"], label+" preset")
	case "plan/mode":
		_, ok := data["active"].(bool)
		if !ok {
			return sessionformat.FormatErrorf("%s active must be a boolean", label)
		}
		return nil
	case "request/context":
		if err := nonEmptyString(data["provider"], label+" provider"); err != nil {
			return err
		}
		if err := nonEmptyString(data["model"], label+" model"); err != nil {
			return err
		}
		if value, ok := data["contextWindow"]; ok {
			_, err := positiveIntegerValue(value, label+" contextWindow")
			return err
		}
		return nil
	case "request/header":
		if err := requestHeaderValue(data["header"], label+" header"); err != nil {
			return err
		}
		if err := literalValue(data["reason"], []string{"initial", "resume", "change", "series"}, label+" reason"); err != nil {
			return err
		}
		if value, ok := data["startsSeries"]; ok {
			return literalBool(value, true, label+" startsSeries")
		}
		return nil
	case "sandbox/mode":
		if err := literalValue(data["mode"], []string{"read-only", "workspace-write", "danger-full-access"}, label+" mode"); err != nil {
			return err
		}
		if value, ok := data["source"]; ok {
			return literalValue(value, []string{"delegation"}, label+" source")
		}
		return nil
	case "schedule/change":
		return scheduleChangeValue(data, label)
	case "session-log-deepseek/delivery-accepted":
		acceptedVersion := int64(0)
		if value, ok := data["sessionFormatVersion"]; ok {
			parsed, err := countValue(value, label+" sessionFormatVersion")
			if err != nil {
				return err
			}
			acceptedVersion = parsed
		}
		if acceptedVersion != version {
			return nil
		}
		if err := nonEmptyString(data["sessionId"], label+" sessionId"); err != nil {
			return err
		}
		_, err := earlierSeq(data["throughSeq"], event.Seq, label+" throughSeq")
		return err
	case "session/end-seed":
		return nil
	case "session/title":
		if err := nonEmptyString(data["title"], label+" title"); err != nil {
			return err
		}
		if err := seqArray(data["messageSeqs"], event.Seq, label+" messageSeqs", false); err != nil {
			return err
		}
		return titleSourceValue(data["source"], label+" source")
	case "session/title-llm-request":
		if err := nonEmptyString(data["titleProvider"], label+" titleProvider"); err != nil {
			return err
		}
		if err := seqArray(data["messageSeqs"], event.Seq, label+" messageSeqs", true); err != nil {
			return err
		}
		if err := modelRouteValue(data["route"], label+" route"); err != nil {
			return err
		}
		if err := stringValue(data["system"], label+" system"); err != nil {
			return err
		}
		if err := arrayValue(data["messages"], label+" messages", func(member any, memberLabel string) error {
			return messageValue(member, memberLabel, version, "")
		}); err != nil {
			return err
		}
		_, err := positiveIntegerValue(data["maxTokens"], label+" maxTokens")
		return err
	case "step/end", "step/start":
		return coordinatePair(data, label)
	case "subagent/descriptor":
		return subagentDescriptorValue(data, label)
	case "subagent/model-selection-policy":
		return allowedModelsValue(data["allowedModels"], label+" allowedModels")
	case "team/member":
		if err := teamSelector(data, label); err != nil {
			return err
		}
		return teamMemberValue(data["member"], label+" member")
	case "team/message/delivered":
		if err := teamSelector(data, label); err != nil {
			return err
		}
		if err := nonEmptyString(data["messageId"], label+" messageId"); err != nil {
			return err
		}
		return nonEmptyString(data["targetId"], label+" targetId")
	case "team/message/queued":
		if err := teamSelector(data, label); err != nil {
			return err
		}
		return teamMessageValue(data["message"], label+" message", version)
	case "team/task":
		if err := teamSelector(data, label); err != nil {
			return err
		}
		return teamTaskValue(data["task"], label+" task")
	case "todo/write":
		err := arrayValue(data["todos"], label+" todos", func(member any, memberLabel string) error {
			item, err := exactRecord(member, memberLabel, []string{"content", "status"}, nil)
			if err != nil {
				return err
			}
			if err := stringValue(item["content"], memberLabel+" content"); err != nil {
				return err
			}
			return literalValue(item["status"], []string{"pending", "in_progress", "completed"}, memberLabel+" status")
		})
		return err
	case "tool-workflow/agent-end":
		if err := workflowIdentity(data, label); err != nil {
			return err
		}
		return literalValue(data["outcome"], []string{"completed", "failed", "cancelled"}, label+" outcome")
	case "tool-workflow/agent-start":
		if err := workflowIdentity(data, label); err != nil {
			return err
		}
		if err := stringValue(data["label"], label+" label"); err != nil {
			return err
		}
		if value, ok := data["phase"]; ok {
			if err := stringValue(value, label+" phase"); err != nil {
				return err
			}
		}
		return nonEmptyString(data["childId"], label+" childId")
	case "tool-workflow/run-end":
		if err := nonEmptyString(data["runId"], label+" runId"); err != nil {
			return err
		}
		return literalValue(data["stopReason"], []string{"completed", "cancelled", "error"}, label+" stopReason")
	case "tool-workflow/run-start":
		if err := nonEmptyString(data["runId"], label+" runId"); err != nil {
			return err
		}
		return nonEmptyString(data["name"], label+" name")
	case "tool/call":
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		if err := nonEmptyString(data["callId"], label+" callId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["name"], label+" name"); err != nil {
			return err
		}
		return stringValue(data["arguments"], label+" arguments")
	case "tool/code-dispatch", "tool/code-dispatch-start":
		if err := nonEmptyString(data["rootCallId"], label+" rootCallId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["parentCallId"], label+" parentCallId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["subCallId"], label+" subCallId"); err != nil {
			return err
		}
		if err := nonEmptyString(data["name"], label+" name"); err != nil {
			return err
		}
		if event.Type == "tool/code-dispatch" {
			if _, ok := data["isError"].(bool); !ok {
				return sessionformat.FormatErrorf("%s isError must be a boolean", label)
			}
			return contentBlocksValue(data["content"], label+" content", version)
		}
		return nil
	case "tool/result":
		if err := coordinatePair(data, label); err != nil {
			return err
		}
		if err := messageValue(data["message"], label+" message", version, "tool"); err != nil {
			return err
		}
		if value, ok := data["error"]; ok {
			errorRecord, err := exactRecord(value, label+" error", []string{"name", "code"}, nil)
			if err != nil {
				return err
			}
			if err := nonEmptyString(errorRecord["name"], label+" error name"); err != nil {
				return err
			}
			return nonEmptyString(errorRecord["code"], label+" error code")
		}
		return nil
	case "turn/end":
		if _, err := countValue(data["turn"], label+" turn"); err != nil {
			return err
		}
		return turnEndReasonValue(data["reason"], label+" reason")
	case "turn/start":
		_, err := countValue(data["turn"], label+" turn")
		return err
	case "user/message":
		return messageValue(data, label, version, "user")
	case "web/deepseek-search-llm-request":
		if err := nonEmptyString(data["endpoint"], label+" endpoint"); err != nil {
			return err
		}
		if err := nonEmptyString(data["apiVersion"], label+" apiVersion"); err != nil {
			return err
		}
		return deepSeekSearchBodyValue(data["body"], label+" body")
	default:
		return sessionformat.FormatErrorf("released payload validator is missing event %q", event.Type)
	}
}

func exactRecord(value any, label string, required, optional []string) (map[string]any, error) {
	record, err := recordOf(value, label)
	if err != nil {
		return nil, err
	}
	if err := assertKeys(record, required, optional, label); err != nil {
		return nil, err
	}
	return record, nil
}

func stringValue(value any, label string) error {
	if _, ok := value.(string); !ok {
		return sessionformat.FormatErrorf("%s must be a string", label)
	}
	return nil
}

func nonEmptyString(value any, label string) error {
	text, ok := value.(string)
	if !ok || len(text) == 0 {
		return sessionformat.FormatErrorf("%s must be a non-empty string", label)
	}
	return nil
}

func literalValue(value any, allowed []string, label string) error {
	text, ok := value.(string)
	if !ok {
		return sessionformat.FormatErrorf("%s must be one of %s", label, strings.Join(allowed, ", "))
	}
	for _, candidate := range allowed {
		if candidate == text {
			return nil
		}
	}
	return sessionformat.FormatErrorf("%s must be one of %s", label, strings.Join(allowed, ", "))
}

func literalBool(value any, allowed bool, label string) error {
	if value != allowed {
		return sessionformat.FormatErrorf("%s must be one of %v", label, allowed)
	}
	flag, ok := value.(bool)
	if !ok || flag != allowed {
		return sessionformat.FormatErrorf("%s must be one of %v", label, allowed)
	}
	return nil
}

func safeIntegerValue(value any, label string) (int64, error) {
	return sessionformat.SafeInteger(value, label)
}

func countValue(value any, label string) (int64, error) {
	return sessionformat.Count(value, label)
}

func positiveIntegerValue(value any, label string) (int64, error) {
	parsed, err := countValue(value, label)
	if err != nil {
		return 0, err
	}
	if parsed == 0 {
		return 0, sessionformat.FormatErrorf("%s must be positive", label)
	}
	return parsed, nil
}

func finiteNumberValue(value any, label string) (float64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, sessionformat.FormatErrorf("%s must be a finite number", label)
	}
	parsed, err := number.Float64()
	if err != nil {
		return 0, sessionformat.FormatErrorf("%s must be a finite number", label)
	}
	return parsed, nil
}

func arrayValue(value any, label string, validate func(member any, memberLabel string) error) error {
	members, ok := value.([]any)
	if !ok {
		return sessionformat.FormatErrorf("%s must be an array", label)
	}
	for index, member := range members {
		if err := validate(member, label+"["+itoa(int64(index))+"]"); err != nil {
			return err
		}
	}
	return nil
}

// arrayMembers validates like arrayValue and additionally returns the
// members for counting callers.
func arrayMembers(value any, label string, validate func(member any, memberLabel string) error) ([]any, error) {
	members, ok := value.([]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("%s must be an array", label)
	}
	for index, member := range members {
		if err := validate(member, label+"["+itoa(int64(index))+"]"); err != nil {
			return nil, err
		}
	}
	return members, nil
}

func coordinatePair(data map[string]any, label string) error {
	if _, err := countValue(data["turn"], label+" turn"); err != nil {
		return err
	}
	_, err := countValue(data["step"], label+" step")
	return err
}

func earlierSeq(value any, eventSeq int64, label string) (int64, error) {
	seq, err := countValue(value, label)
	if err != nil {
		return 0, err
	}
	if seq >= eventSeq {
		return 0, sessionformat.FormatErrorf("%s must identify an earlier event", label)
	}
	return seq, nil
}

func seqArray(value any, eventSeq int64, label string, requireNonEmpty bool) error {
	seen := map[int64]bool{}
	members, err := arrayMembers(value, label, func(member any, memberLabel string) error {
		seq, err := earlierSeq(member, eventSeq, memberLabel)
		if err != nil {
			return err
		}
		if seen[seq] {
			return sessionformat.FormatErrorf("%s repeats seq %d", label, seq)
		}
		seen[seq] = true
		return nil
	})
	if err != nil {
		return err
	}
	if requireNonEmpty && len(members) == 0 {
		return sessionformat.FormatErrorf("%s must be non-empty", label)
	}
	return nil
}

func llmFailureValue(value any, label string) error {
	failure, err := exactRecord(value, label, []string{"message", "code"}, []string{"status", "providerRetryAfterMs", "requestId"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(failure["message"], label+" message"); err != nil {
		return err
	}
	if err := nonEmptyString(failure["code"], label+" code"); err != nil {
		return err
	}
	if value, ok := failure["status"]; ok {
		status, err := safeIntegerValue(value, label+" status")
		if err != nil {
			return err
		}
		if status < 100 || status > 599 {
			return sessionformat.FormatErrorf("%s status must be 100 through 599", label)
		}
	}
	if value, ok := failure["providerRetryAfterMs"]; ok {
		retryAfter, err := finiteNumberValue(value, label+" providerRetryAfterMs")
		if err != nil {
			return err
		}
		if retryAfter <= 0 {
			return sessionformat.FormatErrorf("%s providerRetryAfterMs must be positive", label)
		}
	}
	if value, ok := failure["requestId"]; ok {
		return nonEmptyString(value, label+" requestId")
	}
	return nil
}

func tokenUsageValue(value any, label string) error {
	usage, err := exactRecord(value, label,
		[]string{"inputTokens", "outputTokens"},
		[]string{"totalTokens", "cacheReadTokens", "cacheWriteTokens", "reasoningTokens"})
	if err != nil {
		return err
	}
	for key, member := range usage {
		if _, err := countValue(member, label+" "+key); err != nil {
			return err
		}
	}
	return nil
}

func contentBlocksValue(value any, label string, version int64) error {
	err := arrayValue(value, label, func(member any, memberLabel string) error {
		return contentBlockValue(member, memberLabel, version)
	})
	return err
}

func contentBlockValue(value any, label string, version int64) error {
	block, err := recordOf(value, label)
	if err != nil {
		return err
	}
	switch block["type"] {
	case "text", "reasoning":
		if err := assertKeys(block, []string{"type", "text"}, nil, label); err != nil {
			return err
		}
		return stringValue(block["text"], label+" text")
	case "image":
		if err := assertKeys(block, []string{"type", "attachment"}, nil, label); err != nil {
			return err
		}
		return imageAttachmentValue(block["attachment"], label+" attachment")
	case "tool-call":
		if err := assertKeys(block, []string{"type", "id", "name", "arguments"}, nil, label); err != nil {
			return err
		}
		if err := nonEmptyString(block["id"], label+" id"); err != nil {
			return err
		}
		if err := nonEmptyString(block["name"], label+" name"); err != nil {
			return err
		}
		return stringValue(block["arguments"], label+" arguments")
	case "tool-result":
		if err := assertKeys(block, []string{"type", "toolCallId", "content"}, []string{"isError"}, label); err != nil {
			return err
		}
		if err := nonEmptyString(block["toolCallId"], label+" toolCallId"); err != nil {
			return err
		}
		if err := contentBlocksValue(block["content"], label+" content", version); err != nil {
			return err
		}
		if value, ok := block["isError"]; ok {
			if _, ok := value.(bool); !ok {
				return sessionformat.FormatErrorf("%s isError must be a boolean", label)
			}
		}
		return nil
	default:
		return nonEmptyString(block["type"], label+" type")
	}
}

func imageAttachmentValue(value any, label string) error {
	attachment, err := exactRecord(value, label,
		[]string{"attachmentId", "mediaType", "bytes", "width", "height"},
		[]string{"name", "originalDimensions"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(attachment["attachmentId"], label+" attachmentId"); err != nil {
		return err
	}
	if err := literalValue(attachment["mediaType"], []string{"image/png", "image/jpeg", "image/webp", "image/gif"}, label+" mediaType"); err != nil {
		return err
	}
	if _, err := countValue(attachment["bytes"], label+" bytes"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(attachment["width"], label+" width"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(attachment["height"], label+" height"); err != nil {
		return err
	}
	if value, ok := attachment["name"]; ok {
		if err := stringValue(value, label+" name"); err != nil {
			return err
		}
	}
	if value, ok := attachment["originalDimensions"]; ok {
		dimensions, err := exactRecord(value, label+" originalDimensions", []string{"width", "height"}, nil)
		if err != nil {
			return err
		}
		if _, err := positiveIntegerValue(dimensions["width"], label+" original width"); err != nil {
			return err
		}
		if _, err := positiveIntegerValue(dimensions["height"], label+" original height"); err != nil {
			return err
		}
	}
	return nil
}

func messageValue(value any, label string, version int64, expected string) error {
	message, err := exactRecord(value, label, []string{"id", "role", "content", "source"}, nil)
	if err != nil {
		return err
	}
	if err := nonEmptyString(message["id"], label+" id"); err != nil {
		return err
	}
	switch expected {
	case "assistant":
		if err := literalValue(message["role"], []string{"assistant"}, label+" role"); err != nil {
			return err
		}
	case "user", "tool":
		if err := literalValue(message["role"], []string{"user"}, label+" role"); err != nil {
			return err
		}
	default:
		if err := literalValue(message["role"], []string{"system", "user", "assistant"}, label+" role"); err != nil {
			return err
		}
	}
	if err := contentBlocksValue(message["content"], label+" content", version); err != nil {
		return err
	}
	if err := messageSourceValue(message["source"], label+" source", version, expected); err != nil {
		return err
	}
	if expected == "tool" {
		content, _ := message["content"].([]any)
		var block map[string]any
		if len(content) == 1 {
			block, _ = content[0].(map[string]any)
		}
		source, err := recordOf(message["source"], label+" source")
		if err != nil {
			return err
		}
		if block == nil || block["type"] != "tool-result" || block["toolCallId"] != source["callId"] {
			return sessionformat.FormatErrorf("%s must contain exactly one tool-result block", label)
		}
	}
	return nil
}

func messageSourceValue(value any, label string, version int64, expected string) error {
	source, err := recordOf(value, label)
	if err != nil {
		return err
	}
	if expected == "assistant" && source["kind"] != "model" {
		return sessionformat.FormatErrorf("%s must be model source", label)
	}
	if expected == "tool" && source["kind"] != "tool" {
		return sessionformat.FormatErrorf("%s must be tool source", label)
	}
	switch source["kind"] {
	case "user":
		if err := assertKeys(source, []string{"kind"}, []string{"rpcId", "clientTimeZone"}, label); err != nil {
			return err
		}
		if value, ok := source["rpcId"]; ok {
			if err := nonEmptyString(value, label+" rpcId"); err != nil {
				return err
			}
		}
		if value, ok := source["clientTimeZone"]; ok {
			return nonEmptyString(value, label+" clientTimeZone")
		}
		return nil
	case "plugin":
		return pluginSourceValue(source, label)
	case "model":
		if err := assertKeys(source, []string{"kind", "provider", "model"}, []string{"replayState"}, label); err != nil {
			return err
		}
		if err := nonEmptyString(source["provider"], label+" provider"); err != nil {
			return err
		}
		return nonEmptyString(source["model"], label+" model")
	case "tool":
		if err := assertKeys(source, []string{"kind", "callId"}, nil, label); err != nil {
			return err
		}
		return nonEmptyString(source["callId"], label+" callId")
	case "agent-instructions":
		if err := assertKeys(source, []string{"kind", "form", "changes"}, []string{"baseline", "baselineIdentity"}, label); err != nil {
			return err
		}
		if err := literalValue(source["form"], []string{"instructions"}, label+" form"); err != nil {
			return err
		}
		if value, ok := source["baseline"]; ok {
			if err := literalBool(value, true, label+" baseline"); err != nil {
				return err
			}
		}
		if value, ok := source["baselineIdentity"]; ok {
			if err := nonEmptyString(value, label+" baselineIdentity"); err != nil {
				return err
			}
		}
		if err := arrayValue(source["changes"], label+" changes", func(member any, memberLabel string) error {
			change, err := exactRecord(member, memberLabel, []string{"action", "scope", "path"}, []string{"digest"})
			if err != nil {
				return err
			}
			if err := literalValue(change["action"], []string{"set", "replace", "remove"}, memberLabel+" action"); err != nil {
				return err
			}
			if err := stringValue(change["scope"], memberLabel+" scope"); err != nil {
				return err
			}
			if err := stringValue(change["path"], memberLabel+" path"); err != nil {
				return err
			}
			if value, ok := change["digest"]; ok {
				return stringValue(value, memberLabel+" digest")
			}
			return nil
		}); err != nil {
			return err
		}
		return nil
	case "session-reference":
		return sessionReferenceSourceValue(source, label, version)
	case "team-message":
		if err := assertKeys(source, []string{"kind", "teamId", "messageId", "senderId", "senderName"}, nil, label); err != nil {
			return err
		}
		for _, key := range []string{"teamId", "messageId", "senderId"} {
			if err := nonEmptyString(source[key], label+" "+key); err != nil {
				return err
			}
		}
		return stringValue(source["senderName"], label+" senderName")
	case "goal":
		if err := assertKeys(source, []string{"kind", "goalId", "revision", "round"}, nil, label); err != nil {
			return err
		}
		if err := nonEmptyString(source["goalId"], label+" goalId"); err != nil {
			return err
		}
		if _, err := positiveIntegerValue(source["revision"], label+" revision"); err != nil {
			return err
		}
		_, err := positiveIntegerValue(source["round"], label+" round")
		return err
	case "skill-invocation":
		if err := assertKeys(source, []string{"kind", "name", "form"}, nil, label); err != nil {
			return err
		}
		if err := nonEmptyString(source["name"], label+" name"); err != nil {
			return err
		}
		return literalValue(source["form"], []string{"instructions"}, label+" form")
	case "skill-catalog":
		if err := assertKeys(source, []string{"kind", "form", "entries"}, []string{"update"}, label); err != nil {
			return err
		}
		if err := literalValue(source["form"], []string{"catalog"}, label+" form"); err != nil {
			return err
		}
		if value, ok := source["update"]; ok {
			if err := literalBool(value, true, label+" update"); err != nil {
				return err
			}
		}
		if err := arrayValue(source["entries"], label+" entries", func(member any, memberLabel string) error {
			entry, err := exactRecord(member, memberLabel, []string{"name", "description"}, nil)
			if err != nil {
				return err
			}
			if err := nonEmptyString(entry["name"], memberLabel+" name"); err != nil {
				return err
			}
			return stringValue(entry["description"], memberLabel+" description")
		}); err != nil {
			return err
		}
		return nil
	case "coordinator", "subagent-report":
		if err := assertKeys(source, []string{"kind", "form", "senderSessionId"}, nil, label); err != nil {
			return err
		}
		if err := literalValue(source["form"], []string{"relay"}, label+" form"); err != nil {
			return err
		}
		return nonEmptyString(source["senderSessionId"], label+" senderSessionId")
	case "subagent-settled":
		if err := assertKeys(source, []string{"kind", "form", "summary", "senderSessionId"}, nil, label); err != nil {
			return err
		}
		if err := literalValue(source["form"], []string{"notice"}, label+" form"); err != nil {
			return err
		}
		if err := stringValue(source["summary"], label+" summary"); err != nil {
			return err
		}
		return nonEmptyString(source["senderSessionId"], label+" senderSessionId")
	case "webhook":
		if err := assertKeys(source, []string{"kind", "provider", "source", "deliveryId", "ruleId", "form", "summary"}, nil, label); err != nil {
			return err
		}
		for _, key := range []string{"provider", "source", "deliveryId", "ruleId"} {
			if err := nonEmptyString(source[key], label+" "+key); err != nil {
				return err
			}
		}
		if err := literalValue(source["form"], []string{"notice"}, label+" form"); err != nil {
			return err
		}
		return stringValue(source["summary"], label+" summary")
	default:
		return nonEmptyString(source["kind"], label+" kind")
	}
}

func pluginSourceValue(source map[string]any, label string) error {
	optional := []string{"form", "sections", "summary"}
	isCompact := source["plugin"] == "compact"
	if isCompact {
		optional = append(optional, "compactionId", "sourceCommandId")
	}
	if err := assertKeys(source, []string{"kind", "plugin"}, optional, label); err != nil {
		return err
	}
	if err := nonEmptyString(source["plugin"], label+" plugin"); err != nil {
		return err
	}
	if isCompact {
		if err := nonEmptyString(source["compactionId"], label+" compactionId"); err != nil {
			return err
		}
		if value, ok := source["sourceCommandId"]; ok {
			if err := nonEmptyString(value, label+" sourceCommandId"); err != nil {
				return err
			}
		}
	}
	form, hasForm := source["form"]
	if !hasForm {
		return nil
	}
	switch form {
	case "snapshot":
		if err := arrayValue(source["sections"], label+" sections", func(member any, memberLabel string) error {
			section, err := exactRecord(member, memberLabel, []string{"name", "text"}, nil)
			if err != nil {
				return err
			}
			if err := nonEmptyString(section["name"], memberLabel+" name"); err != nil {
				return err
			}
			return stringValue(section["text"], memberLabel+" text")
		}); err != nil {
			return err
		}
	case "instructions", "catalog", "notice", "relay", "recall":
	default:
		return sessionformat.FormatErrorf("%s form must be one of instructions, catalog, snapshot, notice, relay, recall", label)
	}
	if value, ok := source["sections"]; ok && form != "snapshot" {
		_ = value
		return sessionformat.FormatErrorf("%s sections require snapshot form", label)
	}
	if form == "notice" {
		return stringValue(source["summary"], label+" summary")
	}
	if _, ok := source["summary"]; ok && form != "notice" {
		return sessionformat.FormatErrorf("%s summary requires notice form", label)
	}
	return nil
}

func sessionReferenceSourceValue(source map[string]any, label string, version int64) error {
	optional := []string{}
	if version >= 1 {
		optional = []string{"capturedFormatVersion"}
	}
	if err := assertKeys(source, []string{"kind", "form", "version", "references"}, nil, label); err != nil {
		return err
	}
	// NOTE: official admits capturedFormatVersion per reference; asserted below.
	if err := literalValue(source["form"], []string{"recall"}, label+" form"); err != nil {
		return err
	}
	if _, err := literalInt64(source["version"], 1, label+" version"); err != nil {
		return err
	}
	expectedInputIndex := int64(0)
	sessionIds := map[string]bool{}
	var referenceCount int
	if err := arrayValue(source["references"], label+" references", func(member any, memberLabel string) error {
		reference, err := exactRecord(member, memberLabel,
			[]string{"sessionId", "label", "capturedThroughSeq", "compacted", "originalMessages", "retainedMessages", "omittedMessages", "omittedBytes", "truncated", "inputIndex"},
			optional)
		if err != nil {
			return err
		}
		if err := nonEmptyString(reference["sessionId"], memberLabel+" sessionId"); err != nil {
			return err
		}
		if err := stringValue(reference["label"], memberLabel+" label"); err != nil {
			return err
		}
		if value, ok := reference["capturedThroughSeq"]; ok && value != nil {
			if _, err := countValue(value, memberLabel+" capturedThroughSeq"); err != nil {
				return err
			}
		}
		if value, ok := reference["capturedFormatVersion"]; ok {
			capturedVersion, err := countValue(value, memberLabel+" capturedFormatVersion")
			if err != nil {
				return err
			}
			if capturedVersion < 1 || capturedVersion > version {
				return sessionformat.FormatErrorf("%s capturedFormatVersion must be between 1 and %d", memberLabel, version)
			}
		}
		if _, ok := reference["compacted"].(bool); !ok {
			return sessionformat.FormatErrorf("%s compacted must be a boolean", memberLabel)
		}
		original, err := countValue(reference["originalMessages"], memberLabel+" originalMessages")
		if err != nil {
			return err
		}
		retained, err := countValue(reference["retainedMessages"], memberLabel+" retainedMessages")
		if err != nil {
			return err
		}
		omitted, err := countValue(reference["omittedMessages"], memberLabel+" omittedMessages")
		if err != nil {
			return err
		}
		omittedBytes, err := countValue(reference["omittedBytes"], memberLabel+" omittedBytes")
		if err != nil {
			return err
		}
		inputIndex, err := countValue(reference["inputIndex"], memberLabel+" inputIndex")
		if err != nil {
			return err
		}
		if _, ok := reference["truncated"].(bool); !ok {
			return sessionformat.FormatErrorf("%s truncated must be a boolean", memberLabel)
		}
		truncated, _ := reference["truncated"].(bool)
		if retained > original || omitted != original-retained {
			return sessionformat.FormatErrorf("%s message counts are inconsistent", memberLabel)
		}
		if truncated != (omitted > 0 || omittedBytes > 0) {
			return sessionformat.FormatErrorf("%s truncated disagrees with omitted content", memberLabel)
		}
		if inputIndex != expectedInputIndex {
			return sessionformat.FormatErrorf("%s inputIndex must match reference position", label)
		}
		expectedInputIndex++
		sessionId, _ := reference["sessionId"].(string)
		if sessionIds[sessionId] {
			return sessionformat.FormatErrorf("%s repeats sessionId %s", label, sessionId)
		}
		sessionIds[sessionId] = true
		referenceCount++
		return nil
	}); err != nil {
		return err
	}
	if referenceCount == 0 {
		return sessionformat.FormatErrorf("%s references must be non-empty", label)
	}
	return nil
}

func literalInt64(value any, allowed int64, label string) (int64, error) {
	parsed, err := countValue(value, label)
	if err != nil {
		return 0, err
	}
	if parsed != allowed {
		return 0, sessionformat.FormatErrorf("%s must be one of %d", label, allowed)
	}
	return parsed, nil
}

func streamChunkValue(value any, label string) error {
	chunk, err := recordOf(value, label)
	if err != nil {
		return err
	}
	switch chunk["type"] {
	case "block-start":
		if err := assertKeys(chunk, []string{"type", "index", "blockType"}, nil, label); err != nil {
			return err
		}
		if _, err := countValue(chunk["index"], label+" index"); err != nil {
			return err
		}
		return nonEmptyString(chunk["blockType"], label+" blockType")
	case "text-delta", "reasoning-delta":
		if err := assertKeys(chunk, []string{"type", "index", "text"}, nil, label); err != nil {
			return err
		}
		if _, err := countValue(chunk["index"], label+" index"); err != nil {
			return err
		}
		return stringValue(chunk["text"], label+" text")
	case "tool-call-delta":
		if err := assertKeys(chunk, []string{"type", "index", "id", "argumentsDelta"}, []string{"name"}, label); err != nil {
			return err
		}
		if _, err := countValue(chunk["index"], label+" index"); err != nil {
			return err
		}
		if err := nonEmptyString(chunk["id"], label+" id"); err != nil {
			return err
		}
		if value, ok := chunk["name"]; ok {
			if err := stringValue(value, label+" name"); err != nil {
				return err
			}
		}
		return stringValue(chunk["argumentsDelta"], label+" argumentsDelta")
	case "block-end":
		if err := assertKeys(chunk, []string{"type", "index", "block"}, nil, label); err != nil {
			return err
		}
		if _, err := countValue(chunk["index"], label+" index"); err != nil {
			return err
		}
		return contentBlockValue(chunk["block"], label+" block", 1)
	case "usage":
		if err := assertKeys(chunk, []string{"type", "usage"}, nil, label); err != nil {
			return err
		}
		return tokenUsageValue(chunk["usage"], label+" usage")
	case "finish":
		if err := assertKeys(chunk, []string{"type", "reason"}, []string{"replayState"}, label); err != nil {
			return err
		}
		if err := finishReasonValue(chunk["reason"], label+" reason"); err != nil {
			return err
		}
		if value, ok := chunk["replayState"]; ok {
			return replayEnvelopeValue(value, label+" replayState")
		}
		return nil
	default:
		return sessionformat.FormatErrorf("%s has unknown stream chunk type %v", label, chunk["type"])
	}
}

func finishReasonValue(value any, label string) error {
	reason, err := recordOf(value, label)
	if err != nil {
		return err
	}
	kind, _ := reason["kind"].(string)
	switch kind {
	case "aborted", "error":
		if err := assertKeys(reason, []string{"kind", "failure"}, nil, label); err != nil {
			return err
		}
		return llmFailureValue(reason["failure"], label+" failure")
	case "stop", "tool-calls", "max-tokens":
		if err := assertKeys(reason, []string{"kind"}, nil, label); err != nil {
			return err
		}
		return nil
	}
	return nonEmptyString(reason["kind"], label+" kind")
}

func replayEnvelopeValue(value any, label string) error {
	replay, err := exactRecord(value, label, []string{"response"}, []string{"blocks"})
	if err != nil {
		return err
	}
	if value, ok := replay["blocks"]; ok {
		if _, ok := value.([]any); !ok {
			return sessionformat.FormatErrorf("%s blocks must be an array", label)
		}
	}
	return nil
}

func turnEndReasonValue(value any, label string) error {
	reason, err := recordOf(value, label)
	if err != nil {
		return err
	}
	kind, _ := reason["kind"].(string)
	switch kind {
	case "completed", "blocked", "max-tokens", "interrupted":
		return assertKeys(reason, []string{"kind"}, nil, label)
	case "aborted":
		if err := assertKeys(reason, []string{"kind", "reason"}, nil, label); err != nil {
			return err
		}
		cause, err := recordOf(reason["reason"], label+" abort cause")
		if err != nil {
			return err
		}
		if cause["kind"] == "hook" {
			if err := assertKeys(cause, []string{"kind", "reason"}, nil, label+" abort cause"); err != nil {
				return err
			}
			return stringValue(cause["reason"], label+" abort reason")
		}
		if err := assertKeys(cause, []string{"kind"}, nil, label+" abort cause"); err != nil {
			return err
		}
		return literalValue(cause["kind"], []string{"user", "parent", "disposed", "legacy"}, label+" abort kind")
	case "error":
		if err := assertKeys(reason, []string{"kind", "error"}, nil, label); err != nil {
			return err
		}
		return llmFailureValue(reason["error"], label+" error")
	default:
		return nonEmptyString(reason["kind"], label+" kind")
	}
}

func requestHeaderValue(value any, label string) error {
	header, err := exactRecord(value, label, []string{"config"}, []string{"adapterDefaults", "system", "tools"})
	if err != nil {
		return err
	}
	config, err := exactRecord(header["config"], label+" config",
		[]string{"provider", "model"},
		[]string{"reasoningEffort", "temperature", "maxTokens", "stop"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(config["provider"], label+" provider"); err != nil {
		return err
	}
	if err := nonEmptyString(config["model"], label+" model"); err != nil {
		return err
	}
	if value, ok := config["reasoningEffort"]; ok {
		if err := nonEmptyString(value, label+" reasoningEffort"); err != nil {
			return err
		}
	}
	if value, ok := config["temperature"]; ok {
		if _, err := finiteNumberValue(value, label+" temperature"); err != nil {
			return err
		}
	}
	if value, ok := config["maxTokens"]; ok {
		if _, err := positiveIntegerValue(value, label+" maxTokens"); err != nil {
			return err
		}
	}
	if value, ok := config["stop"]; ok {
		if err := arrayValue(value, label+" stop", func(member any, memberLabel string) error {
			return stringValue(member, memberLabel)
		}); err != nil {
			return err
		}
	}
	if value, ok := header["adapterDefaults"]; ok {
		defaults, err := exactRecord(value, label+" adapterDefaults", nil, []string{"reasoningEffort", "maxTokens"})
		if err != nil {
			return err
		}
		for key, marker := range defaults {
			if err := literalBool(marker, true, label+" adapterDefaults "+key); err != nil {
				return err
			}
			if _, ok := config[key]; !ok {
				return sessionformat.FormatErrorf("%s adapter default %s lacks config value", label, key)
			}
		}
	}
	if value, ok := header["system"]; ok {
		if err := stringValue(value, label+" system"); err != nil {
			return err
		}
	}
	if value, ok := header["tools"]; ok {
		err := arrayValue(value, label+" tools", func(member any, memberLabel string) error {
			return toolSchemaValue(member, memberLabel)
		})
		return err
	}
	return nil
}

func toolSchemaValue(value any, label string) error {
	schema, err := exactRecord(value, label, []string{"name", "description", "parameters"}, nil)
	if err != nil {
		return err
	}
	if err := nonEmptyString(schema["name"], label+" name"); err != nil {
		return err
	}
	if err := stringValue(schema["description"], label+" description"); err != nil {
		return err
	}
	_, err = recordOf(schema["parameters"], label+" parameters")
	return err
}

func shadowedValue(data map[string]any, eventSeq int64, label string) error {
	rangeRecord, err := exactRecord(data["shadowedRange"], label+" shadowedRange", []string{"start", "end"}, nil)
	if err != nil {
		return err
	}
	start, err := earlierSeq(rangeRecord["start"], eventSeq, label+" shadowedRange start")
	if err != nil {
		return err
	}
	end, err := earlierSeq(rangeRecord["end"], eventSeq, label+" shadowedRange end")
	if err != nil {
		return err
	}
	members, err := arrayMembers(data["shadowedSeqs"], label+" shadowedSeqs", func(member any, memberLabel string) error {
		_, err := earlierSeq(member, eventSeq, memberLabel)
		return err
	})
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return sessionformat.FormatErrorf("%s shadowedSeqs must be non-empty", label)
	}
	first, err := countValue(members[0], label+" shadowedSeqs member")
	if err != nil {
		return err
	}
	last, err := countValue(members[len(members)-1], label+" shadowedSeqs member")
	if err != nil {
		return err
	}
	if first != start || last != end {
		return sessionformat.FormatErrorf("%s shadowedRange must match shadowedSeqs endpoints", label)
	}
	_, err = countValue(data["shadowedTokenCount"], label+" shadowedTokenCount")
	return err
}

func goalChangeValue(data map[string]any, label string) error {
	if err := literalValue(data["kind"], []string{"goal/change"}, label+" kind"); err != nil {
		return err
	}
	if _, err := literalInt64(data["version"], 1, label+" version"); err != nil {
		return err
	}
	if data["operation"] == "clear" {
		if err := assertKeys(data, []string{"kind", "version", "operation", "cleared", "clearedAt"}, nil, label+" data"); err != nil {
			return err
		}
		if err := goalRefValue(data["cleared"], label+" cleared"); err != nil {
			return err
		}
		_, err := countValue(data["clearedAt"], label+" clearedAt")
		return err
	}
	if err := assertKeys(data,
		[]string{"kind", "version", "operation", "goal", "roundsStarted", "createdAt", "updatedAt"},
		nil, label+" data"); err != nil {
		return err
	}
	if err := literalValue(data["operation"], []string{"create", "edit", "pause", "resume", "complete", "block"}, label+" operation"); err != nil {
		return err
	}
	if err := goalSnapshotValue(data["goal"], label+" goal"); err != nil {
		return err
	}
	if _, err := countValue(data["roundsStarted"], label+" roundsStarted"); err != nil {
		return err
	}
	if _, err := countValue(data["createdAt"], label+" createdAt"); err != nil {
		return err
	}
	_, err := countValue(data["updatedAt"], label+" updatedAt")
	return err
}

func goalRefValue(value any, label string) error {
	ref, err := exactRecord(value, label, []string{"id", "revision"}, nil)
	if err != nil {
		return err
	}
	if err := nonEmptyString(ref["id"], label+" id"); err != nil {
		return err
	}
	_, err = positiveIntegerValue(ref["revision"], label+" revision")
	return err
}

func goalSnapshotValue(value any, label string) error {
	goal, err := exactRecord(value, label,
		[]string{"id", "revision", "objective", "phase", "maxGoalRounds"}, []string{"blockedReason"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(goal["id"], label+" id"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(goal["revision"], label+" revision"); err != nil {
		return err
	}
	if err := nonEmptyString(goal["objective"], label+" objective"); err != nil {
		return err
	}
	if err := literalValue(goal["phase"], []string{"active", "paused", "blocked", "complete"}, label+" phase"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(goal["maxGoalRounds"], label+" maxGoalRounds"); err != nil {
		return err
	}
	if goal["phase"] == "blocked" {
		reason, err := exactRecord(goal["blockedReason"], label+" blockedReason", []string{"code", "message"}, nil)
		if err != nil {
			return err
		}
		if err := nonEmptyString(reason["code"], label+" blocked code"); err != nil {
			return err
		}
		return nonEmptyString(reason["message"], label+" blocked message")
	}
	if _, ok := goal["blockedReason"]; ok {
		return sessionformat.FormatErrorf("%s blockedReason requires blocked phase", label)
	}
	return nil
}

func scheduleChangeValue(data map[string]any, label string) error {
	if _, err := literalInt64(data["version"], 1, label+" version"); err != nil {
		return err
	}
	if data["operation"] == "create" {
		if err := assertKeys(data, []string{"version", "operation", "schedule"}, nil, label+" data"); err != nil {
			return err
		}
		return scheduleRecordValue(data["schedule"], label+" schedule")
	}
	optional := []string{}
	if data["operation"] == "dispatch" {
		optional = []string{"acceptedAt"}
	}
	if err := assertKeys(data, []string{"version", "operation", "id"}, optional, label+" data"); err != nil {
		return err
	}
	if err := literalValue(data["operation"], []string{"delete", "dispatch"}, label+" operation"); err != nil {
		return err
	}
	if err := scheduleIdValue(data["id"], label+" id"); err != nil {
		return err
	}
	if value, ok := data["acceptedAt"]; ok {
		return instantValue(value, label+" acceptedAt")
	}
	return nil
}

func scheduleRecordValue(value any, label string) error {
	record, err := recordOf(value, label)
	if err != nil {
		return err
	}
	switch record["kind"] {
	case "after":
		if err := assertKeys(record, []string{"id", "kind", "prompt", "afterSeconds", "scheduledAt"}, nil, label); err != nil {
			return err
		}
		if _, err := positiveIntegerValue(record["afterSeconds"], label+" afterSeconds"); err != nil {
			return err
		}
	case "at":
		if err := assertKeys(record, []string{"id", "kind", "prompt", "scheduledAt"}, nil, label); err != nil {
			return err
		}
	case "every":
		if err := assertKeys(record, []string{"id", "kind", "prompt", "everySeconds", "scheduledAt"}, nil, label); err != nil {
			return err
		}
		seconds, err := positiveIntegerValue(record["everySeconds"], label+" everySeconds")
		if err != nil {
			return err
		}
		if seconds < 300 {
			return sessionformat.FormatErrorf("%s everySeconds must be at least 300", label)
		}
	default:
		return sessionformat.FormatErrorf("%s has unknown schedule kind", label)
	}
	if err := scheduleIdValue(record["id"], label+" id"); err != nil {
		return err
	}
	if err := nonEmptyString(record["prompt"], label+" prompt"); err != nil {
		return err
	}
	return instantValue(record["scheduledAt"], label+" scheduledAt")
}

func scheduleIdValue(value any, label string) error {
	if err := nonEmptyString(value, label); err != nil {
		return err
	}
	text, _ := value.(string)
	if strings.TrimSpace(text) != text {
		return sessionformat.FormatErrorf("%s must not have surrounding whitespace", label)
	}
	return nil
}

func instantValue(value any, label string) error {
	text, ok := value.(string)
	if !ok || !instantPatternMatches(text) {
		return sessionformat.FormatErrorf("%s must be a canonical UTC instant", label)
	}
	parsed, err := parseInstant(text)
	if err != nil || !instantPatternMatches(text) || !parsed.equalCanonical(text) {
		return sessionformat.FormatErrorf("%s must be a canonical UTC instant", label)
	}
	return nil
}

func titleSourceValue(value any, label string) error {
	source, err := recordOf(value, label)
	if err != nil {
		return err
	}
	if source["kind"] == "provider" {
		if err := assertKeys(source, []string{"kind", "provider"}, []string{"model"}, label); err != nil {
			return err
		}
		if err := nonEmptyString(source["provider"], label+" provider"); err != nil {
			return err
		}
		if model, ok := source["model"]; ok {
			return modelRouteValue(model, label+" model")
		}
		return nil
	}
	if err := assertKeys(source, []string{"kind"}, nil, label); err != nil {
		return err
	}
	return literalValue(source["kind"], []string{"fallback", "user"}, label+" kind")
}

func modelRouteValue(value any, label string) error {
	route, err := exactRecord(value, label, []string{"provider", "model"}, nil)
	if err != nil {
		return err
	}
	if err := nonEmptyString(route["provider"], label+" provider"); err != nil {
		return err
	}
	return nonEmptyString(route["model"], label+" model")
}

func subagentDescriptorValue(data map[string]any, label string) error {
	if _, err := literalInt64(data["version"], 3, label+" version"); err != nil {
		return err
	}
	if err := nonEmptyString(data["provider"], label+" provider"); err != nil {
		return err
	}
	if data["mode"] == "one-shot" {
		if err := assertKeys(data, []string{"mode", "version", "provider"}, []string{"label"}, label+" data"); err != nil {
			return err
		}
		if value, ok := data["label"]; ok {
			return stringValue(value, label+" label")
		}
		return nil
	}
	if err := literalValue(data["mode"], []string{"continuable"}, label+" mode"); err != nil {
		return err
	}
	if err := nonEmptyString(data["label"], label+" label"); err != nil {
		return err
	}
	for _, key := range []string{"agentProvider", "agentModel", "agentReasoningEffort", "persona"} {
		if value, ok := data[key]; ok {
			if err := nonEmptyString(value, label+" "+key); err != nil {
				return err
			}
		}
	}
	_, hasProvider := data["agentProvider"]
	_, hasModel := data["agentModel"]
	if hasProvider != hasModel {
		return sessionformat.FormatErrorf("%s agentProvider and agentModel must be paired", label)
	}
	if value, ok := data["toolFilter"]; ok {
		filter, err := exactRecord(value, label+" toolFilter", nil, []string{"allow", "deny"})
		if err != nil {
			return err
		}
		_, hasAllow := filter["allow"]
		_, hasDeny := filter["deny"]
		if !hasAllow && !hasDeny {
			return sessionformat.FormatErrorf("%s toolFilter requires allow or deny", label)
		}
		if hasAllow {
			if err := arrayValue(filter["allow"], label+" allow", func(member any, memberLabel string) error {
				return nonEmptyString(member, memberLabel)
			}); err != nil {
				return err
			}
		}
		if hasDeny {
			err := arrayValue(filter["deny"], label+" deny", func(member any, memberLabel string) error {
				return nonEmptyString(member, memberLabel)
			})
			return err
		}
	}
	return nil
}

func allowedModelsValue(value any, label string) error {
	seen := map[string]bool{}
	var routeCount int
	if err := arrayValue(value, label, func(member any, memberLabel string) error {
		route, err := exactRecord(member, memberLabel, []string{"provider", "model"}, nil)
		if err != nil {
			return err
		}
		if err := nonEmptyString(route["provider"], memberLabel+" provider"); err != nil {
			return err
		}
		if err := nonEmptyString(route["model"], memberLabel+" model"); err != nil {
			return err
		}
		provider, _ := route["provider"].(string)
		model, _ := route["model"].(string)
		key := provider + "\x00" + model
		if seen[key] {
			return sessionformat.FormatErrorf("%s repeats route %s", label, key)
		}
		seen[key] = true
		routeCount++
		return nil
	}); err != nil {
		return err
	}
	if routeCount == 0 {
		return sessionformat.FormatErrorf("%s must be non-empty", label)
	}
	return nil
}

func teamSelector(data map[string]any, label string) error {
	if _, err := literalInt64(data["version"], 1, label+" version"); err != nil {
		return err
	}
	return nonEmptyString(data["teamId"], label+" teamId")
}

func teamMemberValue(value any, label string) error {
	member, err := exactRecord(value, label,
		[]string{"id", "name", "description", "provider", "context", "phase"}, []string{"error"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(member["id"], label+" id"); err != nil {
		return err
	}
	if err := stringValue(member["name"], label+" name"); err != nil {
		return err
	}
	if err := stringValue(member["description"], label+" description"); err != nil {
		return err
	}
	if err := stringValue(member["provider"], label+" provider"); err != nil {
		return err
	}
	if err := literalValue(member["context"], []string{"fresh", "fork"}, label+" context"); err != nil {
		return err
	}
	if err := literalValue(member["phase"], []string{"provisioning", "active", "failed"}, label+" phase"); err != nil {
		return err
	}
	if value, ok := member["error"]; ok {
		return stringValue(value, label+" error")
	}
	return nil
}

func teamTaskValue(value any, label string) error {
	task, err := exactRecord(value, label,
		[]string{"id", "revision", "subject", "description", "status", "blockedBy", "writeScopes"},
		[]string{"ownerId"})
	if err != nil {
		return err
	}
	if err := nonEmptyString(task["id"], label+" id"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(task["revision"], label+" revision"); err != nil {
		return err
	}
	if err := stringValue(task["subject"], label+" subject"); err != nil {
		return err
	}
	if err := stringValue(task["description"], label+" description"); err != nil {
		return err
	}
	if err := literalValue(task["status"], []string{"pending", "in_progress", "completed", "deleted"}, label+" status"); err != nil {
		return err
	}
	if value, ok := task["ownerId"]; ok {
		if err := nonEmptyString(value, label+" ownerId"); err != nil {
			return err
		}
	}
	if err := arrayValue(task["blockedBy"], label+" blockedBy", func(member any, memberLabel string) error {
		return nonEmptyString(member, memberLabel)
	}); err != nil {
		return err
	}
	return arrayValue(task["writeScopes"], label+" writeScopes", func(member any, memberLabel string) error {
		return stringValue(member, memberLabel)
	})
}

func teamMessageValue(value any, label string, version int64) error {
	message, err := exactRecord(value, label,
		[]string{"id", "senderId", "senderName", "targetId", "delivery", "content"}, nil)
	if err != nil {
		return err
	}
	for _, key := range []string{"id", "senderId", "targetId"} {
		if err := nonEmptyString(message[key], label+" "+key); err != nil {
			return err
		}
	}
	if err := stringValue(message["senderName"], label+" senderName"); err != nil {
		return err
	}
	if err := literalValue(message["delivery"], []string{"quiet", "wakeup"}, label+" delivery"); err != nil {
		return err
	}
	return contentBlocksValue(message["content"], label+" content", version)
}

func workflowIdentity(data map[string]any, label string) error {
	if err := nonEmptyString(data["runId"], label+" runId"); err != nil {
		return err
	}
	_, err := positiveIntegerValue(data["seq"], label+" seq")
	return err
}

func deepSeekSearchBodyValue(value any, label string) error {
	body, err := exactRecord(value, label, []string{"model", "max_tokens", "messages", "tools"}, nil)
	if err != nil {
		return err
	}
	if err := nonEmptyString(body["model"], label+" model"); err != nil {
		return err
	}
	if _, err := positiveIntegerValue(body["max_tokens"], label+" max_tokens"); err != nil {
		return err
	}
	messageCount := 0
	if err := arrayValue(body["messages"], label+" messages", func(member any, memberLabel string) error {
		message, err := exactRecord(member, memberLabel, []string{"role", "content"}, nil)
		if err != nil {
			return err
		}
		if err := literalValue(message["role"], []string{"user"}, memberLabel+" role"); err != nil {
			return err
		}
		blockCount := 0
		if err := arrayValue(message["content"], memberLabel+" content", func(block any, blockLabel string) error {
			text, err := exactRecord(block, blockLabel, []string{"type", "text"}, nil)
			if err != nil {
				return err
			}
			if err := literalValue(text["type"], []string{"text"}, blockLabel+" type"); err != nil {
				return err
			}
			if err := stringValue(text["text"], blockLabel+" text"); err != nil {
				return err
			}
			blockCount++
			return nil
		}); err != nil {
			return err
		}
		if blockCount != 1 {
			return sessionformat.FormatErrorf("%s content must contain one text block", memberLabel)
		}
		messageCount++
		return nil
	}); err != nil {
		return err
	}
	if messageCount != 1 {
		return sessionformat.FormatErrorf("%s messages must contain one user message", label)
	}
	toolCount := 0
	if err := arrayValue(body["tools"], label+" tools", func(member any, memberLabel string) error {
		tool, err := exactRecord(member, memberLabel, []string{"type", "name", "max_uses"}, nil)
		if err != nil {
			return err
		}
		if err := literalValue(tool["type"], []string{"web_search_20250305"}, memberLabel+" type"); err != nil {
			return err
		}
		if err := literalValue(tool["name"], []string{"web_search"}, memberLabel+" name"); err != nil {
			return err
		}
		if _, err := positiveIntegerValue(tool["max_uses"], memberLabel+" max_uses"); err != nil {
			return err
		}
		toolCount++
		return nil
	}); err != nil {
		return err
	}
	if toolCount != 1 {
		return sessionformat.FormatErrorf("%s tools must contain one web search tool", label)
	}
	return nil
}
