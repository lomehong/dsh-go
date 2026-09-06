package gojaruntime

import (
	"testing"

	"dshgo/coderuntime"
)

func TestLanguageAndIsolation(t *testing.T) {
	r := New(Config{})
	if r.Language() != "typescript" || r.Isolation() != "in-process-goja" {
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
