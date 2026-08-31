package toolralph

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/subagent"
	"dshgo/workflow"
)

func TestResolveConfigDefaultsAndGates(t *testing.T) {
	resolved, err := ResolveConfig(Config{})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if resolved.SubagentProvider != "spawn" || resolved.MaxRounds != 256 || resolved.MaxHandoffChars != 16384 || resolved.MaxResultChars != 16384 {
		t.Fatalf("defaults = %+v", resolved)
	}
	if _, err := ResolveConfig(Config{SubagentProvider: " pad "}); err == nil || !strings.Contains(err.Error(), "normalized") {
		t.Fatalf("unnormalized provider = %v", err)
	}
	zero := int64(0)
	if _, err := ResolveConfig(Config{MaxRounds: &zero}); err == nil || !strings.Contains(err.Error(), "maxRounds") {
		t.Fatalf("zero maxRounds = %v", err)
	}
}

func TestResolveMaxRoundsBoundedByCeiling(t *testing.T) {
	if _, err := ResolveMaxRounds(nil, 5); err != nil {
		t.Fatalf("default ceiling: %v", err)
	}
	three := int64(3)
	if got, err := ResolveMaxRounds(&three, 5); err != nil || got != 3 {
		t.Fatalf("requested = %d %v", got, err)
	}
	ten := int64(10)
	if _, err := ResolveMaxRounds(&ten, 5); err == nil || !strings.Contains(err.Error(), "exceeds the deployment ceiling") {
		t.Fatalf("over ceiling = %v", err)
	}
	zero := int64(0)
	if _, err := ResolveMaxRounds(&zero, 5); err == nil || !strings.Contains(err.Error(), "positive safe integer") {
		t.Fatalf("zero = %v", err)
	}
}

func TestDecodeReportStatusRules(t *testing.T) {
	report := func(status, blocker string, evidence, nextSteps []any) map[string]any {
		value := map[string]any{"status": status, "summary": "s", "blocker": blocker}
		if evidence != nil {
			value["evidence"] = evidence
		} else {
			value["evidence"] = []any{}
		}
		if nextSteps != nil {
			value["nextSteps"] = nextSteps
		} else {
			value["nextSteps"] = []any{}
		}
		return value
	}
	if _, err := decodeReport(report("continue", "", nil, []any{"next"}), StatusContinue, 16384); err != nil {
		t.Fatalf("continue: %v", err)
	}
	if _, err := decodeReport(report("continue", "blocked!", nil, []any{"next"}), StatusContinue, 16384); err == nil || !strings.Contains(err.Error(), "invalid continuing") {
		t.Fatalf("continue with blocker = %v", err)
	}
	if _, err := decodeReport(report("complete", "", []any{"done"}, nil), StatusComplete, 16384); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := decodeReport(report("complete", "", nil, nil), StatusComplete, 16384); err == nil || !strings.Contains(err.Error(), "invalid completion") {
		t.Fatalf("complete without evidence = %v", err)
	}
	if _, err := decodeReport(report("blocked", "waiting on human", nil, nil), StatusBlocked, 16384); err != nil {
		t.Fatalf("blocked: %v", err)
	}
	if _, err := decodeReport(report("blocked", "", nil, nil), StatusBlocked, 16384); err == nil || !strings.Contains(err.Error(), "invalid blocked") {
		t.Fatalf("blocked without blocker = %v", err)
	}
	big := strings.Repeat("x", 100)
	oversized := map[string]any{"status": "continue", "summary": big, "evidence": []any{}, "nextSteps": []any{"n"}, "blocker": ""}
	if _, err := decodeReport(oversized, StatusContinue, 10); err == nil || !strings.Contains(err.Error(), "oversized handoff") {
		t.Fatalf("oversized = %v", err)
	}
	unknown := map[string]any{"status": "mystery", "summary": "s", "evidence": []any{}, "nextSteps": []any{}, "blocker": ""}
	if _, err := decodeReport(unknown, StatusContinue, 16384); err == nil || !strings.Contains(err.Error(), "status is invalid") {
		t.Fatalf("unknown status = %v", err)
	}
}

