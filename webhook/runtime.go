package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/llm"
)

// registration is one effect-owned rule registration and the invocations
// that currently use it.
type registration struct {
	rule     Rule
	cancel   context.CancelFunc
	signal   context.Context
	wg       sync.WaitGroup
	mu       sync.Mutex
	closing  bool
	disposal func()

	disposeMu sync.Mutex
}

// WebhookRuntime is the fire-and-forget rule runtime. Session creation is
// the only built-in action.
type WebhookRuntime struct {
	mu         sync.Mutex
	logger     cordis.Logger
	create     SessionCreator
	rules      map[WebhookRuleID]*registration
	closing    bool
	baseCtx    context.Context
	baseCancel context.CancelFunc
	drained    sync.WaitGroup
}

// NewWebhookRuntime builds the runtime. The creator seam receives every
// settled rule request; a nil creator fails invocations that return a
// request (the workspace-backed composition lands with the workspace and
// preset rounds).
func NewWebhookRuntime(logger cordis.Logger, create SessionCreator) *WebhookRuntime {
	baseCtx, cancel := context.WithCancel(context.Background())
	return &WebhookRuntime{
		logger:     logger,
		create:     create,
		rules:      map[WebhookRuleID]*registration{},
		baseCtx:    baseCtx,
		baseCancel: cancel,
	}
}

// Register registers one trusted programmatic rule and returns the
// awaitable effect disposer that aborts and drains this rule's active
// callbacks.
func (r *WebhookRuntime) Register(rule Rule) (func() error, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return nil, fmt.Errorf("webhook runtime is closing")
	}
	if rule == nil {
		return nil, fmt.Errorf("webhook rule must not be nil")
	}
	if rule.ID() == "" || trimSpace(rule.ID()) == "" {
		return nil, fmt.Errorf("webhook rule id must be a non-empty string")
	}
	if rule.Kind() == "" || trimSpace(rule.Kind()) == "" {
		return nil, fmt.Errorf("webhook rule %q kind must be a non-empty string", rule.ID())
	}
	if _, ok := r.rules[rule.ID()]; ok {
		return nil, fmt.Errorf("webhook rule %q is already registered", rule.ID())
	}
	signal, cancel := context.WithCancel(r.baseCtx)
	entry := &registration{rule: rule, cancel: cancel, signal: signal}
	r.rules[rule.ID()] = entry
	r.drained.Add(1)
	dispose := func() error {
		r.disposeRegistration(entry)
		return nil
	}
	return dispose, nil
}

// Dispatch starts every currently matching rule and returns before any
// callback settles. The delivery is snapshotted before dispatch.
func (r *WebhookRuntime) Dispatch(delivery VerifiedWebhookDelivery) error {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return fmt.Errorf("webhook runtime is closing")
	}
	entries := make([]*registration, 0, len(r.rules))
	for _, entry := range r.rules {
		entries = append(entries, entry)
	}
	r.mu.Unlock()

	snapshot, err := snapshotDelivery(delivery)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		entry.mu.Lock()
		live := !entry.closing
		entry.mu.Unlock()
		if !live || entry.rule.Kind() != snapshot.Kind {
			continue
		}
		r.startInvocation(entry, snapshot)
	}
	return nil
}

// startInvocation starts one contained invocation and attaches it to
// registration teardown.
func (r *WebhookRuntime) startInvocation(entry *registration, delivery VerifiedWebhookDelivery) {
	entry.wg.Add(1)
	go func() {
		defer entry.wg.Done()
		r.runInvocation(entry, delivery)
	}()
}

// runInvocation runs the rule and creates the requested session, containing
// every failure: a stop after disposal is a debug note, any other failure a
// warning.
func (r *WebhookRuntime) runInvocation(entry *registration, delivery VerifiedWebhookDelivery) {
	defer func() {
		if rec := recover(); rec != nil {
			r.report(entry, delivery, fmt.Errorf("%v", rec))
		}
	}()
	if entry.signal.Err() != nil {
		return
	}
	request, err := entry.rule.Run(delivery, entry.signal)
	if err != nil {
		// The report itself distinguishes a stop after disposal (a debug
		// note) from a live failure (a warning).
		r.report(entry, delivery, err)
		return
	}
	if entry.signal.Err() != nil {
		return
	}
	if request == nil {
		return
	}
	if r.create == nil {
		r.report(entry, delivery, fmt.Errorf("no webhook Session creator is composed"))
		return
	}
	if createErr := r.create(delivery, entry.rule.ID(), *request, entry.signal); createErr != nil {
		r.report(entry, delivery, createErr)
	}
}

// report logs one invocation failure with the verbatim invocation line.
func (r *WebhookRuntime) report(entry *registration, delivery VerifiedWebhookDelivery, err error) {
	if r.logger == nil {
		return
	}
	invocation := fmt.Sprintf("webhook: provider=%s source=%s delivery=%s rule=%s",
		mustJSON(entry.rule.Kind()), mustJSON(delivery.Source), mustJSON(delivery.DeliveryID), mustJSON(entry.rule.ID()))
	if entry.signal.Err() != nil {
		r.logger.Error(fmt.Sprintf("%s stopped after disposal: %s", invocation, llm.ErrorChain(err)))
		return
	}
	r.logger.Warn(fmt.Sprintf("%s failed: %s", invocation, llm.ErrorChain(err)))
}

// disposeRegistration is the memoized registration teardown: hide, abort,
// then drain.
func (r *WebhookRuntime) disposeRegistration(entry *registration) {
	entry.disposeMu.Lock()
	defer entry.disposeMu.Unlock()
	if entry.disposal != nil {
		return
	}
	entry.disposal = func() {}
	entry.mu.Lock()
	entry.closing = true
	entry.mu.Unlock()
	r.mu.Lock()
	delete(r.rules, entry.rule.ID())
	r.mu.Unlock()
	entry.cancel()
	entry.wg.Wait()
	r.drained.Done()
}

// Dispose closes the runtime: further registration and dispatch fail, every
// rule is aborted, and all in-flight invocations drain before returning.
func (r *WebhookRuntime) Dispose() {
	r.mu.Lock()
	if r.closing {
		r.mu.Unlock()
		return
	}
	r.closing = true
	entries := make([]*registration, 0, len(r.rules))
	for _, entry := range r.rules {
		entries = append(entries, entry)
	}
	r.mu.Unlock()
	r.baseCancel()
	for _, entry := range entries {
		r.disposeRegistration(entry)
	}
}

// mustJSON renders one string with JSON quoting (JSON.stringify in the
// invocation line).
func mustJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `"` + value + `"`
	}
	return string(encoded)
}

// errClosing marks a closed runtime for callers that need the check.
var errClosing = errors.New("webhook runtime is closing")
