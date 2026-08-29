// Package workflow ports packages/workflow/workflow: the Service Definition
// of the workflow capability seam — vocabulary, typed fatal errors, the
// observe-only lifecycle event set, the engine contract, and meta-block
// validation. Service Providers execute orchestration scripts; observe-only
// lifecycle events never expose run control.
//
// Go adaptations: the abstract WorkflowEngine class becomes the Engine
// interface plus package-level emit containment; the never-rejecting result
// promise becomes a one-delivery channel; the worker-thread script runtime
// (workflow-worker-thread: a plain-JS realm for model-written scripts) has no
// Go counterpart — the seam, meta validation, and lifecycle vocabulary are
// the portable surface, and script execution is deferred to the engine round.
package workflow

import (
	"context"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
)

// WorkflowRunID identifies one workflow run. The engine mints UUIDs.
type WorkflowRunID = string

// WorkflowPhase is one phase declared in a script's meta.phases: progress
// vocabulary only — phases group agents in observers/UIs; they impose no
// execution structure.
type WorkflowPhase struct {
	// Title is matched by phase() calls by exact string.
	Title string `json:"title"`
	// Detail is an optional one-line description of what the phase does.
	Detail string `json:"detail,omitempty"`
	// Provider is an optional informational provider override this phase is
	// expected to use.
	Provider string `json:"provider,omitempty"`
	// Model is an optional informational model override this phase is
	// expected to use.
	Model string `json:"model,omitempty"`
}

// WorkflowMeta is the script's identity block, provided as plain JSON data
// alongside the script body (the model-facing tool carries it as its `meta`
// parameter) and validated by the engine before the body runs. Name and
// description are required; the rest is optional annotation. The field
// vocabulary matches the Claude Code dynamic-workflows meta block.
type WorkflowMeta struct {
	// Name is the short kebab-case workflow name (display + persistence
	// key).
	Name string `json:"name"`
	// Description is the one-line description of what the workflow does.
	Description string `json:"description"`
	// WhenToUse is optional guidance on when this workflow applies (shown in
	// listings).
	WhenToUse string `json:"whenToUse,omitempty"`
	// Phases are optional phase declarations matched by phase() calls.
	Phases []WorkflowPhase `json:"phases,omitempty"`
}

// WorkflowStopReason is why a run settled. CLOSED union (engine-owned,
// consumers may exhaust): completed = the script ran to its final return;
// cancelled = the run was cancelled (caller cancel/signal); error = the
// script threw, a fatal WorkflowError propagated, or the result failed
// materialization.
type WorkflowStopReason string

// Stop reasons.
const (
	StopReasonCompleted WorkflowStopReason = "completed"
	StopReasonCancelled WorkflowStopReason = "cancelled"
	StopReasonError     WorkflowStopReason = "error"
)

// WorkflowResult is the outcome resolved by a live workflow run. Value is
// the script's materialized return value (plain host-realm JSON data; nil
// when the script returned nothing) — meaningful only for completed. A
// non-completed reason carries the failure in Error; the consumer maps it to
// an isError tool result rather than reporting partial output.
type WorkflowResult struct {
	// Value is the script's return value (host JSON data; nil for no
	// return).
	Value any `json:"value,omitempty"`
	// StopReason says why the run settled.
	StopReason WorkflowStopReason `json:"stopReason"`
	// Error is the failure message, present iff stopReason is not
	// completed.
	Error string `json:"error,omitempty"`
	// AgentsStarted counts the agent() calls the run accepted over its whole
	// lifetime. On a graceful settlement this is the script-side count
	// (calls still queued for a concurrency slot included); on a termination
	// path (grace force-settle, worker death) it degrades to the
	// host-observed count — calls queued inside a terminated script are
	// unknowable then.
	AgentsStarted int64 `json:"agentsStarted"`
}

