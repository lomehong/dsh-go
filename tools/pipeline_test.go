// Execution-pipeline behaviors: pre-policy ordering, approval ask, guard
// denial, around-dispatch normalization, post-policy accept/replace/block,
// cancellation contracts, deferred context, conclude-turn, and the PTC mode
// collapse. Ports of the ToolRuntime scheduler tests in packages/core/tools.
package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/llm"
)

func runInput(name string, args any) *ToolExecutionInput {
	return &ToolExecutionInput{CallID: "call-" + name, Name: name, Arguments: args, Signal: context.Background()}
}

// makeEcho defines one echo tool with presentation metadata and a content
// finalizer, the common success shape under test.
func makeEcho(t *testing.T, runtime *ToolRuntime, execute func(args map[string]any, exec *ToolRunContext) (any, error)) {
	t.Helper()
	definition, err := DefineTool(DefineToolOptions{
		Name:        "echo",
		Description: "echo the name",
		Parameters: map[string]PropSpec{
			"name": {ValueSchemaSpec: ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: ToolOutput{
			Schema: &ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: "text", Text: "render:" + value.(string)}}
			},
		},
		PresentationMeta: func(args map[string]any, value any) any {
			return map[string]any{"echoed": value}
		},
		FinalizeContent: func(exec *ToolExecution, result *ToolExecutionResult) []llm.ContentBlock {
			return append(append([]llm.ContentBlock{}, result.Content...), llm.ContentBlock{Type: "text", Text: "finalized"})
		},
		Execute: execute,
	})
	if err != nil {
		t.Fatalf("DefineTool: %v", err)
	}
	mustRegister(t, runtime, nil, definition)
}

func TestPipelineHappyPathObservesFrozenOutcome(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
		return "hi " + args["name"].(string), nil
	})
	var observed *ToolExecutionResult
	var observedExec *ToolExecution
	defer runtime.OnResult(nil, func(exec *ToolExecution, result *ToolExecutionResult) {
		observed = result
		observedExec = exec
	})()

	result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Value != "hi ada" {
		t.Fatalf("value = %v", result.Value)
	}
	if len(result.Content) != 2 || result.Content[0].Text != "render:hi ada" || result.Content[1].Text != "finalized" {
		t.Fatalf("content = %+v", result.Content)
	}
	meta, ok := result.Meta.(map[string]any)
	if !ok || meta["echoed"] != "hi ada" {
		t.Fatalf("meta = %#v", result.Meta)
	}
	if observed == nil || observed.Value != "hi ada" || len(observed.Content) != 2 {
		t.Fatalf("observer = %+v", observed)
	}
	if observedExec == nil || observedExec.CallID != "call-echo" || observedExec.RootCallID != "call-echo" {
		t.Fatalf("observer exec = %+v", observedExec)
	}
	// The returned outcome is the same frozen value final observers received.
	if result != observed {
		t.Fatal("Execute must return the notified authoritative result")
	}
	// Mutating the returned content must not corrupt the notified copy.
	if observedExec.Token == nil {
		t.Fatal("registry must mint the correlation token")
	}
}

func TestPipelineUnknownToolKeepsPreExecuteObserved(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	var seen string
	undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
		seen = exec.Name
		return next(exec)
	})
	defer undo()

	result := runtime.Execute(runInput("ghost", map[string]any{}))
	if !result.IsError || result.Content[0].Text != `Error: unknown tool "ghost"` {
		t.Fatalf("result = %+v", result)
	}
	if result.Error == nil || result.Error.Info == nil || result.Error.Info.Code != CodeUnknownTool {
		t.Fatalf("error = %+v", result.Error)
	}
	if seen != "ghost" {
		t.Fatalf("pre-execute observed %q", seen)
	}
}

func TestPipelineArgsValidationFailure(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
		return "hi", nil
	})
	result := runtime.Execute(runInput("echo", map[string]any{}))
	if !result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if result.Error == nil || result.Error.Info == nil || result.Error.Info.Code != "INVALID_ARGS" {
		t.Fatalf("error = %+v", result.Error)
	}
	if !strings.HasPrefix(result.Content[0].Text, `Error: invalid arguments: missing required property "name"`) {
		t.Fatalf("content = %q", result.Content[0].Text)
	}
}

