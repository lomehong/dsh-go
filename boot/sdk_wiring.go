// SDK route adapters: bridge the composed boot services to the faces
// sdk/server expects (AgentFactory, LLMRouter). Each adapter is a thin
// delegate — the business logic lives in the wrapped service.
package boot

import (
	"context"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/llm"
	sdkServer "dshgo/sdk/server"
	"dshgo/session"
)

// sdkAgentFactory creates and disposes agents through the composed
// registry, resolving the default model at construction time. Implements
// sdk/server's AgentFactory face.
type sdkAgentFactory struct {
	registry     *agent.AgentRegistry
	store        *session.Store
	defaultModel *agentdefaultmodel.Config
}

func (f *sdkAgentFactory) Create(sessionID string, options sdkServer.CreateAgentOptions) (*agent.Agent, error) {
	selection := f.defaultModel.CurrentSelection()
	provider := options.Provider
	if provider == "" {
		provider = selection.Provider
	}
	model := options.Model
	if model == "" {
		model = selection.Model
	}
	handle, err := f.registry.Create(context.Background(), agent.CreateAgentOptions{
		SessionID: session.SessionID(sessionID),
		Meta:      agent.CreateAgentMeta{CWD: options.Cwd},
		AgentOptions: agent.AgentOptions{
			Provider:        provider,
			Model:           model,
			ReasoningEffort: llm.ReasoningEffortID(options.ReasoningEffort),
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
