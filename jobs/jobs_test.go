package jobs

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/scope"
)

// stubOwner is one agent lifecycle.
type stubOwner struct {
	id    string
	scope scope.ScopeKey
}

func (o *stubOwner) OwnerID() string            { return o.id }
func (o *stubOwner) OwnerScope() scope.ScopeKey { return o.scope }

// hooksAck is a producer whose Done channel the test controls.
type hooksAck struct {
	mu       sync.Mutex
	cancels  []string
	err      error
	done     chan Result
	calls    int
	consumed []string
	// stream attaches ReadOutput; its absence marks a final-output-only
	// job.
	stream bool
	// autoSettle makes Cancel (when it does not error) settle the job
	// killed, like a compliant producer reacting to termination.
	autoSettle bool
}

func newHooks() *hooksAck {
	return &hooksAck{done: make(chan Result, 1)}
}

func (h *hooksAck) hooks() Hooks {
	hooks := Hooks{
		Cancel: func(reason string) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.cancels = append(h.cancels, reason)
			if h.err == nil && h.autoSettle {
				h.done <- Result{Outcome: Outcome{Status: OutcomeKilled, Detail: "cancelled"}}
			}
			return h.err
		},
		Done: h.done,
	}
	if h.stream {
		hooks.ReadOutput = func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.calls++
			token := fmt.Sprintf("delta-%d", h.calls)
			h.consumed = append(h.consumed, token)
			return token
		}
	}
	return hooks
}

func (h *hooksAck) settle(outcome Outcome) {
	h.done <- Result{Outcome: outcome}
}

func newRegistry(t *testing.T, perOwner int) *LocalRegistry {
	t.Helper()
	registry, err := NewLocalRegistry(Config{MaxConcurrentJobsPerOwner: perOwner}, nil)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return registry
}

func mustStart(t *testing.T, registry *LocalRegistry, spec StartSpec) string {
	t.Helper()
	id, err := registry.Start(spec)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return id
}

