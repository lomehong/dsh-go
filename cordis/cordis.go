// Package cordis re-implements, in Go, the plugin-kernel semantics the DSH
// harness depends on from the vendored Cordis framework: a string-keyed
// service registry with ancestor-scoped resolution, declared injection,
// reversible effects, and waterfall event dispatch with explicit delegation.
//
// Behavior sources: docs/cordis-primer.md and vendor/cordis in the official
// tree at tag dsh-v0.1.2-alpha.1. Where this package differs from those
// sources the official behavior wins; the tests pin the semantics that
// third-party plugins rely on.
package cordis

import (
	"errors"
	"fmt"
	"log"
	"sync"
)

// Logger is the logging surface handed to plugins. It is an interface on
// purpose: the detached-method "this is not a function" failure class of the
// JavaScript kernel cannot be expressed here.
type Logger interface {
	Info(args ...any)
	Warn(args ...any)
	Error(args ...any)
}

// Discard drops every record; the zero-configuration default.
type Discard struct{}

func (Discard) Info(...any)  {}
func (Discard) Warn(...any)  {}
func (Discard) Error(...any) {}

// StdLogger writes records through the standard log package.
type StdLogger struct{}

func (StdLogger) Info(args ...any)  { log.Println(append([]any{"[dsh info]"}, args...)...) }
func (StdLogger) Warn(args ...any)  { log.Println(append([]any{"[dsh warn]"}, args...)...) }
func (StdLogger) Error(args ...any) { log.Println(append([]any{"[dsh error]"}, args...)...) }

// Disposer releases one registration. Disposers run in LIFO order when the
// owning context is disposed.
type Disposer func()

// Plugin is one composition unit, mirroring the JavaScript plugin contract
// (name / inject / apply). Provide is metadata for loader validation; Apply is
// expected to call Provide for every service it contributes.
type Plugin struct {
	Name    string
	Inject  []string
	Provide []string
	Apply   func(*Context) error
}

// WaterfallHandler handles one waterfall event. It must call next to delegate
// to the remaining listeners and return next's result; returning without
// calling next short-circuits the chain and makes the returned value the final
// result. next must be called at most once — a second call fails loudly.
// Source: docs/cordis-primer.md, "Cordis Waterfall Semantics".
type WaterfallHandler func(value any, next func(any) any) any

type listener struct {
	id uint64
	h  WaterfallHandler
}

type pendingInjection struct {
	names []string
	run   func(*Context) error
}

// Context is a service repository and lifecycle scope. Service resolution
// walks the ancestor chain only (self, then parents): a service provided by a
// sibling or a descendant scope is invisible, matching the kernel's fiber
// walk. Waterfall listeners dispatch on the context they registered on; event
// inheritance across the fiber tree is out of scope for this slice.
type Context struct {
	parent *Context
	logger Logger

	mu        sync.Mutex
	services  map[string]any
	listeners map[string][]listener
	pending   []pendingInjection
	disposers []Disposer
	disposed  bool
	nextID    uint64
}

// NewRoot creates a top-level context. A nil logger becomes Discard.
func NewRoot(logger Logger) *Context {
	return newContext(nil, logger)
}

// Child creates a nested scope whose resolution includes the receiver's
// services. Disposing the child never disposes the parent.
func (c *Context) Child() *Context {
	return newContext(c, c.logger)
}

func newContext(parent *Context, logger Logger) *Context {
	if logger == nil {
		logger = Discard{}
	}
	return &Context{
		parent:    parent,
		logger:    logger,
		services:  map[string]any{},
		listeners: map[string][]listener{},
	}
}

// Get resolves a service by key, walking the ancestor chain only. nil means
// "service absent"; callers degrade exactly like ctx.get(name) consumers.
func (c *Context) Get(name string) any {
	c.mu.Lock()
	v, ok := c.services[name]
	c.mu.Unlock()
	if ok {
		return v
	}
	if c.parent != nil {
		return c.parent.Get(name)
	}
	return nil
}

// Provide registers a service on this context, then runs every deferred
// injection this made satisfiable, in declaration order. An injection that
// fails at activation is logged, never silent.
func (c *Context) Provide(name string, service any) {
	c.mu.Lock()
	c.services[name] = service
	ready := c.takeReadyLocked()
	c.mu.Unlock()
	for _, p := range ready {
		if err := p.run(c); err != nil {
			c.logger.Error(fmt.Sprintf("cordis: deferred injection failed: %v", err))
		}
	}
}

