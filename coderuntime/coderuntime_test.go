package coderuntime

import (
	"context"
	"strings"
	"testing"

	"dshgo/cordis"
)

// stubRuntime records requests, "executes" by invoking every binding once in
// declaration order, and lets tests script the outcome — the smallest
// implementation that honors the seam contract.
type stubRuntime struct {
	requests []CodeRunRequest
	next     CodeRunResult
	calls    []CodeJSONValue
}

func (s *stubRuntime) Language() string  { return "typescript" }
func (s *stubRuntime) Isolation() string { return "in-process-stub" }
func (s *stubRuntime) Close() error      { return nil }

func (s *stubRuntime) Run(request CodeRunRequest) (CodeRunResult, error) {
	s.requests = append(s.requests, request)
	if request.Signal != nil && request.Signal.Err() != nil {
		return CodeRunResult{Error: &CodeRunFailure{Kind: FailureAbort, Message: "cancelled"}}, nil
	}
	for _, namespace := range request.Bindings {
		for _, fn := range namespace.Functions {
			value, err := fn(CodeJSONValue(map[string]any{"from": "stub"}))
			if err != nil {
				return CodeRunResult{Logs: []string{"boom"}, Error: &CodeRunFailure{Kind: FailureException, Message: err.Error()}}, nil
			}
			s.calls = append(s.calls, value)
		}
	}
	return s.next, nil
}

func TestServiceSeamRegistersAndServes(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	runtime := &stubRuntime{}
	ContextService.Provide(root, runtime)
	resolved, ok := ContextService.From(root)
	if !ok || resolved == nil {
		t.Fatal("codeRuntime service must resolve from the context")
	}
	if resolved.Language() != "typescript" || resolved.Isolation() != "in-process-stub" {
		t.Fatalf("language/isolation = %s/%s", resolved.Language(), resolved.Isolation())
	}

	calls := 0
	probe := func(args CodeJSONValue) (CodeJSONValue, error) {
		calls++
		return nil, nil
	}
	result, err := resolved.Run(CodeRunRequest{
		Program:  "return 1",
		Bindings: []CodeBindingNamespace{{Global: "tools", Functions: map[string]CodeBindingFunction{"probe": probe}}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Error != nil || len(result.Logs) != 0 {
		t.Fatalf("plain run must resolve without error, got %+v", result)
	}
	if calls != 1 {
		t.Fatalf("binding must be invoked once, got %d", calls)
	}
}

func TestFailedRunIsAFieldNotARejection(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	runtime := &stubRuntime{next: CodeRunResult{Logs: []string{"boom"}, Error: &CodeRunFailure{Kind: FailureException, Message: "boom"}}}
	ContextService.Provide(root, runtime)
	resolved, _ := ContextService.From(root)
	result, err := resolved.Run(CodeRunRequest{Program: `throw new Error("boom")`})
	if err != nil {
		t.Fatalf("failed run must not reject: %v", err)
	}
	if result.Error == nil || result.Error.Kind != FailureException || result.Error.Message != "boom" {
		t.Fatalf("failure field = %+v", result.Error)
	}
	if result.Value != nil {
		t.Fatalf("failed run must leave value absent, got %v", result.Value)
	}
}

func TestPreAbortedSignalReportsAbort(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	runtime := &stubRuntime{}
	ContextService.Provide(root, runtime)
	resolved, _ := ContextService.From(root)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := resolved.Run(CodeRunRequest{Program: "return 1", Signal: signal})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Error == nil || result.Error.Kind != FailureAbort {
		t.Fatalf("pre-aborted signal must report abort, got %+v", result.Error)
	}
}

func TestValidateBindingsRejections(t *testing.T) {
	probe := func(CodeJSONValue) (CodeJSONValue, error) { return nil, nil }
	cases := []struct {
		name    string
		binding CodeBindingNamespace
		wantSub string
	}{
		{"bad identifier", CodeBindingNamespace{Global: "my-tools", Functions: map[string]CodeBindingFunction{"p": probe}}, "not a usable identifier"},
		{"leading digit", CodeBindingNamespace{Global: "1tools", Functions: map[string]CodeBindingFunction{"p": probe}}, "not a usable identifier"},
		{"reserved word", CodeBindingNamespace{Global: "lambda", Functions: map[string]CodeBindingFunction{"p": probe}}, "not a usable identifier"},
		{"reserved global console", CodeBindingNamespace{Global: "console", Functions: map[string]CodeBindingFunction{"p": probe}}, "reserved binding global"},
		{"reserved global dunder name", CodeBindingNamespace{Global: "__name__", Functions: map[string]CodeBindingFunction{"p": probe}}, "reserved binding global"},
		{"error class reserved word", CodeBindingNamespace{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}, ErrorClass: &CodeBindingErrorClass{Name: "class", MemberNameProperty: "member"}}, "not a usable identifier"},
		{"error class clashes namespace", CodeBindingNamespace{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}, ErrorClass: &CodeBindingErrorClass{Name: "tools", MemberNameProperty: "member"}}, "duplicate injected global"},
		{"member empty", CodeBindingNamespace{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}, ErrorClass: &CodeBindingErrorClass{Name: "ToolsError"}}, "not usable"},
		{"member reserved", CodeBindingNamespace{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}, ErrorClass: &CodeBindingErrorClass{Name: "ToolsError", MemberNameProperty: "name"}}, "not usable"},
		{"member dunder", CodeBindingNamespace{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}, ErrorClass: &CodeBindingErrorClass{Name: "ToolsError", MemberNameProperty: "__proto__"}}, "not usable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := CodeRunRequest{Bindings: []CodeBindingNamespace{tc.binding}}
			err := ValidateBindings(request)
			if err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q must contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateBindingsDuplicateGlobalsAcrossNamespaces(t *testing.T) {
	probe := func(CodeJSONValue) (CodeJSONValue, error) { return nil, nil }
	request := CodeRunRequest{Bindings: []CodeBindingNamespace{
		{Global: "tools", Functions: map[string]CodeBindingFunction{"p": probe}},
		{Global: "tools", Functions: map[string]CodeBindingFunction{"q": probe}},
	}}
	if err := ValidateBindings(request); err == nil || !strings.Contains(err.Error(), "duplicate binding global") {
		t.Fatalf("duplicate globals must be rejected, got %v", err)
	}
}

func TestValidateBindingsAcceptsPortableNamespace(t *testing.T) {
	probe := func(CodeJSONValue) (CodeJSONValue, error) { return nil, nil }
	request := CodeRunRequest{Bindings: []CodeBindingNamespace{
		{Global: "tools", Functions: map[string]CodeBindingFunction{"probe": probe, "__proto__": probe}, ErrorClass: &CodeBindingErrorClass{Name: "ToolsError", MemberNameProperty: "memberName"}},
	}}
	if err := ValidateBindings(request); err != nil {
		t.Fatalf("portable namespace must validate: %v", err)
	}
}