// WorkflowRunInfo is the identifying detail for a run, carried by every
// workflow/* event as borrowed immutable data, never the live run.
type WorkflowRunInfo struct {
	// ID is the run's id.
	ID WorkflowRunID `json:"id"`
	// Meta is the run's validated meta block.
	Meta WorkflowMeta `json:"meta"`
}

// WorkflowAgentInfo is one agent() call's identity within a run (the
// workflow/agent-start payload).
type WorkflowAgentInfo struct {
	// Seq is the 1-based sequence number of this agent() call within the
	// run.
	Seq int64 `json:"seq"`
	// Label is the display label (the label option, or a prompt snippet).
	Label string `json:"label"`
	// Phase is the phase this agent belongs to (the phase option, else the
	// current phase() title).
	Phase string `json:"phase,omitempty"`
	// ChildID is the child agent's id on the subagent seam.
	ChildID string `json:"childId"`
}

// WorkflowAgentOutcome says how one agent() call settled: clean result,
// child failure (the script sees nil), or run cancellation.
type WorkflowAgentOutcome string

// Agent outcomes.
const (
	AgentCompleted WorkflowAgentOutcome = "completed"
	AgentFailed    WorkflowAgentOutcome = "failed"
	AgentCancelled WorkflowAgentOutcome = "cancelled"
)

// WorkflowAgentEndInfo is one agent() call's settlement (the
// workflow/agent-end payload).
type WorkflowAgentEndInfo struct {
	WorkflowAgentInfo
	// Outcome says how the call settled.
	Outcome WorkflowAgentOutcome `json:"outcome"`
}

// WorkflowResultInfo is a settled run's outcome as event data (the
// workflow/end payload): the WorkflowResult minus Value — a listener
// observing outcomes must not receive a mutable alias of the caller's
// result value; a consumer that needs the value holds the run and awaits
// Result.
type WorkflowResultInfo struct {
	// StopReason says why the run settled.
	StopReason WorkflowStopReason `json:"stopReason"`
	// Error is the failure message, present iff stopReason is not
	// completed.
	Error string `json:"error,omitempty"`
	// AgentsStarted counts the agent() calls the run accepted.
	AgentsStarted int64 `json:"agentsStarted"`
}

// Workflow error codes: machine-routable fatal workflow failures —
// parse/meta/argument/schema errors, resource caps, subagent infrastructure
// failures, unserializable boundary values, and cancellation. An ordinary
// child failure resolves its item to nil and is NOT one of these fatal
// codes.
const (
	CodeScriptParse          = "SCRIPT_PARSE"
	CodeMetaInvalid          = "META_INVALID"
	CodeInvalidArgument      = "INVALID_ARGUMENT"
	CodeUnsupportedOption    = "UNSUPPORTED_OPTION"
	CodeUnsupportedSchema    = "UNSUPPORTED_SCHEMA"
	CodeAgentCap             = "AGENT_CAP"
	CodeItemCap              = "ITEM_CAP"
	CodeAgentStart           = "AGENT_START"
	CodeAgentResult          = "AGENT_RESULT"
	CodeResultUnserializable = "RESULT_UNSERIALIZABLE"
	CodeCancelled            = "CANCELLED"
)

// WorkflowError is the typed error for workflow-seam failures; Code is
// machine-routable taxonomy. Fatal drives the combinator discipline:
// parallel()/pipeline() re-throw a fatal error (a typo'd option or a tripped
// cap must kill the script loudly), and reserve the per-item nil for
// child-run failures and ordinary in-stage script errors. Every
// WorkflowErrorCode is fatal; the flag exists so the distinction is explicit
// at every catch site rather than implied.
type WorkflowError struct {
	err *llm.Error
	// Fatal marks errors combinators must propagate instead of nulling the
	// item.
	Fatal bool
}

// NewWorkflowError builds one workflow error; fatal defaults to true.
func NewWorkflowError(message, code string, cause error, fatal *bool) WorkflowError {
	isFatal := true
	if fatal != nil {
		isFatal = *fatal
	}
	return WorkflowError{err: llm.NewError(code, message, cause), Fatal: isFatal}
}

