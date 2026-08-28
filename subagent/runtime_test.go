package subagent

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// recordingBusListener collects lifecycle payloads in emission order.
type recordingBusListener struct {
	mu      sync.Mutex
	hits    []string
	release chan struct{}
}

func newRecordingListener() *recordingBusListener {
	return &recordingBusListener{release: make(chan struct{}, 16)}
}

func (l *recordingBusListener) attach(bus *agent.SubjectEventBus) {
	bus.OnEmit(EventSubagentStart, nil, func(payload any) error {
		info := payload.(SubagentRunInfo)
		l.mu.Lock()
		l.hits = append(l.hits, "start:"+info.Provider+":"+string(info.ID))
		l.mu.Unlock()
		l.release <- struct{}{}
		return nil
	})
	bus.OnEmit(EventSubagentEnd, nil, func(payload any) error {
		end := payload.(SubagentRunEndInfo)
		l.mu.Lock()
		l.hits = append(l.hits, fmt.Sprintf("end:%s:%s", end.StopReason, end.RunID))
		l.mu.Unlock()
		l.release <- struct{}{}
		return nil
	})
	bus.OnEmit(EventProviderRemoved, nil, func(payload any) error {
		l.mu.Lock()
		l.hits = append(l.hits, "removed:"+payload.(string))
		l.mu.Unlock()
		l.release <- struct{}{}
		return nil
	})
}

func (l *recordingBusListener) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.hits...)
}

func (l *recordingBusListener) waitFor(count int) bool {
	deadline := time.After(5 * time.Second)
	for i := 0; i < count; i++ {
		select {
		case <-l.release:
		case <-deadline:
			return false
		}
	}
	return true
}

// fakeRun is a controllable one-shot run.
type fakeRun struct {
	id       session.SessionID
	local    *agent.Agent
	result   chan outcome
	done     chan struct{}
	disposed int
}

type outcome struct {
	result SubagentResult
	err    error
}

func (f *fakeRun) ID() session.SessionID           { return f.id }
func (f *fakeRun) LocalAgent() *agent.Agent        { return f.local }
func (f *fakeRun) Result() (SubagentResult, error) { o := <-f.result; return o.result, o.err }
func (f *fakeRun) Dispose() error                  { f.disposed++; closeOnce(f.done); return nil }

func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// fakeProvider records Start calls and hands back scripted runs.
type fakeProvider struct {
	name         string
	caps         SubagentCapabilities
	continuable  bool
	starts       []ResolvedSubagentStartRequest
	run          *fakeRun
	startErr     error
	prepareCalls int
}

func (p *fakeProvider) Name() string                       { return p.name }
func (p *fakeProvider) Capabilities() SubagentCapabilities { return p.caps }
func (p *fakeProvider) InheritsParentContext() bool        { return false }
func (p *fakeProvider) Start(request ResolvedSubagentStartRequest) (SubagentRun, error) {
	p.starts = append(p.starts, request)
	if p.startErr != nil {
		return nil, p.startErr
	}
	return p.run, nil
}

func (p *fakeProvider) PrepareContinuable(request ContinuableCreateRequest) (ContinuableCreateSpec, error) {
	p.prepareCalls++
	return ContinuableCreateSpec{Seed: nil}, nil
}

func newTestRuntime(t *testing.T) (*SubagentRuntime, *agent.SubjectEventBus) {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	runtime := NewSubagentRuntime(RuntimeConfig{Logger: cordis.Discard{}, Events: registry.Events()})
	return runtime, registry.Events()
}

func TestProviderRegistryLifecycle(t *testing.T) {
	runtime, bus := newTestRuntime(t)
	listener := newRecordingListener()
	listener.attach(bus)

	provider := &fakeProvider{name: "spawn"}
	dispose, err := runtime.RegisterProvider(provider)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := runtime.List(); len(got) != 1 || got[0] != "spawn" {
		t.Fatalf("list = %v", got)
	}
	// Duplicate registration fails loud without disturbing the original.
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "spawn"}); err == nil ||
		asCode(err) != CodeDuplicateProvider {
		t.Fatalf("duplicate = %v, want DUPLICATE_PROVIDER", err)
	}
	// Insertion order is preserved across names.
	if _, err := runtime.RegisterProvider(&fakeProvider{name: "fork"}); err != nil {
		t.Fatalf("register fork: %v", err)
	}
	if got := runtime.List(); len(got) != 2 || got[0] != "spawn" || got[1] != "fork" {
		t.Fatalf("list = %v, want insertion order", got)
	}
	// Disposal removes exactly once and notifies unscoped.
	dispose()
	dispose()
	if _, ok := runtime.GetProvider("spawn"); ok {
		t.Fatal("disposed provider must leave the registry")
	}
	if !listener.waitFor(1) {
		t.Fatalf("hits = %v, want one removed edge", listener.snapshot())
	}
	if got := listener.snapshot(); len(got) != 1 || got[0] != "removed:spawn" {
		t.Fatalf("hits = %v, want exactly one removal", got)
	}
	// Removal blocks new starts but does not revoke existing registrations.
	if _, err := runtime.Start("spawn", SubagentStartRequest{}); err == nil || asCode(err) != CodeNoProvider {
		t.Fatalf("start after removal = %v, want NO_PROVIDER", err)
	}
}

