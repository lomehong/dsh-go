// Package llmretry ports @deepseek-ai/dsh-llm-retry: the provider-routed
// model-request retry policy on the agent loop's request-recovery extension
// point. Each scheduled retry is durable (the llm/retry event is appended)
// before its cancellable wait; a stale dispatch after disposal still sees
// the fused lifetime signal cancelled.
package llmretry

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/identity"
	"dshgo/llm"
	"dshgo/session"
)

// Session event names carried by this policy.
const (
	// EventLlmRetry records one scheduled retry: durable before the wait.
	EventLlmRetry = "llm/retry"
	// EventLlmRetryStarted records that the retry wait actually began.
	EventLlmRetryStarted = "llm/retry-started"
)

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventLlmRetry, EventLlmRetryStarted)
}

// RetryID pairs one retry chain's llm/retry records with its
// llm/retry-started record.
type RetryID string

// Internals carries non-serializable hooks used to make timing policy
// deterministic in tests.
type Internals struct {
	// Random is the inclusive zero-to-one jitter sample; nil selects
	// math/rand.
	Random func() float64
}

// Policy owns the provider-routed recovery listener. This executor has no
// config; providers own retryPolicy.
type Policy struct {
	rng        func() float64
	lifetime   context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	mu         sync.Mutex
	disposed   bool
	onDisposed func()
}

// ValidateConfig fails closed on any executor-level key: retryPolicy
// belongs under each provider configuration.
func ValidateConfig(config map[string]any) error {
	for key := range config {
		if key == "retryPolicy" {
			return fmt.Errorf("llm-retry: retryPolicy belongs under each provider configuration")
		}
		return fmt.Errorf("llm-retry: unknown key %q", key)
	}
	return nil
}

// Register installs provider-routed normal or unbounded request recovery on
// the registry's agent/request-error waterfall. The returned disposer
// removes the listener, cancels the lifetime, and drains active recovery.
func Register(registry *agent.AgentRegistry, logger cordis.Logger, internals Internals) (func(), error) {
	if registry == nil {
		return nil, fmt.Errorf("llm-retry: an agent registry is required")
	}
	rng := internals.Random
	if rng == nil {
		rng = rand.Float64
	}
	lifetime, cancel := context.WithCancel(context.Background())
	policy := &Policy{rng: rng, lifetime: lifetime, cancel: cancel}
	undo := registry.Events().RequestError().On(nil, func(payload agent.RequestErrorPayload, next func(agent.RequestErrorPayload) agent.RequestErrorAction) agent.RequestErrorAction {
		policy.mu.Lock()
		stale := policy.disposed
		policy.mu.Unlock()
		if stale {
			// A dispatch may have captured this callback before its
			// registration was removed; lifetime cancellation must keep
			// the stale callback out of downstream policies.
			return agent.RequestErrorAction{}
		}
		policy.wg.Add(1)
		defer policy.wg.Done()
		return policy.recover(payload, next, logger)
	})
	return func() {
		policy.mu.Lock()
		policy.disposed = true
		policy.mu.Unlock()
		cancel()
		policy.wg.Wait()
		undo()
	}, nil
}

// localDelay is bounded exponential backoff with symmetric jitter.
func localDelay(config llm.ResolvedRetryPolicy, retry int64, rng func() float64) int64 {
	exponent := int(retry) - 1
	if exponent > 1024 {
		exponent = 1024
	}
	if exponent < 0 {
		exponent = 0
	}
	exponential := config.InitialDelayMs * (1 << uint(exponent))
	if exponential > config.MaxDelayMs {
		exponential = config.MaxDelayMs
	}
	jitter := 1 - config.JitterRatio + 2*config.JitterRatio*rng()
	delay := int64(float64(exponential) * jitter)
	if delay > config.MaxDelayMs {
		delay = config.MaxDelayMs
	}
	return delay
}

func policyKey(policy llm.ResolvedRetryPolicy) string {
	document := map[string]any{
		"mode":           policy.Mode,
		"initialDelayMs": policy.InitialDelayMs,
		"maxDelayMs":     policy.MaxDelayMs,
		"jitterRatio":    policy.JitterRatio,
	}
	if policy.Mode == llm.RetryModeNormal {
		codes := append([]string(nil), policy.RetryableCodes...)
		sort.Strings(codes)
		document["maxRetries"] = policy.MaxRetries
		document["retryableCodes"] = codes
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return policy.Mode
	}
	return string(raw)
}

func retryable(policy llm.ResolvedRetryPolicy, code string) bool {
	for _, candidate := range policy.RetryableCodes {
		if candidate == code {
			return true
		}
	}
	return false
}