func TestPipelineOutputContractFailures(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	definition, err := DefineTool(DefineToolOptions{
		Name:        "broken",
		Description: "violates its output contract",
		Parameters:  map[string]PropSpec{},
		Output: ToolOutput{
			Schema: &ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock { return nil },
		},
		Execute: func(args map[string]any, exec *ToolRunContext) (any, error) {
			return 42, nil // wrong type for the declared string output
		},
	})
	if err != nil {
		t.Fatalf("DefineTool: %v", err)
	}
	mustRegister(t, runtime, nil, definition)

	result := runtime.Execute(runInput("broken", map[string]any{}))
	if !result.IsError || result.Error == nil || result.Error.Info == nil || result.Error.Info.Code != CodeInvalidToolOutput {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Error.Message, `tool "broken" returned invalid output: "value" must be a string`) {
		t.Fatalf("message = %q", result.Error.Message)
	}

	nonJSON := echoDefinition(t, "nonjson", func(args map[string]any, exec *ToolRunContext) (any, error) {
		return func() {}, nil
	})
	mustRegister(t, runtime, nil, nonJSON)
	result = runtime.Execute(runInput("nonjson", map[string]any{"name": "x"}))
	if !result.IsError || !strings.Contains(result.Error.Message, "value is not lossless JSON") {
		t.Fatalf("nonjson result = %+v", result)
	}
}

func TestPreExecuteDenyAskAndApprovalFlows(t *testing.T) {
	background := context.Background()
	t.Run("deny", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			return "hi", nil
		})
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreDeny, Reason: "policy says no", HasReason: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || result.Content[0].Text != "Error: policy says no" || result.Error.Message != "policy says no" {
			t.Fatalf("result = %+v", result)
		}
	})

	askCases := []struct {
		outcome ApprovalOutcome
		text    string
	}{
		{ApprovalRejected, `the user rejected tool "echo"`},
		{ApprovalCancelled, `approval for tool "echo" was cancelled`},
		{ApprovalUnavailable, `tool "echo" requires approval, but no approval channel is available`},
	}
	for _, testCase := range askCases {
		t.Run(string(testCase.outcome), func(t *testing.T) {
			runtime := newTestRuntime(t, Config{})
			makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
				return "hi", nil
			})
			undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
				return &PreToolDecision{Kind: PreAsk}
			})
			defer undo()
			runtime.Approval = func() ApprovalService {
				return ApprovalServiceFunc(func(ApprovalRequest) ApprovalOutcome { return testCase.outcome })
			}
			scope := NewScopeKey(nil)
			result := runtime.Execute(&ToolExecutionInput{
				CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"},
				Agent: scope, Signal: background,
			})
			if !result.IsError || result.Content[0].Text != "Error: "+testCase.text {
				t.Fatalf("result = %+v", result)
			}
		})
	}

	t.Run("allowed-once runs", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			return "approved", nil
		})
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk}
		})
		defer undo()
		var request ApprovalRequest
		runtime.Approval = func() ApprovalService {
			return ApprovalServiceFunc(func(r ApprovalRequest) ApprovalOutcome {
				request = r
				return ApprovalAllowedOnce
			})
		}
		result := runtime.Execute(&ToolExecutionInput{
			CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"},
			Agent: NewScopeKey(nil), Signal: background,
		})
		if result.IsError || result.Value != "approved" {
			t.Fatalf("result = %+v", result)
		}
		if request.ToolName != "echo" || request.CallID != "c" {
			t.Fatalf("request = %+v", request)
		}
	})

	t.Run("ask without seam degrades to deny", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, nil)
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || result.Content[0].Text != `Error: tool "echo" requires approval (not yet supported)` {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("ask with reason overrides the degrade text", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, nil)
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk, Reason: "needs a human", HasReason: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || result.Content[0].Text != "Error: needs a human" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("agent-less ask with seam denies", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, nil)
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk}
		})
		defer undo()
		runtime.Approval = func() ApprovalService {
			return ApprovalServiceFunc(func(ApprovalRequest) ApprovalOutcome { return ApprovalAllowedOnce })
		}
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || result.Content[0].Text != `Error: tool "echo" requires approval, but the call has no agent to route it through` {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("approval cancel on a live caller denies without aborting", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, nil)
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk}
		})
		defer undo()
		runtime.Approval = func() ApprovalService {
			return ApprovalServiceFunc(func(ApprovalRequest) ApprovalOutcome { return ApprovalCancelled })
		}
		result := runtime.Execute(&ToolExecutionInput{
			CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"},
			Agent: NewScopeKey(nil), Signal: background,
		})
		if !result.IsError || result.Content[0].Text != `Error: approval for tool "echo" was cancelled` {
			t.Fatalf("result = %+v", result)
		}
		if result.Error.Info != nil {
			t.Fatalf("a denial is not a cancellation: %+v", result.Error.Info)
		}
	})

	t.Run("approval cancel on an aborted caller routes the cancellation", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, nil)
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			return &PreToolDecision{Kind: PreAsk}
		})
		defer undo()
		runtime.Approval = func() ApprovalService {
			return ApprovalServiceFunc(func(ApprovalRequest) ApprovalOutcome { return ApprovalCancelled })
		}
		signal, cancel := context.WithCancel(background)
		cancel()
		result := runtime.Execute(&ToolExecutionInput{
			CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"}, Signal: signal,
		})
		if !result.IsError || result.Content[0].Text != "Error: tool call aborted before dispatch" {
			t.Fatalf("result = %+v", result)
		}
		if result.Error.Info == nil || result.Error.Info.Code != CodeToolAbortedBeforeDispatch {
			t.Fatalf("info = %+v", result.Error.Info)
		}
	})
}