// asCode reads a SubagentError's stable code through the chain.
func asCode(err error) string {
	var subagentErr SubagentError
	if asSubagentError(err, &subagentErr) {
		return subagentErr.Code()
	}
	return "<not-subagent-error>"
}

func TestStartHappyPathAndLifecyclePair(t *testing.T) {
	runtime, bus := newTestRuntime(t)
	listener := newRecordingListener()
	listener.attach(bus)

	child := &agent.Agent{}
	run := &fakeRun{id: "child-1", local: child, result: make(chan outcome, 1)}
	run.result <- outcome{result: SubagentResult{
		Output:     []llm.ContentBlock{{Type: llm.BlockText, Text: "done"}},
		StopReason: StopCompleted,
	}}
	provider := &fakeProvider{name: "spawn", run: run, caps: SubagentCapabilities{
		AgentOptions: true, OutputSchema: true, DepthLimit: true, ToolFilter: true, Persona: true,
	}}
	if _, err := runtime.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}

	returned, err := runtime.Start("spawn", SubagentStartRequest{
		Label:        "Explore refs",
		Prompt:       []llm.ContentBlock{{Type: llm.BlockText, Text: "go"}},
		OutputSchema: map[string]any{"type": "object"},
		MaxDepth:     ptrInt64(2),
		Persona:      "scout",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if returned.ID() != "child-1" || returned.LocalAgent() != child {
		t.Fatalf("run identity = %s", returned.ID())
	}
	// The provider saw the resolved descriptor snapshot.
	if len(provider.starts) != 1 {
		t.Fatalf("starts = %d", len(provider.starts))
	}
	resolved := provider.starts[0]
	if resolved.Descriptor.Mode != ModeOneShot || resolved.Descriptor.Provider != "spawn" {
		t.Fatalf("descriptor = %+v", resolved.Descriptor)
	}
	if resolved.Descriptor.Label == nil || *resolved.Descriptor.Label != "Explore refs" {
		t.Fatalf("descriptor label = %+v", resolved.Descriptor.Label)
	}
	if resolved.OutputSchema == nil || resolved.MaxDepth == nil || resolved.Persona != "scout" {
		t.Fatalf("request fields lost: %+v", resolved.SubagentStartRequest)
	}
	// start → end, paired by run id, carrying the final output.
	if !listener.waitFor(2) {
		t.Fatalf("hits = %v", listener.snapshot())
	}
	hits := listener.snapshot()
	if len(hits) != 2 || hits[0] != "start:spawn:child-1" {
		t.Fatalf("hits = %v, want start first", hits)
	}
}

func ptrInt64(value int64) *int64 { return &value }

func TestStartCapabilityAndValidationRejections(t *testing.T) {
	runtime, _ := newTestRuntime(t)
	provider := &fakeProvider{name: "spawn", run: &fakeRun{id: "x", result: make(chan outcome, 1)}}
	if _, err := runtime.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A capability the provider lacks rejects before delegation.
	negative := int64(-1)
	if _, err := runtime.Start("spawn", SubagentStartRequest{MaxDepth: &negative}); err == nil ||
		asCode(err) != CodeUnsupportedCapability {
		t.Fatalf("depthLimit = %v, want UNSUPPORTED_CAPABILITY", err)
	}
	if _, err := runtime.Start("spawn", SubagentStartRequest{Persona: "scout"}); err == nil ||
		asCode(err) != CodeUnsupportedCapability {
		t.Fatalf("persona = %v, want UNSUPPORTED_CAPABILITY", err)
	}
	// The provider never saw a start.
	if len(provider.starts) != 0 {
		t.Fatal("rejections must not reach the provider")
	}
	// A negative maxDepth fails even with the capability.
	provider.caps.DepthLimit = true
	if _, err := runtime.Start("spawn", SubagentStartRequest{MaxDepth: &negative}); err == nil ||
		!strings.Contains(err.Error(), "non-negative safe integer") {
		t.Fatalf("negative depth = %v", err)
	}
	// A non-object output schema fails the schema check.
	provider.caps.OutputSchema = true
	if _, err := runtime.Start("spawn", SubagentStartRequest{
		OutputSchema: map[string]any{"type": "string"},
	}); err == nil {
		t.Fatal("a non-object schema must reject")
	}
}

func TestStartInfrastructureFaultEmitsErrorEnd(t *testing.T) {
	runtime, bus := newTestRuntime(t)
	ends := make(chan SubagentRunEndInfo, 4)
	bus.OnEmit(EventSubagentEnd, nil, func(payload any) error {
		ends <- payload.(SubagentRunEndInfo)
		return nil
	})
	run := &fakeRun{id: "child-fault", result: make(chan outcome, 1)}
	run.result <- outcome{err: errors.New("transport exploded")}
	provider := &fakeProvider{name: "spawn", run: run}
	if _, err := runtime.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := runtime.Start("spawn", SubagentStartRequest{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	end := <-ends
	if end.StopReason != StopError || end.ID != "child-fault" {
		t.Fatalf("end = %s/%s, want error end for child-fault", end.StopReason, end.ID)
	}
	if len(end.LastAssistantMessage) != 0 {
		t.Fatal("a fault edge must withhold output")
	}
}

func TestEpochStopReasonVocabulary(t *testing.T) {
	cases := []struct {
		name string
		kind string
		want StopReason
	}{
		{"completed", session.TurnEndCompleted, StopCompleted},
		{"aborted", session.TurnEndAborted, StopAborted},
		{"interrupted", session.TurnEndInterrupted, StopAborted},
		{"error", session.TurnEndError, StopError},
		{"max-tokens", session.TurnEndMaxTokens, StopMaxTokens},
		{"blocked", session.TurnEndBlocked, StopRefusal},
		{"unknown variant", "wormholed", StopError},
	}
	for _, testCase := range cases {
		sess := newEpochSession(t)
		appendTurnArc(t, sess, 1, testCase.kind)
		if got := EpochStopReason(sess.Events()); got != testCase.want {
			t.Fatalf("%s: got %s, want %s", testCase.name, got, testCase.want)
		}
	}
	// No accounting turn at all: completed unless a cancelled queue says
	// otherwise. The bare-completed arc always accounts, so the empty-log
	// shape exercises the nil arm.
	if got := EpochStopReason(nil); got != StopCompleted {
		t.Fatalf("empty log = %s, want completed", got)
	}
}

// newEpochSession + appendTurnArc build one stepped turn ending with `kind`.
func newEpochSession(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID("epoch"), nil, nil)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return sess
}

func appendTurnArc(t *testing.T, sess *session.Session, turn int64, kind string) {
	t.Helper()
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: turn}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if _, err := sess.Append(session.EventStepStart, session.StepStartData{Turn: turn, Step: 1}, nil); err != nil {
		t.Fatalf("step/start: %v", err)
	}
	if _, err := sess.Append(session.EventTurnEnd, session.TurnEndData{Turn: turn, Reason: session.TurnEndReason{Kind: kind}}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
}

func TestActivationObserverEmitsPairedEdges(t *testing.T) {
	runtime, bus := newTestRuntime(t)
	var mu sync.Mutex
	var edges []string
	endReady := make(chan struct{}, 4)
	bus.OnEmit(EventSubagentStart, nil, func(payload any) error {
		mu.Lock()
		edges = append(edges, "start:"+(payload.(SubagentRunInfo)).RunID)
		mu.Unlock()
		return nil
	})
	bus.OnEmit(EventSubagentEnd, nil, func(payload any) error {
		end := payload.(SubagentRunEndInfo)
		mu.Lock()
		edges = append(edges, fmt.Sprintf("end:%s:%s:%d", end.RunID, end.StopReason, len(end.LastAssistantMessage)))
		mu.Unlock()
		endReady <- struct{}{}
		return nil
	})

	sess := newEpochSession(t)
	observer := runtime.ObserveActivation("spawn", "cont-1", nil)
	// Residency: the boundary is the log length at start.
	observer.Start(nil)
	appendTurnArc(t, sess, 1, session.TurnEndCompleted)
	observer.Capture(nil)
	terminal := observer.Terminal(nil)
	if terminal.StopReason != StopCompleted {
		t.Fatalf("terminal = %s, want completed", terminal.StopReason)
	}
	observer.Settle(nil)
	<-endReady
	mu.Lock()
	defer mu.Unlock()
	if len(edges) != 2 {
		t.Fatalf("edges = %v", edges)
	}
	// start:<runid> vs end:<runid>:<stop>:<blocks> — the run ids must pair.
	if len(edges[0]) < 7 || !strings.HasPrefix(edges[1], "end:"+edges[0][6:]) {
		t.Fatalf("edges = %v, start and end must share one run id", edges)
	}
}

func TestActivationObserverTeardownFailureOverrides(t *testing.T) {
	runtime, bus := newTestRuntime(t)
	ends := make(chan SubagentRunEndInfo, 4)
	bus.OnEmit(EventSubagentEnd, nil, func(payload any) error {
		ends <- payload.(SubagentRunEndInfo)
		return nil
	})
	sess := newEpochSession(t)
	observer := runtime.ObserveActivation("spawn", "cont-2", nil)
	observer.Start(nil)
	appendTurnArc(t, sess, 1, session.TurnEndCompleted)
	observer.Capture(nil)
	// Teardown failure overrides the epoch's own outcome and withholds its
	// output.
	observer.Settle(errors.New("durability failed"))
	end := <-ends
	if end.StopReason != StopError || len(end.LastAssistantMessage) != 0 {
		t.Fatalf("end = %s/%d blocks, want error with withheld output", end.StopReason, len(end.LastAssistantMessage))
	}
}

func TestContinuationUnavailableAndDelegation(t *testing.T) {
	runtime, _ := newTestRuntime(t)
	// Manager-less: continuable starts fail loud; interrupt and drains are
	// accepted no-ops (nothing can own a live Activation).
	if _, err := runtime.StartContinuable(ContinuableStartSpec{}); err == nil ||
		asCode(err) != CodeContinuationUnavailable {
		t.Fatalf("startContinuable = %v, want CONTINUATION_UNAVAILABLE", err)
	}
	if _, err := runtime.Followup(nil, "c", nil, SubagentFollowupOptions{}); err == nil ||
		asCode(err) != CodeContinuationUnavailable {
		t.Fatalf("followup = %v, want CONTINUATION_UNAVAILABLE", err)
	}
	if _, err := runtime.ReportFrom(nil, nil, SubagentReportOptions{}); err == nil ||
		asCode(err) != CodeContinuationUnavailable {
		t.Fatalf("report = %v, want CONTINUATION_UNAVAILABLE", err)
	}
	// Interrupt without a manager fails loud (the official runtime requires
	// the agents service for every continuable operation).
	if err := runtime.Interrupt("c", SubagentInterruptAuthority{}); err == nil ||
		asCode(err) != CodeContinuationUnavailable {
		t.Fatalf("interrupt without manager = %v, want CONTINUATION_UNAVAILABLE", err)
	}
	if err := runtime.DrainContinuableDescendants(nil); err != nil {
		t.Fatalf("drain without manager: %v", err)
	}
	if err := runtime.DrainContinuableChildren(nil, nil); err != nil {
		t.Fatalf("drain children without manager: %v", err)
	}

	// With a stub manager the operations delegate verbatim.
	manager := &stubContinuations{}
	runtime.SetContinuations(manager)
	if _, err := runtime.StartContinuable(ContinuableStartSpec{}); err != nil {
		t.Fatalf("delegated start: %v", err)
	}
	if _, err := runtime.Followup(nil, "c", nil, SubagentFollowupOptions{}); err != nil {
		t.Fatalf("delegated followup: %v", err)
	}
	runtime.Interrupt("c", SubagentInterruptAuthority{})
	if !manager.interrupted {
		t.Fatal("interrupt must reach the manager")
	}
	if err := runtime.DrainContinuableDescendants(nil); err != nil {
		t.Fatalf("delegated drain: %v", err)
	}
	// PrepareContinuable: unknown provider fails; a continuable provider is
	// asked; a non-continuable provider fails with the capability code.
	if _, err := runtime.PrepareContinuable("ghost", ContinuableCreateRequest{}); err == nil ||
		asCode(err) != CodeNoProvider {
		t.Fatalf("prepare unknown = %v, want NO_PROVIDER", err)
	}
	continuable := &fakeProvider{name: "spawn", continuable: true}
	if _, err := runtime.RegisterProvider(continuable); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := runtime.PrepareContinuable("spawn", ContinuableCreateRequest{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if continuable.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d", continuable.prepareCalls)
	}
}

// stubContinuations records delegation without doing work.
type stubContinuations struct {
	interrupted bool
}

func (s *stubContinuations) StartContinuable(spec ContinuableStartSpec) (ContinuableStart, error) {
	return ContinuableStart{ChildID: "c", MessageID: "m"}, nil
}

func (s *stubContinuations) Followup(parent *agent.Agent, childID session.SessionID, content []llm.ContentBlock, options SubagentFollowupOptions) (llm.MessageID, error) {
	return "m", nil
}

func (s *stubContinuations) Interrupt(targetSessionID session.SessionID, authority SubagentInterruptAuthority) error {
	s.interrupted = true
	return nil
}

func (s *stubContinuations) ReportFrom(child *agent.Agent, content []llm.ContentBlock, options SubagentReportOptions) (llm.MessageID, error) {
	return "m", nil
}

func (s *stubContinuations) DrainDescendants(parents []*agent.Agent) error { return nil }
func (s *stubContinuations) DrainChildren(parent *agent.Agent, childIDs []session.SessionID) error {
	return nil
}
