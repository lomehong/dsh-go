// Tool execution pipeline: pre-policy, guards, around-dispatch, post-policy,
// definition-owned content finalization, and final notification. Port of the
// execution half of packages/core/tools/src/index.ts.
//
// Go adaptations: caller cancellation is a context.Context (the source's
// AbortSignal); a listener or body failure is a recovered panic or returned
// error materialized into the same error-result shape the source's try/catch
// produces; deep-freeze is a wire-round-trip clone (Go values are not
// freezable, and every snapshot detaches callers from registry state).
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"dshgo/llm"
)

// Scheduling modes for sibling overlap.
const (
	ModeParallel  = "parallel"
	ModeExclusive = "exclusive"
)

// Canonical cancellation codes, selected by whether the tool body started.
const (
	CodeToolAborted               = "ABORTED"
	CodeToolAbortedBeforeDispatch = "ABORTED_BEFORE_DISPATCH"
)

// Canonical codes for tool-not-found and output-contract failures.
const (
	CodeUnknownTool       = "UNKNOWN_TOOL"
	CodeInvalidToolOutput = "INVALID_TOOL_OUTPUT"
)

// ExecutionToken is the registry-assigned opaque correlation identity shared
// with nested calls only as their opaque Parent token.
type ExecutionToken struct {
	id uint64
}

// ToolExecutionInput is the caller-supplied description of one tool call.
// Execute adds the registry-owned token to form a pipeline execution;
// callers do not choose that token.
type ToolExecutionInput struct {
	// CallID is the provider-issued id of this call.
	CallID string
	// RootCallID is the root model-requested call owning this execution
	// tree; callers omit it for a root execution.
	RootCallID string
	// Name is the tool name as requested.
	Name string
	// Arguments are the losslessly JSON-serializable parsed arguments.
	Arguments any
	// Agent is the scope on whose behalf the call runs; nil for global.
	Agent ScopeKey
	// Parent is the enclosing transport execution's token, when one exists
	// (a PTC SDK sub-dispatch); nil for a model-direct call.
	Parent *ExecutionToken
	// Signal is the required caller-owned cancellation for this invocation.
	Signal context.Context
}

// ToolExecution is one pending tool call inside the registry pipeline:
// identity, frozen arguments, and the registry-assigned token.
type ToolExecution struct {
	Token      *ExecutionToken
	CallID     string
	RootCallID string
	Name       string
	// Arguments are the losslessly snapshotted model arguments.
	Arguments any
	// Agent is the scope routing key, or nil for an agent-less call.
	Agent ScopeKey
	// Parent marks a transport sub-dispatch rather than a model-direct call.
	Parent *ExecutionToken
}

// ToolErrorInfo is the structured error metadata alongside the model-facing
// text.
type ToolErrorInfo struct {
	Name string
	Code string
}

// ToolFailure is the canonical failure detail.
type ToolFailure struct {
	// Message is the human-readable failure message without the Native
	// `Error: ` envelope.
	Message string
	// Info is the internal error class/code used by policy and durable
	// diagnostics.
	Info *ToolErrorInfo
}

// ToolExecutionResult is the discriminated, execution-local outcome of one
// tool call; failures never carry a successful value.
type ToolExecutionResult struct {
	IsError            bool
	Value              any
	Error              *ToolFailure
	Content            []llm.ContentBlock
	Meta               any
	AdditionalContexts []llm.Message
	// ConcludesTurn exists only on successful results and only when the
	// body declared the turn terminal.
	ConcludesTurn bool

	// canonicalToken marks a registry-normalized result for its owning
	// dispatch; wrapper-authored results are re-normalized instead.
	canonicalToken *ExecutionToken
}

// PreToolDecision is the pre-dispatch outcome: allow runs the call, deny
// materializes an error, ask runs only through the approval seam. Input
// rewriting is excluded because arguments are already logged and presented.
type PreToolDecision struct {
	Kind   string // "allow" | "deny" | "ask"
	Reason string
	// HasReason distinguishes an ask without a reason from a deny without
	// one; a deny reason is mandatory in practice.
	HasReason bool
}

// Pre-decision kinds.
const (
	PreAllow = "allow"
	PreDeny  = "deny"
	PreAsk   = "ask"
)

// PostToolDecision is the post-dispatch outcome: accept (optionally
// replacing one projection), or block by turning corrective feedback into an
// error result. Either may attach context for the loop's next request.
type PostToolDecision struct {
	Kind string // "accept" | "block"
	// ReplaceContent replaces the model-facing content (accept only).
	ReplaceContent []llm.ContentBlock
	HasContent     bool
	// ReplaceValue replaces the canonical value (accept only, success only).
	ReplaceValue any
	HasValue     bool
	// Feedback is the corrective content a block turns into the error body.
	Feedback []llm.ContentBlock
	// AdditionalContexts attach context for the next request.
	AdditionalContexts []llm.Message
}

// Post-decision kinds.
const (
	PostAccept = "accept"
	PostBlock  = "block"
)

// ToolNotFoundError reports a model-requested tool that is not resolvable.
// ReachableFrom carries the route the model should take instead when the
// name IS visible and only the presentation denies calling it directly.
type ToolNotFoundError struct {
	ToolName      string
	ReachableFrom string
}

