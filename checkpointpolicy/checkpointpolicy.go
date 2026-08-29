// Package checkpointpolicy ports @deepseek-ai/dsh-session-checkpoint-policy:
// semantic durability checkpoints for model requests, top-level tool
// dispatch, and completed agent steps.
//
// Loop-built model calls checkpoint the logged request before adapter
// dispatch; top-level tool calls checkpoint their recorded call before the
// tool body; the next request boundary checkpoints the preceding
// response/result batch. Nested tool dispatches reuse the durable outer
// call.
//
// Checkpoint failures are fail-closed at the model and tool side-effect
// boundaries: the downstream adapter or tool body is not invoked.
//
// Go adaptation: the sessions registry is an explicit Flusher seam keyed by
// session id, and the tool arm resolves the owning session id from the
// execution's agent key through a caller-provided resolver.
package checkpointpolicy

import (
	"dshgo/agent"
	"dshgo/llm"
	"dshgo/tools"
)

// Name is the cordis plugin name used by loader diagnostics.
const Name = "session-checkpoint-policy"

// Flusher is the sessions-registry seam: everything already committed to the
// named session becomes durable before the call returns.
type Flusher interface {
	FlushSession(sessionID string) error
}

// abortedBeforeDispatchResult materializes the canonical result for a call
// cancelled before tool dispatch (mirrors the tools pipeline's canonical
// shape verbatim).
func abortedBeforeDispatchResult() *tools.ToolExecutionResult {
	return &tools.ToolExecutionResult{
		IsError: true,
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: tool call aborted before dispatch"}},
		Error: &tools.ToolFailure{
			Message: "tool call aborted before dispatch",
			Info:    &tools.ToolErrorInfo{Name: "AbortError", Code: tools.CodeToolAbortedBeforeDispatch},
		},
	}
}

// checkpointFailureResult is the fail-closed tool outcome when the
// pre-dispatch checkpoint could not make the call durable.
func checkpointFailureResult(err error) *tools.ToolExecutionResult {
	return &tools.ToolExecutionResult{
		IsError: true,
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: durability checkpoint failed: " + err.Error()}},
		Error: &tools.ToolFailure{
			Message: "durability checkpoint failed: " + err.Error(),
		},
	}
}

// Attach installs the semantic checkpoint listeners. The returned disposer
// detaches all of them.
func Attach(llmRuntime *llm.Runtime, toolRuntime *tools.ToolRuntime, agents *agent.AgentRegistry, flusher Flusher, resolveSessionID func(tools.ScopeKey) (string, bool)) (func(), error) {
	detachStream := func() {}
	if llmRuntime != nil {
		detachStream = llmRuntime.OnStream(func(options llm.GenerateOptions, next func(llm.GenerateOptions) llm.Seq) llm.Seq {
			if options.SessionID == "" || flusher == nil {
				return next(options)
			}
			// Delay construction of the downstream stream until the
			// complete logged request prefix is durable: the checkpoint
			// runs at the first pull, before the first chunk is requested.
			// A checkpoint failure prevents adapter dispatch and surfaces
			// as the stream's terminal error finish.
			return func(yield func(llm.StreamChunk) bool) {
				if err := flusher.FlushSession(options.SessionID); err != nil {
					yield(llm.StreamChunk{Type: llm.ChunkFinish, Reason: &llm.FinishReason{
						Kind:    llm.FinishError,
						Failure: &llm.LlmFailure{Message: "durability checkpoint failed: " + err.Error(), Code: "CHECKPOINT_FAILED"},
					}})
					return
				}
				next(options)(yield)
			}
		})
	}
	detachExecute := func() {}
	if toolRuntime != nil {
		detachExecute = toolRuntime.OnExecute(nil, func(exec *tools.ToolRunContext, next func(*tools.ToolRunContext) *tools.ToolExecutionResult) *tools.ToolExecutionResult {
			// Nested tool dispatches reuse the durable outer call.
			if exec.Agent == nil || exec.Parent != nil || flusher == nil {
				return next(exec)
			}
			var sessionID string
			if resolveSessionID != nil {
				sessionID, _ = resolveSessionID(exec.Agent)
			}
			if sessionID == "" {
				return next(exec)
			}
			if err := flusher.FlushSession(sessionID); err != nil {
				return checkpointFailureResult(err)
			}
			if exec.Signal != nil && exec.Signal.Err() != nil {
				return abortedBeforeDispatchResult()
			}
			return next(exec)
		})
	}
	detachPreStep := func() {}
	if agents != nil {
		detachPreStep = agents.Events().OnWaterfall(agent.EventPreStep, nil, func(payload any, next func(any) any) any {
			preStep, ok := payload.(agent.PreStepPayload)
			if !ok {
				return next(payload)
			}
			// Before each request, persist everything committed by the
			// preceding step; the first step's call is an intentional no-op
			// beyond any prompt intake.
			if flusher != nil && preStep.Agent != nil && preStep.Agent.Session != nil {
				_ = flusher.FlushSession(string(preStep.Agent.Session.ID()))
			}
			return next(payload)
		})
	}
	return func() {
		detachStream()
		detachExecute()
		detachPreStep()
	}, nil
}