// Error implements error.
func (e WorkflowError) Error() string { return e.err.Error() }

// Code is the stable machine-routable code.
func (e WorkflowError) Code() string { return e.err.Code() }

// Unwrap reaches the harness error chain.
func (e WorkflowError) Unwrap() error { return e.err }

// IsFatalWorkflowError reports whether combinators must re-throw error
// instead of mapping the item to nil. Fatality is a host type assertion
// (unforgeable from a script realm in the source).
func IsFatalWorkflowError(err error) bool {
	if workflowErr, ok := err.(WorkflowError); ok {
		return workflowErr.Fatal
	}
	return false
}

// asWorkflowError walks the Unwrap chain looking for a WorkflowError.
func asWorkflowError(err error, target *WorkflowError) bool {
	for err != nil {
		if workflowErr, ok := err.(WorkflowError); ok {
			*target = workflowErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// The workflow/* lifecycle event set. Observe-only: none of them exposes run
// control. Each event's payload type follows.
const (
	EventWorkflowStart      = "workflow/start"
	EventWorkflowPhase      = "workflow/phase"
	EventWorkflowLog        = "workflow/log"
	EventWorkflowAgentStart = "workflow/agent-start"
	EventWorkflowAgentEnd   = "workflow/agent-end"
	EventWorkflowEnd        = "workflow/end"
)

// StartPayload is the workflow/start payload: the script's meta block
// validated, the body about to execute. Paired with WorkflowEndPayload.
type StartPayload struct {
	Info WorkflowRunInfo `json:"info"`
}

// PhasePayload is the workflow/phase payload: the script entered a phase (a
// phase(title) call) — progress grouping for observers; no execution
// semantics.
type PhasePayload struct {
	Info  WorkflowRunInfo `json:"info"`
	Title string          `json:"title"`
}

// LogPayload is the workflow/log payload: the script emitted a narration
// line (a log(message) call).
type LogPayload struct {
	Info    WorkflowRunInfo `json:"info"`
	Message string          `json:"message"`
}

// AgentStartPayload is the workflow/agent-start payload: one agent() call
// established a published child run. Paired with AgentEndPayload by
// Agent.Seq. A call that never receives a published run from the provider
// emits neither event in this pair.
type AgentStartPayload struct {
	Info  WorkflowRunInfo   `json:"info"`
	Agent WorkflowAgentInfo `json:"agent"`
}

// AgentEndPayload is the workflow/agent-end payload: one agent() call
// settled (clean result, child failure, or run cancellation). Paired with
// AgentStartPayload by Agent.Seq, exactly once per started call on every
// stop path — on an engine termination path (a worker killed past its
// grace) the end is engine-synthesized with outcome cancelled.
type AgentEndPayload struct {
	Info  WorkflowRunInfo      `json:"info"`
	Agent WorkflowAgentEndInfo `json:"agent"`
}

// EndPayload is the workflow/end payload: a workflow run settled (any stop
// reason), fired when WorkflowRun.Result resolves. Paired with
// StartPayload. The result carries deliberately NO value.
type EndPayload struct {
	Info   WorkflowRunInfo    `json:"info"`
	Result WorkflowResultInfo `json:"result"`
}

// StartRequest is what a caller asks for when starting a workflow run.
// Meta and Args are plain JSON data by the seam contract. Parent is
// required because every agent() spawned by the script is attributed to
// that live Agent.
type StartRequest struct {
	// Script is the plain-JS script body (top-level await allowed; ends
	// with `return <json-value>`).
	Script string
	// Program is the Go-realm counterpart of Script: the compiled
	// orchestration the engine executes when no JS worker realm is mounted.
	// Exactly one of Script/Program is meaningful per deployment; the
	// engine requires Program.
	Program Script
	// Meta is the workflow's identity block, as plain JSON data
	// (shape-validated by the engine).
	Meta any
	// Args is the optional input exposed verbatim to the script as the args
	// global.
	Args any
	// SubagentProvider is the optional engine-wide child-provider override
	// for this run.
	SubagentProvider string
	// MaxTotalAgents is the optional per-run total-child ceiling.
	MaxTotalAgents *int64
	// Parent is the agent on whose behalf the run executes (parent of every
	// child).
	Parent *agent.Agent
	// Signal cancels the run when aborted.
	Signal context.Context
}

// Run is the holder-owned live workflow. Result never rejects; consumers
// may cancel and must call idempotent Dispose to await script and child
// quiescence.
type Run interface {
	// ID is the run's id.
	ID() WorkflowRunID
	// Meta is the validated meta block available before the script body
	// runs.
	Meta() WorkflowMeta
	// Result resolves with exactly one outcome when the script settles; it
	// never rejects — a script failure is a WorkflowResult with a
	// non-completed stop reason.
	Result() <-chan WorkflowResult
	// Cancel cancels the run and its children.
	Cancel(reason string)
	// Dispose cancels if needed and awaits bounded settlement and cleanup.
	Dispose() error
}

// Engine is the workflow Service Definition contract: invalid requests fail
// before publication; a live run is holder-owned, its result never rejects,
// cancellation and disposal are bounded, and disposal waits for child
// cleanup within that bound. Lifecycle listener failures are contained, and
// workflow/end fires exactly once as the result settles.
type Engine interface {
	// Start parses and executes a workflow script.
	Start(request StartRequest) (Run, error)
}

// LifecycleListener handles one workflow lifecycle event payload. Returning
// early is fine: there is no delegation chain — lifecycle events are
// observe-only broadcasts.
type LifecycleListener func(payload any)

// EventSink is the engine-owned lifecycle listener registry. The engine
// dispatches each workflow/* event to every registered listener itself,
// containing each invocation: a panicking or failing listener is logged and
// never propagates into the engine path, and the remaining listeners still
// run. Listeners register in order and undo removes them.
type EventSink struct {
	mu        sync.Mutex
	logger    Logger
	listeners map[string][]*listenerEntry
}

// listenerEntry is one registration; enabled lets undo race the emit loop.
type listenerEntry struct {
	fn      LifecycleListener
	removed bool
}

// Logger is the logging surface the sink warns through.
type Logger interface {
	Warn(message string)
}

// NewEventSink builds the sink; logger may be nil to drop containment
// warnings.
func NewEventSink(logger Logger) *EventSink {
	return &EventSink{logger: logger, listeners: map[string][]*listenerEntry{}}
}

// On registers one listener for an event name and returns the undo
// func (idempotent).
func (s *EventSink) On(name string, fn LifecycleListener) func() {
	entry := &listenerEntry{fn: fn}
	s.mu.Lock()
	s.listeners[name] = append(s.listeners[name], entry)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		entry.removed = true
		s.mu.Unlock()
	}
}

// Emit dispatches one lifecycle event to every live listener while
// containing each invocation.
func (s *EventSink) Emit(name string, payload any) {
	s.mu.Lock()
	entries := make([]*listenerEntry, len(s.listeners[name]))
	copy(entries, s.listeners[name])
	s.mu.Unlock()
	for _, entry := range entries {
		s.mu.Lock()
		removed := entry.removed
		s.mu.Unlock()
		if removed {
			continue
		}
		s.invoke(name, entry.fn, payload)
	}
}

// invoke runs one listener with panic containment.
func (s *EventSink) invoke(name string, fn LifecycleListener, payload any) {
	defer func() {
		if rec := recover(); rec != nil && s.logger != nil {
			s.logger.Warn(fmt.Sprintf("workflow: %s listener threw: %s", name, renderListenerError(fmt.Errorf("%v", rec))))
		}
	}()
	fn(payload)
}

// renderListenerError renders any thrown value without violating listener
// containment.
func renderListenerError(err error) string {
	if err == nil {
		return "[unrenderable thrown value]"
	}
	return err.Error()
}
