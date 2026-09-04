// Session creation for the api-session-controller Remote namespace.
// Port of packages/api/session-controller/src/commands.ts create() over the
// Go composition: identity, workspace resolution, default-cwd fallback,
// agent creation through the registry (preset mount + creation-time model
// selection), and the workspace attach whose failure reports the published
// sessionId for client-side reconciliation.
package gateway

import (
	"context"
	"fmt"
	"os"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/identity"
	"dshgo/llm"
	"dshgo/preset"
	"dshgo/session"
	"dshgo/workspace"
)

// SessionAgentCreator is the creation face of the agent registry the
// transaction programs (ctx.agents.create in the source). *agent.AgentRegistry
// satisfies it in production; tests substitute a fake factory.
type SessionAgentCreator interface {
	Create(ctx context.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error)
}

// SessionCreateDeps are the seams session/create reads. Every lookup
// resolves per call, so composition order never gates the controller
// construction; an absent service degrades the endpoint to a loud error
// instead of a silent half-create.
type SessionCreateDeps struct {
	// Workspaces resolves the request's workspace (workspaceRegistry.get).
	Workspaces func() any
	// Agents creates the root agent and answers the idempotent live lookup.
	Agents func() any
	// Presets resolves and mounts the agent composition (agentPresets).
	Presets func() any
	// Sessions answers the cold-identity check (the sessions store).
	Sessions func() any
	// DefaultCwd is the project directory used when create names neither a
	// workspace nor a cwd (official ApiSessionCommands defaultCwd).
	DefaultCwd string
}

// EnableCreate attaches the create seams. The endpoint stays unregistered
// until this runs, so minimal profiles that mount api-gateway without the
// agent plane never advertise a create they cannot serve.
func (c *SessionController) EnableCreate(deps SessionCreateDeps) {
	if deps.Workspaces == nil {
		deps.Workspaces = func() any { return nil }
	}
	if deps.Agents == nil {
		deps.Agents = func() any { return nil }
	}
	if deps.Presets == nil {
		deps.Presets = func() any { return nil }
	}
	if deps.Sessions == nil {
		deps.Sessions = func() any { return nil }
	}
	c.createDeps = &deps
}

// createReady reports whether the create seams are composed.
func (c *SessionController) createReady() bool {
	return c.createDeps != nil
}

// workspaces resolves the composed workspace registry, or nil when absent.
func (c *SessionController) workspaces() *workspace.Registry {
	if c.createDeps == nil {
		return nil
	}
	if r, ok := c.createDeps.Workspaces().(*workspace.Registry); ok && r != nil {
		return r
	}
	return nil
}

// agents resolves the composed agent registry, or nil when absent.
func (c *SessionController) agents() SessionAgentCreator {
	if c.createDeps == nil {
		return nil
	}
	if a, ok := c.createDeps.Agents().(SessionAgentCreator); ok && a != nil {
		return a
	}
	return nil
}

// liveAgent resolves the composed registry's live-agent lookup, or nil.
func (c *SessionController) liveAgent(sessionID session.SessionID) *agent.Agent {
	registry := c.agents()
	if getter, ok := registry.(interface{ Get(session.SessionID) *agent.Agent }); ok {
		return getter.Get(sessionID)
	}
	return nil
}

// presets resolves the composed preset mounts, or nil when absent.
func (c *SessionController) presets() *preset.Mounts {
	if c.createDeps == nil {
		return nil
	}
	if m, ok := c.createDeps.Presets().(*preset.Mounts); ok && m != nil {
		return m
	}
	return nil
}

// coldStore resolves the composed session store, or nil when absent.
func (c *SessionController) coldStore() *session.Store {
	if c.createDeps == nil {
		return nil
	}
	if store, ok := c.createDeps.Sessions().(*session.Store); ok && store != nil {
		return store
	}
	return nil
}

// requestString reads one optional string field from the decoded request
// payload. Non-string JSON values (including missing) read as absent.
func requestString(request map[string]any, field string) string {
	raw, ok := request[field]
	if !ok || raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	return ""
}

