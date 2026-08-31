package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/subagent"
)

// The workflow engine: the Go counterpart of the worker-thread script
// runtime behind the Service Definition. The source realm executes
// model-written plain-JS in a worker; the Go realm executes a Script — a
// compiled Go orchestration function over the same surface (agent,
// parallel, pipeline, phase, log, args) with the same host-enforced
// discipline: fatal errors kill the run loudly, child-run failures null
// their item, lifecycle events observe without exposing control, the result
// delivers exactly once, and caps bound the fan-out.

// disposeGrace bounds how long Dispose waits for the script and its children
// to quiesce after cancellation.
const disposeGrace = 30 * time.Second

// ChildDispatcher is the subagent seam the engine fans out to.
type ChildDispatcher interface {
	// Start dispatches one one-shot child run for the named provider on
	// behalf of the request's parent. The provider resolves per call: the
	// agent() call's override wins, then the run's subagentProvider.
	Start(provider string, request subagent.SubagentStartRequest) (subagent.SubagentRun, error)
}

// AgentCall is the closed option set of one agent() call. Unknown options
// cannot exist (a closed struct), mirroring the worker's loud rejection of
// anything outside {label, phase, schema, provider, model}.
type AgentCall struct {
	// Label is the display label; the prompt snippet backs a default.
	Label string
	// Phase attributes the call to a phase other than the current one.
	Phase string
	// Schema is the optional object-rooted JSON Schema the child's result
	// must match (returned as its structured value).
	Schema map[string]any
	// Provider is the optional child-provider override for this call.
	Provider string
	// Model is the optional model override for this call.
	Model string
}

// PipelineStage is one pipeline stage: it receives the previous stage's
// value for the item (the item itself for stage 0) and the item and index.
type PipelineStage func(prev any, item any, index int) (any, error)

// Script is one workflow program: it receives the realm surface and returns
// the run's JSON value. A returned error settles the run as error; a fatal
// WorkflowError from a combinator propagates through it.
type Script func(api *ScriptAPI) (any, error)

// ScriptAPI is the script realm surface: the script-visible operations the
// engine mediates. Methods are safe for concurrent use.
type ScriptAPI struct {
	run *liveRun
}

// Args returns the verbatim start-request input.
func (api *ScriptAPI) Args() any { return api.run.args }

// Phase records progress grouping for observers; no execution semantics.
func (api *ScriptAPI) Phase(title string) {
	api.run.setPhase(title)
	api.run.engine.sink.Emit(EventWorkflowPhase, PhasePayload{Info: api.run.info(), Title: title})
}

// Log emits one narration line.
func (api *ScriptAPI) Log(message string) {
	api.run.engine.sink.Emit(EventWorkflowLog, LogPayload{Info: api.run.info(), Message: message})
}

// Agent starts one child run and resolves its value. A child-run failure or
// run cancellation resolves as (nil, nil) — the item-null discipline; fatal
// engine errors (a tripped cap, a dispatch failure, unrepresentable result
// data) return a fatal WorkflowError the combinators propagate.
func (api *ScriptAPI) Agent(prompt string, call AgentCall) (any, error) {
	return api.run.agent(prompt, call)
}

// Parallel runs thunks concurrently and resolves their values in input
// order. A thunk error maps that item to nil — unless fatal, which
// propagates after every thunk settles.
func (api *ScriptAPI) Parallel(thunks []func() (any, error)) ([]any, error) {
	if thunks == nil {
		return nil, nil
	}
	values := make([]any, len(thunks))
	errs := make([]error, len(thunks))
	var wg sync.WaitGroup
	for i, thunk := range thunks {
		wg.Add(1)
		go func(i int, thunk func() (any, error)) {
			defer wg.Done()
			values[i], errs[i] = thunk()
		}(i, thunk)
	}
	wg.Wait()
	for _, err := range errs {
		if IsFatalWorkflowError(err) {
			return nil, err
		}
	}
	for i, err := range errs {
		if err != nil {
			values[i] = nil
		}
	}
	return values, nil
}

