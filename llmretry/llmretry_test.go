package llmretry

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

func newRetryAgent(t *testing.T, id string, registry *agent.AgentRegistry) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"})
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

func normalPolicy(maxRetries int64) llm.ResolvedRetryPolicy {
	return llm.ResolvedRetryPolicy{
		Mode:           llm.RetryModeNormal,
		MaxRetries:     maxRetries,
		RetryableCodes: []string{"RATE_LIMIT", "SERVER"},
		InitialDelayMs: 1,
		MaxDelayMs:     4,
		JitterRatio:    0,
	}
}

func deterministicInternals() Internals {
	return Internals{Random: func() float64 { return 0.5 }}
}

func countEvents(sess *session.Session, eventType string) int {
	count := 0
	for _, event := range sess.Events() {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func TestRecoverRetriesDurableBeforeWait(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	undo, err := Register(registry, cordis.Discard{}, deterministicInternals())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	caller := newRetryAgent(t, "retry-1", registry)

	decided := registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent:    caller,
		Turn:     3,
		Step:     1,
		Provider: "deepseek",
		Failure:  llm.LlmFailure{Message: "rate limited", Code: "RATE_LIMIT", Status: 429, ProviderRetryAfterMs: 1},
		RetryPolicy: func() *llm.ResolvedRetryPolicy {
			policy := normalPolicy(3)
			return &policy
		}(),
	}, func(agent.RequestErrorPayload) agent.RequestErrorAction { return agent.RequestErrorAction{} })
	if !decided.Retry {
		t.Fatal("retryable failure did not yield a retry decision")
	}
	// Durable before wait: both the schedule and the started records exist.
	if countEvents(caller.Session, EventLlmRetry) != 1 || countEvents(caller.Session, EventLlmRetryStarted) != 1 {
		t.Fatal("llm/retry or llm/retry-started record missing")
	}
	// One more retry succeeds; the chain shares the retryId.
	registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 3, Step: 1, Provider: "deepseek",
		Failure: llm.LlmFailure{Code: "RATE_LIMIT"},
		RetryPolicy: func() *llm.ResolvedRetryPolicy {
			policy := normalPolicy(3)
			return &policy
		}(),
	}, func(agent.RequestErrorPayload) agent.RequestErrorAction { return agent.RequestErrorAction{} })
	if countEvents(caller.Session, EventLlmRetry) != 2 {
		t.Fatal("second schedule missing")
	}
	var first, second string
	index := 0
	for _, event := range caller.Session.Events() {
		if event.Type == EventLlmRetry {
			raw, _ := json.Marshal(event.Data)
			var data map[string]any
			_ = json.Unmarshal(raw, &data)
			if index == 0 {
				first = data["retryId"].(string)
			} else {
				second = data["retryId"].(string)
			}
			index++
		}
	}
	if first == "" || first != second {
		t.Fatalf("retry chain ids diverged: %q vs %q", first, second)
	}
}

func TestRecoverExhaustedPolicyDelegates(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	undo, err := Register(registry, cordis.Discard{}, deterministicInternals())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	caller := newRetryAgent(t, "retry-2", registry)
	policy := normalPolicy(1)
	delegateCalled := 0
	base := func(agent.RequestErrorPayload) agent.RequestErrorAction {
		delegateCalled++
		return agent.RequestErrorAction{}
	}
	// The first schedule fits the cap: one retry is granted.
	decided := registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 1, Step: 1, Provider: "deepseek",
		Failure:     llm.LlmFailure{Code: "RATE_LIMIT"},
		RetryPolicy: &policy,
	}, base)
	if !decided.Retry {
		t.Fatal("in-cap policy did not grant its retry")
	}
	// The chain is now exhausted: the request delegates downstream.
	decided = registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 1, Step: 1, Provider: "deepseek",
		Failure:     llm.LlmFailure{Code: "RATE_LIMIT"},
		RetryPolicy: &policy,
	}, base)
	if delegateCalled != 1 || decided.Retry {
		t.Fatalf("exhausted policy: delegate=%d retry=%v", delegateCalled, decided.Retry)
	}
	if countEvents(caller.Session, EventLlmRetry) != 1 {
		t.Fatal("exhausted policy scheduled again")
	}
}