func TestStartIssuesPerKindIds(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	first := mustStart(t, registry, StartSpec{Kind: "bash", Label: "one", Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	second := mustStart(t, registry, StartSpec{Kind: "bash", Label: "two", Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	other := mustStart(t, registry, StartSpec{Kind: "subagent", Label: "three", Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	if first != "bash-1" || second != "bash-2" || other != "subagent-1" {
		t.Fatalf("ids = %s %s %s", first, second, other)
	}
}

func TestStartValidation(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	cases := map[string]StartSpec{
		"empty kind":  {Kind: "", Label: "x", Run: func() (Hooks, error) { return Hooks{}, nil }},
		"empty label": {Kind: "bash", Label: "", Run: func() (Hooks, error) { return Hooks{}, nil }},
		"zero limit":  {Kind: "bash", Label: "x", OutputLimitBytes: -1, Run: func() (Hooks, error) { return Hooks{}, nil }},
		"missing run": {Kind: "bash", Label: "x"},
		"failing run": {Kind: "bash", Label: "x", Run: func() (Hooks, error) { return Hooks{}, fmt.Errorf("no tty") }},
	}
	for name, spec := range cases {
		if _, err := registry.Start(spec); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// A rejection leaves nothing registered.
	if jobs := registry.List(""); len(jobs) != 0 {
		t.Fatalf("rejections registered jobs: %+v", jobs)
	}
}

func TestStartRequiresControllerServingOwner(t *testing.T) {
	root := scope.NewScopeKey(nil)
	preset := scope.NewScopeKey(root)
	owner := &stubOwner{id: "a", scope: preset}
	registry := newRegistry(t, 10)
	spec := StartSpec{Kind: "bash", Label: "x", Owner: owner, Run: func() (Hooks, error) { return newHooks().hooks(), nil }}
	if _, err := registry.Start(spec); err == nil || !strings.Contains(err.Error(), "no job controller serves this agent") {
		t.Fatalf("uncontrolled = %v", err)
	}
	// A scoped controller serves the agents composed under its scope.
	detach := registry.AttachControllerIn(preset)
	if _, err := registry.Start(spec); err != nil {
		t.Fatalf("scoped controller refused: %v", err)
	}
	detach()
	otherOwner := &stubOwner{id: "b", scope: root}
	if _, err := registry.Start(StartSpec{Kind: "bash", Label: "y", Owner: otherOwner, Run: func() (Hooks, error) { return newHooks().hooks(), nil }}); err == nil {
		t.Fatal("detached controller still served")
	}
	// A global controller serves every owner.
	registry.AttachControllerIn(nil)
	if _, err := registry.Start(StartSpec{Kind: "bash", Label: "z", Owner: otherOwner, Run: func() (Hooks, error) { return newHooks().hooks(), nil }}); err != nil {
		t.Fatalf("global controller refused: %v", err)
	}
}

func TestConcurrencyLimitPerOwner(t *testing.T) {
	registry := newRegistry(t, 2)
	registry.AttachControllerIn(nil)
	ownerA := &stubOwner{id: "a"}
	ownerB := &stubOwner{id: "b"}
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "1", Owner: ownerA, Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "2", Owner: ownerA, Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	if _, err := registry.Start(StartSpec{Kind: "bash", Label: "3", Owner: ownerA, Run: func() (Hooks, error) { return newHooks().hooks(), nil }}); err == nil ||
		!strings.Contains(err.Error(), "background job limit reached for this owner") {
		t.Fatalf("limit = %v", err)
	}
	// Another owner's bucket is independent; the unowned bucket is its own.
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "b1", Owner: ownerB, Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "u1", Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
}

func TestAccessFence(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	owner := &stubOwner{id: "mine"}
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "secret", Owner: owner, Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	if _, err := registry.Get(id, "other"); err == nil || !strings.Contains(err.Error(), "belongs to another session") {
		t.Fatalf("foreign get = %v", err)
	}
	if _, err := registry.Get(id, "mine"); err != nil {
		t.Fatalf("owner get = %v", err)
	}
	// Unowned jobs are open to any caller, including the empty caller.
	open := mustStart(t, registry, StartSpec{Kind: "bash", Label: "open", Run: func() (Hooks, error) { return newHooks().hooks(), nil }})
	if _, err := registry.Get(open, "anyone"); err != nil {
		t.Fatalf("unowned = %v", err)
	}
	// A non-agent caller sees only unowned jobs in the listing.
	visible := registry.List("")
	if len(visible) != 1 || visible[0].ID != open {
		t.Fatalf("anonymous list = %+v", visible)
	}
	if _, err := registry.Get("ghost-1", "mine"); err == nil || !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("unknown = %v", err)
	}
}

func TestReadStreamCursorAndTerminalOutput(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	hooks := newHooks()
	hooks.stream = true
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "stream", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	first, _ := registry.Read(id, "")
	second, _ := registry.Read(id, "")
	if first.Text != "delta-1" || second.Text != "delta-2" {
		t.Fatalf("cursor = %q %q", first.Text, second.Text)
	}
	hooks.settle(Outcome{Status: OutcomeCompleted, Detail: "exit code: 0"})
	time.Sleep(20 * time.Millisecond)
	final, _ := registry.Read(id, "")
	// A stream job's terminal read consumes from the producer, not the
	// stored output.
	if final.Snapshot.Status != StatusCompleted || final.Snapshot.Detail != "exit code: 0" {
		t.Fatalf("terminal = %+v", final.Snapshot)
	}
	if !final.Snapshot.Reported {
		t.Fatal("terminal read did not mark reported")
	}
	// Final-output-only jobs deliver the stored output idempotently.
	finalHooks := newHooks()
	finalHooks.done <- Result{Outcome: Outcome{Status: OutcomeFailed, Detail: "boom", Output: "the end"}}
	outputID := mustStart(t, registry, StartSpec{Kind: "subagent", Label: "final", Run: func() (Hooks, error) { return finalHooks.hooks(), nil }})
	time.Sleep(20 * time.Millisecond)
	once, _ := registry.Read(outputID, "")
	again, _ := registry.Read(outputID, "")
	if once.Text != "the end" || again.Text != "the end" {
		t.Fatalf("final output = %q %q", once.Text, again.Text)
	}
}

func TestKillLifecycle(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	hooks := newHooks()
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "long", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	result, err := registry.Kill(id, "", "user asked")
	if err != nil || result != KillRequested {
		t.Fatalf("kill = %v %v", result, err)
	}
	snapshot, _ := registry.Get(id, "")
	if snapshot.Status != StatusStopping || !snapshot.Reported {
		t.Fatalf("post-kill = %+v", snapshot)
	}
	if len(hooks.cancels) != 1 || hooks.cancels[0] != "user asked" {
		t.Fatalf("cancels = %v", hooks.cancels)
	}
	// A producer cancel error propagates without changing state.
	errHooks := newHooks()
	errHooks.err = fmt.Errorf("cannot signal")
	errID := mustStart(t, registry, StartSpec{Kind: "bash", Label: "stuck", Run: func() (Hooks, error) { return errHooks.hooks(), nil }})
	if _, err := registry.Kill(errID, "", "x"); err == nil || !strings.Contains(err.Error(), "cannot signal") {
		t.Fatalf("cancel error = %v", err)
	}
	still, _ := registry.Get(errID, "")
	if still.Status != StatusRunning || still.Reported {
		t.Fatalf("post-error state = %+v", still)
	}
	// Settlement wins over the stopping mark; a second kill is a no-op.
	hooks.settle(Outcome{Status: OutcomeKilled, Detail: "sigterm"})
	time.Sleep(20 * time.Millisecond)
	result, err = registry.Kill(id, "", "again")
	if err != nil || result != KillAlreadyFinished {
		t.Fatalf("second kill result = %v %v", result, err)
	}
}

func TestWaitReleasesOnSettlementTimeoutAndCancel(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	hooks := newHooks()
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "job", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	// Timeout returns the live snapshot without cancelling.
	live, err := registry.Wait(id, "", 30, context.Background())
	if err != nil || live.Status != StatusRunning {
		t.Fatalf("timeout wait = %+v %v", live, err)
	}
	// Caller cancellation aborts the wait.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := registry.Wait(id, "", 10_000, ctx); err == nil || !strings.Contains(err.Error(), "wait aborted") {
		t.Fatalf("cancel wait = %v", err)
	}
	// Settlement releases the waiter, marks reported, and hands back the
	// terminal snapshot.
	done := make(chan Snapshot, 1)
	go func() {
		snapshot, _ := registry.Wait(id, "", 10_000, context.Background())
		done <- snapshot
	}()
	time.Sleep(20 * time.Millisecond)
	hooks.settle(Outcome{Status: OutcomeCompleted, Detail: "done"})
	snapshot := <-done
	if snapshot.Status != StatusCompleted || !snapshot.Reported {
		t.Fatalf("settled wait = %+v", snapshot)
	}
	// A terminal job answers immediately.
	final, err := registry.Wait(id, "", 10, context.Background())
	if err != nil || final.Status != StatusCompleted {
		t.Fatalf("terminal wait = %+v %v", final, err)
	}
	// Invalid timeout fails loud.
	if _, err := registry.Wait(id, "", 0, context.Background()); err == nil {
		t.Fatal("zero timeout accepted")
	}
}

func TestSettleFirstWinsAndProducerViolation(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	hooks := newHooks()
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "race", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	hooks.settle(Outcome{Status: OutcomeCompleted, Detail: "first"})
	time.Sleep(20 * time.Millisecond)
	// A late second settlement must not overwrite the record.
	hooks.settle(Outcome{Status: OutcomeFailed, Detail: "late"})
	time.Sleep(20 * time.Millisecond)
	snapshot, _ := registry.Get(id, "")
	if snapshot.Status != StatusCompleted || snapshot.Detail != "first" {
		t.Fatalf("first-wins = %+v", snapshot)
	}
	// A producer contract violation settles failed with the error detail.
	violation := newHooks()
	violation.done <- Result{Err: fmt.Errorf("callback exploded")}
	violatedID := mustStart(t, registry, StartSpec{Kind: "bash", Label: "v", Run: func() (Hooks, error) { return violation.hooks(), nil }})
	time.Sleep(20 * time.Millisecond)
	violated, _ := registry.Get(violatedID, "")
	if violated.Status != StatusFailed || !strings.Contains(violated.Detail, "callback exploded") {
		t.Fatalf("violation = %+v", violated)
	}
}

func TestScopedListenerDelivery(t *testing.T) {
	root := scope.NewScopeKey(nil)
	presetA := scope.NewScopeKey(root)
	presetB := scope.NewScopeKey(root)
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)

	var globalSeen, aSeen, bSeen []string
	registry.OnJobDoneIn(nil, func(snapshot Snapshot, owner Owner) { globalSeen = append(globalSeen, snapshot.ID) })
	registry.OnJobDoneIn(presetA, func(snapshot Snapshot, owner Owner) { aSeen = append(aSeen, snapshot.ID) })
	registry.OnJobDoneIn(presetB, func(snapshot Snapshot, owner Owner) { bSeen = append(bSeen, snapshot.ID) })

	ownerA := &stubOwner{id: "a", scope: presetA}
	hooks := newHooks()
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "x", Owner: ownerA, Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	hooks.settle(Outcome{Status: OutcomeCompleted})
	time.Sleep(20 * time.Millisecond)
	// Global and the owner's own chain deliver; a sibling preset does not.
	if len(globalSeen) != 1 || len(aSeen) != 1 || len(bSeen) != 0 {
		t.Fatalf("delivery = %v %v %v", globalSeen, aSeen, bSeen)
	}
	// A panicking listener is contained and does not block the rest.
	var afterSeen bool
	registry.OnJobDoneIn(nil, func(Snapshot, Owner) { panic("listener bug") })
	registry.OnJobDoneIn(nil, func(Snapshot, Owner) { afterSeen = true })
	otherHooks := newHooks()
	otherID := mustStart(t, registry, StartSpec{Kind: "bash", Label: "y", Run: func() (Hooks, error) { return otherHooks.hooks(), nil }})
	otherHooks.settle(Outcome{Status: OutcomeCompleted})
	time.Sleep(20 * time.Millisecond)
	if !afterSeen {
		t.Fatal("panic blocked later listeners")
	}
	if len(globalSeen) != 2 || globalSeen[1] != otherID {
		t.Fatalf("global after panic = %v", globalSeen)
	}
}

func TestJobsChangedNotification(t *testing.T) {
	root := scope.NewScopeKey(nil)
	registry := newRegistry(t, 10)
	detachController := registry.AttachControllerIn(nil)
	var unownedEvents, otherEvents int
	registry.OnJobsChangedIn(nil, func(owner Owner) {
		if owner == nil {
			unownedEvents++
		} else {
			otherEvents++
		}
	})
	hooks := newHooks()
	id := mustStart(t, registry, StartSpec{Kind: "bash", Label: "x", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	registry.Kill(id, "", "stop") // stopping transition notifies
	hooks.settle(Outcome{Status: OutcomeKilled})
	time.Sleep(20 * time.Millisecond)
	if unownedEvents != 3 { // registration + stopping + settlement
		t.Fatalf("unowned events = %d", unownedEvents)
	}
	// Owner-disposal removal notifies with the exact owner.
	owner := &stubOwner{id: "a", scope: root}
	ownedHooks := newHooks()
	ownedHooks.autoSettle = true // a compliant producer settles on cancel
	ownedID := mustStart(t, registry, StartSpec{Kind: "bash", Label: "y", Owner: owner, Run: func() (Hooks, error) { return ownedHooks.hooks(), nil }})
	registry.DisposeOwner(owner)
	time.Sleep(20 * time.Millisecond)
	// Owned events: registration + the teardown stopping transition + the
	// producer settlement + the removal.
	if otherEvents != 4 {
		t.Fatalf("owner events = %d", otherEvents)
	}
	if _, err := registry.Get(ownedID, "a"); err == nil {
		t.Fatal("disposed owner's job survived")
	}
	// A controller detached from a foreign scope still receives the
	// disposal emptying (its layer is reachable from the global table).
	detachController()
}

func TestDisposeCancelsAndClosesListeners(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	hooks := newHooks()
	hooks.err = fmt.Errorf("no process tree")
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "live", Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	var doneSeen bool
	registry.OnJobDoneIn(nil, func(Snapshot, Owner) { doneSeen = true })
	var emptied bool
	registry.OnJobsChangedIn(nil, func(Owner) { emptied = true })
	registry.Dispose()
	// The throwing teardown cancel was attempted with the disposal reason.
	if len(hooks.cancels) != 1 || hooks.cancels[0] != "jobs service disposed" {
		t.Fatalf("teardown cancels = %v", hooks.cancels)
	}
	// The store is emptied.
	if jobs := registry.List(""); len(jobs) != 0 {
		t.Fatalf("records survived disposal: %+v", jobs)
	}
	if !emptied {
		t.Fatal("disposal emptying was not announced")
	}
	// Listeners are closed: no settlement announcement fires after
	// disposal.
	if doneSeen {
		t.Fatal("listener ran after disposal")
	}
	// New listeners never fire after disposal.
	called := false
	registry.OnJobDoneIn(nil, func(Snapshot, Owner) { called = true })
	hooks.settle(Outcome{Status: OutcomeCompleted})
	time.Sleep(20 * time.Millisecond)
	if called {
		t.Fatal("post-dispose listener fired")
	}
}

func TestDisposeOwnerAwaitsSettlement(t *testing.T) {
	registry := newRegistry(t, 10)
	registry.AttachControllerIn(nil)
	owner := &stubOwner{id: "a"}
	hooks := newHooks()
	hooks.autoSettle = true
	mustStart(t, registry, StartSpec{Kind: "bash", Label: "owned", Owner: owner, Run: func() (Hooks, error) { return hooks.hooks(), nil }})
	// DisposeOwner blocks until the producer releases.
	registry.DisposeOwner(owner)
	snapshot := registry.List("a")
	if len(snapshot) != 0 {
		t.Fatalf("records survived owner disposal: %+v", snapshot)
	}
	if len(hooks.cancels) != 1 || hooks.cancels[0] != "owner disposed" {
		t.Fatalf("teardown cancels = %v", hooks.cancels)
	}
}
