package gojaruntime

import (
	"strings"
	"testing"

	"dshgo/coderuntime"
)

func TestLanguageAndIsolation(t *testing.T) {
	r := New(Config{})
	if r.Language() != "javascript" || r.Isolation() != "in-process-goja" {
		t.Fatalf("Language=%q Isolation=%q", r.Language(), r.Isolation())
	}
}

func TestRunSimpleExpression(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, err := r.Run(coderuntime.CodeRunRequest{Program: `return 1 + 2`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Error != nil {
		t.Fatalf("error: %+v", result.Error)
	}
	if result.Value != float64(3) {
		t.Fatalf("value = %#v, want 3", result.Value)
	}
}

func TestRunStringReturn(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, _ := r.Run(coderuntime.CodeRunRequest{Program: `return "hello"`})
	if result.Value != "hello" {
		t.Fatalf("value = %#v", result.Value)
	}
}

func TestRunObjectReturn(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, _ := r.Run(coderuntime.CodeRunRequest{Program: `return {key: "value", n: 42}`})
	obj, ok := result.Value.(map[string]any)
	if !ok || obj["key"] != "value" {
		t.Fatalf("value = %#v", result.Value)
	}
}

func TestRunSyntaxErrorIsFailureField(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, err := r.Run(coderuntime.CodeRunRequest{Program: `this is not valid js !!!`})
	if err != nil {
		t.Fatalf("run must not error for a bad program: %v", err)
	}
	if result.Error == nil || result.Error.Kind != coderuntime.FailureException {
		t.Fatalf("error = %+v, want exception", result.Error)
	}
}

func TestCloseRejectsRun(t *testing.T) {
	r := New(Config{})
	r.Close()
	_, err := r.Run(coderuntime.CodeRunRequest{Program: `return 1`})
	if err == nil {
		t.Fatal("closed runtime accepted a run")
	}
}

// Promise resolution: async-wrapped programs return *goja.Promise — the
// settled result must be extracted, not the Promise object itself.
func TestRunAsyncReturnValue(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, err := r.Run(coderuntime.CodeRunRequest{Program: `return await Promise.resolve(42)`})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Error != nil {
		t.Fatalf("error: %+v", result.Error)
	}
	if result.Value != float64(42) {
		t.Fatalf("value = %#v, want 42 (Promise must resolve to its settled result)", result.Value)
	}
}

// Async-body throw: the promise rejects — must surface as FailureException,
// not a silently empty result.
func TestRunAsyncThrowIsFailureException(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, err := r.Run(coderuntime.CodeRunRequest{Program: `throw new Error("boom")`})
	if err != nil {
		t.Fatalf("run must not error: %v", err)
	}
	if result.Error == nil || result.Error.Kind != coderuntime.FailureException {
		t.Fatalf("error = %+v, want exception (async throw must not be silently swallowed)", result.Error)
	}
	if !strings.Contains(result.Error.Message, "boom") {
		t.Fatalf("message = %q, want it to contain the throw reason", result.Error.Message)
	}
}

// Multi-await composition: sequential promises must chain correctly.
func TestRunMultiAwait(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	result, _ := r.Run(coderuntime.CodeRunRequest{Program: `
		const a = await Promise.resolve(10);
		const b = await Promise.resolve(20);
		return a + b;
	`})
	if result.Value != float64(30) {
		t.Fatalf("value = %#v, want 30", result.Value)
	}
}
