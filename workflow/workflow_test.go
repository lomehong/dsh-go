package workflow

import (
	"strings"
	"testing"
)

func TestValidateMetaAcceptsAndNormalizes(t *testing.T) {
	meta, err := ValidateMeta(map[string]any{
		"name":        "audit-deps",
		"description": "Audit dependencies.",
		"whenToUse":   "Quarterly",
		"phases": []any{
			map[string]any{"title": "scan", "detail": "Scan the tree", "provider": "deepseek"},
			map[string]any{"title": "report"},
		},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if meta.Name != "audit-deps" || meta.Description != "Audit dependencies." || meta.WhenToUse != "Quarterly" {
		t.Fatalf("meta = %+v", meta)
	}
	if len(meta.Phases) != 2 || meta.Phases[0].Title != "scan" || meta.Phases[0].Detail != "Scan the tree" || meta.Phases[0].Provider != "deepseek" {
		t.Fatalf("phases = %+v", meta.Phases)
	}
	if meta.Phases[1].Detail != "" {
		t.Fatalf("absent detail = %+v", meta.Phases[1])
	}
}

func TestValidateMetaNamesEveryViolation(t *testing.T) {
	_, err := ValidateMeta(map[string]any{
		"bogus":     1,
		"name":      5,
		"whenToUse": 5,
		"phases":    "nope",
	})
	var workflowErr WorkflowError
	if !asWorkflowError(err, &workflowErr) || workflowErr.Code() != CodeMetaInvalid {
		t.Fatalf("err = %v, want META_INVALID", err)
	}
	message := workflowErr.Error()
	for _, needle := range []string{
		"meta.bogus is not a recognized field (name/description/whenToUse/phases)",
		"meta.name must be a non-empty string",
		"meta.description must be a non-empty string",
		"meta.whenToUse must be a string",
		"meta.phases must be an array",
	} {
		if !strings.Contains(message, needle) {
			t.Fatalf("message %q missing %q", message, needle)
		}
	}
}

func TestValidateMetaPhaseViolations(t *testing.T) {
	_, err := ValidateMeta(map[string]any{
		"name":        "w",
		"description": "d",
		"phases": []any{
			map[string]any{"title": "", "extra": 1, "detail": 5, "model": 5},
			"not-an-object",
		},
	})
	var workflowErr WorkflowError
	if !asWorkflowError(err, &workflowErr) || workflowErr.Code() != CodeMetaInvalid {
		t.Fatalf("err = %v, want META_INVALID", err)
	}
	message := workflowErr.Error()
	for _, needle := range []string{
		"meta.phases[0].extra is not a recognized field",
		"meta.phases[0].title must be a non-empty string",
		"meta.phases[0].detail must be a string",
		"meta.phases[0].model must be a string",
		"meta.phases[1] must be an object",
	} {
		if !strings.Contains(message, needle) {
			t.Fatalf("message %q missing %q", message, needle)
		}
	}
}

func TestValidateMetaJSON(t *testing.T) {
	meta, err := ValidateMetaJSON([]byte(`{"name":"w","description":"d"}`))
	if err != nil || meta.Name != "w" {
		t.Fatalf("meta = %+v, %v", meta, err)
	}
	if _, err := ValidateMetaJSON([]byte(`"banana"`)); err == nil {
		t.Fatal("expected the scalar document to be rejected")
	}
	if _, err := ValidateMetaJSON([]byte(`{"name":"","description":"d"}`)); err == nil {
		t.Fatal("expected the empty name to be rejected")
	}
}

func TestValidateMetaDoesNotAliasCaller(t *testing.T) {
	caller := map[string]any{"name": "w", "description": "d"}
	meta, err := ValidateMeta(caller)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	// Mutating the returned copy never reaches the caller's map (the copy is
	// built from validated scalar fields).
	meta.Name = "mutated"
	if caller["name"] != "w" {
		t.Fatal("the normalized meta aliased the caller's object")
	}
}

func TestWorkflowErrorFatalFlag(t *testing.T) {
	fatal := NewWorkflowError("bad option", CodeInvalidArgument, nil, nil)
	if !IsFatalWorkflowError(fatal) {
		t.Fatal("workflow errors default to fatal")
	}
	nonFatal := false
	notFatal := NewWorkflowError("child failed", CodeAgentResult, nil, &nonFatal)
	if IsFatalWorkflowError(notFatal) {
		t.Fatal("the explicit flag must win")
	}
	// Foreign errors are ordinary in-stage script errors: combinators map
	// them to a per-item nil rather than re-throwing.
	if IsFatalWorkflowError(plainError("random")) {
		t.Fatal("foreign errors must not be fatal")
	}
}

type plainError string

func (e plainError) Error() string { return string(e) }

func TestEmitWorkflowEventContainsListenerFailures(t *testing.T) {
	logger := &collectLogger{}
	sink := NewEventSink(logger)
	observed := []string{}
	// First listener panics, second observes: the panic is contained and the
	// remaining listeners still run.
	sink.On(EventWorkflowPhase, func(payload any) {
		panic("listener exploded")
	})
	sink.On(EventWorkflowPhase, func(payload any) {
		observed = append(observed, payload.(PhasePayload).Title)
	})
	sink.Emit(EventWorkflowPhase, PhasePayload{Info: WorkflowRunInfo{ID: "r1"}, Title: "scan"})
	if len(observed) != 1 || observed[0] != "scan" {
		t.Fatalf("observed = %v, want the panic contained with the chain running", observed)
	}
	if len(logger.warnings) != 1 ||
		!strings.Contains(logger.warnings[0], "workflow: workflow/phase listener threw: listener exploded") {
		t.Fatalf("warnings = %v", logger.warnings)
	}
}

func TestEmitWorkflowEventWithoutListenersIsQuiet(t *testing.T) {
	logger := &collectLogger{}
	sink := NewEventSink(logger)
	sink.Emit(EventWorkflowLog, LogPayload{Info: WorkflowRunInfo{ID: "r1"}, Message: "hello"})
	if len(logger.warnings) != 0 {
		t.Fatalf("warnings = %v", logger.warnings)
	}
}

func TestEventSinkUndoStopsDelivery(t *testing.T) {
	sink := NewEventSink(nil)
	observed := 0
	undo := sink.On(EventWorkflowLog, func(payload any) { observed++ })
	undo()
	undo() // idempotent
	sink.Emit(EventWorkflowLog, LogPayload{Info: WorkflowRunInfo{ID: "r1"}, Message: "hello"})
	if observed != 0 {
		t.Fatalf("observed = %d, want no delivery after undo", observed)
	}
}

// collectLogger captures sink warnings.
type collectLogger struct {
	warnings []string
}

func (l *collectLogger) Warn(message string) {
	l.warnings = append(l.warnings, message)
}
