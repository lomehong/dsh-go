package cordis

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

type recordingLogger struct {
	lines []string
}

func (l *recordingLogger) Info(args ...any)  { l.lines = append(l.lines, fmt.Sprint(args...)) }
func (l *recordingLogger) Warn(args ...any)  { l.lines = append(l.lines, fmt.Sprint(args...)) }
func (l *recordingLogger) Error(args ...any) { l.lines = append(l.lines, fmt.Sprint(args...)) }

func TestGetResolvesAncestorChainOnly(t *testing.T) {
	root := NewRoot(Discard{})
	root.Provide("core", "root-core")

	child := root.Child()
	child.Provide("extra", "child-extra")

	sibling := root.Child()

	if root.Get("extra") != nil {
		t.Fatal("parent must not see child services (ancestor-only walk)")
	}
	if sibling.Get("extra") != nil {
		t.Fatal("sibling must not see a sibling's services")
	}
	if child.Get("core") != "root-core" {
		t.Fatalf("child must see ancestor service, got %v", child.Get("core"))
	}
}

func TestInjectRunsInlineWhenSatisfied(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.Provide("llm", "svc")

	ran := false
	if err := ctx.Inject([]string{"llm"}, func(c *Context) error {
		ran = c.Get("llm") == "svc"
		return nil
	}); err != nil {
		t.Fatalf("satisfied injection must run inline without error: %v", err)
	}
	if !ran {
		t.Fatal("inline injection must receive the context")
	}
}

func TestProvideFulfillsDeferredInjection(t *testing.T) {
	ctx := NewRoot(Discard{})
	ran := false
	if err := ctx.Inject([]string{"llm", "settings"}, func(c *Context) error {
		ran = c.Get("llm") != nil && c.Get("settings") != nil
		return nil
	}); err != nil {
		t.Fatalf("deferred Inject must return nil: %v", err)
	}
	if ran {
		t.Fatal("injection must stay parked until services exist")
	}
	ctx.Provide("llm", "llm-service")
	if ran {
		t.Fatal("injection must stay parked until every declared name resolves")
	}
	ctx.Provide("settings", "settings-service")
	if !ran {
		t.Fatal("Provide must run the deferred injection once all names resolve")
	}
}

func TestDeferredInjectionFailureIsReportedNotSilent(t *testing.T) {
	lg := &recordingLogger{}
	ctx := NewRoot(lg)
	ctx.Inject([]string{"llm"}, func(c *Context) error {
		return errors.New("activation failed")
	})
	ctx.Provide("llm", "svc")
	if len(lg.lines) == 0 || !strings.Contains(lg.lines[0], "deferred injection failed: activation failed") {
		t.Fatalf("activation failure must reach the logger, got %v", lg.lines)
	}
}

func TestPendingInjectionsNamesTheMissingServices(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.Inject([]string{"llm", "settings"}, func(c *Context) error { return nil })
	ctx.Inject([]string{"llm"}, func(c *Context) error { return nil })
	pending := ctx.PendingInjections()
	if len(pending) != 2 {
		t.Fatalf("pending = %v", pending)
	}
	if len(pending[0]) != 2 || pending[0][0] != "llm" || pending[0][1] != "settings" {
		t.Fatalf("first set = %v", pending[0])
	}
	// A satisfied ancestor removes a name from the missing set.
	ctx.Provide("llm", "svc")
	pending = ctx.PendingInjections()
	if len(pending) != 1 || len(pending[0]) != 1 || pending[0][0] != "settings" {
		t.Fatalf("pending after partial satisfaction = %v", pending)
	}
	ctx.Provide("settings", "svc")
	if pending := ctx.PendingInjections(); len(pending) != 0 {
		t.Fatalf("pending after full satisfaction = %v", pending)
	}
}

func TestPendingInjectionsSeeAncestorServices(t *testing.T) {
	root := NewRoot(Discard{})
	root.Provide("llm", "svc")
	child := root.Child()
	// An ancestor-satisfied injection runs inline and never parks.
	child.Inject([]string{"llm"}, func(c *Context) error { return nil })
	if child.PendingInjections() != nil {
		t.Fatal("inline-satisfied injection counted as pending")
	}
}

