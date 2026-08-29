// Agent-scoped model selection shared by runtime entry points. Port of
// packages/core/agent/src/model-selection.ts.
package agent

import (
	"dshgo/llm"
	"dshgo/systemprompt"
)

// ModelSelection is the complete provider, model, and optional reasoning
// effort selected for one live Agent.
type ModelSelection struct {
	// Provider is the registered provider route.
	Provider string
	// Model is the provider-owned model id.
	Model string
	// ReasoningEffort is the adapter-owned reasoning effort, or
	// provider/default behavior when empty.
	ReasoningEffort llm.ReasoningEffortID
	// HasReasoningEffort distinguishes an absent effort (clears any
	// inherited effort) from an explicit value.
	HasReasoningEffort bool
}

// ModelSelectionRef is the mutable model selection plus the value captured
// for the current step. The loop drives it from a single goroutine; the
// installed listeners read it on that goroutine's dispatch path.
type ModelSelectionRef struct {
	// Current is the model selected for the next step that enters prompt
	// assembly.
	Current *ModelSelection
	// Assembled is the selection captured when the current step entered
	// prompt assembly.
	Assembled *ModelSelection
}

// InstallModelSelection couples one mutable selection to Agent-scoped prompt
// assembly and request routing. Prompt assembly snapshots the selected model
// before delegating, then applies its provider/model pair and effort to
// request config so a concurrent switch takes effect on a later step instead
// of splitting the two surfaces. An absent selected effort clears any
// inherited effort, restoring the selected model's provider/default behavior.
// Returns the disposer for both registrations.
func InstallModelSelection(
	prompt *systemprompt.SystemPrompt,
	events *SubjectEventBus,
	agentScope systemprompt.ScopeKey,
	selection *ModelSelectionRef,
) func() {
	disposeAssembly := prompt.OnAssemble(agentScope, func(assembly *systemprompt.PromptAssembly, assembleContext systemprompt.AssembleContext, next func() *systemprompt.PromptAssembly) *systemprompt.PromptAssembly {
		selected := selection.Current
		assembled := next()
		selection.Assembled = selected
		if selected == nil {
			return assembled
		}
		variables := assembled.Variables.Clone()
		provider := selected.Provider
		model := selected.Model
		variables.Set("provider", &provider)
		variables.Set("model", &model)
		return &systemprompt.PromptAssembly{
			Sections:  assembled.Sections,
			Contexts:  assembled.Contexts,
			Tools:     assembled.Tools,
			Variables: variables,
		}
	})
	disposeRequest := events.Request().On(agentScope, func(payload RequestPayload, next func(RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		resolved := next(payload)
		if resolved == nil {
			return nil
		}
		selected := selection.Assembled
		if selected == nil {
			return resolved
		}
		// The selected effort replaces any inherited one; absence clears it,
		// restoring the selected model's provider/default behavior.
		out := *resolved
		out.Provider = selected.Provider
		out.Model = selected.Model
		out.ReasoningEffort = ""
		if selected.HasReasoningEffort {
			out.ReasoningEffort = selected.ReasoningEffort
		}
		return &out
	})
	return func() {
		disposeAssembly()
		disposeRequest()
	}
}
