package tools

import (
	"context"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/llm"
)

// newSchedulerRuntime builds a quiet runtime with one echoing tool.
func newSchedulerRuntime(t *testing.T) (*ToolRuntime, func()) {
	t.Helper()
	runtime, err := NewToolRuntime(cordis.Discard{}, Config{})
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	definition, err := DefineTool(DefineToolOptions{
		Name:        "echo",
		Description: "echo",
		Parameters: map[string]PropSpec{
			"text": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: ToolOutput{
			Schema: &ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: value.(string)}}
			},
		},
		Execute: func(args map[string]any, exec *ToolRunContext) (any, error) {
			return args["text"].(string), nil
		},
		IsConcurrencySafe: func(args map[string]any) bool { return true },
	})
	if err != nil {
		t.Fatalf("DefineTool: %v", err)
	}
	dispose, err := runtime.Register(definition)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	return runtime, dispose
}

func schedulerInput(name string, arguments any) *ToolExecutionInput {
	return &ToolExecutionInput{CallID: "call-1", Name: name, Arguments: arguments, Signal: context.Background()}
}

func TestStagedPipelineAgreesWithExecute(t *testing.T) {
	runtime, dispose := newSchedulerRuntime(t)
	t.Cleanup(dispose)

	direct := runtime.Execute(schedulerInput("echo", map[string]any{"text": "hi"}))
	if direct.IsError || direct.Value != "hi" {
		t.Fatalf("direct = %+v", direct)
	}

	// The staged path must produce the same final outcome for a plain
	// concurrency-safe tool: prepare dispatches, the dispatch result still
	// receives post-execute, and finalize completes it.
	prepared := runtime.Prepare(schedulerInput("echo", map[string]any{"text": "hi"}))
	if prepared.Kind != PreparedDispatch {
		t.Fatalf("prepare kind = %q", prepared.Kind)
	}
	dispatched := runtime.Dispatch(prepared.Exec)
	if dispatched.Kind != PreparedPostResult {
		t.Fatalf("dispatch kind = %q", dispatched.Kind)
	}
	final := runtime.Finalize(prepared.Exec, dispatched.Result)
	if final.IsError || final.Value != "hi" {
		t.Fatalf("staged = %+v", final)
	}
}

func TestStagedPostResultRunsFinalize(t *testing.T) {
	runtime, dispose := newSchedulerRuntime(t)
	t.Cleanup(dispose)

	// A post-execute waterfall that replaces the canonical value forces the
	// staged pipeline through the post-result branch.
	rewrite := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
		if result.IsError {
			return next(result)
		}
		decision := next(result)
		if decision.Kind == "block" {
			return decision
		}
		decision.ReplaceValue = result.Value.(string) + "!"
		decision.HasValue = true
		return decision
	})
	t.Cleanup(rewrite)

	prepared := runtime.Prepare(schedulerInput("echo", map[string]any{"text": "hi"}))
	if prepared.Kind != PreparedDispatch {
		t.Fatalf("prepare kind = %q", prepared.Kind)
	}
	dispatched := runtime.Dispatch(prepared.Exec)
	if dispatched.Kind != PreparedPostResult {
		t.Fatalf("dispatch kind = %q, want post-result", dispatched.Kind)
	}
	final := runtime.Finalize(prepared.Exec, dispatched.Result)
	if final.IsError || final.Value != "hi!" {
		t.Fatalf("finalized = %+v", final)
	}
}

func TestStagedInvalidArgumentsFailAtDispatch(t *testing.T) {
	runtime, dispose := newSchedulerRuntime(t)
	t.Cleanup(dispose)

	// Invalid arguments pass the prepare gate and fail inside dispatch, so
	// the failure flows through the same result machinery as Execute.
	prepared := runtime.Prepare(schedulerInput("echo", map[string]any{"wrong": true}))
	if prepared.Kind != PreparedDispatch {
		t.Fatalf("prepare kind = %q, want dispatch", prepared.Kind)
	}
	dispatched := runtime.Dispatch(prepared.Exec)
	if !dispatched.Result.IsError {
		t.Fatalf("dispatched result = %+v", dispatched.Result)
	}
	direct := runtime.Execute(schedulerInput("echo", map[string]any{"wrong": true}))
	if !direct.IsError {
		t.Fatalf("direct = %+v", direct)
	}

	// An unknown tool fails the same way.
	missing := runtime.Prepare(schedulerInput("nope", map[string]any{}))
	if missing.Kind != PreparedDispatch {
		t.Fatalf("missing prepare kind = %q", missing.Kind)
	}
	missingDispatch := runtime.Dispatch(missing.Exec)
	if !missingDispatch.Result.IsError {
		t.Fatalf("missing dispatched = %+v", missingDispatch.Result)
	}
}

func TestStagedFinishBypassesPostExecute(t *testing.T) {
	runtime, dispose := newSchedulerRuntime(t)
	t.Cleanup(dispose)

	rewritten := 0
	rewrite := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
		rewritten++
		return next(result)
	})
	t.Cleanup(rewrite)

	prepared := runtime.Prepare(schedulerInput("echo", map[string]any{"text": "hi"}))
	dispatched := runtime.Dispatch(prepared.Exec)
	_ = runtime.Finish(prepared.Exec, dispatched.Result)
	if rewritten != 0 {
		t.Fatalf("post-execute ran %d times through Finish", rewritten)
	}
}

func TestStagedFailureCarriesStructuredInfo(t *testing.T) {
	runtime, dispose := newSchedulerRuntime(t)
	t.Cleanup(dispose)

	prepared := runtime.Prepare(schedulerInput("echo", "not-an-object"))
	if prepared.Kind != PreparedDispatch {
		t.Fatalf("prepared = %+v", prepared.Kind)
	}
	dispatched := runtime.Dispatch(prepared.Exec)
	if !dispatched.Result.IsError {
		t.Fatalf("dispatched = %+v", dispatched.Result)
	}
	if dispatched.Result.Error == nil || dispatched.Result.Error.Info == nil {
		t.Fatalf("structured failure missing: %+v", dispatched.Result)
	}
	if !strings.Contains(dispatched.Result.Error.Message, "invalid arguments") {
		t.Fatalf("failure message = %q", dispatched.Result.Error.Message)
	}
}