func TestDisposeRunsDisposersLIFOAndIsIdempotent(t *testing.T) {
	ctx := NewRoot(Discard{})
	var order []string
	effect := func(name string) func() (Disposer, error) {
		return func() (Disposer, error) {
			return func() { order = append(order, name) }, nil
		}
	}
	if err := ctx.Effect(effect("first")); err != nil {
		t.Fatalf("effect setup failed: %v", err)
	}
	if err := ctx.Effect(effect("second")); err != nil {
		t.Fatalf("effect setup failed: %v", err)
	}
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose returned error: %v", err)
	}
	if len(order) != 2 || order[0] != "second" || order[1] != "first" {
		t.Fatalf("disposers must run LIFO, got %v", order)
	}
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("second dispose must be a no-op, got %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("second dispose must not rerun disposers, got %v", order)
	}
}

func TestEffectSetupErrorLeavesNothingRegistered(t *testing.T) {
	ctx := NewRoot(Discard{})
	ran := false
	if err := ctx.Effect(func() (Disposer, error) {
		return Disposer(func() { ran = true }), errors.New("setup failed")
	}); err == nil {
		t.Fatal("setup error must be returned")
	}
	if ran {
		t.Fatal("failed setup must not register a disposer")
	}
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose after failed setup must be clean, got %v", err)
	}
}

func TestEffectAfterDisposeRunsDisposerImmediately(t *testing.T) {
	ctx := NewRoot(Discard{})
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose failed: %v", err)
	}
	ran := false
	err := ctx.Effect(func() (Disposer, error) {
		return func() { ran = true }, nil
	})
	if err == nil {
		t.Fatal("effect after dispose must fail loudly")
	}
	if !ran {
		t.Fatal("effect after dispose must run its disposer immediately, not leak")
	}
}

func TestDisposeContainsPanickingDisposerAndRunsTheRest(t *testing.T) {
	lg := &recordingLogger{}
	ctx := NewRoot(lg)
	ran := false
	_ = ctx.Effect(func() (Disposer, error) {
		return func() { ran = true }, nil
	})
	_ = ctx.Effect(func() (Disposer, error) {
		return func() { panic("stuck disposer") }, nil
	})
	err := ctx.Dispose()
	if err == nil || !strings.Contains(err.Error(), "disposer panicked") {
		t.Fatalf("disposer panic must be reported, got %v", err)
	}
	if !ran {
		t.Fatal("one panicking disposer must not strand the rest (LIFO: first still runs)")
	}
}

func TestWaterfallDelegatesInRegistrationOrder(t *testing.T) {
	ctx := NewRoot(Discard{})
	var trace []string
	ctx.On("agent/request", func(v any, next func(any) any) any {
		trace = append(trace, "A-in")
		out := next(v)
		trace = append(trace, "A-out")
		return "A(" + out.(string) + ")"
	})
	ctx.On("agent/request", func(v any, next func(any) any) any {
		trace = append(trace, "B-in")
		return "B(" + next(v).(string) + ")"
	})
	ctx.On("agent/request", func(v any, next func(any) any) any {
		trace = append(trace, "C")
		return "C:" + v.(string)
	})

	got := ctx.Waterfall("agent/request", "x")
	if got != "A(B(C:x))" {
		t.Fatalf("value must propagate through next returns, got %q", got)
	}
	want := []string{"A-in", "B-in", "C", "A-out"}
	if strings.Join(trace, ",") != strings.Join(want, ",") {
		t.Fatalf("execution order must follow registration, got %v", trace)
	}
}

func TestWaterfallShortCircuitSkipsRemainingListeners(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.On("e", func(v any, next func(any) any) any {
		return "A(" + next(v).(string) + ")"
	})
	ctx.On("e", func(v any, next func(any) any) any {
		return "B-only"
	})
	cRan := false
	ctx.On("e", func(v any, next func(any) any) any {
		cRan = true
		return next(v)
	})

	if got := ctx.Waterfall("e", "x"); got != "A(B-only)" {
		t.Fatalf("short-circuit value must become the final result, got %q", got)
	}
	if cRan {
		t.Fatal("returning without next must skip the remaining listeners")
	}
}

