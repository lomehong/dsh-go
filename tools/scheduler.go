package tools

// Scheduler-only staged view over the execution pipeline, consumed by
// dshgo/agentloop's parallel scheduler. Ordinary callers use Execute; this is
// not a plugin extension point.
//
// Port of the ToolRuntimeScheduler symbol-keyed view in
// packages/core/tools/src/index.ts. Go adaptations: the staged methods are
// exported methods on ToolRuntime (Go has no symbol-keyed members), and
// preparation/dispatch errors materialize into the same result shapes the
// source's try/catch produces instead of rejecting a promise.

// Staged preparation kinds.
const (
	PreparedDispatch    = "dispatch"     // run the around-dispatch/body stage
	PreparedPostResult  = "post-result"  // a result that still receives post-execute
	PreparedFinalResult = "final-result" // a result that bypasses post-execute
)

// ScheduledToolPreparation is the scheduler-only result after ordered
// pre-execute and guards: either a dispatch instruction or a pre-staged
// result. Kind selects which; Result is set exactly when Kind is not
// PreparedDispatch.
type ScheduledToolPreparation struct {
	Kind   string
	Exec   *ToolRunContext
	Result *ToolExecutionResult
}

// ScheduledToolDispatch is the scheduler-only dispatch result. Kind is
// PreparedPostResult (the result still receives post-execute) or
// PreparedFinalResult (already matching Execute failure semantics).
type ScheduledToolDispatch struct {
	Kind   string
	Result *ToolExecutionResult
}

// Prepare materializes input, runs the ordered pre-execute/guard gate, and
// decides what stage follows. The returned execution is registry-minted: it
// must flow through Dispatch/Finalize/Finish of this runtime.
func (rt *ToolRuntime) Prepare(input *ToolExecutionInput) *ScheduledToolPreparation {
	exec, staged := rt.prepareExecution(input)
	switch {
	case staged.final:
		return &ScheduledToolPreparation{Kind: PreparedFinalResult, Exec: exec, Result: staged.result}
	case staged.post:
		return &ScheduledToolPreparation{Kind: PreparedPostResult, Exec: exec, Result: staged.result}
	default:
		return &ScheduledToolPreparation{Kind: PreparedDispatch, Exec: exec}
	}
}

// Dispatch runs only the around-dispatch/body stage for a PreparedDispatch
// execution.
func (rt *ToolRuntime) Dispatch(exec *ToolRunContext) *ScheduledToolDispatch {
	dispatched := rt.dispatchScheduledExecution(exec)
	if dispatched.post {
		return &ScheduledToolDispatch{Kind: PreparedPostResult, Result: dispatched.result}
	}
	return &ScheduledToolDispatch{Kind: PreparedFinalResult, Result: dispatched.result}
}

// Finalize runs ordered post-execute, definition-owned content finalization,
// materialization, and the final notification for a PreparedPostResult result.
func (rt *ToolRuntime) Finalize(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	return rt.finalizeScheduledExecution(exec, result)
}

// Finish runs definition-owned content finalization, materialization, and the
// final notification without post-execute.
func (rt *ToolRuntime) Finish(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	return rt.finishScheduledExecution(exec, result)
}