// Pipeline runs each item through the stages independently with no barrier
// between stages: stage 1 starts on an item while stage 0 still works on
// another. A stage error drops that item to nil and skips its remaining
// stages — unless fatal, which propagates after every item settles.
func (api *ScriptAPI) Pipeline(items []any, stages ...PipelineStage) ([]any, error) {
	values := make([]any, len(items))
	errs := make([]error, len(items))
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		go func(i int, item any) {
			defer wg.Done()
			prev := item
			for _, stage := range stages {
				next, err := stage(prev, item, i)
				if err != nil {
					errs[i] = err
					return
				}
				prev = next
			}
			values[i] = prev
		}(i, item)
	}
	wg.Wait()
	for _, err := range errs {
		if IsFatalWorkflowError(err) {
			return nil, err
		}
	}
	return values, nil
}

// EngineOptions configures one engine instance.
type EngineOptions struct {
	// Sink receives the lifecycle events.
	Sink *EventSink
	// Children dispatches one-shot child runs.
	Children ChildDispatcher
	// NewID mints run ids; nil uses a random hex id.
	NewID func() string
	// MaxTotalAgents is the default per-run total-child ceiling; a request
	// override wins. Zero means unlimited.
	MaxTotalAgents int64
}

// engine is the Engine implementation.
type engine struct {
	sink     *EventSink
	children ChildDispatcher
	newID    func() string
	cap      int64
}

// NewEngine builds one engine; options.Sink and options.Children are
// required.
func NewEngine(options EngineOptions) (Engine, error) {
	if options.Sink == nil || options.Children == nil {
		return nil, errors.New("workflow engine requires an event sink and a child dispatcher")
	}
	mint := options.NewID
	if mint == nil {
		mint = mintRunID
	}
	return &engine{sink: options.Sink, children: options.Children, newID: mint, cap: options.MaxTotalAgents}, nil
}

// mintRunID mints one random run id.
func mintRunID() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand cannot fail on supported platforms.
		panic(fmt.Sprintf("workflow: mint run id: %v", err))
	}
	return hex.EncodeToString(raw)
}

// Start validates the request and publishes a live run. Invalid requests
// fail before publication.
func (e *engine) Start(request StartRequest) (Run, error) {
	meta, err := ValidateMeta(request.Meta)
	if err != nil {
		return nil, err
	}
	if request.Parent == nil {
		return nil, NewWorkflowError("workflow start requires a parent agent", CodeInvalidArgument, nil, nil)
	}
	if request.Program == nil {
		return nil, NewWorkflowError("workflow start requires a script", CodeInvalidArgument, nil, nil)
	}
	cap := e.cap
	if request.MaxTotalAgents != nil {
		if *request.MaxTotalAgents <= 0 {
			return nil, NewWorkflowError("maxTotalAgents must be a positive integer", CodeInvalidArgument, nil, nil)
		}
		cap = *request.MaxTotalAgents
	}
	run := &liveRun{
		engine:     e,
		id:         e.newID(),
		meta:       meta,
		args:       request.Args,
		parent:     request.Parent,
		provider:   request.SubagentProvider,
		cap:        cap,
		resultChan: make(chan WorkflowResult, 1),
		workers:    &sync.WaitGroup{},
	}
	signal := request.Signal
	if signal == nil {
		signal = context.Background()
	}
	run.signal, run.cancel = context.WithCancel(signal)
	run.scriptDone = make(chan struct{})
	// The start event precedes the body, exactly as the source realm does.
	e.sink.Emit(EventWorkflowStart, StartPayload{Info: run.info()})
	go func() {
		result := run.execute(request.Program)
		run.settle(result)
	}()
	return run, nil
}

// liveRun is one published run.
type liveRun struct {
	engine     *engine
	id         WorkflowRunID
	meta       WorkflowMeta
	args       any
	parent     *agent.Agent
	provider   string
	cap        int64
	signal     context.Context
	cancel     context.CancelFunc
	resultChan chan WorkflowResult
	scriptDone chan struct{}
	workers    *sync.WaitGroup
	// settleOnce guards the exactly-once end-event/result publication.
	settleOnce sync.Once
	// phaseMu guards currentPhase.
	phaseMu sync.Mutex
	// currentPhase is the latest phase() title.
	currentPhase string
	// seq is the 1-based agent() call counter.
	seq atomic.Int64
	// agentsStarted counts the accepted agent() calls.
	agentsStarted atomic.Int64
}