func (e *ToolNotFoundError) Error() string {
	if e.ReachableFrom == "" {
		return fmt.Sprintf("unknown tool %q", e.ToolName)
	}
	return fmt.Sprintf("unknown tool %q: %s", e.ToolName, e.ReachableFrom)
}

// Code returns UNKNOWN_TOOL.
func (e *ToolNotFoundError) Code() string { return CodeUnknownTool }

// ToolOutputError reports a tool body or post-policy value that violates its
// declared output.
type ToolOutputError struct {
	ToolName   string
	Violations []string
}

func (e *ToolOutputError) Error() string {
	return fmt.Sprintf("tool %q returned invalid output: %s", e.ToolName, joinViolations(e.Violations))
}

// Code returns INVALID_TOOL_OUTPUT.
func (e *ToolOutputError) Code() string { return CodeInvalidToolOutput }

// codeError matches this package's structured errors for errorInfo.
type codeError interface {
	Code() string
}

// errorInfo extracts {name, code} from a structured error, else nil.
func errorInfo(err error) *ToolErrorInfo {
	var coded codeError
	if errors.As(err, &coded) {
		return &ToolErrorInfo{Name: errorTypeName(err), Code: coded.Code()}
	}
	return nil
}

func errorTypeName(err error) string {
	return fmt.Sprintf("%T", err)
}

// errorMessage is the best-effort human-readable message from any failure.
func errorMessage(err error) string {
	if err == nil {
		return "<nil error>"
	}
	return err.Error()
}

// snapshotJSONValue detaches one value through a lossless JSON round trip;
// ok is false for values the canonical vocabulary cannot carry.
func snapshotJSONValue(candidate any) (any, bool) {
	if !sessionIsJSONValue(candidate) {
		return nil, false
	}
	return deepCloneViaJSON(candidate), true
}

func deepCloneViaJSON(candidate any) any {
	switch typed := candidate.(type) {
	case nil, bool, string, int, int64, float64:
		return typed
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = deepCloneViaJSON(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = deepCloneViaJSON(item)
		}
		return out
	default:
		return typed
	}
}

// sessionIsJSONValue mirrors the session-owned lossless-JSON test without a
// package dependency cycle risk: the accepted shapes are identical.
func sessionIsJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil, bool, string, int, int64, float64:
		return true
	case []any:
		for _, item := range typed {
			if !sessionIsJSONValue(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, item := range typed {
			if !sessionIsJSONValue(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// failureMessageFromContent derives one failure message from policy feedback
// without changing its rendered blocks.
func failureMessageFromContent(content []llm.ContentBlock) string {
	text := ""
	for i, block := range content {
		if i > 0 {
			text += "\n"
		}
		if block.Type == "text" {
			text += block.Text
		} else {
			text += fmt.Sprintf("[%s content]", block.Type)
		}
	}
	if len(text) > 0 {
		return text
	}
	return "tool result blocked by post-execute policy"
}

// ToolRunContext is the runtime context handed to a tool implementation
// after the registry has accepted the execution.
type ToolRunContext struct {
	ToolExecution
	// Signal is the cancellation visible to the body. The registry fuses
	// the original caller signal in before dispatch, so wrapper replacement
	// cannot detach caller cancellation.
	Signal context.Context

	// Registry-owned dispatch state, outside the around-wrapper view.
	callerCtx        context.Context
	bodyInvoked      bool
	concluding       bool
	deferredContexts []llm.Message
	finalizer        func(exec *ToolExecution, result *ToolExecutionResult) []llm.ContentBlock

	mu sync.Mutex
}

// DeferContext defers one context — typically a nested-dispatch context
// ferried by a composite tool — until this tool's final result reaches the
// agent loop, in call order.
func (exec *ToolRunContext) DeferContext(message llm.Message) {
	exec.mu.Lock()
	exec.deferredContexts = append(exec.deferredContexts, message)
	exec.mu.Unlock()
}

// ConcludeTurn marks a successful final result as terminal for the current
// agent turn; the loop stops after committing this result batch.
func (exec *ToolRunContext) ConcludeTurn() {
	exec.mu.Lock()
	exec.concluding = true
	exec.mu.Unlock()
}

// --- pipeline event carriers ---------------------------------------------------

type preExecuteCarrier struct {
	Exec     *ToolExecution
	Decision *PreToolDecision
}

type executeCarrier struct {
	Exec   *ToolRunContext
	Result *ToolExecutionResult
}

type postExecuteCarrier struct {
	Exec     *ToolExecution
	Result   *ToolExecutionResult
	Decision *PostToolDecision
}

type dispatchLogCarrier struct {
	Log     *PtcDispatchLog
	Content []llm.ContentBlock
}

// PtcDispatchLog is one settled run_code sub-dispatch about to be logged.
type PtcDispatchLog struct {
	Exec      *ToolExecution
	Agent     ScopeKey
	SubCallID string
	Name      string
	IsError   bool
	Content   []llm.ContentBlock
}

// --- pipeline event seams ------------------------------------------------------

// OnPreExecute registers a tools/pre-execute waterfall listener: allow, deny,
// or ask before dispatch. next() delegates to allow. Scope-filtered: nil
// scope receives every call; a tagged scope receives only its chain's calls.
func (rt *ToolRuntime) OnPreExecute(scope ScopeKey, h func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision) func() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.preExecEvent.On(scope, func(c *preExecuteCarrier, next func(*preExecuteCarrier) *preExecuteCarrier) *preExecuteCarrier {
		decision := h(c.Exec, func(*ToolExecution) *PreToolDecision {
			return next(c).Decision
		})
		c.Decision = decision
		return c
	})
}

// OnExecute registers a tools/execute around-dispatch listener for timeout,
// retry, or metrics. next() returns the normalized body result; wrappers may
// change only the execution's visible signal, and the registry re-fuses the
// caller signal before the body.
func (rt *ToolRuntime) OnExecute(scope ScopeKey, h func(exec *ToolRunContext, next func(*ToolRunContext) *ToolExecutionResult) *ToolExecutionResult) func() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.execEvent.On(scope, func(c *executeCarrier, next func(*executeCarrier) *executeCarrier) *executeCarrier {
		c.Result = h(c.Exec, func(inner *ToolRunContext) *ToolExecutionResult {
			return next(c).Result
		})
		return c
	})
}

// OnPostExecute registers a tools/post-execute waterfall listener: accept,
// replace one projection, attach context, or block. next() accepts unchanged.
func (rt *ToolRuntime) OnPostExecute(scope ScopeKey, h func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision) func() {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.postExecEvent.On(scope, func(c *postExecuteCarrier, next func(*postExecuteCarrier) *postExecuteCarrier) *postExecuteCarrier {
		decision := h(c.Exec, c.Result, func(*ToolExecutionResult) *PostToolDecision {
			return next(c).Decision
		})
		c.Decision = decision
		return c
	})
}

// OnResult observes the frozen, lossless-JSON final outcome. Listener
// failures are contained. Scope-filtered by exec.Agent.
func (rt *ToolRuntime) OnResult(scope ScopeKey, h func(exec *ToolExecution, result *ToolExecutionResult)) func() {
	rt.mu.Lock()
	id := nextAnonymousID()
	rt.resultEvents = append(rt.resultEvents, resultListener{scope: scope, id: id, fn: h})
	rt.mu.Unlock()
	return func() {
		rt.mu.Lock()
		for i, listener := range rt.resultEvents {
			if listener.id == id {
				rt.resultEvents = append(rt.resultEvents[:i], rt.resultEvents[i+1:]...)
				break
			}
		}
		rt.mu.Unlock()
	}
}

// ShapeDispatchLog runs the tools/ptc-dispatch-log waterfall over one settled
// sub-dispatch and returns the content the bridge should log. Contained: a
// throwing listener logs the original settled content.
func (rt *ToolRuntime) ShapeDispatchLog(dispatch *PtcDispatchLog) (content []llm.ContentBlock) {
	rt.mu.Lock()
	listeners := rt.ptcEvent.Snapshot(dispatch.Agent)
	rt.mu.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			rt.logger.Warn(fmt.Sprintf("tools: ptc-dispatch-log listener failed for %s: %v; logging the original settled content", dispatch.Name, rec))
			content = dispatch.Content
		}
	}()
	return runWaterfall(listeners, &dispatchLogCarrier{Log: dispatch}, func(c *dispatchLogCarrier) *dispatchLogCarrier {
		c.Content = dispatch.Content
		return c
	}).Content
}

