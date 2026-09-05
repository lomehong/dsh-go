// Session model-selection overrides (official api-session selectModel +
// the Session-local selection the prompt assembly consults) and the
// openWorkspacePath desktop handoff. The override rides the request
// waterfall at the agent scope, so the next model call after selectModel
// uses the new route; the in-memory registry is a recorded deviation from
// the official durable model-selection fold (restart resets to the default
// selection until the projection round).
package gateway

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// SessionSelections is the per-session override registry shared by the
// selectModel endpoint and the agents' request waterfalls.
type SessionSelections struct {
	mu         sync.Mutex
	selections map[session.SessionID]agent.ModelSelection
}

// NewSessionSelections builds an empty override registry.
func NewSessionSelections() *SessionSelections {
	return &SessionSelections{selections: map[session.SessionID]agent.ModelSelection{}}
}

// Set records one session's selection override.
func (s *SessionSelections) Set(sessionID session.SessionID, selection agent.ModelSelection) {
	s.mu.Lock()
	s.selections[sessionID] = selection
	s.mu.Unlock()
}

// Get reads one session's override; ok is false without one.
func (s *SessionSelections) Get(sessionID session.SessionID) (agent.ModelSelection, bool) {
	s.mu.Lock()
	selection, ok := s.selections[sessionID]
	s.mu.Unlock()
	return selection, ok
}

// selections is the controller's lazily-resolved override registry.
func (c *SessionController) selections() *SessionSelections {
	if c.createDeps == nil || c.createDeps.Selections == nil {
		return nil
	}
	if selections, ok := c.createDeps.Selections().(*SessionSelections); ok && selections != nil {
		return selections
	}
	return nil
}

// SelectModel validates one complete selection against the composed
// adapters and records it as the session's override: the next prompt
// assembly uses the new route (official session/selectModel).
func (c *SessionController) SelectModel(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/selectModel", "", nil, "model selection is not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/selectModel", "sessionId", nil, "model selection requires a sessionId")
	}
	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/not-found", "session/selectModel", "sessionId", nil, "session %q is not live", sessionID)
	}
	provider := requestString(request, "provider")
	model := requestString(request, "model")
	if provider == "" || model == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/selectModel", "", nil, "model selection requires provider and model")
	}
	selection := agent.ModelSelection{Provider: provider, Model: model}
	if effort := requestString(request, "reasoningEffort"); effort != "" {
		selection.HasReasoningEffort = true
		selection.ReasoningEffort = effort
	}
	rt := c.runtime()
	if rt != nil {
		if _, err := rt.ResolveModelInfo(provider, model); err != nil {
			return nil, wrapGatewayError("session/model-unavailable", "session/selectModel", "", err,
				"no adapter serves %q/%q: %v", provider, model, err)
		}
	}
	selections := c.selections()
	if selections == nil {
		return nil, wrapGatewayError("gateway/not-composed", "session/selectModel", "", nil, "model selection has no override registry")
	}
	selections.Set(sessionID, selection)
	value := map[string]any{"accepted": true, "provider": provider, "model": model}
	if selection.HasReasoningEffort {
		value["reasoningEffort"] = selection.ReasoningEffort
	}
	return value, nil
}

// installSelectionOverride wires the session's override registry into the
// agent's request waterfall: an override rewrites the resolved route until
// cleared (the webhook transaction's identical seam shape).
func (c *SessionController) installSelectionOverride(agentCtx *cordis.Context, sessionID session.SessionID) error {
	target, ok := agent.ContextService.From(agentCtx)
	if !ok {
		return errSessionCreateSetupNoAgent
	}
	selections := c.selections()
	if selections == nil {
		return nil
	}
	dispose := target.Events().Request().On(target.Scope, func(payload agent.RequestPayload, next func(agent.RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		resolved := next(payload)
		if resolved == nil {
			return nil
		}
		selection, ok := selections.Get(sessionID)
		if !ok {
			return resolved
		}
		out := *resolved
		out.Provider = selection.Provider
		out.Model = selection.Model
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

// OpenWorkspacePath hands one path to the Host's native opener (official
// session/openWorkspacePath). A session-scoped request resolves the path
// against the live session's workspace first: a path outside it is refused
// rather than opened.
func (c *SessionController) OpenWorkspacePath(ctx context.Context, request map[string]any) (any, error) {
	path := requestString(request, "path")
	if path == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/openWorkspacePath", "path", nil, "openWorkspacePath requires a path")
	}
	if sessionID := requestString(request, "sessionId"); sessionID != "" {
		live := c.liveAgent(session.SessionID(sessionID))
		if live == nil {
			return nil, wrapGatewayError("session/not-found", "session/openWorkspacePath", "sessionId", nil, "session %q is not live", sessionID)
		}
		cwd := live.Session.Header().CWD
		if cwd != "" && !pathWithin(cwd, path) {
			return nil, wrapGatewayError("gateway/bad-request", "session/openWorkspacePath", "path", nil,
				"path %q is outside session %q workspace %q", path, sessionID, cwd)
		}
	}
	if err := openNatively(path); err != nil {
		return nil, wrapGatewayError("gateway/internal", "session/openWorkspacePath", "", err, "%v", err)
	}
	return map[string]any{"opened": true}, nil
}

// openNatively hands one filesystem path to the platform opener. The
// handoff is best effort: the opener process may or may not surface a
// window, matching the official canOpenPath posture.
func openNatively(path string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", path).Start()
	case "darwin":
		return exec.Command("open", path).Start()
	default:
		return exec.Command("xdg-open", path).Start()
	}
}

// pathWithin reports whether path resolves inside root lexically (the
// workspace fence for the desktop handoff).
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
