// Codec ports hook-protocol/src/codec.ts: decode hook process outcomes for
// both dialects. Exit 0 may carry structured JSON or plain stdout; exit 2
// blocks with stderr as the reason; every other exit is a non-blocking
// error. Bridges decide which recognized fields apply.
package hookprotocol

import (
	"encoding/json"
	"strings"
)

// blockingExitCode is the exit code a hook uses to signal a blocking error
// (stderr → model).
const blockingExitCode = 2

// topLevelDecisionOf folds the legacy TOP-LEVEL decision, which is only
// approve/block in both reference schemas — allow/deny/ask are reserved for
// hookSpecificOutput.permissionDecision. An out-of-band {"decision":"deny"}
// is invalid and ignored here (it must not become a real blocking decision).
func topLevelDecisionOf(value string) HookDecision {
	if value == string(DecisionApprove) || value == string(DecisionBlock) {
		return HookDecision(value)
	}
	return ""
}

// permissionDecisionOf accepts a hookSpecificOutput.permissionDecision,
// which is allow/deny/ask only.
func permissionDecisionOf(value string) HookDecision {
	if value == string(DecisionAllow) || value == string(DecisionDeny) || value == string(DecisionAsk) {
		return HookDecision(value)
	}
	return ""
}

// asObject reports a plain (non-null, non-array) JSON object.
func asObject(value json.RawMessage) (map[string]any, bool) {
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, false
	}
	obj, ok := decoded.(map[string]any)
	return obj, ok
}

// parseHookOutput decodes process output into a dialect-neutral hook
// outcome. This function is total: malformed JSON remains plain stdout.
// When expectedEventName is set, a missing or different
// hookSpecificOutput.hookEventName discards only its event-scoped fields;
// top-level fields and the claimed discriminator remain. An empty
// expectedEventName applies the block as-is.
//
// exitCode is nil when the process could not be run; stdout is parsed as
// structured JSON only on exit 0; stderr becomes the blocking reason on
// exit 2.
func ParseHookOutput(exitCode *int, stdout, stderr, expectedEventName string) HookOutput {
	trimmedErr := strings.TrimSpace(stderr)
	trimmedOut := strings.TrimSpace(stdout)
	// Plain stdout remains available even when it is not JSON.
	output := HookOutput{ExitCode: exitCode, Stderr: trimmedErr, Stdout: trimmedOut}

	// Both dialects treat exit 2 as a block with stderr as its reason.
	if exitCode != nil && *exitCode == blockingExitCode {
		output.Decision = DecisionBlock
		if trimmedErr != "" {
			reason := trimmedErr
			output.Reason = &reason
		}
	}

	// Structured stdout is valid only for a clean exit.
	if exitCode != nil && *exitCode == 0 {
		// Only attempt JSON when stdout looks like a JSON object — matches
		// the reference engines, which treat other stdout as plain text, not
		// an error.
		if strings.HasPrefix(trimmedOut, "{") {
			if parsed, ok := asObject(json.RawMessage(trimmedOut)); ok {
				applyStructured(&output, parsed, expectedEventName)
			}
			// Malformed JSON on a clean exit = no structured output
			// (lenient, as the reference engines are). The plain stdout
			// remains the bridge's to use.
		}
	}

	return output
}

// applyStructured folds a parsed structured-stdout object into output.
// expectedEventName (the firing event) gates the per-event
// hookSpecificOutput block: a block whose hookEventName names a different
// event — OR omits it — has its event-scoped fields discarded (any present
// hookEventName is still recorded).
func applyStructured(output *HookOutput, parsed map[string]any, expectedEventName string) {
	if cont, ok := parsed["continue"].(bool); ok {
		output.Continue = &cont
	}
	if stopReason, ok := parsed["stopReason"].(string); ok {
		output.StopReason = &stopReason
	}
	if sysMsg, ok := parsed["systemMessage"].(string); ok {
		output.SystemMessage = sysMsg
	}

	// Top-level legacy decision (approve/block ONLY — allow/deny/ask there
	// are invalid per both schemas) + its reason.
	if raw, ok := parsed["decision"].(string); ok {
		if topDecision := topLevelDecisionOf(raw); topDecision != "" {
			output.Decision = topDecision
		}
	}
	if topReason, ok := parsed["reason"].(string); ok {
		output.Reason = &topReason
	}

	// hookSpecificOutput: the per-event channel, keyed by hookEventName.
	// The permissionDecision (allow/deny/ask) OVERRIDES the legacy top-level
	// decision; additionalContext and updatedInput live here too.
	hso, ok := parsed["hookSpecificOutput"].(map[string]any)
	if !ok {
		return
	}
	eventName, _ := hso["hookEventName"].(string)
	// Always surface the discriminator (for the log/diagnostics), even on a
	// mismatch — the record should show what the malformed block claimed.
	if eventName != "" {
		output.HookEventName = eventName
	}
	// A missing or mismatched discriminator cannot affect the firing event.
	if expectedEventName != "" && eventName != expectedEventName {
		return
	}
	if permission, ok := hso["permissionDecision"].(string); ok {
		if permissionDecision := permissionDecisionOf(permission); permissionDecision != "" {
			output.Decision = permissionDecision
		}
	}
	if permissionReason, ok := hso["permissionDecisionReason"].(string); ok {
		output.Reason = &permissionReason
	}
	if addCtx, ok := hso["additionalContext"].(string); ok {
		output.AdditionalContext = addCtx
	}
	if updated, ok := hso["updatedInput"].(map[string]any); ok {
		output.UpdatedInput = updated
	}
}
