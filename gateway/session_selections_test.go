// Tests for session/selectModel: the override registry records the
// selection and the prompt route gate reads it first.
package gateway

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// The override lands in the registry and the prompt route gate reads it
// first; the echo carries the full selection.
func TestSelectModelRecordsOverride(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)

	value, err := controller.SelectModel(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"provider":  "deepseek-official",
		"model":     "other-model",
	})
	if err != nil {
		t.Fatalf("selectModel: %v", err)
	}
	echo := value.(map[string]any)
	if echo["accepted"] != true || echo["provider"] != "deepseek-official" || echo["model"] != "other-model" {
		t.Fatalf("echo = %#v", echo)
	}
	live := controller.liveAgent(sessionID)
	selection := controller.selectionFor(live)
	if selection.Provider != "deepseek-official" || selection.Model != "other-model" {
		t.Fatalf("selection = %#v, want the override", selection)
	}
}

// The selection echo carries an explicit reasoning effort.
func TestSelectModelEchoesReasoningEffort(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	value, err := controller.SelectModel(context.Background(), map[string]any{
		"sessionId": string(sessionID), "provider": "deepseek-official", "model": "m",
		"reasoningEffort": "high",
	})
	if err != nil {
		t.Fatalf("selectModel: %v", err)
	}
	echo := value.(map[string]any)
	if echo["reasoningEffort"] != "high" {
		t.Fatalf("echo = %#v", echo)
	}
	live := controller.liveAgent(sessionID)
	selection := controller.selectionFor(live)
	if !selection.HasReasoningEffort || selection.ReasoningEffort != "high" {
		t.Fatalf("selection = %#v", selection)
	}
}

// A cold session answers session/not-found.
func TestSelectModelAnswersNotFoundForColdSession(t *testing.T) {
	controller, _, _ := newPromptController(t, &promptFakeStore{})
	_, err := controller.SelectModel(context.Background(), map[string]any{
		"sessionId": "session-missing", "provider": "p", "model": "m",
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/not-found" {
		t.Fatalf("err = %v, want session/not-found", err)
	}
}

// Incomplete selections refuse as arguments failures.
func TestSelectModelRequiresProviderAndModel(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	_, err := controller.SelectModel(context.Background(), map[string]any{
		"sessionId": string(sessionID), "provider": "p",
	})
	if err == nil || !strings.Contains(err.Error(), "provider and model") {
		t.Fatalf("err = %v, want the provider+model refusal", err)
	}
}

// The workspace fence for the desktop handoff: inside passes, outside and
// traversal fail.
func TestPathWithinFence(t *testing.T) {
	root := `C:\work\project`
	if !pathWithin(root, `C:\work\project\src\main.go`) {
		t.Fatal("an inside path must pass the fence")
	}
	if pathWithin(root, `C:\work\other\main.go`) {
		t.Fatal("an outside path must fail the fence")
	}
	if pathWithin(filepath.Join(root), filepath.Join(root, "..", "escape.txt")) {
		t.Fatal("a traversal path must fail the fence")
	}
}
