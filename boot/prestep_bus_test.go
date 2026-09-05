// Minimal bus-level isolation: two global PreStep listeners on the agents
// bus must both fire in registration order.
package boot

import (
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

func TestPreStepBusBothListenersFire(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	sess, err := session.NewDetached("session-bus", nil,
		&session.SessionHeader{Version: session.SESSION_FORMAT_VERSION, ID: "session-bus"}, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	live := agent.NewAgent(agent.AgentConfig{ID: "session-bus", Session: sess, Ctx: root.Child()}, registry.Events())
	if _, err := registry.Register(live); err != nil {
		t.Fatalf("register: %v", err)
	}

	early := false
	late := false
	undoEarly := registry.Events().PreStep().On(nil, func(payload agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		early = true
		return next(payload)
	})
	defer undoEarly()
	undoLate := registry.Events().PreStep().On(nil, func(payload agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		late = true
		return next(payload)
	})
	defer undoLate()

	registry.Events().PreStep().Dispatch(live.Scope, agent.PreStepPayload{
		Agent:    live,
		Messages: []llm.Message{llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "go"}}, llm.MessageSource{Kind: llm.SourceUser})},
	}, func(payload agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepEnter(payload.Messages)
	})
	if !early || !late {
		t.Fatalf("early=%v late=%v", early, late)
	}
}
