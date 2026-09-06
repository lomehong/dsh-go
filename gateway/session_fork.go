// Session fork (official api-session-controller fork()): one observed
// session's completed-turn prefix becomes a seeded child session — boundary
// lands after the last completed turn (or the turn containing atSeq, with a
// past-end atSeq clamping to the last completed turn), trailing inter-turn
// events ride along, the child composes with the source's preset and the
// deployment default model. Cold (stored, not live) sessions fork through
// the session-query observation. The workspace attach of the child stays
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
	"dshgo/sessionquery"
)

// Fork forks one observed session into a seeded child (official
// session/fork). The source may be live or cold (stored): live sources fork
// through the session store's validated Fork; cold sources seed directly
// from the session-query observation.
func (c *SessionController) Fork(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/fork", "", nil, "session fork is not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/fork", "sessionId", nil, "session fork requires a sessionId")
	}

	// atSeq admission (official SessionSeq): a non-negative safe integer;
	// fractional or negative values refuse with gateway/bad-request.
	var atSeq *int64
	if raw, present := request["atSeq"]; present && raw != nil {
		number, ok := raw.(float64)
		if !ok || number != float64(int64(number)) || number < 0 {
			return nil, wrapGatewayError("gateway/bad-request", "session/fork", "atSeq", nil,
				"atSeq must be a non-negative safe integer")
		}
		seq := int64(number)
		atSeq = &seq
	}

	// Source observation: the live agent wins (its in-memory events are the
	// freshest); otherwise the session-query engine observes the stored
	// session (cold fork — the official observeSession path). Neither
	// present → session/not-found.
	var sourceEvents []session.Event
	var sourceHeader session.SessionHeader
	live := c.liveAgent(sessionID)
	if live != nil {
		sourceEvents = live.Session.Events()
		sourceHeader = live.Session.Header()
		if depth := sourceHeader.DelegationDepth; depth != nil && *depth > 0 {
			return nil, wrapGatewayError("session/subagent-owned", "session/fork", "sessionId", nil,
				"session %q is owned by a live subagent", sessionID)
		}
	} else if engine := c.engine(); engine != nil {
		observation, observeErr := engine.ObserveSession(ctx, sessionID, sessionquery.SessionObservationOptions{})
		if observeErr != nil {
			return nil, wrapGatewayError("session/not-found", "session/fork", "sessionId", observeErr,
				"session %q not found", sessionID)
		}
		defer observation.Release()
		sourceEvents = observation.Events
		sourceHeader = observation.Header
	} else {
		return nil, wrapGatewayError("session/not-found", "session/fork", "sessionId", nil,
			"session %q not found", sessionID)
	}

	store := c.coldStore()
	if store == nil {
		return nil, wrapGatewayError("gateway/not-composed", "session/fork", "", nil, "session fork has no session store")
	}

	boundary, err := forkBoundary(sourceEvents, atSeq)
	if err != nil {
		return nil, wrapGatewayError("session/fork-unavailable", "session/fork", "sessionId", err, "%v", err)
	}

	selection := c.selectionForSource(sourceHeader)
	if selection.Provider == "" || selection.Model == "" {
		return nil, wrapGatewayError("session/model-unavailable", "session/fork", "", nil,
			"no adapter serves provider %q; select a model for this session", selection.Provider)
	}

	// The source's preset rides to the child (official presetForObservation);
	// an unresolvable source preset is a loud refusal, matching an explicit
	// create request.
	presetID := sourceHeader.AgentPreset
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
	var seed []session.Event
	if live != nil {
		seed, err = store.Fork(live.Session, childID, boundary)
		if err != nil {
			var forkErr *session.ForkError
			if errors.As(err, &forkErr) && forkErr.Code == session.ForkOpenTurn {
				return nil, wrapGatewayError("session/fork-unavailable", "session/fork", "sessionId", err, "%v", err)
			}
			return nil, wrapGatewayError("gateway/internal", "session/fork", "", err, "%v", err)
		}
	} else {
		// Cold source: the seed is the observed prefix directly (the store's
		// live-instance validation does not apply to a stored session).
		seed = make([]session.Event, boundary+1)
		copy(seed, sourceEvents[:boundary+1])
	}

	if _, err := c.agents().Create(ctx, agent.CreateAgentOptions{
		SessionID: childID,
		Meta: agent.CreateAgentMeta{
			CWD:                 sourceHeader.CWD,
			AgentPreset:         presetID,
			ParentSession:       sourceHeader.ID,
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

// forkBoundary resolves the inclusive event index a fork may copy, per the
// official cut semantics: the anchored boundary is the FIRST turn/end at or
// after atSeq; when the anchor misses (or atSeq is absent), the fallback is
// the LAST completed turn — including a PAST-END atSeq, which clamps to it
// rather than refusing. Trailing inter-turn events ride along until the
// next turn opens. Errors distinguish "the turn containing atSeq never
// completed" (atSeq inside the log) from "no completed turn at all".
func forkBoundary(events []session.Event, atSeq *int64) (int64, error) {
	lastSeq := int64(-1)
	if len(events) > 0 {
		lastSeq = events[len(events)-1].Seq
	}

	boundary := -1
	if atSeq != nil {
		// Anchored: the first turn/end at or after atSeq.
		for index, event := range events {
			if event.Type == session.EventTurnEnd && event.Seq >= *atSeq {
				boundary = index
				break
			}
		}
	}
	if boundary < 0 && (atSeq == nil || *atSeq > lastSeq) {
		// Fallback: the last completed turn (absent anchor, or a past-end
		// anchor clamping to it).
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type == session.EventTurnEnd {
				boundary = index
				break
			}
		}
	}
	if boundary < 0 {
		if atSeq != nil && *atSeq <= lastSeq {
			return 0, fmt.Errorf("session has not completed the turn containing event %d", *atSeq)
		}
		return 0, errors.New("session has no completed turn to fork from")
	}

	// Trailing inter-turn events (title, goal attribution, ...) ride along
	// until the next turn opens.
	cut := boundary + 1
	for cut < len(events) && events[cut].Type != session.EventTurnStart {
		cut++
	}
	return int64(cut - 1), nil
}

// selectionForSource resolves the model selection from the composed live
// selection registry (the live session's overrides) with the deployment
// default as the fallback. Cold sources have no live overrides.
func (c *SessionController) selectionForSource(header session.SessionHeader) agent.ModelSelection {
	if live := c.liveAgent(header.ID); live != nil {
		return c.selectionFor(live)
	}
	return c.defaultSelection()
}