// info is the run's identity snapshot.
func (r *liveRun) info() WorkflowRunInfo { return WorkflowRunInfo{ID: r.id, Meta: r.meta} }

// ID implements Run.
func (r *liveRun) ID() WorkflowRunID { return r.id }

// Meta implements Run.
func (r *liveRun) Meta() WorkflowMeta { return r.meta }

// Result implements Run: one delivery when the run settles.
func (r *liveRun) Result() <-chan WorkflowResult { return r.resultChan }

// Cancel implements Run.
func (r *liveRun) Cancel(reason string) { r.cancel() }

// Dispose implements Run: cancel if needed and await bounded settlement.
// The holder may have already drained the one-delivery result channel, so
// settlement (not result consumption) is the await point; settle() closes
// scriptDone before publishing the result.
func (r *liveRun) Dispose() error {
	r.cancel()
	select {
	case <-r.scriptDone:
	case <-time.After(disposeGrace):
		return errors.New("workflow run did not settle within the dispose grace")
	}
	return nil
}

// setPhase records the current phase title.
func (r *liveRun) setPhase(title string) {
	r.phaseMu.Lock()
	r.currentPhase = title
	r.phaseMu.Unlock()
}

// phaseFor labels a call without an explicit phase option.
func (r *liveRun) phaseFor(explicit string) string {
	if explicit != "" {
		return explicit
	}
	r.phaseMu.Lock()
	defer r.phaseMu.Unlock()
	return r.currentPhase
}

// execute runs the script to its outcome, mapping every path to a
// WorkflowResult.
func (r *liveRun) execute(script Script) (result WorkflowResult) {
	defer func() {
		// A panicking script is a failed run, not a dead engine.
		if rec := recover(); rec != nil {
			result = WorkflowResult{
				StopReason:    StopReasonError,
				Error:         fmt.Sprintf("workflow script panicked: %v", rec),
				AgentsStarted: r.agentsStarted.Load(),
			}
			r.workers.Wait()
		}
	}()
	value, err := script(&ScriptAPI{run: r})
	r.workers.Wait()
	if r.signal.Err() != nil {
		return WorkflowResult{StopReason: StopReasonCancelled, Error: "workflow run was cancelled", AgentsStarted: r.agentsStarted.Load()}
	}
	if err != nil {
		return WorkflowResult{StopReason: StopReasonError, Error: err.Error(), AgentsStarted: r.agentsStarted.Load()}
	}
	raw, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return WorkflowResult{
			StopReason:    StopReasonError,
			Error:         fmt.Sprintf("workflow result is not host-realm JSON: %s", marshalErr),
			AgentsStarted: r.agentsStarted.Load(),
		}
	}
	var materialized any
	if raw != nil {
		if unmarshalErr := json.Unmarshal(raw, &materialized); unmarshalErr != nil {
			return WorkflowResult{
				StopReason:    StopReasonError,
				Error:         fmt.Sprintf("workflow result is not host-realm JSON: %s", unmarshalErr),
				AgentsStarted: r.agentsStarted.Load(),
			}
		}
	}
	return WorkflowResult{Value: materialized, StopReason: StopReasonCompleted, AgentsStarted: r.agentsStarted.Load()}
}

// settle publishes the outcome exactly once: the one-delivery result and the
// paired workflow/end event at the same commit point.
func (r *liveRun) settle(result WorkflowResult) {
	r.settleOnce.Do(func() {
		close(r.scriptDone)
		r.engine.sink.Emit(EventWorkflowEnd, EndPayload{
			Info: r.info(),
			Result: WorkflowResultInfo{
				StopReason:    result.StopReason,
				Error:         result.Error,
				AgentsStarted: result.AgentsStarted,
			},
		})
		r.resultChan <- result
	})
}

// providerFor resolves one agent() call's provider: the call override
// wins, then the run's subagentProvider (the official option merge).
func (r *liveRun) providerFor(call AgentCall) string {
	if call.Provider != "" {
		return call.Provider
	}
	return r.provider
}

