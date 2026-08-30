package fsobservationpolicy

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

type noopSessionNotifications struct{}

func (noopSessionNotifications) Inserted(llm.Message)       {}
func (noopSessionNotifications) Discarded(llm.Message)      {}
func (noopSessionNotifications) Claimed(llm.Message, int64) {}

// newScopedAgent builds one agent with its own minted scope (the owner key).
func newScopedAgent(t *testing.T, root *cordis.Context, id string) *agent.Agent {
	t.Helper()
	sid := session.SessionID(id)
	sess, err := session.NewDetached(sid, nil, &session.SessionHeader{ID: sid, CWD: "D:\\work"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopSessionNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

func execOf(agent1 *agent.Agent) *tools.ToolRunContext {
	return &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: agent1.Scope}, Signal: context.Background()}
}

func TestWriteIntentUnseenCreatesPresentReplaces(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	detach := Apply(root)
	defer detach()
	agent1 := newScopedAgent(t, root, "obs-1")
	target := fs.Target{Key: "k1", DisplayPath: "doc.txt"}

	decided := root.Waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: execOf(agent1)})
	intent, ok := decided.(*fs.WriteIntent)
	if !ok || intent.Kind != fs.IntentCreateIfAbsent {
		t.Fatalf("unseen intent: %+v", decided)
	}

	// The provider records the authoritative observation after commit.
	root.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: fs.ObservationPresent("v3"), Actor: execOf(agent1)})
	decided = root.Waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: execOf(agent1)})
	intent, ok = decided.(*fs.WriteIntent)
	if !ok || intent.Kind != fs.IntentReplaceIfVersion || intent.Version != "v3" {
		t.Fatalf("present intent: %+v", decided)
	}
}

func TestEditIntentGuards(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	detach := Apply(root)
	defer detach()
	agent1 := newScopedAgent(t, root, "obs-2")
	target := fs.Target{Key: "k2", DisplayPath: "code.go"}

	// Unseen: FS_NOT_OBSERVED with the read-first wording.
	decided := root.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: execOf(agent1)})
	err, ok := decided.(error)
	if !ok || !strings.Contains(err.Error(), `edit requires reading "code.go" first`) {
		t.Fatalf("unseen edit: %+v", decided)
	}
	if !fsIsCode(err, fs.CodeNotObserved) {
		t.Fatalf("unseen edit code: %v", err)
	}

	// Confirmed absence: FS_NOT_FOUND.
	root.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: fs.ObservationAbsent(), Actor: execOf(agent1)})
	decided = root.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: execOf(agent1)})
	err, _ = decided.(error)
	if !fsIsCode(err, fs.CodeNotFound) || !strings.Contains(err.Error(), `cannot edit "code.go": not found`) {
		t.Fatalf("absent edit: %+v", decided)
	}

	// Presence: the observed version becomes the CAS basis.
	root.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: fs.ObservationPresent("v9"), Actor: execOf(agent1)})
	decided = root.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: execOf(agent1)})
	version, ok := decided.(*fs.Version)
	if !ok || *version != "v9" {
		t.Fatalf("present edit: %+v", decided)
	}
}

func TestOwnersAreIsolatedAndAgentlessReadsFreely(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	detach := Apply(root)
	defer detach()
	agentA := newScopedAgent(t, root, "obs-a")
	agentB := newScopedAgent(t, root, "obs-b")
	target := fs.Target{Key: "shared", DisplayPath: "shared.txt"}

	// A's observation does not become B's prior.
	root.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: fs.ObservationPresent("va"), Actor: execOf(agentA)})
	decided := root.Waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: execOf(agentB)})
	if intent, ok := decided.(*fs.WriteIntent); !ok || intent.Kind != fs.IntentCreateIfAbsent {
		t.Fatalf("B must not inherit A's observation: %+v", decided)
	}

	// A direct call with no agent derives no owner: the edit gate refuses
	// with FS_NOT_OBSERVED (reads freely, cannot satisfy the policy).
	decided = root.Waterfall(fs.EventEditIntent, fs.EditIntentEvent{Target: target, Actor: &tools.ToolRunContext{Signal: context.Background()}})
	if err, ok := decided.(error); !ok || !fsIsCode(err, fs.CodeNotObserved) {
		t.Fatalf("agentless edit: %+v", decided)
	}
}

func TestDisposeDropsState(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	agent1 := newScopedAgent(t, root, "obs-dispose")
	target := fs.Target{Key: "k", DisplayPath: "f.txt"}
	detach := Apply(root)
	root.Waterfall(fs.EventObserved, fs.ObservedEvent{Target: target, Observation: fs.ObservationPresent("v1"), Actor: execOf(agent1)})
	detach()
	// After disposal the listeners are gone: the waterfall falls through to
	// the bare provider behavior (no *WriteIntent decision at all).
	decided := root.Waterfall(fs.EventWriteIntent, fs.WriteIntentEvent{Target: target, Actor: execOf(agent1)})
	if _, still := decided.(*fs.WriteIntent); still {
		t.Fatalf("state survived disposal: %+v", decided)
	}
}

func fsIsCode(err error, code string) bool {
	var fsErr *fs.Error
	if errorsAs(err, &fsErr) {
		return fsErr.Code == code
	}
	return false
}

func errorsAs(err error, target **fs.Error) bool {
	if e, ok := err.(*fs.Error); ok {
		*target = e
		return true
	}
	return false
}
