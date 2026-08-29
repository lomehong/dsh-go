package workflow

import (
	"sync"
	"testing"
)

// failOnce collects the first invariant violation.
type failOnce struct {
	mu      sync.Mutex
	message string
}

func (f *failOnce) fail(message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.message == "" {
		f.message = message
	}
}

func startInfo() WorkflowRunInfo {
	return WorkflowRunInfo{ID: "run-1", Meta: WorkflowMeta{Name: "audit", Description: "fan out"}}
}

func TestRunTraceValidatorHappyPath(t *testing.T) {
	failures := &failOnce{}
	validator := NewRunTraceValidator(failures.fail)
	validator.ObserveStart(startInfo())
	validator.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, Label: "L", Phase: "P", ChildID: "kid-1"})
	validator.ObserveAgentEnd(startInfo(), WorkflowAgentEndInfo{WorkflowAgentInfo: WorkflowAgentInfo{Seq: 1, Label: "L", Phase: "P", ChildID: "kid-1"}, Outcome: AgentCompleted})
	validator.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonCompleted, AgentsStarted: 1})
	if failures.message != "" {
		t.Fatalf("violation: %s", failures.message)
	}
	// progress events pass through and observe-only phase/log keep identity.
	validator.ObserveStart(startInfo())
	validator.ObservePhase(startInfo(), "P")
	validator.ObserveLog(startInfo(), "hi")
	validator.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonCancelled, Error: "cancelled", AgentsStarted: 0})
	if failures.message != "" {
		t.Fatalf("violation: %s", failures.message)
	}
}

func TestRunTraceValidatorViolations(t *testing.T) {
	cases := []struct {
		name  string
		drive func(v *RunTraceValidator)
		want  string
	}{
		{"empty meta", func(v *RunTraceValidator) {
			v.ObserveStart(WorkflowRunInfo{ID: "r", Meta: WorkflowMeta{Name: "", Description: "d"}})
		}, "workflow/start id, meta.name, and meta.description must be non-empty"},
		{"repeated id", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveStart(startInfo())
		}, `workflow/start repeated run id "run-1"`},
		{"no start", func(v *RunTraceValidator) {
			v.ObservePhase(startInfo(), "P")
		}, `workflow event has no matching workflow/start for run "run-1"`},
		{"meta divergence", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			diverged := startInfo()
			diverged.Meta.Description = "changed"
			v.ObservePhase(diverged, "P")
		}, `workflow event meta diverges from workflow/start for run "run-1"`},
		{"bad agent start", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 0, ChildID: ""})
		}, "workflow/agent-start seq must be positive and childId must be non-empty"},
		{"repeated seq", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"})
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, ChildID: "kid-2"})
		}, "workflow/agent-start repeated seq 1"},
		{"end without start", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentEnd(startInfo(), WorkflowAgentEndInfo{WorkflowAgentInfo: WorkflowAgentInfo{Seq: 3}, Outcome: AgentFailed})
		}, "workflow/agent-end has no matching start for seq 3"},
		{"identity divergence", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, Label: "L", ChildID: "kid-1"})
			v.ObserveAgentEnd(startInfo(), WorkflowAgentEndInfo{WorkflowAgentInfo: WorkflowAgentInfo{Seq: 1, Label: "L", ChildID: "kid-X"}, Outcome: AgentFailed})
		}, "workflow/agent-end identity diverges from workflow/agent-start for seq 1"},
		{"unknown outcome", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"})
			v.ObserveAgentEnd(startInfo(), WorkflowAgentEndInfo{WorkflowAgentInfo: WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"}, Outcome: "exploded"})
		}, `workflow/agent-end carries unknown outcome "exploded"`},
		{"outstanding agents", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"})
			v.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonCompleted, AgentsStarted: 1})
		}, "workflow/end has 1 agent call(s) without workflow/agent-end"},
		{"undercounted starts", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveAgentStart(startInfo(), WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"})
			v.ObserveAgentEnd(startInfo(), WorkflowAgentEndInfo{WorkflowAgentInfo: WorkflowAgentInfo{Seq: 1, ChildID: "kid-1"}, Outcome: AgentCompleted})
			v.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonCompleted, AgentsStarted: 0})
		}, "workflow/end agentsStarted must be a safe integer covering every observed agent start"},
		{"error discipline", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonCompleted, Error: "oops", AgentsStarted: 0})
		}, "workflow/end error must be absent exactly for completed runs"},
		{"error discipline inverse", func(v *RunTraceValidator) {
			v.ObserveStart(startInfo())
			v.ObserveEnd(startInfo(), WorkflowResultInfo{StopReason: StopReasonError, AgentsStarted: 0})
		}, "workflow/end error must be absent exactly for completed runs"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			failures := &failOnce{}
			validator := NewRunTraceValidator(failures.fail)
			testCase.drive(validator)
			if failures.message != testCase.want {
				t.Fatalf("violation = %q, want %q", failures.message, testCase.want)
			}
		})
	}
}
