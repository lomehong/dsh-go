package subagent

import (
	"iter"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// imageGateAdapter is a minimal LLM adapter whose ResolveModel reports a
// configurable input modality set (text-only or image-capable).
type imageGateAdapter struct {
	modalities []string
}

func (a *imageGateAdapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	return func(yield func(llm.StreamChunk) bool) {}
}

func (a *imageGateAdapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: provider}
}

func (a *imageGateAdapter) ProviderRetryPolicy(string) *llm.ResolvedRetryPolicy { return nil }

func (a *imageGateAdapter) ListModels(string) ([]llm.LlmModelInfo, error) { return nil, nil }

func (a *imageGateAdapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	return llm.LlmResolvedModelInfo{
		LlmModelInfo: llm.LlmModelInfo{
			Provider:        provider,
			ID:              model,
			Name:            model,
			InputModalities: a.modalities,
		},
	}, nil
}

// imageGateManager builds a continuation manager over a child whose Agent
// options carry the given provider/model, wired to a fake LLM runtime.
func imageGateManager(t *testing.T, modalities []string) (*SubagentContinuationManager, *agent.Agent) {
	t.Helper()
	testingT = t
	parent, _ := newManagedAgent(t, "image-parent", "")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	llmRuntime := llm.NewRuntime()
	if _, err := llmRuntime.RegisterAdapter([]string{"test-image"}, &imageGateAdapter{modalities: modalities}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	manager := NewSubagentContinuationManager(ManagerDeps{Logger: cordis.Discard{}, Agents: registry, Setup: nil})
	manager.SetManagerExt(ManagerExt{
		Host:      managerHost{&SubagentRuntime{}},
		Snapshots: &stubSnapshots{},
		LLM:       llmRuntime,
	})
	return manager, parent
}

func imageContent() []llm.ContentBlock {
	return []llm.ContentBlock{{Type: llm.BlockImage, Attachment: "att-1"}}
}

func textContent() []llm.ContentBlock {
	return []llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}
}

func TestB4ImageFollowupRefusedForTextOnlyModel(t *testing.T) {
	manager, parent := imageGateManager(t, []string{"text"})
	child, _ := newManagedAgent(t, "image-child", string(parent.ID))
	child.Options = agent.AgentOptions{Provider: "test-image", Model: "text-model"}
	activation := &Activation{ChildID: child.ID, Handle: agent.AgentHandle{Agent: child}}
	ext := manager.extSnapshot()
	if err := manager.assertImageCapable(activation, imageContent()); err == nil {
		t.Fatal("image follow-up to a text-only model must be refused")
	} else {
		code := errorCodeOf(err)
		if code != CodeModelDoesNotSupportImages {
			t.Fatalf("code = %q, want %q (%v)", code, CodeModelDoesNotSupportImages, err)
		}
		if !strings.Contains(err.Error(), `Model "text-model" does not support image input.`) {
			t.Fatalf("message = %q", err.Error())
		}
	}
	_ = ext
}

func TestB4ImageFollowupAllowedForImageCapableModel(t *testing.T) {
	manager, parent := imageGateManager(t, []string{"text", "image"})
	child, _ := newManagedAgent(t, "image-child", string(parent.ID))
	child.Options = agent.AgentOptions{Provider: "test-image", Model: "vision-model"}
	activation := &Activation{ChildID: child.ID, Handle: agent.AgentHandle{Agent: child}}
	if err := manager.assertImageCapable(activation, imageContent()); err != nil {
		t.Fatalf("image follow-up to an image-capable model must pass: %v", err)
	}
}

func TestB4ImageFollowupWithoutLLMRegistryDefers(t *testing.T) {
	manager, parent := imageGateManager(t, nil)
	manager.SetManagerExt(ManagerExt{Host: managerHost{&SubagentRuntime{}}, Snapshots: &stubSnapshots{}})
	child, _ := newManagedAgent(t, "image-child", string(parent.ID))
	child.Options = agent.AgentOptions{Provider: "test-image", Model: "vision-model"}
	activation := &Activation{ChildID: child.ID, Handle: agent.AgentHandle{Agent: child}}
	if err := manager.assertImageCapable(activation, imageContent()); err != nil {
		t.Fatalf("image follow-up without an LLM registry must defer (nil gate): %v", err)
	}
}

func TestB4TextFollowupSkipsImageGate(t *testing.T) {
	manager, parent := imageGateManager(t, []string{"text"})
	child, _ := newManagedAgent(t, "image-child", string(parent.ID))
	child.Options = agent.AgentOptions{Provider: "test-image", Model: "text-model"}
	activation := &Activation{ChildID: child.ID, Handle: agent.AgentHandle{Agent: child}}
	if err := manager.assertImageCapable(activation, textContent()); err != nil {
		t.Fatalf("text-only follow-up must skip the image gate: %v", err)
	}
}

func TestB4ContentHasImage(t *testing.T) {
	if !contentHasImage(imageContent()) {
		t.Fatal("image content must report has-image")
	}
	if contentHasImage(textContent()) {
		t.Fatal("text content must report no image")
	}
	if contentHasImage(nil) {
		t.Fatal("empty content must report no image")
	}
}

func errorCodeOf(err error) string {
	if se, ok := err.(SubagentError); ok {
		return se.Code()
	}
	return ""
}

var _ = session.SessionID("")
