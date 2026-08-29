package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/subagent"
)

// fakeChild is a published one-shot child run.
type fakeChild struct {
	id       string
	result   subagent.SubagentResult
	err      error
	disposed int
}

func (c *fakeChild) ID() session.SessionID    { return session.SessionID(c.id) }
func (c *fakeChild) LocalAgent() *agent.Agent { return nil }
func (c *fakeChild) Result() (subagent.SubagentResult, error) {
	return c.result, c.err
}
func (c *fakeChild) Dispose() error { c.disposed++; return nil }

// fakeChildren dispatches scripted children.
type fakeChildren struct {
	mu      sync.Mutex
	queue   []*fakeChild
	starts  []subagent.SubagentStartRequest
	fail    error
	blockCh chan struct{}
}

func (f *fakeChildren) Start(request subagent.SubagentStartRequest) (subagent.SubagentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, request)
	if f.fail != nil {
		return nil, f.fail
	}
	if len(f.queue) == 0 {
		return nil, errors.New("fakeChildren: no scripted child")
	}
	child := f.queue[0]
	f.queue = f.queue[1:]
	if f.blockCh != nil {
		<-f.blockCh
	}
	return child, nil
}

// recordingSink captures lifecycle payloads in order.
type recordingSink struct {
	*EventSink
	mu      sync.Mutex
	events  []string
	payload []any
}

func newRecordingSink() *recordingSink {
	sink := &recordingSink{EventSink: NewEventSink(nil)}
	for _, name := range []string{EventWorkflowStart, EventWorkflowPhase, EventWorkflowLog, EventWorkflowAgentStart, EventWorkflowAgentEnd, EventWorkflowEnd} {
		name := name
		sink.On(name, func(payload any) {
			sink.mu.Lock()
			sink.events = append(sink.events, name)
			sink.payload = append(sink.payload, payload)
			sink.mu.Unlock()
		})
	}
	return sink
}

func (r *recordingSink) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func textChild(id string, text string) *fakeChild {
	return &fakeChild{id: id, result: subagent.SubagentResult{
		StopReason: subagent.StopCompleted,
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}}
}

// awaitResult reads the run's result with a test timeout.
func awaitResult(t *testing.T, run Run) WorkflowResult {
	t.Helper()
	select {
	case result := <-run.Result():
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("run result did not settle in time")
		return WorkflowResult{}
	}
}

func testMeta() map[string]any {
	return map[string]any{"name": "audit", "description": "fan out"}
}

func startTest(t *testing.T, children ChildDispatcher, sink *EventSink, script Script, mutate func(*StartRequest)) (Run, WorkflowResult) {
	t.Helper()
	engine, err := NewEngine(EngineOptions{Sink: sink, Children: children, NewID: func() string { return "run-1" }})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	request := StartRequest{Meta: testMeta(), Program: script, Parent: &agent.Agent{}}
	if mutate != nil {
		mutate(&request)
	}
	run, err := engine.Start(request)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return run, awaitResult(t, run)
}

func TestEngineStartValidationBeforePublication(t *testing.T) {
	engine, err := NewEngine(EngineOptions{Sink: NewEventSink(nil), Children: &fakeChildren{}})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	badMeta := map[string]any{"name": "x"}
	if _, err := engine.Start(StartRequest{Meta: badMeta, Program: func(*ScriptAPI) (any, error) { return nil, nil }}); err == nil {
		t.Fatal("invalid meta must fail before publication")
	}
	if _, err := engine.Start(StartRequest{Meta: testMeta(), Program: func(*ScriptAPI) (any, error) { return nil, nil }}); err == nil {
		t.Fatal("missing parent must fail")
	}
	if _, err := engine.Start(StartRequest{Meta: testMeta(), Parent: &agent.Agent{}}); err == nil {
		t.Fatal("missing program must fail")
	}
	negative := int64(-1)
	if _, err := engine.Start(StartRequest{Meta: testMeta(), Program: func(*ScriptAPI) (any, error) { return nil, nil }, MaxTotalAgents: &negative}); err == nil {
		t.Fatal("negative cap must fail")
	}
}

