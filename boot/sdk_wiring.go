// SDK route adapters: bridge the composed boot services to the faces
// sdk/server expects (AgentFactory, LLMRouter). Each adapter is a thin
// delegate — the business logic lives in the wrapped service.
package boot

import (
	"context"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/llm"
	"dshgo/session"
)

// sdkAgentFactory creates and disposes agents through the composed
// registry, resolving the default model at construction time.
type sdkAgentFactory struct {
	registry     *agent.AgentRegistry
	store        *session.Store
	defaultModel *agentdefaultmodel.Config
}

func (f *sdkAgentFactory) Create(sessionID string, opts sdkCreateOptions) (*agent.Agent, error) {
	selection := f.defaultModel.CurrentSelection()
	provider := opts.Provider
	if provider == "" {
		provider = selection.Provider
	}
	model := opts.Model
	if model == "" {
		model = selection.Model
	}
	handle, err := f.registry.Create(context.Background(), agent.CreateAgentOptions{
		SessionID: session.SessionID(sessionID),
		Meta:      agent.CreateAgentMeta{CWD: opts.CWD},
		AgentOptions: agent.AgentOptions{
			Provider:        provider,
			Model:           model,
			ReasoningEffort: llm.ReasoningEffortID(opts.ReasoningEffort),
		},
	})
	if err != nil {
		return nil, err
	}
	return handle.Agent, nil
}

func (f *sdkAgentFactory) Dispose(a *agent.Agent) error {
	a.Cancel(session.TurnEndCancelCause{Kind: session.CancelDisposed}, agent.CancelOptions{})
	return nil
}

// sdkCreateOptions mirrors the server's CreateAgentOptions to avoid a
// circular import.
type sdkCreateOptions struct {
	CWD             string
	Provider        string
	Model           string
	ReasoningEffort string
	MaxTokens       int64
}

// sdkLLMRouter wraps the llm.Runtime for the sdk/server's HasAdapter /
// MountDefault / ResolveCallConfig face.
type sdkLLMRouter struct {
	runtime *llm.Runtime
}

func (r *sdkLLMRouter) HasAdapter(provider string) bool {
	for _, info := range r.runtime.ListProviders() {
		if info.ID == provider {
			return true
		}
	}
	return false
}

func (r *sdkLLMRouter) MountDefault() (func(), error) {
	return func() {}, nil
}

func (r *sdkLLMRouter) ResolveCallConfig(provider, model, reasoningEffort string, maxTokens int64) error {
	_, err := r.runtime.ResolveModelInfo(provider, model)
	return err
}