// scriptedChildren dispatches one fake structured child per agent() call,
// replaying the scripted values in order (nil = the item-null discipline).
type scriptedChildren struct {
	mu     sync.Mutex
	values []any
	calls  []string
}

func (s *scriptedChildren) Start(provider string, request subagent.SubagentStartRequest) (subagent.SubagentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, request.Label+"|"+provider)
	index := len(s.calls) - 1
	value := s.values[index]
	structured, _ := value.(map[string]any)
	return &fakeRun{id: subagent.SubagentRunID("child-" + request.Label), value: value, structured: structured}, nil
}

type fakeRun struct {
	id         subagent.SubagentRunID
	value      any
	structured map[string]any
}

func (f *fakeRun) ID() subagent.SubagentRunID { return f.id }
func (f *fakeRun) LocalAgent() *agent.Agent   { return nil }
func (f *fakeRun) Result() (subagent.SubagentResult, error) {
	return subagent.SubagentResult{
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "round done"}},
		Structured: f.structured,
		StopReason: subagent.StopCompleted,
	}, nil
}
func (f *fakeRun) Dispose() error { return nil }

func TestProgramLoopsToCompletion(t *testing.T) {
	children := &scriptedChildren{values: []any{
		map[string]any{"status": "continue", "summary": "s1", "evidence": []any{}, "nextSteps": []any{"step-2"}, "blocker": ""},
		map[string]any{"status": "complete", "summary": "s2", "evidence": []any{"it works"}, "nextSteps": []any{}, "blocker": ""},
	}}
	sink := workflow.NewEventSink(nil)
	engine, err := workflow.NewEngine(workflow.EngineOptions{Sink: sink, Children: children})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	run, err := engine.Start(workflow.StartRequest{
		Program:          Program("ship the thing", 5, 16384),
		Meta:             RalphMeta(),
		SubagentProvider: "spawn",
		Parent:           &agent.Agent{},
		Signal:           context.Background(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	settled := <-run.Result()
	if settled.StopReason != workflow.StopReasonCompleted {
		t.Fatalf("settled = %+v", settled)
	}
	result, err := DecodeRunResult(settled.Value, 5, 16384)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != RunComplete || result.RoundsStarted != 2 {
		t.Fatalf("result = %+v", result)
	}
	if result.Report.Summary != "s2" || len(result.Report.Evidence) != 1 {
		t.Fatalf("report = %+v", result.Report)
	}
	children.mu.Lock()
	defer children.mu.Unlock()
	if len(children.calls) != 2 || !strings.Contains(children.calls[0], "Ralph round 1|spawn") {
		t.Fatalf("calls = %v", children.calls)
	}
}

func TestProgramStopsOnBlockedAndBudget(t *testing.T) {
	children := &scriptedChildren{values: []any{
		map[string]any{"status": "blocked", "summary": "s", "evidence": []any{}, "nextSteps": []any{}, "blocker": "need human sign-off"},
	}}
	sink := workflow.NewEventSink(nil)
	engine, _ := workflow.NewEngine(workflow.EngineOptions{Sink: sink, Children: children})
	run, err := engine.Start(workflow.StartRequest{
		Program: Program("obj", 4, 16384),
		Meta:    RalphMeta(),
		Parent:  &agent.Agent{},
		Signal:  context.Background(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	settled := <-run.Result()
	result, err := DecodeRunResult(settled.Value, 4, 16384)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Status != RunBlocked || result.RoundsStarted != 1 || result.Report.Blocker != "need human sign-off" {
		t.Fatalf("blocked result = %+v", result)
	}

	// Budget exhaustion keeps the last continue handoff.
	budget := &scriptedChildren{values: []any{
		map[string]any{"status": "continue", "summary": "r1", "evidence": []any{}, "nextSteps": []any{"keep"}, "blocker": ""},
		map[string]any{"status": "continue", "summary": "r2", "evidence": []any{}, "nextSteps": []any{"keep"}, "blocker": ""},
	}}
	engine2, _ := workflow.NewEngine(workflow.EngineOptions{Sink: sink, Children: budget})
	run2, err := engine2.Start(workflow.StartRequest{
		Program: Program("obj", 2, 16384),
		Meta:    RalphMeta(),
		Parent:  &agent.Agent{},
		Signal:  context.Background(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	settled2 := <-run2.Result()
	budgetResult, err := DecodeRunResult(settled2.Value, 2, 16384)
	if err != nil {
		t.Fatalf("decode budget: %v", err)
	}
	if budgetResult.Status != RunBudgetLimited || budgetResult.RoundsStarted != 2 || budgetResult.Report.Summary != "r2" {
		t.Fatalf("budget result = %+v", budgetResult)
	}
}

func TestProgramRoundFailureCarriesLastHandoff(t *testing.T) {
	children := &scriptedChildren{values: []any{nil}}
	sink := workflow.NewEventSink(nil)
	engine, _ := workflow.NewEngine(workflow.EngineOptions{Sink: sink, Children: children})
	run, err := engine.Start(workflow.StartRequest{
		Program: Program("obj", 3, 16384),
		Meta:    RalphMeta(),
		Parent:  &agent.Agent{},
		Signal:  context.Background(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	settled := <-run.Result()
	if settled.StopReason != workflow.StopReasonCompleted {
		t.Fatalf("a round failure settles the run completed: %+v", settled)
	}
	if _, err := DecodeRunResult(settled.Value, 3, 16384); err == nil || !strings.Contains(err.Error(), "Ralph round 1 child failed") {
		t.Fatalf("round failure decode = %v", err)
	}
	var failure RoundFailure
	encoded, _ := json.Marshal(settled.Value)
	if err := json.Unmarshal(encoded, &failure); err != nil {
		t.Fatalf("failure shape: %v", err)
	}
	if failure.Status != RoundFailureStatus || failure.RoundsStarted != 1 || failure.LastReport != nil {
		t.Fatalf("round failure = %+v", failure)
	}
}

func TestRenderResultAndRoundFailure(t *testing.T) {
	rendered := RenderResult(RunResult{
		Status:        RunComplete,
		RoundsStarted: 2,
		Report:        RalphRoundReport{Status: StatusComplete, Summary: "s", Evidence: []string{"e"}},
	}, 16384)
	if !strings.Contains(rendered, "completion after 2 rounds") || !strings.Contains(rendered, `"summary": "s"`) {
		t.Fatalf("render = %q", rendered)
	}
	single := RenderResult(RunResult{Status: RunBlocked, RoundsStarted: 1, Report: RalphRoundReport{Status: StatusBlocked, Blocker: "b"}}, 16384)
	if !strings.Contains(single, "after 1 round.") {
		t.Fatalf("singular render = %q", single)
	}
	budget := RenderResult(RunResult{Status: RunBudgetLimited, RoundsStarted: 3, Report: RalphRoundReport{}}, 16384)
	if !strings.Contains(budget, "3 rounds limit") {
		t.Fatalf("budget render = %q", budget)
	}
	failed := RenderRoundFailure(RoundFailure{Status: RoundFailureStatus, RoundsStarted: 2, LastReport: &RalphRoundReport{Status: StatusContinue, Summary: "last"}}, 16384)
	if !strings.Contains(failed, "Ralph round 2 child failed") || !strings.Contains(failed, "Last successful handoff") {
		t.Fatalf("failure render = %q", failed)
	}
	huge := strings.Repeat("y", 200)
	clamped := RenderResult(RunResult{Status: RunComplete, RoundsStarted: 1, Report: RalphRoundReport{Status: StatusComplete, Summary: huge, Evidence: []string{"e"}}}, 40)
	if int64(len(clamped)) > 40 || !strings.HasSuffix(clamped, "[truncated]") {
		t.Fatalf("clamp = %q", clamped)
	}
}
