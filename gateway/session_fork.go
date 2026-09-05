// Session fork (official api-session-controller fork()): one live session's
// completed-turn prefix becomes a seeded child session — boundary lands
// after the last completed turn (or the turn containing atSeq), trailing
// inter-turn events ride along, the child composes with the source's preset
// and the deployment default model. The workspace attach of the child stays
// with the workspace round (the official attach failure reports the
// published child id for reconciliation).
package gateway

import (
	"context"
	"errors"
	"fmt"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/identity"
	"dshgo/session"
)

// Fork forks one live session into a seeded child (official session/fork).
func (c *SessionController) Fork(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/fork", "", nil, "session fork is not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/fork", "sessionId", nil, "session fork requires a sessionId")
	}
	var atSeq *int64
	if raw, ok := request["atSeq"].(float64); ok {
		seq := int64(raw)
		atSeq = &seq
	}
	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/not-found", "session/fork", "sessionId", nil, "session %q is not live", sessionID)
	}
	if depth := live.Session.Header().DelegationDepth; depth != nil && *depth > 0 {
		return nil, wrapGatewayError("session/subagent-owned", "session/fork", "sessionId", nil,
			"session %q is owned by a live subagent", sessionID)
	}
	store := c.coldStore()
	if store == nil {
		return nil, wrapGatewayError("gateway/not-composed", "session/fork", "", nil, "session fork has no session store")
	}

	boundary, err := forkBoundary(live.Session.Events(), atSeq)
	if err != nil {
		return nil, wrapGatewayError("session/fork-unavailable", "session/fork", "sessionId", err, "%v", err)
	}

	selection := c.selectionFor(live)
	if selection.Provider == "" || selection.Model == "" {
		return nil, wrapGatewayError("session/model-unavailable", "session/fork", "", nil,
			"no adapter serves provider %q; select a model for this session", selection.Provider)
	}

	// The source's preset rides to the child (official presetForObservation);
	// an unresolvable source preset is a loud refusal, matching an explicit
	// create request.
	presetID := live.Session.Header().AgentPreset
	if presetID != "" {
		mounts := c.presets()
		if mounts == nil {
			return nil, wrapGatewayError("gateway/not-composed", "session/fork", "agentPreset", nil, "session fork has no preset mounts to resolve %q", presetID)
		}
		if _, err := mounts.Resolve(presetID); err != nil {
			return nil, wrapGatewayError("agent-preset/conflict", "session/fork", "agentPreset", err, "agent preset %q not resolvable", presetID)
		}
	}

	childID := session.SessionID("session-" + identity.RandomUUID())
	seed, err := store.Fork(live.Session, childID, boundary)
	if err != nil {
		var forkErr *session.ForkError
		if errors.As(err, &forkErr) {
			if forkErr.Code == session.ForkOpenTurn {
				return nil, wrapGatewayError("session/fork-unavailable", "session/fork", "sessionId", err, "%v", err)
			}
		}
		return nil, wrapGatewayError("gateway/internal", "session/fork", "", err, "%v", err)
	}

	source := live.Session.Header()
	if _, err := c.agents().Create(ctx, agent.CreateAgentOptions{
		SessionID: childID,
		Meta: agent.CreateAgentMeta{
			CWD:                 source.CWD,
			AgentPreset:         presetID,
			ParentSession:       source.ID,
			IsSeeded:            true,
			InheritedEventCount: session.SessionLogOffset(len(seed)),
		},
		Seed:         seed,
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
			if err := c.installSelectionOverride(agentCtx, childID); err != nil {
				return agent.AgentSetupCommit{}, err
			}
			return agent.AgentSetupCommit{}, nil
		},
	}); err != nil {
		return nil, wrapGatewayError("gateway/internal", "session/fork", "", err, "agent creation for fork of %q failed", sessionID)
	}
	if c.emitAdded != nil {
		c.emitAdded(createValue(childID, presetID))
	}
	return map[string]any{"sessionId": string(childID)}, nil
}

// forkBoundary resolves the inclusive event index a fork may copy: the last
// completed turn's end (atSeq selects the turn containing it), extended
// forward across trailing inter-turn events (official cut semantics).
func forkBoundary(events []session.Event, atSeq *int64) (int64, error) {
	boundary := -1
	if atSeq != nil {
		// The turn containing atSeq: the first turn/end at or after it.
		for index, event := range events {
			if event.Seq < *atSeq {
				continue
			}
			if event.Type == session.EventTurnEnd {
				boundary = index
				break
			}
		}
		if boundary < 0 {
			return 0, fmt.Errorf("session has not completed the turn containing event %d", *atSeq)
		}
	} else {
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type == session.EventTurnEnd {
				boundary = index
				break
			}
		}
		if boundary < 0 {
			return 0, errors.New("session has no completed turn to fork from")
		}
	}
	// Trailing inter-turn events (title, goal attribution, ...) ride along
	// until the next turn opens.
	cut := boundary + 1
	for cut < len(events) && events[cut].Type != session.EventTurnStart {
		cut++
	}
	return int64(cut - 1), nil
}