// --- execution pipeline ---------------------------------------------------------

// Execute runs one call through pre-policy, guards, around-dispatch,
// post-policy, definition-owned content finalization, and final
// notification. Tool and listener failures resolve as materialized error
// results; an invisible tool reports UNKNOWN_TOOL. The returned outcome is
// the same lossless snapshot final observers receive.
func (rt *ToolRuntime) Execute(input *ToolExecutionInput) *ToolExecutionResult {
	prepared, staged := rt.prepareExecution(input)
	if staged.post {
		return rt.finalizeScheduledExecution(prepared, staged.result)
	}
	if staged.final {
		return rt.finishScheduledExecution(prepared, staged.result)
	}
	dispatched := rt.dispatchScheduledExecution(prepared)
	if dispatched.post {
		return rt.finalizeScheduledExecution(prepared, dispatched.result)
	}
	return rt.finishScheduledExecution(prepared, dispatched.result)
}

// stagedResult carries the prepare stage's routing decision.
type stagedResult struct {
	result *ToolExecutionResult
	post   bool // the result still receives post-execute
	final  bool // the result bypasses post-execute
}

// prepareExecution materializes input and runs the ordered pre-execute/guard
// gate. Listener failures are contained the way the source's try/catch does:
// they become the canonical error result.
func (rt *ToolRuntime) prepareExecution(input *ToolExecutionInput) (*ToolRunContext, stagedResult) {
	exec, finalResult := rt.createExecution(input)
	if finalResult != nil {
		return exec, stagedResult{result: finalResult, final: true}
	}
	if rt.callerCancelled(exec) {
		return exec, stagedResult{result: toolAbortedBeforeDispatchResult(nil), final: true}
	}
	return rt.prepareExecutionInner(exec)
}