// recover is the request-error listener body: delegate immediately when no
// policy or a non-retryable failure, otherwise schedule one durable,
// cancellable retry.
func (p *Policy) recover(payload agent.RequestErrorPayload, next func(agent.RequestErrorPayload) agent.RequestErrorAction, logger cordis.Logger) agent.RequestErrorAction {
	policy := payload.RetryPolicy
	if policy == nil {
		return next(payload)
	}
	if policy.Mode == llm.RetryModeAlways {
		if p.aborted(payload.Signal) {
			return agent.RequestErrorAction{}
		}
		// The loop and the plugin stay open until delegated recovery
		// settles; the downstream retry decision wins.
		downstream := next(payload)
		if p.aborted(payload.Signal) {
			return agent.RequestErrorAction{}
		}
		if downstream.Retry {
			return downstream
		}
	} else if !retryable(*policy, payload.Failure.Code) {
		return next(payload)
	}

	key := policyKey(*policy)
	prior, priorRetryID := p.findPriorRetry(payload.Agent, payload.Turn, payload.Step, payload.Provider, key)
	if policy.Mode == llm.RetryModeNormal && prior >= policy.MaxRetries {
		return next(payload)
	}
	retry := prior + 1
	retryID := priorRetryID
	if retryID == "" {
		retryID = RetryID(identity.RandomUUID())
	}
	var delayMs int64
	if after := payload.Failure.ProviderRetryAfterMs; after > 0 {
		if after > policy.MaxDelayMs {
			if policy.Mode == llm.RetryModeNormal {
				return next(payload)
			}
			delayMs = localDelay(*policy, retry, p.rng)
		} else {
			delayMs = after
		}
	} else {
		delayMs = localDelay(*policy, retry, p.rng)
	}
	return p.backoff(payload, *policy, key, retry, retryID, delayMs, logger)
}

// findPriorRetry scans the session log backwards for this
// turn/step/provider/policy chain's latest scheduled retry.
func (p *Policy) findPriorRetry(agentObj *agent.Agent, turn, step int64, provider, key string) (int64, RetryID) {
	var (
		retry   int64
		retryID RetryID
	)
	events := agentObj.Session.Events()
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventLlmRetry {
			continue
		}
		var data struct {
			Turn      int64  `json:"turn"`
			Step      int64  `json:"step"`
			Provider  string `json:"provider"`
			PolicyKey string `json:"policyKey"`
			Retry     int64  `json:"retry"`
			RetryID   string `json:"retryId"`
		}
		raw, err := json.Marshal(event.Data)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			continue
		}
		if data.Turn == turn && data.Step == step && data.Provider == provider && data.PolicyKey == key {
			return data.Retry, RetryID(data.RetryID)
		}
	}
	return retry, retryID
}

// aborted reports whether the request signal or the plugin lifetime is
// already cancelled.
func (p *Policy) aborted(signal context.Context) bool {
	if signal != nil && signal.Err() != nil {
		return true
	}
	return p.lifetime.Err() != nil
}

// fused returns a context done when either the request signal or the
// plugin lifetime cancels.
func (p *Policy) fused(signal context.Context) context.Context {
	if signal == nil {
		return p.lifetime
	}
	fused, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		select {
		case <-signal.Done():
		case <-p.lifetime.Done():
		case <-fused.Done():
		}
	}()
	return fused
}

// backoff appends the durable llm/retry record, waits cancellably, and —
// when not aborted — appends llm/retry-started and demands the retry.
func (p *Policy) backoff(payload agent.RequestErrorPayload, policy llm.ResolvedRetryPolicy, key string, retry int64, retryID RetryID, delayMs int64, logger cordis.Logger) agent.RequestErrorAction {
	if p.aborted(payload.Signal) {
		return agent.RequestErrorAction{}
	}
	data := map[string]any{
		"retryId":   string(retryID),
		"turn":      payload.Turn,
		"step":      payload.Step,
		"provider":  payload.Provider,
		"mode":      policy.Mode,
		"policyKey": key,
		"retry":     retry,
		"delayMs":   delayMs,
		"failure":   payload.Failure,
	}
	if policy.Mode == llm.RetryModeNormal {
		data["maxRetries"] = policy.MaxRetries
	}
	if _, err := payload.Agent.Session.Append(EventLlmRetry, data, nil); err != nil && logger != nil {
		logger.Warn("llm-retry: the llm/retry record failed: %v", err)
	}
	fused := p.fused(payload.Signal)
	select {
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
	case <-fused.Done():
		return agent.RequestErrorAction{}
	}
	if _, err := payload.Agent.Session.Append(EventLlmRetryStarted, map[string]any{
		"retryId": string(retryID),
		"turn":    payload.Turn,
		"step":    payload.Step,
		"retry":   retry,
	}, nil); err != nil && logger != nil {
		logger.Warn("llm-retry: the llm/retry-started record failed: %v", err)
	}
	return agent.RequestErrorAction{Retry: true}
}