func TestEngineHappyPathEventsAndResult(t *testing.T) {
	children := &fakeChildren{queue: []*fakeChild{textChild("kid-1", "found it")}}
	sink := newRecordingSink()
	validator := NewRunTraceValidator(func(message string) { t.Fatalf("invariant: %s", message) })
	sink.On(EventWorkflowStart, func(p any) { validator.ObserveStart(p.(StartPayload).Info) })
	sink.On(EventWorkflowPhase, func(p any) { v := p.(PhasePayload); validator.ObservePhase(v.Info, v.Title) })
	sink.On(EventWorkflowLog, func(p any) { v := p.(LogPayload); validator.ObserveLog(v.Info, v.Message) })
	sink.On(EventWorkflowAgentStart, func(p any) { validator.ObserveAgentStart(p.(AgentStartPayload).Info, p.(AgentStartPayload).Agent) })
	sink.On(EventWorkflowAgentEnd, func(p any) { validator.ObserveAgentEnd(p.(AgentEndPayload).Info, p.(AgentEndPayload).Agent) })
	sink.On(EventWorkflowEnd, func(p any) { validator.ObserveEnd(p.(EndPayload).Info, p.(EndPayload).Result) })
	run, result := startTest(t, children, sink.EventSink, func(api *ScriptAPI) (any, error) {
		api.Phase("audit")
		api.Log("starting")
		first, err := api.Agent("look for widgets", AgentCall{})
		if err != nil {
			return nil, err
		}
		return []any{first}, nil
	}, nil)
	if result.StopReason != StopReasonCompleted || result.AgentsStarted != 1 {
		t.Fatalf("result = %+v", result)
	}
	values := result.Value.([]any)
	if values[0] != "found it" {
		t.Fatalf("agent value = %v", values[0])
	}
	want := []string{EventWorkflowStart, EventWorkflowPhase, EventWorkflowLog, EventWorkflowAgentStart, EventWorkflowAgentEnd, EventWorkflowEnd}
	if got := sink.names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", got)
	}
	// The label defaults to a prompt snippet and the phase follows phase().
	children.mu.Lock()
	label := children.starts[0].Label
	children.mu.Unlock()
	if label != "look for widgets" {
		t.Fatalf("label = %q", label)
	}
	for _, payload := range sink.payload {
		if end, ok := payload.(AgentEndPayload); ok && end.Agent.Seq != 1 {
			t.Fatalf("seq = %d", end.Agent.Seq)
		}
	}
	if run.ID() != "run-1" {
		t.Fatalf("id = %s", run.ID())
	}
}

func TestEngineCapsAndDispatchFailureAreFatal(t *testing.T) {
	// A tripped cap kills the run loudly with AGENT_CAP.
	children := &fakeChildren{queue: []*fakeChild{textChild("kid-1", "one")}}
	sink := NewEventSink(nil)
	cap := int64(1)
	_, result := startTest(t, children, sink, func(api *ScriptAPI) (any, error) {
		if _, err := api.Agent("first", AgentCall{}); err != nil {
			return nil, err
		}
		_, err := api.Agent("second", AgentCall{})
		return nil, err
	}, func(r *StartRequest) { r.MaxTotalAgents = &cap })
	if result.StopReason != StopReasonError || !strings.Contains(result.Error, "total-child cap") {
		t.Fatalf("result = %+v", result)
	}
	// A dispatch failure is AGENT_START-fatal and publishes no pair.
	failing := &fakeChildren{fail: errors.New("provider down")}
	_, result = startTest(t, failing, NewEventSink(nil), func(api *ScriptAPI) (any, error) {
		return api.Agent("anything", AgentCall{})
	}, nil)
	if result.StopReason != StopReasonError || !strings.Contains(result.Error, "dispatch failed") {
		t.Fatalf("dispatch result = %+v", result)
	}
}

