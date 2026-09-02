package subagent

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// newTestAgent builds one live registry-registered agent with explicit
// options and a header delegation depth.
func newTestAgent(t *testing.T, registry *agent.AgentRegistry, id string, options agent.AgentOptions, headerDepth *int64) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), DelegationDepth: headerDepth}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: options, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

func TestDelegationDepthOf(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	depth := func(id string, options agent.AgentOptions, header *int64) int64 {
		t.Helper()
		a := newTestAgent(t, registry, id, options, header)
		value, err := DelegationDepthOf(a)
		if err != nil {
			t.Fatalf("DelegationDepthOf: %v", err)
		}
		return value
	}
	if got := depth("d0", agent.AgentOptions{}, nil); got != 0 {
		t.Fatalf("absent = %d, want 0", got)
	}
	two := int64(2)
	if got := depth("d1", agent.AgentOptions{}, &two); got != 2 {
		t.Fatalf("header only = %d, want 2", got)
	}
	five := int64(5)
	if got := depth("d2", agent.AgentOptions{SubagentDepth: &five}, &two); got != 5 {
		t.Fatalf("runtime deepens = %d, want 5", got)
	}
	one := int64(1)
	if got := depth("d3", agent.AgentOptions{SubagentDepth: &one}, &two); got != 2 {
		t.Fatalf("runtime cannot lower = %d, want the header 2", got)
	}
	if got := depth("d4", agent.AgentOptions{SubagentDepth: &one}, nil); got != 1 {
		t.Fatalf("runtime without header = %d, want 1", got)
	}
}

func TestDelegationDepthRejectsUnsafeRuntimeValues(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	negative := int64(-1)
	a := newTestAgent(t, registry, "bad", agent.AgentOptions{SubagentDepth: &negative}, nil)
	_, err := DelegationDepthOf(a)
	if err == nil || err.Error() != "agent subagentDepth must be a non-negative safe integer" {
		t.Fatalf("err = %v", err)
	}
	unsafe := int64(1<<53 + 1)
	big := newTestAgent(t, registry, "big", agent.AgentOptions{SubagentDepth: &unsafe}, nil)
	_, err = DelegationDepthOf(big)
	if err == nil || err.Error() != "agent subagentDepth must be a non-negative safe integer" {
		t.Fatalf("err = %v", err)
	}
}

func TestAssertSubagentMaxDepth(t *testing.T) {
	if err := AssertSubagentMaxDepth(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	zero := int64(0)
	if err := AssertSubagentMaxDepth(&zero); err != nil {
		t.Fatalf("zero: %v", err)
	}
	negative := int64(-1)
	err := AssertSubagentMaxDepth(&negative)
	if err == nil || err.Error() != "subagent maxDepth must be a non-negative safe integer" {
		t.Fatalf("negative: %v", err)
	}
	unsafe := int64(1<<53 + 1)
	if err := AssertSubagentMaxDepth(&unsafe); err == nil {
		t.Fatal("unsafe: expected rejection")
	}
}

func TestSubagentErrorCarriesCode(t *testing.T) {
	err := newSubagentError("boom", "TEST_CODE", nil)
	var typed SubagentError
	if !asSubagentError(err, &typed) || typed.Code() != "TEST_CODE" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(typed.Error(), "boom") {
		t.Fatalf("message = %q", typed.Error())
	}
	// A wrapped failure still restores.
	var restored SubagentError
	if !asSubagentError(llm.NewError("OUTER", "outer", err), &restored) {
		t.Fatal("wrapping lost the typed error")
	}
}
