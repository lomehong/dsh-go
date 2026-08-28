// Process-local initiator boundaries. The JS implementation threads the
// initiating Agent through AsyncLocalStorage; Go has no ambient goroutine
// state, so the boundary rides an explicit context.Context value and the
// operation runs inside the WithInitiator closure — the same "wrap the
// complete returned foreground lifetime" shape the source documents.
package agent

import (
	"context"
)

type initiatorKey struct{}

type initiatorValue struct {
	// agent is the initiating agent; nil inside an explicit clearing
	// boundary.
	agent *Agent
}

// CurrentInitiator reads the Agent that initiated the inherited driver chain,
// or nil outside an initiator boundary and inside an explicit clearing
// boundary. Use this optional form for attribution that also supports
// agentless calls. Nil result is an answer, not an error.
func CurrentInitiator(ctx context.Context) *Agent {
	value, ok := ctx.Value(initiatorKey{}).(initiatorValue)
	if !ok {
		return nil
	}
	return value.agent
}

// RequireInitiator reads the initiating Agent and fails when no initiator
// boundary is active.
func RequireInitiator(ctx context.Context) (*Agent, error) {
	agent := CurrentInitiator(ctx)
	if agent == nil {
		return nil, ErrNoInitiator
	}
	return agent, nil
}

// WithInitiator runs the operation with one exact Agent as its process-local
// initiator. The operation's error return is preserved. A queue or wire
// receiver may establish this boundary only after validating explicit
// identity and resolving the exact live Agent; this method does neither.
// Detached work remains owned by the subsystem that starts it. Fails when
// the initiator scope is closing or disposed.
func (r *AgentRegistry) WithInitiator(ctx context.Context, agent *Agent, operation func(ctx context.Context) error) error {
	if err := r.beginInitiatorRun(); err != nil {
		return err
	}
	defer r.endInitiatorRun()
	return operation(context.WithValue(ctx, initiatorKey{}, initiatorValue{agent: agent}))
}

// WithoutInitiator runs the operation inside a boundary that hides any
// inherited initiating Agent. Use this while creating lazy shared timers,
// queue pumps, pool maintenance, watchers, or exporters so they do not
// inherit the first Agent that happens to initialize them. It clears only
// initiator attribution, not explicit fields, and does not own or drain
// detached resources.
func (r *AgentRegistry) WithoutInitiator(ctx context.Context, operation func(ctx context.Context) error) error {
	return r.WithInitiator(ctx, nil, operation)
}

// CloseInitiators rejects new initiator boundaries while inherited work
// drains. Driven by owner-fiber unload in the JS host; Go callers invoke it
// from their teardown path before DisposeInitiators.
func (r *AgentRegistry) CloseInitiators() {
	r.initiatorMu.Lock()
	if r.initiatorState == 0 {
		r.initiatorState = 1
	}
	r.initiatorMu.Unlock()
}

// DisposeInitiators waits for in-flight initiator boundaries, then
// invalidates the registry's initiator surface. Idempotent: concurrent and
// repeated calls await the same drain.
func (r *AgentRegistry) DisposeInitiators() error {
	r.initiatorMu.Lock()
	r.closeLocked()
	if r.activeInitiatorRuns != 0 {
		if r.initiatorDrain == nil {
			r.initiatorDrain = make(chan struct{})
			r.initiatorDrainClosed = false
		}
		drain := r.initiatorDrain
		r.initiatorMu.Unlock()
		<-drain
	} else {
		r.initiatorMu.Unlock()
	}
	r.initiatorMu.Lock()
	r.initiatorState = 2
	r.initiatorMu.Unlock()
	return nil
}

func (r *AgentRegistry) closeLocked() {
	if r.initiatorState == 0 {
		r.initiatorState = 1
	}
}

func (r *AgentRegistry) beginInitiatorRun() error {
	r.initiatorMu.Lock()
	defer r.initiatorMu.Unlock()
	if r.initiatorState != 0 {
		return ErrInitiatorDisposed
	}
	r.activeInitiatorRuns++
	return nil
}

func (r *AgentRegistry) endInitiatorRun() {
	r.initiatorMu.Lock()
	defer r.initiatorMu.Unlock()
	r.activeInitiatorRuns--
	if r.activeInitiatorRuns != 0 {
		return
	}
	if r.initiatorDrain != nil && !r.initiatorDrainClosed {
		close(r.initiatorDrain)
		r.initiatorDrainClosed = true
		r.initiatorDrain = nil
	}
}

// InitiatorSession derives the initiating agent's session at an orchestration
// entry and returns it for operation-local capture: initiator-owned chains
// derive, then capture.
func InitiatorSession(ctx context.Context) (*Agent, error) {
	agent, err := RequireInitiator(ctx)
	if err != nil {
		return nil, err
	}
	return agent, nil
}