func (rt *ToolRuntime) prepareExecutionInner(exec *ToolRunContext) (prepared *ToolRunContext, staged stagedResult) {
	defer func() {
		if rec := recover(); rec != nil {
			staged = stagedResult{result: toolErrorResult(recoverError(rec)), final: true}
		}
	}()
	exec.mu.Lock()
	carrier := exec.Agent
	exec.mu.Unlock()
	rt.mu.Lock()
	preListeners := rt.preExecEvent.Snapshot(carrier)
	rt.mu.Unlock()
	gate := runWaterfall(preListeners, &preExecuteCarrier{Exec: &exec.ToolExecution}, func(c *preExecuteCarrier) *preExecuteCarrier {
		c.Decision = &PreToolDecision{Kind: PreAllow}
		return c
	}).Decision

	askResolution := &toolAskResolution{decision: gate, approvalCancelled: false}
	if gate.Kind == PreAsk {
		askResolution = rt.serviceAsk(exec, gate)
	}
	if rt.callerCancelled(exec) && askResolution.approvalCancelled {
		return exec, stagedResult{result: toolAbortedBeforeDispatchResult(nil), post: true}
	}
	denialReason := ""
	denied := false
	if askResolution.decision.Kind != PreAllow {
		denied = true
		denialReason = askResolution.decision.Reason
	} else if reason, deny := rt.guardReason(&exec.ToolExecution); deny {
		denied = true
		denialReason = reason
	}
	if denied {
		result := rt.markCanonical(&exec.ToolExecution, &ToolExecutionResult{
			IsError: true,
			Content: []llm.ContentBlock{{Type: "text", Text: "Error: " + denialReason}},
			Error:   &ToolFailure{Message: denialReason},
		})
		return exec, stagedResult{result: result, post: true}
	}
	if rt.callerCancelled(exec) {
		return exec, stagedResult{result: toolAbortedBeforeDispatchResult(nil), post: true}
	}
	return exec, stagedResult{}
}

// toolAskResolution is the approval decision plus whether the approval
// channel reported cancellation.
type toolAskResolution struct {
	decision          *PreToolDecision
	approvalCancelled bool
}

// ApprovalRequest is one approval-seam request.
type ApprovalRequest struct {
	Agent     ScopeKey
	ToolName  string
	CallID    string
	Reason    string
	HasReason bool
	Signal    context.Context
}

// ApprovalOutcome is the approval seam's decision vocabulary.
type ApprovalOutcome string

// Approval outcomes.
const (
	ApprovalAllowedOnce ApprovalOutcome = "allowed-once"
	ApprovalRejected    ApprovalOutcome = "rejected"
	ApprovalCancelled   ApprovalOutcome = "cancelled"
	ApprovalUnavailable ApprovalOutcome = "unavailable"
)

// ApprovalService is the optional approval seam.
type ApprovalService interface {
	Request(request ApprovalRequest) ApprovalOutcome
}

// serviceAsk resolves an `ask` decision through the approval seam. A
// deployment with no seam keeps the historical degrade to deny; an agent-less
// execution degrades the same way.
func (rt *ToolRuntime) serviceAsk(exec *ToolRunContext, ask *PreToolDecision) *toolAskResolution {
	var approval ApprovalService
	if rt.Approval != nil {
		approval = rt.Approval()
	}
	if approval == nil {
		reason := ask.Reason
		if !ask.HasReason || reason == "" {
			reason = fmt.Sprintf("tool %q requires approval (not yet supported)", exec.Name)
		}
		return &toolAskResolution{decision: &PreToolDecision{Kind: PreDeny, Reason: reason, HasReason: true}}
	}
	if exec.Agent == nil {
		return &toolAskResolution{decision: &PreToolDecision{
			Kind: PreDeny, HasReason: true,
			Reason: fmt.Sprintf("tool %q requires approval, but the call has no agent to route it through", exec.Name),
		}}
	}
	request := ApprovalRequest{Agent: exec.Agent, ToolName: exec.Name, CallID: exec.CallID, Signal: exec.Signal}
	if ask.HasReason && ask.Reason != "" {
		request.Reason = ask.Reason
		request.HasReason = true
	}
	outcome := approval.Request(request)
	switch outcome {
	case ApprovalAllowedOnce:
		return &toolAskResolution{decision: &PreToolDecision{Kind: PreAllow}}
	case ApprovalRejected:
		return &toolAskResolution{decision: &PreToolDecision{
			Kind: PreDeny, HasReason: true,
			Reason: fmt.Sprintf("the user rejected tool %q", exec.Name),
		}}
	case ApprovalCancelled:
		return &toolAskResolution{
			decision: &PreToolDecision{
				Kind: PreDeny, HasReason: true,
				Reason: fmt.Sprintf("approval for tool %q was cancelled", exec.Name),
			},
			approvalCancelled: true,
		}
	case ApprovalUnavailable:
		return &toolAskResolution{decision: &PreToolDecision{
			Kind: PreDeny, HasReason: true,
			Reason: fmt.Sprintf("tool %q requires approval, but no approval channel is available", exec.Name),
		}}
	default:
		return &toolAskResolution{decision: &PreToolDecision{
			Kind: PreDeny, HasReason: true,
			Reason: fmt.Sprintf("tool %q requires approval, but the approval channel returned an unknown outcome", exec.Name),
		}}
	}
}