// Create creates or idempotently adopts one ordinary Session (official
// session/create): workspaceId XOR cwd validation, `session-<uuid>`
// identity minting, workspace resolution with the default-cwd fallback,
// agent creation (default preset mount + creation-time model selection),
// and the workspace attach. The adopt path covers live agents only; a
// persisted-but-cold identity answers a loud not-live error until the
// resume domain round.
func (c *SessionController) Create(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/create", "", nil, "session create is not composed on this profile")
	}
	workspaceID := requestString(request, "workspaceId")
	requestedCwd := requestString(request, "cwd")
	requestedSessionID := requestString(request, "sessionId")
	requestedPreset := requestString(request, "agentPreset")

	if workspaceID != "" && requestedCwd != "" {
		return nil, wrapGatewayError("gateway/bad-request", "session/create", "", nil, "session.create accepts workspaceId or cwd, not both")
	}

	sessionID := session.SessionID(requestedSessionID)
	if sessionID == "" {
		sessionID = session.SessionID("session-" + identity.RandomUUID())
	}

	var entity *workspace.Entity
	if workspaceID != "" {
		registry := c.workspaces()
		if registry == nil {
			return nil, wrapGatewayError("gateway/not-composed", "session/create", "workspaceId", nil, "session create has no workspace registry")
		}
		entity = registry.Get(workspace.WorkspaceID(workspaceID))
		if entity == nil {
			return nil, wrapGatewayError("workspace/not-found", "session/create", "workspaceId", nil, "workspace %q not found", workspaceID)
		}
	}
	cwd := requestedCwd
	if entity != nil {
		cwd = entity.Path()
	}
	if cwd == "" {
		cwd = c.createDeps.DefaultCwd
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	registry := c.agents()
	if registry == nil {
		return nil, wrapGatewayError("gateway/not-composed", "session/create", "", nil, "session create has no agent registry")
	}

	// Idempotent live adoption: a live agent with the requested identity is
	// returned as-is (official createOrAdopt live branch). A cwd conflict
	// still refuses, matching the official ensureSession guard.
	if live := c.liveAgent(sessionID); live != nil {
		if liveCwd := live.Session.Header().CWD; liveCwd != cwd {
			return nil, wrapGatewayError("session/cwd-conflict", "session/create", "sessionId", nil,
				"session %q already owns cwd %q, requested %q", sessionID, liveCwd, cwd)
		}
		return createValue(sessionID, live.Session.Header().AgentPreset), nil
	}

	// A cold identity (persisted but no live agent) needs the resume domain.
	if store := c.coldStore(); store != nil && store.Get(sessionID) != nil {
		return nil, wrapGatewayError("session/exists-cold", "session/create", "sessionId", nil,
			"session %q exists but has no live agent; resume is a pending domain round", sessionID)
	}

	// Preset composition: the requested id, else the roster default, else
	// the official agentless fallback (no preset field, selection-only
	// setup) — composeAgent's two branches. An explicitly requested preset
	// that fails composition is a loud refusal; a default-derived preset
	// whose plugin closure is not yet ported degrades to the agentless
	// composition instead of dead-ending the New Session flow.
	presetID := requestedPreset
	presetExplicit := presetID != ""
	if !presetExplicit {
		if mounts := c.presets(); mounts != nil {
			presetID = mounts.DefaultID()
		}
	}
	if presetID != "" {
		mounts := c.presets()
		if mounts == nil {
			return nil, wrapGatewayError("gateway/not-composed", "session/create", "agentPreset", nil, "session create has no preset mounts to resolve %q", presetID)
		}
		if _, err := mounts.Resolve(presetID); err != nil {
			if presetExplicit {
				return nil, wrapGatewayError("gateway/arguments-invalid", "session/create", "agentPreset", err, "agent preset %q not resolvable", presetID)
			}
			presetID = ""
		} else if _, err := mounts.StandingKeyFor(presetID); err != nil {
			if presetExplicit {
				return nil, wrapGatewayError("gateway/arguments-invalid", "session/create", "agentPreset", err, "agent preset %q is not mountable", presetID)
			}
			presetID = ""
		}
	}

	selection := c.defaultSelection()
	if selection.Provider == "" || selection.Model == "" {
		return nil, wrapGatewayError("gateway/not-composed", "session/create", "", nil, "session create has no default model selection")
	}

	// The Dispose capability returned here stays with the registry's
	// lifecycle owner (the agent-loop service), matching the official
	// transaction's end of ownership at successful attach.
	_, err := registry.Create(ctx, agent.CreateAgentOptions{
		SessionID:    sessionID,
		Meta:         agent.CreateAgentMeta{CWD: cwd, AgentPreset: presetID},
		AgentOptions: agent.AgentOptions{Provider: selection.Provider, Model: selection.Model},
		Setup: func(agentCtx *cordis.Context) (agent.AgentSetupCommit, error) {
			if presetID != "" {
				if _, err := c.presets().Mount(agentCtx, presetID); err != nil {
					return agent.AgentSetupCommit{}, err
				}
			}
			if err := installCreationModelSelection(agentCtx, selection); err != nil {
				return agent.AgentSetupCommit{}, err
			}
			return agent.AgentSetupCommit{}, nil
		},
	})
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "session/create", "", err, "agent creation for session %q failed", sessionID)
	}

	if entity != nil {
		if err := entity.AttachSession(sessionID); err != nil {
			// The official transaction reports the published sessionId and
			// lets the client reconcile it into the list; the Session stays
			// published, so the agent is NOT disposed here.
			return nil, wrapGatewayError("session/workspace-attach-failed", "session/create", "workspaceId", err,
				"session %q was created but could not attach to workspace %q", sessionID, workspaceID)
		}
	}
	return createValue(sessionID, presetID), nil
}

// createValue is the SessionCreateValue wire shape: the identity plus the
// preset when one is composed (official `agentPreset?` spread).
func createValue(sessionID session.SessionID, agentPreset string) any {
	value := map[string]any{"sessionId": string(sessionID)}
	if agentPreset != "" {
		value["agentPreset"] = agentPreset
	}
	return value
}

// installCreationModelSelection applies the creation-time selection until
// the session's first durable request header exists (the webhook
// transaction's identical seam, ported for the api-session namespace).
func installCreationModelSelection(agentCtx *cordis.Context, selection agent.ModelSelection) error {
	target, ok := agent.ContextService.From(agentCtx)
	if !ok {
		return fmt.Errorf("session create setup has no scoped Agent")
	}
	dispose := target.Events().Request().On(target.Scope, func(payload agent.RequestPayload, next func(agent.RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		resolved := next(payload)
		if resolved == nil {
			return nil
		}
		built, ok := agent.ContextService.From(agentCtx)
		if !ok {
			panic("session create setup has no scoped Agent")
		}
		if built.Session.RequestHeader() != nil ||
			resolved.Provider != selection.Provider ||
			resolved.Model != selection.Model {
			return resolved
		}
		out := *resolved
		out.ReasoningEffort = ""
		if selection.HasReasoningEffort {
			out.ReasoningEffort = selection.ReasoningEffort
		}
		return &out
	})
	return agentCtx.Effect(func() (cordis.Disposer, error) {
		return cordis.Disposer(dispose), nil
	})
}