func TestWaterfallListenerCanRewriteValueThroughNext(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.On("e", func(v any, next func(any) any) any {
		return "A(" + next(v).(string) + ")"
	})
	ctx.On("e", func(v any, next func(any) any) any {
		return "B(" + next("rewritten").(string) + ")"
	})
	ctx.On("e", func(v any, next func(any) any) any {
		return "C:" + v.(string)
	})
	if got := ctx.Waterfall("e", "x"); got != "A(B(C:rewritten))" {
		t.Fatalf("downstream listeners must see the value passed to next, got %q", got)
	}
}

func TestWaterfallWithoutListenersReturnsInitialValue(t *testing.T) {
	ctx := NewRoot(nil)
	if got := ctx.Waterfall("e", "raw"); got != "raw" {
		t.Fatalf("empty chain must pass the value through, got %q", got)
	}
}

func TestWaterfallRejectsDoubleNext(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.On("e", func(v any, next func(any) any) any {
		next("1")
		return next("2")
	})
	defer func() {
		if recover() == nil {
			t.Fatal("calling next twice must fail loudly")
		}
	}()
	ctx.Waterfall("e", "x")
}

func TestOnDisposerRemovesListener(t *testing.T) {
	ctx := NewRoot(Discard{})
	called := false
	dispose := ctx.On("e", func(v any, next func(any) any) any {
		called = true
		return next(v)
	})
	dispose()
	ctx.Waterfall("e", "x")
	if called {
		t.Fatal("disposed listener must not run")
	}
}

func TestMountFailsLoudlyOnMissingInjection(t *testing.T) {
	ctx := NewRoot(Discard{})
	err := ctx.Mount(&Plugin{Name: "p", Inject: []string{"missing"}})
	if err == nil || !strings.Contains(err.Error(), `"missing"`) {
		t.Fatalf("Mount must fail loudly on missing injection, got %v", err)
	}
}

func TestMountRunsApplyWhenInjectionsResolve(t *testing.T) {
	ctx := NewRoot(Discard{})
	ctx.Provide("dep", "service")
	applied := false
	err := ctx.Mount(&Plugin{
		Name:   "p",
		Inject: []string{"dep"},
		Apply: func(c *Context) error {
			applied = c.Get("dep") == "service"
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Mount failed: %v", err)
	}
	if !applied {
		t.Fatal("Apply must run with resolved services")
	}
}

func TestMountPropagatesApplyError(t *testing.T) {
	ctx := NewRoot(Discard{})
	err := ctx.Mount(&Plugin{Name: "p", Apply: func(c *Context) error {
		return errors.New("apply failed")
	}})
	if err == nil || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("Apply error must propagate, got %v", err)
	}
}

// Typed services pin the assertion at the definition site: From resolves
// through the ancestor chain, ok=false on absence, on a nil context, and
// defensively on a wrong-typed value under the same name.
func TestTypedService(t *testing.T) {
	type greeting string
	svc := DefineService[greeting]("greeting")
	if _, ok := svc.From(nil); ok {
		t.Fatal("nil context must not resolve")
	}
	root := NewRoot(Discard{})
	if _, ok := svc.From(root); ok {
		t.Fatal("absent service must not resolve")
	}
	svc.Provide(root, "hello")
	if got, ok := svc.From(root); !ok || got != "hello" {
		t.Fatalf("From = %q %v, want hello true", got, ok)
	}
	// The chain walks ancestors only.
	_ = root.Mount(&Plugin{Name: "child-holder", Apply: func(child *Context) error {
		svc.Provide(child, "child-greeting")
		if got, _ := svc.From(child); got != "child-greeting" {
			t.Fatalf("child resolution = %q, want the child's own service", got)
		}
		return nil
	}})
	// A wrong-typed value under the same name degrades to absent instead
	// of panicking at the consumer.
	wrong := NewRoot(Discard{})
	wrong.Provide("greeting", 42)
	if _, ok := svc.From(wrong); ok {
		t.Fatal("wrong-typed service must not resolve")
	}
}