// agent mediates one agent() call: cap, dispatch, event pairing, outcome
// mapping.
func (r *liveRun) agent(prompt string, call AgentCall) (any, error) {
	if prompt == "" {
		return nil, NewWorkflowError("agent() requires a non-empty prompt", CodeInvalidArgument, nil, nil)
	}
	if r.signal.Err() != nil {
		// Cancellation before dispatch publishes nothing.
		return nil, nil
	}
	started := r.agentsStarted.Add(1)
	if r.cap > 0 && started > r.cap {
		return nil, NewWorkflowError(fmt.Sprintf("agent() exceeds the run's total-child cap of %d", r.cap), CodeAgentCap, nil, nil)
	}
	dispatchCtx, dispatchCancel := context.WithCancel(r.signal)
	defer dispatchCancel()
	start := subagent.SubagentStartRequest{
		Label:        agentLabel(call, prompt),
		Prompt:       []llm.ContentBlock{{Type: llm.BlockText, Text: prompt}},
		Parent:       r.parent,
		Signal:       dispatchCtx,
		OutputSchema: call.Schema,
	}
	child, err := r.engine.children.Start(r.providerFor(call), start)
	if err != nil {
		// No published run: neither event of the pair fires.
		return nil, NewWorkflowError(fmt.Sprintf("agent() dispatch failed: %s", err), CodeAgentStart, err, nil)
	}
	info := WorkflowAgentInfo{
		Seq:     r.seq.Add(1),
		Label:   start.Label,
		Phase:   r.phaseFor(call.Phase),
		ChildID: string(child.ID()),
	}
	r.engine.sink.Emit(EventWorkflowAgentStart, AgentStartPayload{Info: r.info(), Agent: info})
	type childOutcome struct {
		result    subagent.SubagentResult
		err       error
		cancelled bool
	}
	// Track the outcome watcher so execute() settles only after every
	// in-flight child wait has finished.
	r.workers.Add(1)
	outcomeCh := make(chan childOutcome, 1)
	go func() {
		defer r.workers.Done()
		result, resultErr := child.Result()
		select {
		case outcomeCh <- childOutcome{result: result, err: resultErr}:
		case <-dispatchCtx.Done():
		}
	}()
	var outcome childOutcome
	select {
	case outcome = <-outcomeCh:
	case <-dispatchCtx.Done():
		outcome.cancelled = true
	}
	if outcome.cancelled {
		_ = child.Dispose()
		r.engine.sink.Emit(EventWorkflowAgentEnd, AgentEndPayload{Info: r.info(), Agent: WorkflowAgentEndInfo{WorkflowAgentInfo: info, Outcome: AgentCancelled}})
		return nil, nil
	}
	_ = child.Dispose()
	endInfo := WorkflowAgentEndInfo{WorkflowAgentInfo: info}
	var value any
	switch {
	case outcome.err == nil && outcome.result.StopReason == subagent.StopCompleted:
		endInfo.Outcome = AgentCompleted
		if call.Schema != nil && isUnsetStructured(outcome.result.Structured) {
			// A schema call whose child finished without a valid capture
			// yields null — the script's null-check is the failure path
			// (the official agent() structured contract). The check is
			// reflect-guarded: a provider can hand back a typed-nil inside
			// the interface, which must read as unset, not as a value.
			value = nil
		} else {
			value = agentValue(outcome.result)
		}
	default:
		endInfo.Outcome = AgentFailed
	}
	r.engine.sink.Emit(EventWorkflowAgentEnd, AgentEndPayload{Info: r.info(), Agent: endInfo})
	return value, nil
}

// agentLabel resolves the call's display label: the explicit label, else a
// prompt snippet.
func agentLabel(call AgentCall, prompt string) string {
	if call.Label != "" {
		return call.Label
	}
	snippet := strings.TrimSpace(prompt)
	if len(snippet) > 80 {
		snippet = snippet[:80]
	}
	return snippet
}

// isUnsetStructured reports a structured capture that is absent: an
// untyped nil or any typed nil (a nil map/slice/pointer inside the
// interface must read as unset, never as a value).
func isUnsetStructured(value any) bool {
	if value == nil {
		return true
	}
	return reflect.ValueOf(value).IsNil()
}

// agentValue resolves the child's script value: the structured result when
// the call carried a schema, else the final text.
func agentValue(result subagent.SubagentResult) any {
	if result.Structured != nil {
		return result.Structured
	}
	var text strings.Builder
	for _, block := range result.Output {
		if block.Type == llm.BlockText {
			text.WriteString(block.Text)
		}
	}
	return text.String()
}
