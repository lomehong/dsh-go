package gateway

import (
	"context"
	"errors"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projectioncache"
	"dshgo/sessiontitle"
	"dshgo/sessionquery"
	"dshgo/typert"
)

// SessionController hosts the session Remote namespace pieces the Go web
// surface serves today (official dsh-api-session-controller): the session
// list and the model catalog the conversation selector renders.
type SessionController struct {
	engineLookup  func() any
	cacheLookup   func() any
	llmLookup     func() any
	defaultLookup func() any
	// createDeps carries the session/create seams; nil until EnableCreate
	// runs, which also gates whether the create invocation is advertised.
	createDeps *SessionCreateDeps
}

// NewSessionController builds the namespace host. Lookups resolve per call —
// nil services answer honest empty values.
func NewSessionController(engineLookup func() any, cacheLookup func() any, llmLookup func() any, defaultLookup func() any) *SessionController {
	if engineLookup == nil {
		engineLookup = func() any { return nil }
	}
	if cacheLookup == nil {
		cacheLookup = func() any { return nil }
	}
	if llmLookup == nil {
		llmLookup = func() any { return nil }
	}
	if defaultLookup == nil {
		defaultLookup = func() any { return nil }
	}
	return &SessionController{engineLookup: engineLookup, cacheLookup: cacheLookup, llmLookup: llmLookup, defaultLookup: defaultLookup}
}

// engine resolves the composed session query engine, or nil when absent.
func (c *SessionController) engine() *sessionquery.Engine {
	if e, ok := c.engineLookup().(*sessionquery.Engine); ok && e != nil {
		return e
	}
	return nil
}

// projectionCache resolves the composed persisted projection cache, or nil
// when absent (headless profiles answer the honest cache-miss posture).
func (c *SessionController) projectionCache() *projectioncache.Service {
	if cache, ok := c.cacheLookup().(*projectioncache.Service); ok && cache != nil {
		return cache
	}
	return nil
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

// List answers the session list (official session/list): every session in
// the corpus (live + persisted merged), newest first, with the fields the
// session sidebar renders. running stays false (the Go host tracks no live
// agent turn today). updatedAt prefers the sessionListMetadata projection's
// lastPromptAt over the header's creation stamp; blank reflects the same
// projection, falling back to false (visible) when no usable cache row
// exists for the session lifecycle.
func (c *SessionController) List(ctx context.Context, request map[string]any) (any, error) {
	engine := c.engine()
	if engine == nil {
		return map[string]any{"items": []any{}}, nil
	}
	records, err := engine.ListSessions(ctx)
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "session/list", "", err, "session list failed")
	}
	items := make([]any, 0, len(records))
	for _, record := range records {
		header := record.Header
		blank := false
		updatedAt := header.CreatedAt
		if cache := c.projectionCache(); cache != nil {
			if snapshot, ok := cache.CachedSnapshot(header, SessionListMetadataKey); ok {
				if metadata, ok := snapshot.Values[SessionListMetadataKey].(SessionListMetadata); ok {
					blank = metadata.Blank
					if metadata.LastPromptAt != nil && *metadata.LastPromptAt > updatedAt {
						updatedAt = *metadata.LastPromptAt
					}
				}
			}
		}
		item := map[string]any{
			"sessionId": string(header.ID),
			"updatedAt": float64(updatedAt),
			"running":   false,
			"blank":     blank,
		}
		if header.CWD != "" {
			item["cwd"] = header.CWD
		}
		if header.ParentSession != "" {
			item["parentSessionId"] = string(header.ParentSession)
		}
		if header.Origin != "" {
			item["origin"] = header.Origin
		}
		items = append(items, item)
	}
	return map[string]any{"items": items}, nil
}

// Rename pins an explicit user title on one live session (official
// session/rename, commands.rename): live-agent lookup resolves the session,
// then session-title supersedes with the normalized text. Empty-normalized
// titles answer session/title-invalid (the service's ErrInvalid seam).
func (c *SessionController) Rename(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/rename", "", nil, "session rename is not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/rename", "sessionId", nil, "session rename requires a sessionId")
	}
	title := requestString(request, "title")
	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/not-found", "session/rename", "sessionId", nil, "session %q is not live", sessionID)
	}
	service := c.titles()
	if service == nil {
		return nil, wrapGatewayError("gateway/internal", "session/rename", "", nil, "renaming is unavailable: this deployment mounts no session-title service")
	}
	snapshot, err := service.Rename(live.Session, title)
	if err != nil {
		if errors.Is(err, sessiontitle.ErrInvalid) {
			return nil, wrapGatewayError("session/title-invalid", "session/rename", "title", err, "session title must contain visible characters")
		}
		return nil, wrapGatewayError("gateway/internal", "session/rename", "", err, "failed to rename session %q", sessionID)
	}
	return map[string]any{
		"title": snapshot.Title,
		"seq":   snapshot.EventSeq,
	}, nil
}

// Contribution is the strict typert definition of the served session slice.
// Only modelCatalog and list carry Go ports today; the rest of the session
// namespace stays unregistered until its domain round.
func (c *SessionController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	requestParam := typert.InvocationParameterDescriptor{
		// Wire name is "_request" (official generated descriptor): the
		// browser sends the SessionListRequest under that field.
		Name: "_request", Wire: "_request", Source: typert.SourceJSON, Codec: jsonCodec,
	}
	inv := typert.InvocationReceiver{Kind: typert.ReceiverDirect}
	descriptor := func(id, method, implementation string, params ...typert.InvocationParameterDescriptor) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID:                    id,
			Service:               "sessionController",
			Namespace:             "session",
			Method:                method,
			Implementation:        implementation,
			Invocation:            inv,
			CancellationParameter: "signal",
			Parameters:            params,
			Result:                jsonCodec,
		}
	}
	return typert.Contribution{
		Package: "session-controller",
		Face:    typert.FaceHost,
		Invocations: func() []typert.InvocationDescriptor {
			invocations := []typert.InvocationDescriptor{
				descriptor("session.modelCatalog", "modelCatalog", "ModelCatalog"),
				descriptor("session.list", "list", "List", requestParam),
				descriptor("session.page", "page", "Page", requestParam),
			}
			if c.createDeps != nil {
				invocations = append(invocations,
					descriptor("session.create", "create", "Create", requestParam),
					descriptor("session.rename", "rename", "Rename", requestParam))
			}
			return invocations
		}(),
	}
}
