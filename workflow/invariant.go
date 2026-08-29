package workflow

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Package-owned workflow lifecycle invariants (official invariant.ts): every
// event for a run must retain its validated identity snapshot, agent calls
// pair start/end by seq, and terminal results cover every observed start.
// The Go port is a validator over the lifecycle payloads; a host invariant
// registry feeds it every workflow/* event in dispatch order and fails loud
// on the first violation.

// RunTrace is one live run's observed identity: the validated meta JSON, the
// outstanding agent calls by seq, and the count of observed agent starts.
type RunTrace struct {
	Meta   string
	Agents map[int64]WorkflowAgentInfo
	Starts int64
}

// RunTraceValidator accumulates workflow lifecycle events per run and fails
// through `fail` on the first violated relation. Safe for concurrent use:
// combinator stages emit agent events concurrently.
type RunTraceValidator struct {
	mu     sync.Mutex
	traces map[WorkflowRunID]*RunTrace
	// Fail receives the violation message for the first violated relation.
	Fail func(message string)
}

// NewRunTraceValidator builds the validator; fail must be non-nil.
func NewRunTraceValidator(fail func(message string)) *RunTraceValidator {
	return &RunTraceValidator{traces: map[WorkflowRunID]*RunTrace{}, Fail: fail}
}

// failf reports one violation.
func (v *RunTraceValidator) failf(format string, args ...any) {
	v.Fail(fmt.Sprintf(format, args...))
}

// ObserveStart stages a workflow/start event.
func (v *RunTraceValidator) ObserveStart(info WorkflowRunInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(info.ID) == 0 || len(info.Meta.Name) == 0 || len(info.Meta.Description) == 0 {
		v.failf("workflow/start id, meta.name, and meta.description must be non-empty")
		return
	}
	if _, exists := v.traces[info.ID]; exists {
		v.failf("workflow/start repeated run id %q", info.ID)
		return
	}
	v.traces[info.ID] = &RunTrace{Meta: marshalMeta(info.Meta), Agents: map[int64]WorkflowAgentInfo{}}
}

// traceFor returns the run's trace or reports the missing/diverged identity.
func (v *RunTraceValidator) traceFor(info WorkflowRunInfo) *RunTrace {
	trace, exists := v.traces[info.ID]
	if !exists {
		v.failf("workflow event has no matching workflow/start for run %q", info.ID)
		return nil
	}
	if trace.Meta != marshalMeta(info.Meta) {
		v.failf("workflow event meta diverges from workflow/start for run %q", info.ID)
		return nil
	}
	return trace
}

// ObservePhase stages a workflow/phase event.
func (v *RunTraceValidator) ObservePhase(info WorkflowRunInfo, title string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.traceFor(info)
}

// ObserveLog stages a workflow/log event.
func (v *RunTraceValidator) ObserveLog(info WorkflowRunInfo, message string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.traceFor(info)
}

// ObserveAgentStart stages a workflow/agent-start event.
func (v *RunTraceValidator) ObserveAgentStart(info WorkflowRunInfo, agent WorkflowAgentInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	trace := v.traceFor(info)
	if trace == nil {
		return
	}
	if agent.Seq < 1 || len(agent.ChildID) == 0 {
		v.failf("workflow/agent-start seq must be positive and childId must be non-empty")
		return
	}
	if _, exists := trace.Agents[agent.Seq]; exists {
		v.failf("workflow/agent-start repeated seq %d", agent.Seq)
		return
	}
	trace.Agents[agent.Seq] = agent
	trace.Starts++
}

// ObserveAgentEnd stages a workflow/agent-end event.
func (v *RunTraceValidator) ObserveAgentEnd(info WorkflowRunInfo, agent WorkflowAgentEndInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	trace := v.traceFor(info)
	if trace == nil {
		return
	}
	start, exists := trace.Agents[agent.Seq]
	if !exists {
		v.failf("workflow/agent-end has no matching start for seq %d", agent.Seq)
		return
	}
	if start.Label != agent.Label || start.Phase != agent.Phase || start.ChildID != agent.ChildID {
		v.failf("workflow/agent-end identity diverges from workflow/agent-start for seq %d", agent.Seq)
		return
	}
	switch agent.Outcome {
	case AgentCompleted, AgentFailed, AgentCancelled:
	default:
		v.failf("workflow/agent-end carries unknown outcome %q", string(agent.Outcome))
		return
	}
	delete(trace.Agents, agent.Seq)
}

// ObserveEnd stages a workflow/end event.
func (v *RunTraceValidator) ObserveEnd(info WorkflowRunInfo, result WorkflowResultInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	trace := v.traceFor(info)
	if trace == nil {
		return
	}
	if len(trace.Agents) > 0 {
		v.failf("workflow/end has %d agent call(s) without workflow/agent-end", len(trace.Agents))
		return
	}
	if result.AgentsStarted < trace.Starts {
		v.failf("workflow/end agentsStarted must be a safe integer covering every observed agent start")
		return
	}
	if (result.StopReason == StopReasonCompleted && result.Error != "") ||
		(result.StopReason != StopReasonCompleted && result.Error == "") {
		v.failf("workflow/end error must be absent exactly for completed runs")
		return
	}
	delete(v.traces, info.ID)
}

// marshalMeta renders the meta block for identity comparison.
func marshalMeta(meta WorkflowMeta) string {
	raw, err := json.Marshal(meta)
	if err != nil {
		// WorkflowMeta is plain JSON data by the seam contract; a marshal
		// failure means the validator was handed impossible input.
		return fmt.Sprintf("[unmarshalable meta: %v]", err)
	}
	return string(raw)
}
