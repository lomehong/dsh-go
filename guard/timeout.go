package guard

import (
	"context"
	"fmt"
	"time"

	"dshgo/llm"
	"dshgo/tools"
)

// ToolTimeout is the code owned by the timeout policy, used both as the
// deadline classification and as the structured error code on the
// replacement tool result.
const ToolTimeout = "TOOL_TIMEOUT"

// ToolTimeoutResult is the structured result substituted when the policy's
// deadline wins: the model-facing message plus a TOOL_TIMEOUT error so a
// retry/sandbox policy (and replay) can route on it.
func ToolTimeoutResult(timeoutMs int) *tools.ToolExecutionResult {
	message := fmt.Sprintf("tool call timed out after %dms", timeoutMs)
	return &tools.ToolExecutionResult{
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: " + message}},
		IsError: true,
		Error: &tools.ToolFailure{
			Message: message,
			Info:    &tools.ToolErrorInfo{Name: "ToolTimeoutError", Code: ToolTimeout},
		},
	}
}

// AttachTimeoutPolicy registers the cooperative timeout wrapper: it resolves
// the caller-visible tool definition, arms the declared budget, swaps the
// derived deadline onto the execution for dispatch, restores the upstream
// signal after delegation, and replaces the result only when this wrapper's
// own timer fired. The returned disposer detaches the wrapper.
func AttachTimeoutPolicy(runtime *tools.ToolRuntime) func() {
	detach := runtime.OnExecute(nil, func(exec *tools.ToolRunContext, next func(*tools.ToolRunContext) *tools.ToolExecutionResult) *tools.ToolExecutionResult {
		definition, ok := runtime.Get(exec.Name, exec.Agent)
		if !ok || definition.TimeoutMs == 0 {
			// A tool that declares no budget: no deadline, delegate
			// unchanged.
			return next(exec)
		}
		timeoutMs := int(definition.TimeoutMs)
		deadlineCtx, cancel := context.WithTimeout(exec.Signal, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
		upstream := exec.Signal
		exec.Signal = deadlineCtx
		defer func() { exec.Signal = upstream }()
		result := next(exec)
		// Only OUR timer firing replaces the result (a nested outer
		// deadline reads as an ordinary upstream cancel here): the
		// tool/capability saw the abort and reached quiescence, so its own
		// abort result yields to the structured TOOL_TIMEOUT.
		if deadlineCtx.Err() == context.DeadlineExceeded {
			return ToolTimeoutResult(timeoutMs)
		}
		return result
	})
	return detach
}