// guardReason returns the first monotonic denial from the global then the
// scope chain's guard layers, farthest first.
func (rt *ToolRuntime) guardReason(exec *ToolExecution) (string, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if reason, deny := rt.layers.Global.guardReason(exec); deny {
		return reason, true
	}
	if exec.Agent == nil {
		return "", false
	}
	for _, layer := range rt.layers.ChainLayers(exec.Agent) {
		if reason, deny := layer.guardReason(exec); deny {
			return reason, true
		}
	}
	return "", false
}

// createExecution mints the execution, snapshots and freezes the arguments,
// and resolves the mode collapse BEFORE the extensible policy pipeline: a
// collapsed call is deterministically denied, so pre-execute listeners,
// approval `ask`, and guards must never observe — or worse, approve — a call
// that can only fail. An unknown tool keeps the dispatch-stage UNKNOWN_TOOL
// path so policy listeners still see every name that reaches the registry.
func (rt *ToolRuntime) createExecution(input *ToolExecutionInput) (*ToolRunContext, *ToolExecutionResult) {
	rt.mu.Lock()
	rt.tokenCounter++
	token := &ExecutionToken{id: rt.tokenCounter}
	visible, hasVisible := rt.viewLocked(input.Agent).visible[input.Name]
	collapsed := hasVisible && rt.collapsesLocked(input.Name, input.Agent, input.Parent != nil)
	rt.mu.Unlock()

	rootCallID := input.RootCallID
	if rootCallID == "" {
		rootCallID = input.CallID
	}
	exec := &ToolRunContext{
		ToolExecution: ToolExecution{
			Token:      token,
			CallID:     input.CallID,
			RootCallID: rootCallID,
			Name:       input.Name,
			Agent:      input.Agent,
			Parent:     input.Parent,
		},
		callerCtx: input.Signal,
		Signal:    input.Signal,
	}
	// Capture the finalizer BEFORE argument materialization (the contract
	// snapshots the callback when the call starts). The collapse only
	// decides whether the CAPTURED callback is retained: a pre-dispatch
	// abort keeps it, while the denial and the invalid-args failure of a
	// NON-ABORTED collapsed call drop it (the call could never execute).
	var capturedFinalizer func(*ToolExecution, *ToolExecutionResult) []llm.ContentBlock
	if hasVisible && visible.FinalizeContent != nil {
		capturedFinalizer = visible.FinalizeContent
	}
	finalizerFor := func() func(*ToolExecution, *ToolExecutionResult) []llm.ContentBlock {
		if collapsed && input.Signal.Err() == nil {
			return nil
		}
		return capturedFinalizer
	}
	if input.Signal == nil {
		panic("tools: execution input requires a caller signal")
	}
	detached, ok := snapshotJSONValue(input.Arguments)
	if !ok {
		exec.finalizer = finalizerFor()
		return exec, toolErrorResult(errors.New("tool execution arguments must be losslessly JSON-serializable"))
	}
	exec.Arguments = deepCloneViaJSON(detached)
	exec.finalizer = finalizerFor()
	if collapsed {
		if input.Signal.Err() != nil {
			return exec, toolAbortedBeforeDispatchResult(nil)
		}
		reason := fmt.Sprintf("only `%s` is callable directly — call `%s` from inside a `%s` program instead", ReservedRunCodeName, input.Name, ReservedRunCodeName)
		return exec, toolErrorResult(&ToolNotFoundError{ToolName: input.Name, ReachableFrom: reason})
	}
	return exec, nil
}

// callerCancelled reports whether the original caller signal is currently
// aborted.
func (rt *ToolRuntime) callerCancelled(exec *ToolRunContext) bool {
	return exec.callerCtx.Err() != nil
}

// cancellationResult is the canonical cancellation outcome selected by
// whether the tool body started.
func (rt *ToolRuntime) cancellationResult(exec *ToolRunContext) *ToolExecutionResult {
	exec.mu.Lock()
	invoked := exec.bodyInvoked
	exec.mu.Unlock()
	if invoked {
		return toolAbortedResult(nil)
	}
	return toolAbortedBeforeDispatchResult(nil)
}

// dispatchedResult routes the dispatch stage's outcome.
type dispatchedResult struct {
	result *ToolExecutionResult
	post   bool
	final  bool
}

// dispatchScheduledExecution runs around-dispatch and the tool body. Tool
// and unknown-tool failures still receive post-execute; pipeline failures
// are already final. A caller-cancelled successful body is converted to the
// canonical aborted result on the way INTO post-execute, matching the
// source's post-result routing.
func (rt *ToolRuntime) dispatchScheduledExecution(exec *ToolRunContext) (dispatched dispatchedResult) {
	defer func() {
		if rec := recover(); rec != nil {
			dispatched = dispatchedResult{result: toolErrorResult(recoverError(rec)), final: true}
		}
	}()
	carrier := exec.Agent
	rt.mu.Lock()
	listeners := rt.execEvent.Snapshot(carrier)
	rt.mu.Unlock()
	result := runWaterfall(listeners, &executeCarrier{Exec: exec}, func(c *executeCarrier) *executeCarrier {
		c.Result = rt.dispatchToolBody(c.Exec)
		return c
	}).Result
	if result == nil {
		return dispatchedResult{result: toolErrorResult(errors.New("tools/execute waterfall produced no result")), final: true}
	}
	normalized := rt.normalizeDispatchResult(exec, result)
	normalized = rt.attachDeferredContexts(exec, normalized)
	if rt.callerCancelled(exec) && !normalized.IsError {
		normalized = rt.cancellationResult(exec)
	}
	return dispatchedResult{result: normalized, post: true}
}