func (c *Context) takeReadyLocked() []pendingInjection {
	var ready []pendingInjection
	kept := c.pending[:0]
	for _, p := range c.pending {
		if c.satisfiedLocked(p.names) {
			ready = append(ready, p)
		} else {
			kept = append(kept, p)
		}
	}
	c.pending = kept
	return ready
}

func (c *Context) satisfiedLocked(names []string) bool {
	for _, n := range names {
		if _, ok := c.services[n]; ok {
			continue
		}
		if c.parent != nil && c.parent.Get(n) != nil {
			continue
		}
		return false
	}
	return true
}

// Inject runs fn once every named service resolves from this context: inline
// when already present, otherwise deferred until a Provide satisfies it. The
// deferred run reports its failures through the logger and returns nil here.
func (c *Context) Inject(names []string, fn func(*Context) error) error {
	c.mu.Lock()
	if c.satisfiedLocked(names) {
		c.mu.Unlock()
		return fn(c)
	}
	c.pending = append(c.pending, pendingInjection{
		names: append([]string(nil), names...),
		run:   fn,
	})
	c.mu.Unlock()
	return nil
}

// Effect runs setup and registers the returned disposer for LIFO execution at
// Dispose. A setup error leaves nothing registered. Registering on an already
// disposed context runs the disposer immediately and fails loudly — resources
// never leak silently past teardown.
func (c *Context) Effect(setup func() (Disposer, error)) error {
	disposer, err := setup()
	if err != nil {
		return err
	}
	if disposer == nil {
		disposer = func() {}
	}
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		disposer()
		return errors.New("cordis: effect registered after dispose; disposer ran immediately")
	}
	c.disposers = append(c.disposers, disposer)
	c.mu.Unlock()
	return nil
}

// On registers a waterfall listener in registration order and returns a
// disposer that removes it.
func (c *Context) On(event string, h WaterfallHandler) Disposer {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.listeners[event] = append(c.listeners[event], listener{id: id, h: h})
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		current := c.listeners[event]
		for i, l := range current {
			if l.id == id {
				c.listeners[event] = append(current[:i], current[i+1:]...)
				return
			}
		}
	}
}

// Waterfall dispatches event with the initial value through registered
// listeners in registration order and returns the outermost listener's return
// value. Calling next descends into the remaining listeners with the given
// value; returning without next short-circuits them.
func (c *Context) Waterfall(event string, initial any) any {
	c.mu.Lock()
	handlers := make([]WaterfallHandler, 0, len(c.listeners[event]))
	for _, l := range c.listeners[event] {
		handlers = append(handlers, l.h)
	}
	c.mu.Unlock()
	return dispatch(handlers, 0, initial)
}

func dispatch(handlers []WaterfallHandler, i int, value any) any {
	if i >= len(handlers) {
		return value
	}
	var delegated bool
	return handlers[i](value, func(v any) any {
		if delegated {
			panic(fmt.Sprintf("cordis: waterfall listener %d called next twice", i))
		}
		delegated = true
		return dispatch(handlers, i+1, v)
	})
}

// Dispose runs registered disposers in LIFO order. Every disposer runs even
// when an earlier one fails; errors and panics are joined into the returned
// error — one stuck disposer never strands the rest.
func (c *Context) Dispose() error {
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return nil
	}
	c.disposed = true
	disposers := c.disposers
	c.disposers = nil
	c.mu.Unlock()

	var errs []error
	for i := len(disposers) - 1; i >= 0; i-- {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					err := fmt.Errorf("cordis: disposer panicked: %v", rec)
					errs = append(errs, err)
					c.logger.Error(err.Error())
				}
			}()
			disposers[i]()
		}()
	}
	return errors.Join(errs...)
}

// Mount resolves the plugin's declared injections and runs Apply on this
// context. A missing injection fails loudly here: deferral is the loader's
// job, and Mount hiding a misconfiguration would violate fail-loud.
func (c *Context) Mount(p *Plugin) error {
	for _, name := range p.Inject {
		if c.Get(name) == nil {
			return fmt.Errorf("cordis: plugin %q requires missing service %q", p.Name, name)
		}
	}
	if p.Apply == nil {
		return nil
	}
	return p.Apply(c)
}
