// Agent-scoped subject event dispatch. The fused dispatcher couples the agent
// subject to its scope carrier, so the scope key and the payload's agent
// cannot diverge. Port of packages/core/agent/src/dispatch.ts, minus the
// TypeScript type-level machinery: Go callers pass the typed payload structs
// from agent.go directly.
package agent

import (
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/scope"
)

// emitListener is one scope-tagged fire-and-forget listener. Returning an
// error is a synchronous failure with listener-kind semantics chosen by the
// dispatch (veto for creation, contained elsewhere).
type emitListener struct {
	scope scope.ScopeKey
	id    uint64
	fn    func(payload any) error
}

// serialListener is one scope-tagged serial listener; returning ok=true bails
// the chain with value.
type serialListener struct {
	scope scope.ScopeKey
	id    uint64
	fn    func(payload any) (value any, ok bool)
}

// SubjectEventBus carries the agent-subject events for one registry. Emit
// dispatches contain per-listener failures; emitVeto stops at the first
// listener error (creation veto); serial is awaited in order with bail
// semantics; waterfall composes around-middleware first-registered
// outermost. All dispatches filter listeners by the agent subject's scope
// key: untagged listeners receive every agent, tagged listeners follow the
// dsh-scope ancestor admission rules.
type SubjectEventBus struct {
	logger cordis.Logger

	mu        sync.Mutex
	nextID    uint64
	emit      map[string][]emitListener
	serial    map[string][]serialListener
	waterfall map[string]*scope.WaterfallEvent[any, any]
}

func newSubjectEventBus(logger cordis.Logger) *SubjectEventBus {
	return &SubjectEventBus{
		logger:    logger,
		emit:      map[string][]emitListener{},
		serial:    map[string][]serialListener{},
		waterfall: map[string]*scope.WaterfallEvent[any, any]{},
	}
}

func (b *SubjectEventBus) mintID() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	return b.nextID
}

// OnEmit registers an emit-style listener; the disposer removes it.
func (b *SubjectEventBus) OnEmit(event string, listenerScope scope.ScopeKey, fn func(payload any) error) func() {
	id := b.mintID()
	b.mu.Lock()
	b.emit[event] = append(b.emit[event], emitListener{scope: listenerScope, id: id, fn: fn})
	b.mu.Unlock()
	return b.removeEmit(event, id)
}

func (b *SubjectEventBus) removeEmit(event string, id uint64) func() {
	var once bool
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if once {
			return
		}
		once = true
		listeners := b.emit[event]
		for i, entry := range listeners {
			if entry.id == id {
				b.emit[event] = append(listeners[:i], listeners[i+1:]...)
				return
			}
		}
	}
}

// OnSerial registers a serial listener with bail semantics.
func (b *SubjectEventBus) OnSerial(event string, listenerScope scope.ScopeKey, fn func(payload any) (value any, ok bool)) func() {
	id := b.mintID()
	b.mu.Lock()
	b.serial[event] = append(b.serial[event], serialListener{scope: listenerScope, id: id, fn: fn})
	b.mu.Unlock()
	var once bool
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if once {
			return
		}
		once = true
		listeners := b.serial[event]
		for i, entry := range listeners {
			if entry.id == id {
				b.serial[event] = append(listeners[:i], listeners[i+1:]...)
				return
			}
		}
	}
}

// OnWaterfall registers an around-middleware listener; first-registered is
// outermost. The handler must call next to delegate.
func (b *SubjectEventBus) OnWaterfall(event string, listenerScope scope.ScopeKey, fn func(payload any, next func(any) any) any) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	eventListeners := b.waterfall[event]
	if eventListeners == nil {
		eventListeners = &scope.WaterfallEvent[any, any]{}
		b.waterfall[event] = eventListeners
	}
	return eventListeners.On(listenerScope, fn)
}

// admitted reports whether one listener's tag admits the agent subject's
// scope: untagged listeners are admitted for every dispatch; tagged listeners
// follow the scope admission rules.
func admitted(listenerScope scope.ScopeKey, agentScope scope.ScopeKey) bool {
	if listenerScope == nil {
		return true
	}
	return scope.Admits(listenerScope, agentScope)
}

// snapshotEmit copies the emit listeners admitted for agentScope.
func (b *SubjectEventBus) snapshotEmit(event string, agentScope scope.ScopeKey) []func(payload any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []func(payload any) error
	for _, entry := range b.emit[event] {
		if admitted(entry.scope, agentScope) {
			fn := entry.fn
			out = append(out, fn)
		}
	}
	return out
}

