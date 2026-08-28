package agentloop

import (
	"context"
	"encoding/json"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// Schedules one assistant step's tool calls. Exclusive calls form barriers;
// parallel calls use a bounded rolling pool and are reclassified before start.
// Dispatch may overlap, while policy, results, and result context remain
// model-ordered. Abort or an internal scheduler failure stops replenishment
// and drains started calls.
//
// Port of packages/core/agent-loop/src/tool-calls.ts. Go adaptations: the
// AbortSignal is a context.Context; ordered start (tool/call append, prepare)
// and ordered commit run on the driver goroutine while only the body stage
// overlaps on per-call goroutines, so session appends need no locking; a
// scheduler failure is any panic escaped the staged methods, recovered into
// the same drain-then-fail shape.

// plannedCall is one tool call after argument parsing, ready to schedule.
type plannedCall struct {
	block llm.ContentBlock
	input *tools.ToolExecutionInput
}

// slot is one settled dispatch awaiting model-order finalization.
type slot struct {
	exec      *tools.ToolRunContext
	result    *tools.ToolExecutionResult
	needsPost bool
	settled   bool
}

// groupOutcome is one scheduler group outcome, including a drained cancellation.
type groupOutcome struct {
	consumed  int
	aborted   bool
	concluded bool
}

// dispatchSettlement is one goroutine's finished dispatch handed back to the
// driver goroutine.
type dispatchSettlement struct {
	index     int
	exec      *tools.ToolRunContext
	result    *tools.ToolExecutionResult
	needsPost bool
	failed    bool
}

// toolScheduler carries the collaborators of one step's scheduling.
type toolScheduler struct {
	tools       *tools.ToolRuntime
	session     *session.Session
	maxParallel int
}

// executeToolCalls schedules one assistant step's tool calls by their live
// concurrency mode. Ordinary completion and abort commit started-call results
// in order. Abort drains them, records synthetic results for unstarted calls,
// and returns with the signal still aborted after accepting started-call
// context through the caller-supplied acceptor (the driver stages it in its
// next-step inbox for the step boundary). An internal scheduler failure stops
// new dispatches, drains already-started dispatches, and fails without
// fabricating tool results.
func executeToolCalls(sched *toolScheduler, goCtx context.Context, turn, step int64, toolCalls []llm.ContentBlock, signal context.Context, acceptContext func(llm.Message)) (concluded bool, err error) {
	initiator, err := agent.RequireInitiator(goCtx)
	if err != nil {
		return false, err
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("tool-call scheduler failure: %v", rec)
			concluded = false
		}
	}()

	// Inputs are distinct because tools/execute wrappers may replace the body
	// signal (the registry fuses the caller back in).
	planned := make([]plannedCall, 0, len(toolCalls))
	for _, block := range toolCalls {
		planned = append(planned, plannedCall{
			block: block,
			input: &tools.ToolExecutionInput{
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: parseArguments(block.Arguments),
				Agent:     initiator.Scope,
				Signal:    signal,
			},
		})
	}

	next := 0
	for next < len(planned) {
		// Commit before classifying again so registry changes affect
		// unstarted calls.
		mode := sched.tools.ExecutionMode(planned[next].input)
		group := planned[next : next+1]
		if mode == tools.ModeParallel {
			group = planned[next:]
		}
		outcome, groupErr := runGroup(sched, turn, step, group, mode, signal, acceptContext)
		if groupErr != nil {
			return concluded, groupErr
		}
		next += outcome.consumed
		concluded = concluded || outcome.concluded
		if outcome.aborted {
			for _, call := range planned[next:] {
				appendSkippedToolCall(sched.session, turn, step, call.block)
			}
			return concluded, nil
		}
	}
	return concluded, nil
}

// parseArguments parses model arguments, preserving invalid JSON as text and
// mapping empty input to `{}`.
func parseArguments(raw string) any {
	if raw == "" {
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return raw
	}
	return parsed
}

