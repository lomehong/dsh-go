package agentdefaultmodel

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/settings"
)

func TestNewValidatesEntry(t *testing.T) {
	if _, err := New(Settings{Provider: "", Model: "m"}); err == nil || !strings.Contains(err.Error(), "provider must be a non-empty string") {
		t.Fatalf("empty provider: %v", err)
	}
	if _, err := New(Settings{Provider: "p", Model: ""}); err == nil || !strings.Contains(err.Error(), "model must be a non-empty string") {
		t.Fatalf("empty model: %v", err)
	}
}

func TestCurrentSelectionFromCompositionEntry(t *testing.T) {
	config, err := New(Settings{Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	selection := config.CurrentSelection()
	if selection.Provider != "deepseek" || selection.Model != "deepseek-chat" || selection.HasReasoningEffort {
		t.Fatalf("selection: %+v", selection)
	}
	config, err = New(Settings{Provider: "deepseek", Model: "deepseek-reasoner", ReasoningEffort: "high"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	selection = config.CurrentSelection()
	if !selection.HasReasoningEffort || selection.ReasoningEffort != llm.ReasoningEffortID("high") {
		t.Fatalf("effort: %+v", selection)
	}
}

func TestRegisterSectionLiveUserLayer(t *testing.T) {
	store := settings.NewStore(cordis.Discard{})
	config, scope, err := RegisterSection(store, Settings{Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	_ = scope
	// The composition default stands until the user layer speaks.
	selection := config.CurrentSelection()
	if selection.Model != "deepseek-chat" {
		t.Fatalf("default selection: %+v", selection)
	}
	// The user layer is read live.
	if err := store.ProviderPush(SettingsNamespace, map[string]any{"provider": "deepseek", "model": "deepseek-reasoner", "reasoningEffort": "low"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	selection = config.CurrentSelection()
	if selection.Model != "deepseek-reasoner" || !selection.HasReasoningEffort || selection.ReasoningEffort != llm.ReasoningEffortID("low") {
		t.Fatalf("user-layer selection: %+v", selection)
	}
	// Save writes through the section scope; the read-back reflects it.
	if err := config.SaveSelection(store, scope, agent.ModelSelection{Provider: "deepseek", Model: "deepseek-chat"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	selection = config.CurrentSelection()
	if selection.Model != "deepseek-chat" || selection.HasReasoningEffort {
		t.Fatalf("after save: %+v", selection)
	}
}

func TestParseSectionValidates(t *testing.T) {
	if _, err := ParseSection(map[string]any{"provider": "p"}); err == nil || !strings.Contains(err.Error(), "model must be a non-empty string") {
		t.Fatalf("missing model: %v", err)
	}
	if _, err := ParseSection(map[string]any{"provider": "p", "model": "m", "reasoningEffort": 3}); err == nil {
		t.Fatal("non-string effort accepted")
	}
	parsed, err := ParseSection(map[string]any{"provider": "p", "model": "m", "reasoningEffort": "low"})
	if err != nil || parsed.Provider != "p" || parsed.ReasoningEffort != "low" {
		t.Fatalf("parsed: %+v err %v", parsed, err)
	}
}