func TestEngineChildFailureNullsItem(t *testing.T) {
	badChild := &fakeChild{id: "kid-bad", result: subagent.SubagentResult{StopReason: subagent.StopError}}
	children := &fakeChildren{queue: []*fakeChild{badChild}}
	sink := newRecordingSink()
	_, result := startTest(t, children, sink.EventSink, func(api *ScriptAPI) (any, error) {
		first, err := api.Agent("explode", AgentCall{Label: "bad"})
		if err != nil {
			return nil, err
		}
		return []any{first}, nil
	}, nil)
	if result.StopReason != StopReasonCompleted {
		t.Fatalf("child failure must null the item, got %+v", result)
	}
	if values := result.Value.([]any); values[0] != nil {
		t.Fatalf("value = %v, want nil", values[0])
	}
	for _, payload := range sink.payload {
		if end, ok := payload.(AgentEndPayload); ok && end.Agent.Outcome != AgentFailed {
			t.Fatalf("outcome = %s", end.Agent.Outcome)
		}
	}
	if badChild.disposed != 1 {
		t.Fatalf("disposed = %d", children.queue[0].disposed)
	}
}

func TestEngineCancellationMidFlight(t *testing.T) {
	engine, err := NewEngine(EngineOptions{Sink: NewEventSink(nil), Children: &fakeChildren{}, NewID: func() string { return "run-1" }})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	signal, cancel := context.WithCancel(context.Background())
	run, err := engine.Start(StartRequest{
		Meta: testMeta(), Parent: &agent.Agent{}, Signal: signal,
		Program: func(api *ScriptAPI) (any, error) {
			value, err := api.Agent("slow", AgentCall{})
			return []any{value}, err
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cancel()
	result := awaitResult(t, run)
	if result.StopReason != StopReasonCancelled {
		t.Fatalf("result = %+v", result)
	}
	if err := run.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestParallelAndPipelineCombinators(t *testing.T) {
	// Parallel: values in input order; a plain thunk error nulls its slot.
	api := &ScriptAPI{run: &liveRun{signal: context.Background(), engine: &engine{}}}
	values, err := api.Parallel([]func() (any, error){
		func() (any, error) { return "a", nil },
		func() (any, error) { return nil, errors.New("boom") },
		func() (any, error) { return "c", nil },
	})
	if err != nil || values[0] != "a" || values[1] != nil || values[2] != "c" {
		t.Fatalf("parallel = %v %v", values, err)
	}
	// A fatal thunk error propagates.
	fatal := NewWorkflowError("cap", CodeAgentCap, nil, nil)
	if _, err := api.Parallel([]func() (any, error){
		func() (any, error) { return nil, fatal },
	}); !IsFatalWorkflowError(err) {
		t.Fatalf("fatal parallel err = %v", err)
	}
	// Pipeline: no barrier; stage errors drop that item and skip its rest.
	// "x" parks in stage 0 behind a gate; "y" must reach stage 1 (and die
	// there) before the gate opens.
	slowGate := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(slowGate)
	}()
	seen := make(chan string, 4)
	results, err := api.Pipeline([]any{"x", "y"},
		func(prev any, item any, index int) (any, error) {
			if item == "x" {
				<-slowGate
				seen <- "x0"
			} else {
				seen <- "y0"
			}
			return fmt.Sprintf("%s!", item), nil
		},
		func(prev any, item any, index int) (any, error) {
			seen <- fmt.Sprintf("%s1", item)
			if item == "y" {
				return nil, errors.New("stage blew up")
			}
			return prev, nil
		},
	)
	if err != nil {
		t.Fatalf("pipeline err = %v", err)
	}
	if results[0] != "x!" || results[1] != nil {
		t.Fatalf("pipeline = %v", results)
	}
	if first := <-seen; first != "y0" {
		t.Fatalf("first stage sighting = %s, want y0 before x leaves stage 0", first)
	}
	// A fatal stage error propagates.
	if _, err := api.Pipeline([]any{"z"}, func(prev any, item any, index int) (any, error) {
		return nil, fatal
	}); !IsFatalWorkflowError(err) {
		t.Fatalf("fatal pipeline err = %v", err)
	}
}