// attachDeferredContexts ferries body-deferred context onto the result in
// call order, ahead of any policy-supplied contexts.
func (rt *ToolRuntime) attachDeferredContexts(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	exec.mu.Lock()
	deferred := append([]llm.Message{}, exec.deferredContexts...)
	exec.mu.Unlock()
	if len(deferred) == 0 {
		return result
	}
	merged := &ToolExecutionResult{}
	*merged = *result
	merged.AdditionalContexts = append(append([]llm.Message{}, deferred...), result.AdditionalContexts...)
	return rt.markCanonical(&exec.ToolExecution, merged)
}

// dispatchToolBody dispatches the registered body with the original caller
// signal fused back into any around-wrapper replacement.
func (rt *ToolRuntime) dispatchToolBody(exec *ToolRunContext) *ToolExecutionResult {
	tool, ok := rt.ResolveExecution(exec.Name, exec.Agent, exec.Parent != nil)
	if !ok {
		return toolErrorResult(&ToolNotFoundError{ToolName: exec.Name})
	}
	// The registry fuses the caller and wrapper-visible signals: the body
	// observes caller cancellation regardless of any wrapper's replacement
	// view, and a wrapper abort supersedes a successful outcome.
	wrapperSignal := exec.Signal
	fused, dispose := fuseSignals(exec.callerCtx, wrapperSignal)
	defer dispose()
	if fused.Err() != nil {
		return toolAbortedBeforeDispatchResult(nil)
	}
	exec.mu.Lock()
	exec.bodyInvoked = true
	exec.Signal = fused
	exec.mu.Unlock()

	var result *ToolExecutionResult
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				result = toolErrorResult(recoverError(rec))
			}
			exec.mu.Lock()
			exec.Signal = wrapperSignal
			exec.mu.Unlock()
		}()
		value, err := tool.Execute(exec.Arguments, exec)
		if err != nil {
			result = toolErrorResult(err)
			return
		}
		result = rt.createSuccessResult(exec, tool, value)
	}()
	if fused.Err() != nil && !result.IsError {
		return toolAbortedResult(result)
	}
	return result
}

// fuseSignals relays caller and wrapper cancellation into one context without
// nesting either. The dispose stops both relays. A signal that is already
// done cancels the fused view synchronously, matching the source's
// pre-checked listener attach (AfterFunc alone relays asynchronously).
func fuseSignals(caller, wrapper context.Context) (context.Context, func()) {
	if caller == wrapper {
		return caller, func() {}
	}
	fused, cancel := context.WithCancel(context.Background())
	stopCaller := context.AfterFunc(caller, cancel)
	stopWrapper := context.AfterFunc(wrapper, cancel)
	if caller.Err() != nil || wrapper.Err() != nil {
		cancel()
	}
	return fused, func() {
		stopCaller()
		stopWrapper()
		cancel()
	}
}

// finalizeScheduledExecution runs ordered post-execute, then
// definition-owned content finalization, materialization, and notification.
func (rt *ToolRuntime) finalizeScheduledExecution(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	var postResult *ToolExecutionResult
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				postResult = toolErrorResult(recoverError(rec))
			}
		}()
		postResult = rt.postExecute(exec, result)
	}()
	if rt.callerCancelled(exec) && !postResult.IsError {
		postResult = rt.cancellationResult(exec)
	}
	return rt.finishScheduledExecution(exec, postResult)
}

// finishScheduledExecution materializes the candidate, applies
// definition-owned content finalization, then materializes and notifies the
// authoritative result.
func (rt *ToolRuntime) finishScheduledExecution(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	materialized := result
	if detached, err := rt.materializeFinalResult(result); err == nil {
		materialized = detached
	} else {
		materialized, _ = rt.materializeFinalResult(toolErrorResult(err))
	}
	final := materialized
	if withFinal, err := rt.materializeFinalResult(rt.applyFinalContent(exec, materialized)); err == nil {
		final = withFinal
	} else {
		final, _ = rt.materializeFinalResult(toolErrorResult(err))
	}
	rt.notifyResult(&exec.ToolExecution, final)
	return final
}

// applyFinalContent applies the snapshotted tool-owned content transform
// without exposing other result fields.
func (rt *ToolRuntime) applyFinalContent(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	exec.mu.Lock()
	finalizer := exec.finalizer
	exec.mu.Unlock()
	if finalizer == nil {
		return result
	}
	content := finalizer(&exec.ToolExecution, result)
	if content == nil {
		return result
	}
	merged := &ToolExecutionResult{}
	*merged = *result
	merged.Content = content
	return merged
}

// notifyResult fires the contained result emit after the execution's final
// materialization.
func (rt *ToolRuntime) notifyResult(exec *ToolExecution, result *ToolExecutionResult) {
	rt.mu.Lock()
	listeners := make([]resultListener, len(rt.resultEvents))
	copy(listeners, rt.resultEvents)
	rt.mu.Unlock()
	for _, listener := range listeners {
		if !scopeAdmits(listener.scope, exec.Agent) {
			continue
		}
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					rt.logger.Warn(fmt.Sprintf("tool %q (%s): tools/result observer failed: %v", exec.Name, exec.CallID, rec))
				}
			}()
			listener.fn(exec, result)
		}()
	}
}

