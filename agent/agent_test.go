// Contract tests for the agent registry: enter/announce/dispose lifecycle,
// creation veto and rollback, scope-filtered dispatch, initiator boundaries,
// and the factory slot.
package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
)

// recordingNotifications collects inbox notifications in commit order.
type recordingNotifications struct {
	events []string
}

func (r *recordingNotifications) Inserted(message llm.Message) {
	r.events = append(r.events, "inserted:"+message.ID)
}

func (r *recordingNotifications) Discarded(message llm.Message) {
	r.events = append(r.events, "discarded:"+message.ID)
}

func (r *recordingNotifications) Claimed(message llm.Message, turn int64) {
	r.events = append(r.events, "claimed:"+message.ID)
}

func newTestAgent(t *testing.T, registry *AgentRegistry, id string, parentScope ScopeKey) *Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	inbox, err := NewInbox(sess, &recordingNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	return NewAgent(AgentConfig{ID: session.SessionID(id), Session: sess, Inbox: inbox, ParentScope: parentScope}, registry.Events())
}

func TestRegistryLifecycleEmitsPairedEdges(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	observed := []string{}
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		observed = append(observed, "created:"+payload.(AgentLifecyclePayload).Agent.ID)
		return nil
	})
	registry.Events().OnEmit(EventAgentDisposed, nil, func(payload any) error {
		observed = append(observed, "disposed:"+payload.(AgentLifecyclePayload).Agent.ID)
		return nil
	})

	detach, err := registry.Enter(agent, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if registry.Get("agent-1") != agent {
		t.Fatal("entered agent must be live")
	}
	if err := registry.Announce(agent); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if slices.Contains(observed, "disposed:agent-1") {
		t.Fatalf("premature disposed: %v", observed)
	}
	detach()
	if registry.Get("agent-1") != nil {
		t.Fatal("detached agent must be gone")
	}
	if len(observed) != 2 || observed[0] != "created:agent-1" || observed[1] != "disposed:agent-1" {
		t.Fatalf("observed = %v", observed)
	}
	// The disposer is idempotent.
	detach()
	if len(observed) != 2 {
		t.Fatalf("double dispose emitted again: %v", observed)
	}
}

func TestRegistryRegistrationGuards(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	if _, err := registry.Enter(agent, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if _, err := registry.Enter(agent, nil); err == nil || err.Error() != `agent "agent-1" is already registered` {
		t.Fatalf("dup err = %v", err)
	}
	if err := registry.Announce(agent); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if err := registry.Announce(agent); err == nil || err.Error() != `agent "agent-1" was already announced` {
		t.Fatalf("re-announce err = %v", err)
	}

	stranger := newTestAgent(t, registry, "agent-2", nil)
	if err := registry.Announce(stranger); err == nil || err.Error() != `agent "agent-2" is not live in this registry` {
		t.Fatalf("unentered announce err = %v", err)
	}

	mismatched := NewAgent(AgentConfig{ID: "other", Session: agent.Session}, registry.Events())
	if _, err := registry.Enter(mismatched, nil); err == nil ||
		err.Error() != `agent id "other" does not match session id "agent-1"` {
		t.Fatalf("mismatch err = %v", err)
	}
}

func TestAnnounceVetoRollsBackPublication(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	observed := []string{}
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		observed = append(observed, "created")
		return nil
	})
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		return errors.New("veto")
	})
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		observed = append(observed, "after-veto")
		return nil
	})
	disposed := 0
	registry.Events().OnEmit(EventAgentDisposed, nil, func(payload any) error {
		disposed++
		return nil
	})

	agent := newTestAgent(t, registry, "agent-1", nil)
	if _, err := registry.Register(agent); err == nil || err.Error() != "veto" {
		t.Fatalf("Register err = %v", err)
	}
	if registry.Get("agent-1") != nil {
		t.Fatal("a vetoed publication must roll back")
	}
	// Later created listeners do not run after the veto (JS Array.map
	// starvation), and the announced-but-rolled-back entry still pairs its
	// disposal edge.
	if slices.Contains(observed, "after-veto") {
		t.Fatalf("post-veto listener ran: %v", observed)
	}
	if disposed != 1 {
		t.Fatalf("disposed = %d", disposed)
	}
}