func TestPostExecuteAcceptReplaceAndBlock(t *testing.T) {

	t.Run("replace content", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) { return "hi ada", nil })
		undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
			return &PostToolDecision{Kind: PostAccept, ReplaceContent: []llm.ContentBlock{{Type: "text", Text: "presented"}}, HasContent: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if result.IsError || len(result.Content) != 2 || result.Content[0].Text != "presented" || result.Content[1].Text != "finalized" {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("replace value re-runs the output contract", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) { return "hi ada", nil })
		undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
			return &PostToolDecision{Kind: PostAccept, ReplaceValue: "replaced", HasValue: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if result.IsError || result.Value != "replaced" {
			t.Fatalf("result = %+v", result)
		}
		if len(result.Content) != 2 || result.Content[0].Text != "render:replaced" {
			t.Fatalf("content = %+v", result.Content)
		}
	})

	t.Run("block turns feedback into the error body", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			exec.DeferContext(llm.Message{Role: llm.RoleUser})
			return "hi ada", nil
		})
		undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
			return &PostToolDecision{Kind: PostBlock, Feedback: []llm.ContentBlock{{Type: "text", Text: "correct the input"}}}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || len(result.Content) != 2 || result.Content[0].Text != "correct the input" {
			t.Fatalf("result = %+v", result)
		}
		if result.Error == nil || result.Error.Message != "correct the input" {
			t.Fatalf("error = %+v", result.Error)
		}
		if len(result.AdditionalContexts) != 0 {
			t.Fatal("a block discards body-deferred context")
		}
	})

	t.Run("value replacement on a failed result is contained", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			return nil, errors.New("boom")
		})
		undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
			return &PostToolDecision{Kind: PostAccept, ReplaceValue: "x", HasValue: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || !strings.Contains(result.Error.Message, "tools/post-execute cannot replace the value of a failed result") {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("both replacements is contained", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) { return "hi ada", nil })
		undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
			return &PostToolDecision{Kind: PostAccept, ReplaceValue: "x", HasValue: true, ReplaceContent: []llm.ContentBlock{{Type: "text", Text: "y"}}, HasContent: true}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || !strings.Contains(result.Error.Message, "cannot replace both value and content") {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestCancellationBeforeDispatchAndAfterBody(t *testing.T) {
	t.Run("before dispatch skips policy", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			return "hi", nil
		})
		observed := false
		undo := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
			observed = true
			return next(exec)
		})
		defer undo()
		signal, cancel := context.WithCancel(context.Background())
		cancel()
		result := runtime.Execute(&ToolExecutionInput{
			CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"}, Signal: signal,
		})
		if observed {
			t.Fatal("a pre-cancelled call must not reach policy")
		}
		if !result.IsError || result.Content[0].Text != "Error: tool call aborted before dispatch" {
			t.Fatalf("result = %+v", result)
		}
		if result.Error.Info == nil || result.Error.Info.Code != CodeToolAbortedBeforeDispatch || result.Error.Info.Name != "AbortError" {
			t.Fatalf("info = %+v", result.Error.Info)
		}
	})

	t.Run("after body converts success to ABORTED", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		signal, cancel := context.WithCancel(context.Background())
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			cancel() // the caller aborted while the body ran
			return "finished", nil
		})
		result := runtime.Execute(&ToolExecutionInput{
			CallID: "c", Name: "echo", Arguments: map[string]any{"name": "ada"}, Signal: signal,
		})
		if !result.IsError || result.Content[0].Text != "Error: tool call aborted" {
			t.Fatalf("result = %+v", result)
		}
		if result.Error.Info == nil || result.Error.Info.Code != CodeToolAborted {
			t.Fatalf("info = %+v", result.Error.Info)
		}
	})
}