func TestRecoverSkipsNonRetryableAndPolicyless(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	undo, err := Register(registry, cordis.Discard{}, deterministicInternals())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	caller := newRetryAgent(t, "retry-3", registry)
	calls := 0
	base := func(agent.RequestErrorPayload) agent.RequestErrorAction { calls++; return agent.RequestErrorAction{} }
	// No policy: delegate.
	registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 1, Step: 1, Provider: "deepseek", Failure: llm.LlmFailure{Code: "RATE_LIMIT"},
	}, base)
	// Non-retryable code: delegate.
	policy := normalPolicy(3)
	registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 1, Step: 1, Provider: "deepseek",
		Failure: llm.LlmFailure{Code: "AUTH"}, RetryPolicy: &policy,
	}, base)
	if calls != 2 {
		t.Fatalf("delegate calls: %d", calls)
	}
	if countEvents(caller.Session, EventLlmRetry) != 0 {
		t.Fatal("non-retryable failure recorded a schedule")
	}
}

func TestAlwaysPolicyIgnoresProviderDelayAndUsesLocal(t *testing.T) {
	var samples atomic.Int64
	registry := agent.NewAgentRegistry(nil, nil)
	internals := Internals{Random: func() float64 {
		samples.Add(1)
		return 0.5
	}}
	undo, err := Register(registry, cordis.Discard{}, internals)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	caller := newRetryAgent(t, "retry-4", registry)
	always := llm.ResolvedRetryPolicy{
		Mode:           llm.RetryModeAlways,
		InitialDelayMs: 1,
		MaxDelayMs:     4,
		JitterRatio:    0,
	}
	// A provider delay beyond maxDelayMs is ignored under the always
	// policy: local backoff applies (the jitter sample is consumed).
	decided := registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 2, Step: 1, Provider: "deepseek",
		Failure:     llm.LlmFailure{Code: "SERVER", ProviderRetryAfterMs: 60_000},
		RetryPolicy: &always,
	}, func(agent.RequestErrorPayload) agent.RequestErrorAction { return agent.RequestErrorAction{} })
	if !decided.Retry || samples.Load() != 1 {
		t.Fatalf("always policy: retry=%v samples=%d", decided.Retry, samples.Load())
	}
	// A within-cap provider delay is honored verbatim (no jitter sample).
	decided = registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
		Agent: caller, Turn: 3, Step: 1, Provider: "deepseek",
		Failure:     llm.LlmFailure{Code: "SERVER", ProviderRetryAfterMs: 2},
		RetryPolicy: &always,
	}, func(agent.RequestErrorPayload) agent.RequestErrorAction { return agent.RequestErrorAction{} })
	if !decided.Retry || samples.Load() != 1 {
		t.Fatalf("provider delay: retry=%v samples=%d", decided.Retry, samples.Load())
	}
}

func TestDisposeCancelsActiveWait(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, nil)
	undo, err := Register(registry, cordis.Discard{}, deterministicInternals())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	caller := newRetryAgent(t, "retry-5", registry)
	// A slow schedule is in flight; disposal must cancel its wait. The
	// provider delay is within the policy cap, so the wait would run for
	// 30 seconds uncancelled.
	slowPolicy := llm.ResolvedRetryPolicy{
		Mode:           llm.RetryModeNormal,
		MaxRetries:     3,
		RetryableCodes: []string{"SERVER"},
		InitialDelayMs: 1,
		MaxDelayMs:     60_000,
		JitterRatio:    0,
	}
	done := make(chan agent.RequestErrorAction, 1)
	go func() {
		done <- registry.Events().RequestError().Dispatch(caller.Scope, agent.RequestErrorPayload{
			Agent: caller, Turn: 4, Step: 1, Provider: "deepseek",
			Failure:     llm.LlmFailure{Code: "SERVER", ProviderRetryAfterMs: 30_000},
			RetryPolicy: &slowPolicy,
		}, func(agent.RequestErrorPayload) agent.RequestErrorAction { return agent.RequestErrorAction{} })
	}()
	time.Sleep(20 * time.Millisecond)
	undo()
	select {
	case decided := <-done:
		if decided.Retry {
			t.Fatal("disposed wait produced a retry decision")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispose did not unblock the active wait")
	}
	// The schedule is durable, but the aborted wait never started.
	if countEvents(caller.Session, EventLlmRetry) != 1 {
		t.Fatal("schedule record missing")
	}
	if countEvents(caller.Session, EventLlmRetryStarted) != 0 {
		t.Fatal("aborted wait recorded a start")
	}
}
