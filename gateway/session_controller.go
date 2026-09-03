package gateway

import (
	"context"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/llm"
	"dshgo/typert"
)

// SessionController hosts the session Remote namespace pieces the Go web
// surface serves today (official dsh-api-session-controller): the model
// catalog the conversation model selector renders.
type SessionController struct {
	llmLookup     func() any
	defaultLookup func() any
}

// NewSessionController builds the namespace host. Both lookups resolve per
// call — a nil llm runtime answers an honest empty catalog, a nil default
// selection omits the default field.
func NewSessionController(llmLookup func() any, defaultLookup func() any) *SessionController {
	if llmLookup == nil {
		llmLookup = func() any { return nil }
	}
	if defaultLookup == nil {
		defaultLookup = func() any { return nil }
	}
	return &SessionController{llmLookup: llmLookup, defaultLookup: defaultLookup}
}

// runtime resolves the composed llm runtime, or nil when absent.
func (c *SessionController) runtime() *llm.Runtime {
	if rt, ok := c.llmLookup().(*llm.Runtime); ok && rt != nil {
		return rt
	}
	return nil
}

// defaultSelection resolves the deployment's default model selection, or the
// zero selection when no default-model service is composed.
func (c *SessionController) defaultSelection() agent.ModelSelection {
	if cfg, ok := c.defaultLookup().(*agentdefaultmodel.Config); ok && cfg != nil {
		return cfg.CurrentSelection()
	}
	return agent.ModelSelection{}
}

// ModelCatalog assembles the conversation model catalog (official
// session/modelCatalog, buildModelCatalog): the deployment's default
// selection, the routable provider ids, one group per provider carrying each
// model's reasoning efforts, and a failure row per provider whose catalog
// could not be read.
func (c *SessionController) ModelCatalog(ctx context.Context) (any, error) {
	catalog := map[string]any{
		"routableProviders": []any{},
		"groups":            []any{},
		"failures":          []any{},
	}
	rt := c.runtime()
	if rt == nil {
		return catalog, nil
	}
	providers := rt.ListProviders()
	routable := make([]any, 0, len(providers))
	groups := make([]any, 0, len(providers))
	failures := make([]any, 0, len(providers))
	for _, provider := range providers {
		routable = append(routable, provider.ID)
		models, err := rt.ListModels(provider.ID)
		if err != nil {
			failures = append(failures, map[string]any{
				"id": provider.ID, "name": provider.Name, "message": err.Error(),
			})
			continue
		}
		catalogModels := make([]any, 0, len(models))
		for _, model := range models {
			row := map[string]any{"id": model.ID, "name": model.Name}
			if model.Description != "" {
				row["description"] = model.Description
			}
			resolved, err := rt.ResolveModelInfo(provider.ID, model.ID)
			if err == nil && resolved.Reasoning != nil {
				efforts := make([]any, 0, len(resolved.Reasoning.Efforts))
				for _, effort := range resolved.Reasoning.Efforts {
					effortRow := map[string]any{"id": effort.ID, "name": effort.Name}
					if effort.Description != "" {
						effortRow["description"] = effort.Description
					}
					efforts = append(efforts, effortRow)
				}
				reasoning := map[string]any{"efforts": efforts}
				if resolved.Reasoning.DefaultEffort != "" {
					reasoning["defaultEffort"] = string(resolved.Reasoning.DefaultEffort)
				}
				row["reasoning"] = reasoning
			}
			catalogModels = append(catalogModels, row)
		}
		if len(catalogModels) == 0 {
			continue
		}
		groups = append(groups, map[string]any{
			"id":     provider.ID,
			"name":   provider.Name,
			"models": catalogModels,
		})
	}
	catalog["routableProviders"] = routable
	catalog["groups"] = groups
	catalog["failures"] = failures

	selection := c.defaultSelection()
	if selection.Provider != "" && selection.Model != "" {
		def := map[string]any{"provider": selection.Provider, "model": selection.Model}
		if selection.HasReasoningEffort && selection.ReasoningEffort != "" {
			def["reasoningEffort"] = selection.ReasoningEffort
		}
		catalog["default"] = def
	}
	return catalog, nil
}

// Contribution is the strict typert definition of the served session slice.
// Only modelCatalog carries a Go port today; the rest of the session
// namespace stays unregistered until its domain round.
func (c *SessionController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "session-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "session.modelCatalog",
				Service:               "sessionController",
				Namespace:             "session",
				Method:                "modelCatalog",
				Implementation:        "ModelCatalog",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
		},
	}
}
