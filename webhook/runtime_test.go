package webhook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// funcRule drives one Run through a channel so tests control settlement.
type funcRule struct {
	id, kind string
	run      func(delivery VerifiedWebhookDelivery, signal context.Context) (*WebhookSessionRequest, error)
}

func (r *funcRule) ID() string   { return r.id }
func (r *funcRule) Kind() string { return r.kind }
func (r *funcRule) Run(delivery VerifiedWebhookDelivery, signal context.Context) (*WebhookSessionRequest, error) {
	return r.run(delivery, signal)
}

// captureLogger records warnings and errors.
type captureLogger struct {
	mu       sync.Mutex
	warnings []string
	errors   []string
}

func (l *captureLogger) Info(args ...any) {}
func (l *captureLogger) Warn(args ...any) {
	l.mu.Lock()
	l.warnings = append(l.warnings, fmt.Sprint(args...))
	l.mu.Unlock()
}
func (l *captureLogger) Error(args ...any) {
	l.mu.Lock()
	l.errors = append(l.errors, fmt.Sprint(args...))
	l.mu.Unlock()
}

func sampleDelivery() VerifiedWebhookDelivery {
	return VerifiedWebhookDelivery{
		Kind:       "github",
		Source:     "primary-github",
		DeliveryID: "d-1",
		Event:      map[string]any{"action": "opened"},
		ReceivedAt: 1700000000000,
	}
}

func TestRegisterValidatesTheRule(t *testing.T) {
	runtime := NewWebhookRuntime(nil, nil)
	cases := []struct {
		rule  Rule
		using func(Rule) Rule
		want  string
	}{
		{&funcRule{id: "", kind: "github"}, func(r Rule) Rule { return r }, "webhook rule id must be a non-empty string"},
		{&funcRule{id: "  ", kind: "github"}, func(r Rule) Rule { return r }, "webhook rule id must be a non-empty string"},
		{&funcRule{id: "r1", kind: ""}, func(r Rule) Rule { return r }, `webhook rule "r1" kind must be a non-empty string`},
	}
	for _, testCase := range cases {
		if _, err := runtime.Register(testCase.rule); err == nil || err.Error() != testCase.want {
			t.Fatalf("err = %v, want %q", err, testCase.want)
		}
	}
	// Duplicate ids fail loud.
	good := &funcRule{id: "dup", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) { return nil, nil }}
	undo, err := runtime.Register(good)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := runtime.Register(good); err == nil || err.Error() != `webhook rule "dup" is already registered` {
		t.Fatalf("err = %v, want the duplicate rejection", err)
	}
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// After disposal the id is free again.
	if _, err := runtime.Register(good); err != nil {
		t.Fatalf("re-register: %v", err)
	}
}

func TestDispatchValidatesAndDetachesTheDelivery(t *testing.T) {
	runtime := NewWebhookRuntime(nil, nil)
	cases := []struct {
		delivery VerifiedWebhookDelivery
		want     string
	}{
		{VerifiedWebhookDelivery{Source: "s", DeliveryID: "d", ReceivedAt: 1}, "webhook delivery kind must be a non-empty string"},
		{VerifiedWebhookDelivery{Kind: "github", DeliveryID: "d", ReceivedAt: 1}, "webhook delivery source must be a non-empty string"},
		{VerifiedWebhookDelivery{Kind: "github", Source: "s", ReceivedAt: 1}, "webhook delivery id must be a non-empty string"},
		{VerifiedWebhookDelivery{Kind: "github", Source: "s", DeliveryID: "d", ReceivedAt: -1}, "webhook delivery receivedAt must be a non-negative safe integer"},
	}
	for _, testCase := range cases {
		if err := runtime.Dispatch(testCase.delivery); err == nil || err.Error() != testCase.want {
			t.Fatalf("err = %v, want %q", err, testCase.want)
		}
	}
	// A non-JSON event fails the lossless gate.
	if err := runtime.Dispatch(VerifiedWebhookDelivery{
		Kind: "github", Source: "s", DeliveryID: "d", ReceivedAt: 1,
		Event: make(chan int),
	}); err == nil || err.Error() != "webhook delivery must be lossless JSON" {
		t.Fatalf("err = %v, want the lossless rejection", err)
	}
}