func TestDeferContextAndConcludeTurn(t *testing.T) {
	runtime := newTestRuntime(t, Config{})
	makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
		exec.DeferContext(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "deferred-1"}}})
		exec.DeferContext(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "deferred-2"}}})
		exec.ConcludeTurn()
		return "hi ada", nil
	})
	undo := runtime.OnPostExecute(nil, func(exec *ToolExecution, result *ToolExecutionResult, next func(*ToolExecutionResult) *PostToolDecision) *PostToolDecision {
		decision := next(result)
		decision.AdditionalContexts = []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "policy"}}}}
		return decision
	})
	defer undo()

	result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
	if result.IsError {
		t.Fatalf("result = %+v", result)
	}
	if !result.ConcludesTurn {
		t.Fatal("a successful concluded call must surface concludesTurn")
	}
	if len(result.AdditionalContexts) != 3 ||
		result.AdditionalContexts[0].Content[0].Text != "deferred-1" ||
		result.AdditionalContexts[1].Content[0].Text != "deferred-2" ||
		result.AdditionalContexts[2].Content[0].Text != "policy" {
		t.Fatalf("contexts = %+v", result.AdditionalContexts)
	}
}

func TestExecuteWrapperNormalization(t *testing.T) {
	t.Run("wrapper-authored success re-runs the output contract", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			t.Fatal("the wrapper must replace the body")
			return "", nil
		})
		undo := runtime.OnExecute(nil, func(exec *ToolRunContext, next func(*ToolRunContext) *ToolExecutionResult) *ToolExecutionResult {
			return &ToolExecutionResult{Value: "wrapped", AdditionalContexts: []llm.Message{{Role: llm.RoleUser}}}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if result.IsError || result.Value != "wrapped" {
			t.Fatalf("result = %+v", result)
		}
		if len(result.Content) != 2 || result.Content[0].Text != "render:wrapped" {
			t.Fatalf("content = %+v", result.Content)
		}
		if len(result.AdditionalContexts) != 1 {
			t.Fatalf("contexts = %+v", result.AdditionalContexts)
		}
	})

	t.Run("wrapper-authored invalid value fails INVALID_TOOL_OUTPUT", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) { return "hi", nil })
		undo := runtime.OnExecute(nil, func(exec *ToolRunContext, next func(*ToolRunContext) *ToolExecutionResult) *ToolExecutionResult {
			return &ToolExecutionResult{Value: 42}
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if !result.IsError || result.Error.Info == nil || result.Error.Info.Code != CodeInvalidToolOutput {
			t.Fatalf("result = %+v", result)
		}
	})

	t.Run("wrapper cancellation fuses with the caller signal", func(t *testing.T) {
		runtime := newTestRuntime(t, Config{})
		bodyRan := false
		makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
			bodyRan = true
			return "hi", nil
		})
		wrapperCtx, cancelWrapper := context.WithCancel(context.Background())
		undo := runtime.OnExecute(nil, func(exec *ToolRunContext, next func(*ToolRunContext) *ToolExecutionResult) *ToolExecutionResult {
			// The wrapper replaces the wrapper-visible signal; the registry
			// fuses the original caller signal back in before the body.
			exec.Signal = wrapperCtx
			cancelWrapper() // the wrapper's view aborts before the body
			return next(exec)
		})
		defer undo()
		result := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
		if bodyRan {
			t.Fatal("an aborted wrapper signal must stop before the body")
		}
		if !result.IsError || result.Content[0].Text != "Error: tool call aborted before dispatch" {
			t.Fatalf("result = %+v", result)
		}
	})
}