// Emit fires one fire-and-forget notification in the agent's scope. Every
// listener is invoked; synchronous throws (panics) and returned errors are
// logged and contained per listener, so a notification cannot veto lifecycle
// progress or starve a later observer.
func (b *SubjectEventBus) Emit(event string, agentScope scope.ScopeKey, payload any) {
	b.emitContained(event, agentScope, payload)
}

func (b *SubjectEventBus) emitContained(event string, agentScope scope.ScopeKey, payload any) {
	for _, fn := range b.snapshotEmit(event, agentScope) {
		b.runContained(event, fn, payload)
	}
}

func (b *SubjectEventBus) runContained(event string, fn func(payload any) error, payload any) {
	defer func() {
		if rec := recover(); rec != nil {
			b.logger.Warn(fmt.Sprintf("agent event %q listener panicked: %v", event, rec))
		}
	}()
	if err := fn(payload); err != nil {
		b.logger.Warn(fmt.Sprintf("agent event %q listener failed: %v", event, err))
	}
}

// emitVeto runs the admitted listeners in order and stops at the first
// error: a synchronous creation failure vetoes publication and rolls back.
func (b *SubjectEventBus) emitVeto(event string, agentScope scope.ScopeKey, payload any) error {
	for _, fn := range b.snapshotEmit(event, agentScope) {
		if err := runVetoListener(fn, payload); err != nil {
			return err
		}
	}
	return nil
}

func runVetoListener(fn func(payload any) error, payload any) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("listener panicked: %v", rec)
		}
	}()
	return fn(payload)
}

// Serial dispatches one awaited in-order chain: the first listener that
// bails wins and its value is returned; with no bail the result is nil.
func (b *SubjectEventBus) Serial(event string, agentScope scope.ScopeKey, payload any) any {
	b.mu.Lock()
	var fns []func(payload any) (value any, ok bool)
	for _, entry := range b.serial[event] {
		if admitted(entry.scope, agentScope) {
			fn := entry.fn
			fns = append(fns, fn)
		}
	}
	b.mu.Unlock()
	for _, fn := range fns {
		if value, ok := fn(payload); ok {
			return value
		}
	}
	return nil
}

// Waterfall dispatches one around-middleware chain in the agent's scope with
// base as the innermost default.
func (b *SubjectEventBus) Waterfall(event string, agentScope scope.ScopeKey, payload any, base func(any) any) any {
	b.mu.Lock()
	eventListeners := b.waterfall[event]
	b.mu.Unlock()
	if eventListeners == nil {
		return base(payload)
	}
	return eventListeners.Dispatch(agentScope, payload, base)
}

// TypedWaterfall binds one waterfall event name to its payload and result
// types. It drives the same any-erased listener table as the raw
// OnWaterfall/Waterfall pair; the only type assertions in the path sit at
// this boundary, where construction guarantees they hold, so every listener
// downstream of a typed event works with real types instead of an
// assert-and-decode ritual. Register and dispatch one event name through
// exactly one accessor: a raw listener on a typed event's name would
// receive values its assertions cannot decode (Go has no generic methods,
// hence the value-typed handle rather than bus-level type parameters).
type TypedWaterfall[T any, R any] struct {
	bus   *SubjectEventBus
	event string
}

// NewTypedWaterfall binds an event name to its payload and result types.
func NewTypedWaterfall[T any, R any](bus *SubjectEventBus, event string) TypedWaterfall[T, R] {
	return TypedWaterfall[T, R]{bus: bus, event: event}
}

// On registers an around-middleware listener; first-registered is
// outermost. The handler must call next to delegate.
func (w TypedWaterfall[T, R]) On(listenerScope scope.ScopeKey, fn func(payload T, next func(T) R) R) func() {
	return w.bus.OnWaterfall(w.event, listenerScope, func(payload any, next func(any) any) any {
		return fn(payload.(T), func(value T) R {
			return next(value).(R)
		})
	})
}

// Dispatch composes the chain in the agent's scope with base as the
// innermost default.
func (w TypedWaterfall[T, R]) Dispatch(agentScope scope.ScopeKey, payload T, base func(T) R) R {
	return w.bus.Waterfall(w.event, agentScope, payload, func(value any) any {
		return base(value.(T))
	}).(R)
}

// PreStep is the typed accessor for the agent/pre-step waterfall: whether
// and with which messages the loop enters a proposed step.
func (b *SubjectEventBus) PreStep() TypedWaterfall[PreStepPayload, PreStepDecision] {
	return NewTypedWaterfall[PreStepPayload, PreStepDecision](b, EventPreStep)
}