func TestDispatchRoutesByKindAndDetaches(t *testing.T) {
	logger := &captureLogger{}
	created := make(chan WebhookSessionRequest, 1)
	runtime := NewWebhookRuntime(logger, func(delivery VerifiedWebhookDelivery, ruleID string, request WebhookSessionRequest, signal context.Context) error {
		created <- request
		return nil
	})
	observed := make(chan VerifiedWebhookDelivery, 1)
	undo, err := runtime.Register(&funcRule{id: "rules/notify", kind: "github", run: func(delivery VerifiedWebhookDelivery, signal context.Context) (*WebhookSessionRequest, error) {
		observed <- delivery
		return &WebhookSessionRequest{WorkspacePath: "/ws", Title: "T", Prompt: "P", AgentPreset: "default", PermissionPreset: "default"}, nil
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = undo() })
	// A mismatched kind never invokes the rule.
	if err := runtime.Dispatch(VerifiedWebhookDelivery{Kind: "gitlab", Source: "s", DeliveryID: "d", ReceivedAt: 1}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	select {
	case <-observed:
		t.Fatal("a gitlab delivery reached a github rule")
	case <-time.After(50 * time.Millisecond):
	}

	delivery := sampleDelivery()
	if err := runtime.Dispatch(delivery); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	got := <-observed
	if got.Kind != "github" || got.DeliveryID != "d-1" {
		t.Fatalf("delivery = %+v", got)
	}
	// Detachment: mutating the caller's map never reaches the snapshot.
	delivery.Event.(map[string]any)["action"] = "closed"
	request := <-created
	if request.WorkspacePath != "/ws" || request.Title != "T" {
		t.Fatalf("request = %+v", request)
	}
}

func TestInvocationFailureIsContainedAndLogged(t *testing.T) {
	logger := &captureLogger{}
	runtime := NewWebhookRuntime(logger, nil)
	boom := errors.New("rule exploded")
	undo, err := runtime.Register(&funcRule{id: "bad", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) {
		return nil, boom
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = undo() })
	if err := runtime.Dispatch(sampleDelivery()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logger.mu.Lock()
		count := len(logger.warnings)
		logger.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.warnings) != 1 {
		t.Fatalf("warnings = %v", logger.warnings)
	}
	want := `webhook: provider="github" source="primary-github" delivery="d-1" rule="bad" failed: rule exploded`
	if logger.warnings[0] != want {
		t.Fatalf("warning = %q, want %q", logger.warnings[0], want)
	}
}

func TestPanickingRuleIsContained(t *testing.T) {
	logger := &captureLogger{}
	runtime := NewWebhookRuntime(logger, nil)
	undo, err := runtime.Register(&funcRule{id: "panic", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) {
		panic("rule blew up")
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = undo() })
	// Dispatch returns immediately; the panic must not escape the runtime.
	if err := runtime.Dispatch(sampleDelivery()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logger.mu.Lock()
		count := len(logger.warnings)
		logger.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.warnings) != 1 || !strings.Contains(logger.warnings[0], "failed: rule blew up") {
		t.Fatalf("warnings = %v", logger.warnings)
	}
}

func TestDisposeAbortsAndDrainsActiveInvocations(t *testing.T) {
	logger := &captureLogger{}
	runtime := NewWebhookRuntime(logger, nil)
	started := make(chan struct{})
	observedAbort := make(chan struct{}, 1)
	undo, err := runtime.Register(&funcRule{id: "slow", kind: "github", run: func(_ VerifiedWebhookDelivery, signal context.Context) (*WebhookSessionRequest, error) {
		close(started)
		<-signal.Done()
		observedAbort <- struct{}{}
		return nil, errors.New("interrupted")
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := runtime.Dispatch(sampleDelivery()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	<-started
	// Disposal waits for the drained callback and the abort is observable.
	if err := undo(); err != nil {
		t.Fatalf("undo: %v", err)
	}
	select {
	case <-observedAbort:
	default:
		t.Fatal("the callback never observed the abort")
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.errors) != 1 || !strings.Contains(logger.errors[0], "stopped after disposal: interrupted") {
		t.Fatalf("errors = %v", logger.errors)
	}
	// After the rule's disposal, dispatch still succeeds — the disposed rule
	// simply no longer matches (only the runtime's own close rejects).
	if err := runtime.Dispatch(sampleDelivery()); err != nil {
		t.Fatalf("dispatch after rule disposal: %v", err)
	}
}

func TestRuntimeDisposeClosesEverything(t *testing.T) {
	runtime := NewWebhookRuntime(nil, nil)
	undo, err := runtime.Register(&funcRule{id: "r", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) { return nil, nil }})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	runtime.Dispose()
	// Idempotent.
	runtime.Dispose()
	if err := undo(); err != nil {
		t.Fatalf("undo after dispose: %v", err)
	}
	if _, err := runtime.Register(&funcRule{id: "late", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) { return nil, nil }}); err == nil || err.Error() != "webhook runtime is closing" {
		t.Fatalf("err = %v, want the closing rejection", err)
	}
}

func TestNilCreatorFailsLoud(t *testing.T) {
	logger := &captureLogger{}
	runtime := NewWebhookRuntime(logger, nil)
	undo, err := runtime.Register(&funcRule{id: "want-session", kind: "github", run: func(VerifiedWebhookDelivery, context.Context) (*WebhookSessionRequest, error) {
		return &WebhookSessionRequest{WorkspacePath: "/ws", Title: "T", Prompt: "P", AgentPreset: "a", PermissionPreset: "p"}, nil
	}})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = undo() })
	if err := runtime.Dispatch(sampleDelivery()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logger.mu.Lock()
		count := len(logger.warnings)
		logger.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	if len(logger.warnings) != 1 || !strings.Contains(logger.warnings[0], "no webhook Session creator is composed") {
		t.Fatalf("warnings = %v", logger.warnings)
	}
}