func TestPtcCollapseDeniesBeforePolicyAndNestedBypasses(t *testing.T) {
	runtime := newTestRuntime(t, Config{Mode: ModePtc})
	var preSeen, guardSeen []string
	undoPre := runtime.OnPreExecute(nil, func(exec *ToolExecution, next func(*ToolExecution) *PreToolDecision) *PreToolDecision {
		preSeen = append(preSeen, exec.Name)
		return next(exec)
	})
	defer undoPre()
	undoGuard, err := runtime.Guard(func(execution *ToolExecution) (string, bool) {
		guardSeen = append(guardSeen, execution.Name)
		return "", false
	})
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	defer undoGuard()
	makeEcho(t, runtime, func(args map[string]any, exec *ToolRunContext) (any, error) {
		return "hi " + args["name"].(string), nil
	})

	direct := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
	if !direct.IsError {
		t.Fatalf("direct = %+v", direct)
	}
	want := `Error: unknown tool "echo": only ` + "`run_code`" + ` is callable directly — call ` + "`echo`" + ` from inside a ` + "`run_code`" + ` program instead`
	if direct.Content[0].Text != want {
		t.Fatalf("content = %q", direct.Content[0].Text)
	}
	if direct.Error.Info == nil || direct.Error.Info.Code != CodeUnknownTool {
		t.Fatalf("info = %+v", direct.Error.Info)
	}
	if len(preSeen) != 0 || len(guardSeen) != 0 {
		t.Fatalf("collapse must deny before policy: pre=%v guard=%v", preSeen, guardSeen)
	}

	token := &ExecutionToken{}
	nested := &ToolExecutionInput{
		CallID: "nested", Name: "echo", Arguments: map[string]any{"name": "ada"},
		Parent: token, Signal: context.Background(),
	}
	result := runtime.Execute(nested)
	if result.IsError || result.Value != "hi ada" {
		t.Fatalf("nested = %+v", result)
	}
	// A nested sub-dispatch is never a direct top-level presentation.
	if result.Meta != nil {
		t.Fatalf("nested meta = %#v", result.Meta)
	}
	directTop := runtime.Execute(runInput("echo", map[string]any{"name": "ada"}))
	_ = directTop
	runtime2 := newTestRuntime(t, Config{})
	makeEcho(t, runtime2, func(args map[string]any, exec *ToolRunContext) (any, error) {
		return "hi " + args["name"].(string), nil
	})
	topLevel := runtime2.Execute(runInput("echo", map[string]any{"name": "ada"}))
	if topLevel.IsError || topLevel.Meta == nil {
		t.Fatalf("top-level meta = %+v (%v)", topLevel.Meta, topLevel.Error)
	}
}

// ApprovalServiceFunc adapts a function to the approval seam.
type ApprovalServiceFunc func(request ApprovalRequest) ApprovalOutcome

func (fn ApprovalServiceFunc) Request(request ApprovalRequest) ApprovalOutcome {
	return fn(request)
}