// runGroup runs one exclusive barrier or parallel pool. Later calls are
// reclassified before start; an exclusive reclassification waits for the
// current pool to drain and remains for the caller's next barrier. Results and
// contexts commit in model order. Abort stops starts, drains and commits
// started calls, accepts their contexts into the owning batch, records results
// for skipped calls, and returns an aborted outcome. Scheduler failure drains
// dispatches without committing synthetic recovery results.
func runGroup(sched *toolScheduler, turn, step int64, group []plannedCall, mode string, signal context.Context, acceptContext func(llm.Message)) (outcome groupOutcome, groupErr error) {
	slots := make([]slot, len(group))
	// Started slots retain their tool/call seq so the result can cite it.
	callSeqs := make([]int64, len(group))
	for i := range callSeqs {
		callSeqs[i] = -1
	}
	nextToStart := 0
	committed := 0
	started := 0
	aborted := signal.Err() != nil
	concluded := false

	settlements := make(chan dispatchSettlement, len(group))
	inFlight := 0

	// committed advances only across contiguous model-order slots.
	commitReady := func() error {
		for committed < len(group) && slots[committed].settled {
			call := group[committed]
			var result *tools.ToolExecutionResult
			if slots[committed].needsPost {
				result = sched.tools.Finalize(slots[committed].exec, slots[committed].result)
			} else {
				result = sched.tools.Finish(slots[committed].exec, slots[committed].result)
			}
			appendToolResult(sched.session, turn, step, call.block, result, callSeqs[committed])
			for _, context := range result.AdditionalContexts {
				acceptContext(context)
			}
			concluded = concluded || result.ConcludesTurn
			committed++
		}
		return nil
	}

	startCall := func(index int) {
		call := group[index]
		callSeqs[index] = appendToolCall(sched.session, turn, step, call.block)
		started++
		prepared := sched.tools.Prepare(call.input)
		switch prepared.Kind {
		case tools.PreparedDispatch:
			exec := prepared.Exec
			inFlight++
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						settlements <- dispatchSettlement{index: index, failed: true}
						_ = rec
					}
				}()
				outcome := sched.tools.Dispatch(exec)
				settlements <- dispatchSettlement{
					index:     index,
					exec:      exec,
					result:    outcome.Result,
					needsPost: outcome.Kind == tools.PreparedPostResult,
				}
			}()
		case tools.PreparedPostResult:
			slots[index] = slot{exec: prepared.Exec, result: prepared.Result, needsPost: true, settled: true}
		case tools.PreparedFinalResult:
			slots[index] = slot{exec: prepared.Exec, result: prepared.Result, settled: true}
		}
	}

	fillPool := func() error {
		for !aborted && nextToStart < len(group) && inFlight < sched.maxParallel {
			// Re-read later modes after ordered commits so registry changes
			// can create a barrier.
			nextCall := group[nextToStart]
			if nextToStart > 0 && mode == tools.ModeParallel && sched.tools.ExecutionMode(nextCall.input) != tools.ModeParallel {
				break
			}
			startCall(nextToStart)
			nextToStart++
			if err := commitReady(); err != nil {
				return err
			}
			// Abort may arrive while pre-execute runs.
			aborted = aborted || signal.Err() != nil
		}
		return nil
	}

	// Ordered pre-execute is sequential; only dispatch/body overlaps. A
	// scheduler failure stops new dispatches and reaches the turn boundary
	// after every already-started dispatch settles.
	drainFailure := func(failure error) (groupOutcome, error) {
		for inFlight > 0 {
			settlement := <-settlements
			inFlight--
			slots[settlement.index].settled = true
		}
		return groupOutcome{}, failure
	}
	if err := fillPool(); err != nil {
		return drainFailure(err)
	}
	for inFlight > 0 {
		settlement := <-settlements
		inFlight--
		slots[settlement.index] = slot{
			exec:      settlement.exec,
			result:    settlement.result,
			needsPost: settlement.needsPost,
			settled:   true,
		}
		if settlement.failed {
			return drainFailure(fmt.Errorf("tool-call scheduler: dispatch failed"))
		}
		if err := commitReady(); err != nil {
			return drainFailure(err)
		}
		// Abort may arrive while a tool or ordered commit awaits.
		aborted = aborted || signal.Err() != nil
		if err := fillPool(); err != nil {
			return drainFailure(err)
		}
	}

	if aborted {
		// Started calls and accepted context settle first; every remaining
		// model call then receives an ordered synthetic result before the
		// turn aborts.
		for _, call := range group[started:] {
			appendSkippedToolCall(sched.session, turn, step, call.block)
		}
		return groupOutcome{consumed: len(group), aborted: true, concluded: concluded}, nil
	}
	if committed != started {
		return groupOutcome{}, fmt.Errorf("tool-call scheduler: uncommitted settled calls")
	}
	return groupOutcome{consumed: started, concluded: concluded}, nil
}

// appendSkippedToolCall appends the durable call/result pair for a model call
// skipped after cancellation.
func appendSkippedToolCall(sess *session.Session, turn, step int64, block llm.ContentBlock) {
	callSeq := appendToolCall(sess, turn, step, block)
	appendToolResult(sess, turn, step, block, &tools.ToolExecutionResult{
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "Error: tool call aborted before dispatch"}},
		IsError: true,
		Error: &tools.ToolFailure{
			Message: "tool call aborted before dispatch",
			Info:    &tools.ToolErrorInfo{Name: "AbortError", Code: tools.CodeToolAbortedBeforeDispatch},
		},
	}, callSeq)
}

// appendToolCall appends a started call and returns the event seq that its
// result must cite.
func appendToolCall(sess *session.Session, turn, step int64, block llm.ContentBlock) int64 {
	event, err := sess.Append(session.EventToolCall, session.ToolCallData{
		Turn:      turn,
		Step:      step,
		CallID:    llm.ToolCallID(block.ID),
		Name:      block.Name,
		Arguments: block.Arguments,
	}, nil)
	if err != nil {
		panic(fmt.Sprintf("tool/call append: %v", err))
	}
	return event.Seq
}

// appendToolResult appends a model-ordered result linked to its call event.
func appendToolResult(sess *session.Session, turn, step int64, block llm.ContentBlock, result *tools.ToolExecutionResult, callSeq int64) {
	message := llm.NewToolResultMessage(llm.ToolCallID(block.ID), result.Content, result.IsError)
	data := session.ToolResultData{Turn: turn, Step: step, Message: message}
	if result.Error != nil && result.Error.Info != nil {
		data.Error = &session.ToolResultError{Name: result.Error.Info.Name, Code: result.Error.Info.Code}
	}
	if result.Meta != nil {
		// The tool's private presentation payload (e.g. a result-time diff),
		// persisted so a UI bridge reproduces the card on replay.
		encoded, err := json.Marshal(result.Meta)
		if err != nil {
			panic(fmt.Sprintf("tool/result meta: %v", err))
		}
		data.Meta = encoded
	}
	intent := &session.SurfaceIntent{
		SurfaceOp:         session.SurfaceOp{Kind: session.SurfaceAppend},
		SourceEventSeqs:   []int64{callSeq},
		SourceSeqsPresent: true,
	}
	if _, err := sess.Append(session.EventToolResult, data, intent); err != nil {
		panic(fmt.Sprintf("tool/result append: %v", err))
	}
}