// postExecute runs the tools/post-execute waterfall and applies its
// decision: accept keeps the call successful (replacing content or value),
// block turns it into an error result. Context deferred by the body survives
// an accepted result but is discarded on a block.
func (rt *ToolRuntime) postExecute(exec *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	carrier := &postExecuteCarrier{Exec: &exec.ToolExecution, Result: result}
	rt.mu.Lock()
	postListeners := rt.postExecEvent.Snapshot(exec.Agent)
	rt.mu.Unlock()
	decision := runWaterfall(postListeners, carrier, func(c *postExecuteCarrier) *postExecuteCarrier {
		c.Decision = &PostToolDecision{Kind: PostAccept}
		return c
	}).Decision

	decisionContexts := decision.AdditionalContexts
	if decision.Kind == PostBlock {
		message := failureMessageFromContent(decision.Feedback)
		blocked := &ToolExecutionResult{
			IsError: true,
			Content: decision.Feedback,
			Error:   &ToolFailure{Message: message},
		}
		if len(decisionContexts) > 0 {
			blocked.AdditionalContexts = append([]llm.Message{}, decisionContexts...)
		}
		return rt.markCanonical(&exec.ToolExecution, blocked)
	}
	if decision.HasContent && decision.HasValue {
		panic("tools/post-execute accept decision cannot replace both value and content")
	}
	additionalContexts := append(append([]llm.Message{}, result.AdditionalContexts...), decisionContexts...)
	if decision.HasValue {
		if result.IsError {
			panic("tools/post-execute cannot replace the value of a failed result")
		}
		tool, ok := rt.ResolveExecution(exec.Name, exec.Agent, exec.Parent != nil)
		if !ok {
			panic(&ToolNotFoundError{ToolName: exec.Name})
		}
		replaced := rt.createSuccessResult(exec, tool, decision.ReplaceValue)
		merged := &ToolExecutionResult{}
		*merged = *replaced
		if len(additionalContexts) > 0 {
			merged.AdditionalContexts = additionalContexts
		}
		return rt.markCanonical(&exec.ToolExecution, merged)
	}
	merged := &ToolExecutionResult{}
	*merged = *result
	if decision.HasContent {
		merged.Content = decision.ReplaceContent
	}
	if len(additionalContexts) > 0 {
		merged.AdditionalContexts = additionalContexts
	}
	return rt.markCanonical(&exec.ToolExecution, merged)
}

// markCanonical marks one registry-normalized result as canonical only for
// its owning dispatch.
func (rt *ToolRuntime) markCanonical(exec *ToolExecution, result *ToolExecutionResult) *ToolExecutionResult {
	result.canonicalToken = exec.Token
	return result
}

// normalizeDispatchResult normalizes an around-dispatch wrapper's authored
// result through the owning output contract: registry-produced results pass
// through; anything else is re-validated, re-rendered, and re-canonicalized.
func (rt *ToolRuntime) normalizeDispatchResult(runCtx *ToolRunContext, result *ToolExecutionResult) *ToolExecutionResult {
	exec := &runCtx.ToolExecution
	if result.canonicalToken == exec.Token {
		return result
	}
	if result.IsError {
		normalized := &ToolExecutionResult{
			IsError: result.IsError,
			Error:   result.Error,
			Content: result.Content,
			Meta:    result.Meta,
		}
		if result.AdditionalContexts != nil {
			normalized.AdditionalContexts = result.AdditionalContexts
		}
		return rt.markCanonical(exec, normalized)
	}
	tool, ok := rt.ResolveExecution(exec.Name, exec.Agent, exec.Parent != nil)
	if !ok {
		return toolErrorResult(&ToolNotFoundError{ToolName: exec.Name})
	}
	normalized := rt.createSuccessResult(runCtx, tool, result.Value)
	if result.AdditionalContexts != nil {
		merged := &ToolExecutionResult{}
		*merged = *normalized
		merged.AdditionalContexts = result.AdditionalContexts
		normalized = rt.markCanonical(exec, merged)
	}
	return normalized
}

// ConcludedTurn reads the body-declared terminal marker.
func (exec *ToolRunContext) ConcludedTurn() bool {
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return exec.concluding
}

// createSuccessResult snapshots, validates, renders, and optionally projects
// one successful body value.
func (rt *ToolRuntime) createSuccessResult(exec *ToolRunContext, tool *ToolDefinition, candidate any) *ToolExecutionResult {
	detached, ok := snapshotJSONValue(candidate)
	if !ok {
		panic(&ToolOutputError{ToolName: tool.Name, Violations: []string{"value is not lossless JSON"}})
	}
	violations := ValidateJsonSchemaValue(tool.OutputSchema, detached, "value")
	if len(violations) > 0 {
		panic(&ToolOutputError{ToolName: tool.Name, Violations: violations})
	}
	value := deepCloneViaJSON(detached)
	rendered, err := rt.snapshotProjection(tool.Name, "render", func() []llm.ContentBlock {
		return tool.render(asArgsMap(exec.Arguments), value)
	})
	if err != nil {
		panic(err)
	}
	var meta any
	if exec.Parent == nil && tool.PresentationMeta != nil {
		projected, projErr := rt.snapshotProjectionAny(tool.Name, "presentationMeta", func() any {
			return tool.PresentationMeta(asArgsMap(exec.Arguments), value)
		})
		if projErr != nil {
			panic(projErr)
		}
		meta = projected
	}
	success := &ToolExecutionResult{
		Value:   value,
		Content: rendered,
		Meta:    meta,
	}
	success.ConcludesTurn = exec.ConcludedTurn()
	return rt.markCanonical(&exec.ToolExecution, success)
}

