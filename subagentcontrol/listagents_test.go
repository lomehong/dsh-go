package subagentcontrol

import (
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/subagent"
	"dshgo/tools"
)

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

// newCallerAgent builds one entered agent so the execute path resolves a
// receiving caller (the registry-backed scope resolution).
func newCallerAgent(t *testing.T, id string, registry *agent.AgentRegistry) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{
		ID: sess.ID(), Options: agent.AgentOptions{Provider: "deepseek", Model: "deepseek-chat"},
		Session: sess, Inbox: inbox,
	}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

// TestRegisterListAgentsStandalone: the separately loadable discovery tool
// registers alone — continuation delivery (send_message/interrupt_agent)
// stays unexposed.
func TestRegisterListAgentsStandalone(t *testing.T) {
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	subagents := subagent.NewSubagentRuntime(subagent.RuntimeConfig{})
	registry := agent.NewAgentRegistry(nil, nil)
	newCallerAgent(t, "caller-la", registry)

	undo, err := RegisterListAgents(runtime, subagents, registry, ListingDeps{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()

	if _, ok := runtime.Get("list_agents", nil); !ok {
		t.Fatal("list_agents not registered")
	}
	if _, ok := runtime.Get("send_message", nil); ok {
		t.Fatal("send_message must not ride the standalone discovery row")
	}
	if _, ok := runtime.Get("interrupt_agent", nil); ok {
		t.Fatal("interrupt_agent must not ride the standalone discovery row")
	}
}