func TestDetachDuringAnnounceDefersDisposal(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	observed := []string{}
	var detach func()
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		observed = append(observed, "created")
		detach()
		return nil
	})
	registry.Events().OnEmit(EventAgentCreated, nil, func(payload any) error {
		observed = append(observed, "second")
		return nil
	})
	registry.Events().OnEmit(EventAgentDisposed, nil, func(payload any) error {
		observed = append(observed, "disposed")
		return nil
	})

	agent := newTestAgent(t, registry, "agent-1", nil)
	detach, err := registry.Enter(agent, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	if err := registry.Announce(agent); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if err := registry.Announce(agent); err == nil {
		t.Fatal("announce after deferred detach must fail")
	}
	// Every creation listener observed the stable entry before disposal.
	if len(observed) != 3 || observed[0] != "created" || observed[1] != "second" || observed[2] != "disposed" {
		t.Fatalf("observed = %v", observed)
	}
}

func TestOwnershipAndRoots(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	root := newTestAgent(t, registry, "root", nil)
	child := newTestAgent(t, registry, "child", root.Scope)
	other := newTestAgent(t, registry, "other", nil)

	rootDetach, err := registry.Enter(root, nil)
	if err != nil {
		t.Fatalf("Enter root: %v", err)
	}
	defer rootDetach()
	childDetach, err := registry.Enter(child, root)
	if err != nil {
		t.Fatalf("Enter child: %v", err)
	}
	defer childDetach()
	otherDetach, err := registry.Enter(other, nil)
	if err != nil {
		t.Fatalf("Enter other: %v", err)
	}
	defer otherDetach()
	if err := registry.Announce(root); err != nil {
		t.Fatalf("Announce root: %v", err)
	}
	if err := registry.Announce(child); err != nil {
		t.Fatalf("Announce child: %v", err)
	}
	if err := registry.Announce(other); err != nil {
		t.Fatalf("Announce other: %v", err)
	}

	if !registry.IsOwnedBy("child", root) {
		t.Fatal("child must be owned by root")
	}
	if registry.IsOwnedBy("child", other) {
		t.Fatal("ownership is exact-identity, not structural")
	}
	roots := registry.Roots()
	if len(roots) != 2 {
		t.Fatalf("roots = %v", roots)
	}
	list := registry.List()
	if len(list) != 3 || list[0].ID != "root" || list[1].ID != "child" || list[2].ID != "other" {
		t.Fatalf("list order = %v", list)
	}
}

func TestScopeFilteredSubjectDispatch(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	parent := newTestAgent(t, registry, "parent", nil)
	child := newTestAgent(t, registry, "child", parent.Scope)
	unrelated := newTestAgent(t, registry, "unrelated", nil)

	hits := []string{}
	registry.Events().OnEmit(EventAgentStatus, nil, func(payload any) error {
		hits = append(hits, "global:"+(payload.(AgentStatusPayload).Agent.ID))
		return nil
	})
	parentTag := registry.Events().OnEmit(EventAgentStatus, parent.Scope, func(payload any) error {
		hits = append(hits, "parent-tag:"+(payload.(AgentStatusPayload).Agent.ID))
		return nil
	})
	registry.Events().OnEmit(EventAgentStatus, unrelated.Scope, func(payload any) error {
		hits = append(hits, "unrelated-tag")
		return nil
	})

	// A status flip dispatches through the agent's own bus with its scope.
	parent.setStatus(AgentRunning)
	// parent-tag admits the child (tag ∈ child's chain), but not the
	// unrelated agent.
	child.setStatus(AgentRunning)
	unrelated.setStatus(AgentRunning)

	want := []string{
		"global:parent", "parent-tag:parent",
		"global:child", "parent-tag:child",
		"global:unrelated", "unrelated-tag",
	}
	if len(hits) != len(want) {
		t.Fatalf("hits = %v", hits)
	}
	for i := range want {
		if hits[i] != want[i] {
			t.Fatalf("hits[%d] = %q, want %q (all: %v)", i, hits[i], want[i], hits)
		}
	}
	parentTag()
	parent.setStatus(AgentIdle)
	if len(hits) != 7 || hits[6] != "global:parent" {
		t.Fatalf("after parent-tag disposal: %v", hits)
	}
}

