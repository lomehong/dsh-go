package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/session"
)

// TestAdmissionErrorClassification pins the official two-way branch: a
// caller-correctable ImageAdmissionError settles as a gentle error result
// (no throw); any other admission failure settles thrown and propagates.
func TestAdmissionErrorClassification(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, CommandDefinition{Name: "visual", Description: "d", Handler: func(Invocation) (CommandResult, error) {
		t.Fatal("the handler must not run after a failed admission")
		return CommandResult{}, nil
	}, Input: &CommandInputDescriptor{Hint: "describe", Images: true}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-admission")

	// Gentle branch: the caller can correct the image batch.
	runtime.SetImageAdmitter(func(images []any) ([]ImageAttachment, error) {
		return nil, &ImageAdmissionError{Message: "too many images: 9 > 8", Code: AdmissionTooManyImages}
	})
	execution, err := runtime.Execute(context.Background(), nil, sess, "/visual", []any{"img"})
	if err != nil || execution == nil {
		t.Fatalf("admission error must settle, not throw: %v %v", execution, err)
	}
	if execution.Result.Kind != ResultError || execution.Result.Text != "too many images: 9 > 8" {
		t.Fatalf("result = %+v, want the gentle message result", execution.Result)
	}
	if !strings.Contains(lastDoneKind(t, sess), ResultError) {
		t.Fatal("the gentle branch must still settle command/done")
	}

	// Thrown branch: a storage fault is a runtime failure.
	runtime.SetImageAdmitter(func(images []any) ([]ImageAttachment, error) {
		return nil, errors.New("attachment store unavailable")
	})
	sess2 := newSession(t, "cmd-admission-fault")
	if _, err := runtime.Execute(context.Background(), nil, sess2, "/visual", []any{"img"}); err == nil {
		t.Fatal("a non-admission failure must propagate loud")
	}
	if !strings.Contains(lastDoneKind(t, sess2), ResultError) {
		t.Fatal("the thrown branch must settle command/done as error")
	}
}

// lastDoneKind reads the latest command/done settlement's kind.
func lastDoneKind(t *testing.T, sess *session.Session) string {
	t.Helper()
	events := sess.Events()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == EventCommandDone {
			var done CommandDoneData
			if err := json.Unmarshal(events[i].Data, &done); err != nil {
				t.Fatalf("decode done: %v", err)
			}
			return done.Kind
		}
	}
	return ""
}