// snapshotProjection snapshots one projector result before later
// durable-result materialization.
func (rt *ToolRuntime) snapshotProjection(toolName string, projector string, project func() []llm.ContentBlock) (content []llm.ContentBlock, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &ToolOutputError{ToolName: toolName, Violations: []string{fmt.Sprintf("output.%s failed: %v", projector, rec)}}
		}
	}()
	rendered := project()
	detached, ok := cloneContentBlocks(rendered)
	if !ok {
		return nil, &ToolOutputError{ToolName: toolName, Violations: []string{fmt.Sprintf("output.%s returned non-lossless JSON", projector)}}
	}
	return detached, nil
}

// cloneContentBlocks detaches model-facing content through a wire round
// trip; ok is false for content the wire vocabulary cannot carry.
func cloneContentBlocks(content []llm.ContentBlock) ([]llm.ContentBlock, bool) {
	raw, err := json.Marshal(content)
	if err != nil {
		return nil, false
	}
	var out []llm.ContentBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// snapshotProjectionAny is snapshotProjection for scalar projections.
func (rt *ToolRuntime) snapshotProjectionAny(toolName string, projector string, project func() any) (value any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = &ToolOutputError{ToolName: toolName, Violations: []string{fmt.Sprintf("output.%s failed: %v", projector, rec)}}
		}
	}()
	projected := project()
	detached, ok := snapshotJSONValue(projected)
	if !ok {
		return nil, &ToolOutputError{ToolName: toolName, Violations: []string{fmt.Sprintf("output.%s returned non-lossless JSON", projector)}}
	}
	return detached, nil
}

// materializeFinalResult materializes the authoritative commit outcome once,
// immediately before tools/result. A lossy projection fails the
// materialization; the finish stage converts that into an error result.
func (rt *ToolRuntime) materializeFinalResult(result *ToolExecutionResult) (*ToolExecutionResult, error) {
	detachedContent, ok := cloneContentBlocks(result.Content)
	if !ok {
		return nil, errors.New("tool result must be losslessly JSON-serializable")
	}
	out := &ToolExecutionResult{IsError: result.IsError, Content: detachedContent}
	if result.Meta != nil {
		detachedMeta, ok := snapshotJSONValue(result.Meta)
		if !ok {
			return nil, errors.New("tool result must be losslessly JSON-serializable")
		}
		out.Meta = detachedMeta
	}
	if result.AdditionalContexts != nil {
		out.AdditionalContexts = append([]llm.Message{}, result.AdditionalContexts...)
	}
	if result.IsError {
		out.Error = result.Error
		return out, nil
	}
	out.Value = result.Value
	out.ConcludesTurn = result.ConcludesTurn
	return out, nil
}

// recoverError keeps a recovered panic's structured error identity: the
// canonical result vocabulary reads {name, code} off error values.
func recoverError(rec any) error {
	if err, ok := rec.(error); ok {
		return err
	}
	return fmt.Errorf("%v", rec)
}

// toolErrorResult converts any failure into the canonical error result.
func toolErrorResult(err error) *ToolExecutionResult {
	info := errorInfo(err)
	message := errorMessage(err)
	failure := &ToolFailure{Message: message}
	if info != nil {
		failure.Info = info
	}
	return &ToolExecutionResult{
		IsError: true,
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: " + message}},
		Error:   failure,
	}
}

// toolAbortedResult is the canonical outcome when cancellation supersedes
// success after body invocation.
func toolAbortedResult(prior *ToolExecutionResult) *ToolExecutionResult {
	result := &ToolExecutionResult{
		IsError: true,
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: tool call aborted"}},
		Error: &ToolFailure{
			Message: "tool call aborted",
			Info:    &ToolErrorInfo{Name: "AbortError", Code: CodeToolAborted},
		},
	}
	if prior != nil && prior.AdditionalContexts != nil {
		result.AdditionalContexts = prior.AdditionalContexts
	}
	return result
}

// toolAbortedBeforeDispatchResult is the canonical outcome when cancellation
// prevents tool body invocation.
func toolAbortedBeforeDispatchResult(prior *ToolExecutionResult) *ToolExecutionResult {
	result := &ToolExecutionResult{
		IsError: true,
		Content: []llm.ContentBlock{{Type: "text", Text: "Error: tool call aborted before dispatch"}},
		Error: &ToolFailure{
			Message: "tool call aborted before dispatch",
			Info:    &ToolErrorInfo{Name: "AbortError", Code: CodeToolAbortedBeforeDispatch},
		},
	}
	if prior != nil && prior.AdditionalContexts != nil {
		result.AdditionalContexts = prior.AdditionalContexts
	}
	return result
}

// asArgsMap views validated arguments as the object shape tools receive.
func asArgsMap(args any) map[string]any {
	if typed, ok := args.(map[string]any); ok {
		return typed
	}
	return map[string]any{}
}