func TestWaterfallAndSerialDispatch(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	seen := []string{}
	registry.Events().Request().On(nil, func(payload RequestPayload, next func(RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		seen = append(seen, "outer")
		result := next(payload)
		result.Provider = "outer-provider"
		return result
	})
	registry.Events().Request().On(nil, func(payload RequestPayload, next func(RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		seen = append(seen, "inner")
		return next(payload)
	})
	base := func(RequestPayload) *llm.LlmCallConfig {
		seen = append(seen, "base")
		return &llm.LlmCallConfig{Provider: "base", Model: "m"}
	}
	result := registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, base)
	if result.Provider != "outer-provider" || result.Model != "m" {
		t.Fatalf("result = %+v", result)
	}
	if len(seen) != 3 || seen[0] != "outer" || seen[1] != "inner" || seen[2] != "base" {
		t.Fatalf("seen = %v", seen)
	}

	// Serial: first bail wins; scope filtering applies.
	registry.Events().OnSerial(EventTurnStopping, nil, func(payload any) (any, bool) {
		return nil, false
	})
	registry.Events().OnSerial(EventTurnStopping, agent.Scope, func(payload any) (any, bool) {
		return "bail", true
	})
	if got := registry.Events().Serial(EventTurnStopping, agent.Scope, nil); got != "bail" {
		t.Fatalf("serial = %v", got)
	}
	other := newTestAgent(t, registry, "agent-2", nil)
	if got := registry.Events().Serial(EventTurnStopping, other.Scope, nil); got != nil {
		t.Fatalf("scope-filtered serial = %v", got)
	}
}

func TestContainedEmitLogsAndContinues(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	logger := &recordingLogger{}
	registry2 := NewAgentRegistry(nil, logger)
	_ = registry
	agent := newTestAgent(t, registry2, "agent-1", nil)
	runs := 0
	registry2.Events().OnEmit(EventInboxInserted, nil, func(payload any) error {
		runs++
		return errors.New("boom")
	})
	registry2.Events().OnEmit(EventInboxInserted, nil, func(payload any) error {
		runs++
		panic("kaboom")
	})
	registry2.Events().OnEmit(EventInboxInserted, nil, func(payload any) error {
		runs++
		return nil
	})
	registry2.Events().Emit(EventInboxInserted, agent.Scope, AgentMessagePayload{Agent: agent})
	if runs != 3 {
		t.Fatalf("runs = %d (a failing listener must not starve later ones)", runs)
	}
	if len(logger.warns) != 2 {
		t.Fatalf("warns = %v", logger.warns)
	}
}

type recordingLogger struct {
	warns []string
}

func (l *recordingLogger) Info(args ...any)  {}
func (l *recordingLogger) Error(args ...any) {}
func (l *recordingLogger) Warn(args ...any) {
	l.warns = append(l.warns, args[0].(string))
}

func TestFactorySlotContract(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	if _, err := registry.Create(context.Background(), CreateAgentOptions{SessionID: "s"}); !errors.Is(err, ErrNoFactory) {
		t.Fatalf("create without factory = %v", err)
	}
	dispose, err := registry.SetFactory(&stubFactory{})
	if err != nil {
		t.Fatalf("SetFactory: %v", err)
	}
	if _, err := registry.SetFactory(&stubFactory{}); !errors.Is(err, ErrFactoryExists) {
		t.Fatalf("second factory = %v", err)
	}
	handle, err := registry.Create(context.Background(), CreateAgentOptions{SessionID: "s"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if handle.Agent.ID != "s" {
		t.Fatalf("handle = %+v", handle)
	}
	dispose()
	if _, err := registry.Create(context.Background(), CreateAgentOptions{SessionID: "s"}); !errors.Is(err, ErrNoFactory) {
		t.Fatalf("create after dispose = %v", err)
	}
}

type stubFactory struct{}

func (f *stubFactory) CreateAgent(owner *cordis.Context, options CreateAgentOptions) (AgentHandle, error) {
	sess, err := session.NewDetached(options.SessionID, nil, nil)
	if err != nil {
		return AgentHandle{}, err
	}
	inbox, err := NewInbox(sess, &recordingNotifications{})
	if err != nil {
		return AgentHandle{}, err
	}
	registry := NewAgentRegistry(nil, nil)
	agent := NewAgent(AgentConfig{ID: options.SessionID, Session: sess, Inbox: inbox}, registry.Events())
	return AgentHandle{Agent: agent, Dispose: func() error { return nil }}, nil
}

func (f *stubFactory) Resume(owner *cordis.Context, options ResumeAgentOptions) (AgentHandle, error) {
	return AgentHandle{}, errors.New("not implemented")
}

func TestInitiatorBoundaries(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	if _, err := RequireInitiator(context.Background()); !errors.Is(err, ErrNoInitiator) {
		t.Fatalf("outside boundary = %v", err)
	}
	inner := CurrentInitiator(context.Background())
	_ = inner
	err := registry.WithInitiator(context.Background(), agent, func(ctx context.Context) error {
		found, err := RequireInitiator(ctx)
		if err != nil || found != agent {
			t.Fatalf("inside boundary = %v, %v", found, err)
		}
		// A clearing boundary hides the inherited agent.
		return registry.WithoutInitiator(ctx, func(cleared context.Context) error {
			if CurrentInitiator(cleared) != nil {
				t.Fatal("clearing boundary must hide the initiator")
			}
			// And a nested explicit boundary restores it.
			return registry.WithInitiator(cleared, agent, func(restored context.Context) error {
				if CurrentInitiator(restored) != agent {
					t.Fatal("nested boundary must restore the initiator")
				}
				return nil
			})
		})
	})
	if err != nil {
		t.Fatalf("WithInitiator: %v", err)
	}
	if CurrentInitiator(context.Background()) != nil {
		t.Fatal("boundary must not leak past the closure")
	}
}

func TestInitiatorDrainAndClose(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = registry.WithInitiator(context.Background(), agent, func(ctx context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	disposed := make(chan error, 1)
	go func() { disposed <- registry.DisposeInitiators() }()

	// Dispose waits for the in-flight boundary.
	select {
	case err := <-disposed:
		t.Fatalf("DisposeInitiators returned early: %v", err)
	default:
	}
	close(release)
	wg.Wait()
	if runErr != nil {
		t.Fatalf("run err = %v", runErr)
	}
	if err := <-disposed; err != nil {
		t.Fatalf("DisposeInitiators: %v", err)
	}
	// Closed for new boundaries and disposed for reads.
	if err := registry.WithInitiator(context.Background(), agent, func(context.Context) error { return nil }); !errors.Is(err, ErrInitiatorDisposed) {
		t.Fatalf("post-dispose boundary = %v", err)
	}
}

func TestStatusTransitionsEmitOnlyOnChange(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	flips := []AgentStatus{}
	registry.Events().OnEmit(EventAgentStatus, nil, func(payload any) error {
		flips = append(flips, payload.(AgentStatusPayload).Status)
		return nil
	})
	if agent.Status() != AgentIdle {
		t.Fatalf("initial status = %s", agent.Status())
	}
	agent.setStatus(AgentRunning)
	agent.setStatus(AgentRunning) // no flip, no emit
	agent.setStatus(AgentIdle)
	if len(flips) != 2 || flips[0] != AgentRunning || flips[1] != AgentIdle {
		t.Fatalf("flips = %v", flips)
	}
}

func TestScopeKeyUniqueness(t *testing.T) {
	registry := NewAgentRegistry(nil, nil)
	a := newTestAgent(t, registry, "a", nil)
	b := newTestAgent(t, registry, "b", nil)
	if a.Scope == b.Scope {
		t.Fatal("each agent must own a distinct scope key")
	}
	if scope.Admits(a.Scope, b.Scope) {
		t.Fatal("sibling scopes must not admit each other")
	}
}
