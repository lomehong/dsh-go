// Detached ports hook-protocol/src/detached.ts: quiescence tracking for
// emit-shaped hook runs that no extension point awaits. Bridges track the
// run plus its continuation, pass the tracker context into execution, and
// drain on disposal so no process or late callback outlives the fiber.
package hookprotocol

import (
	"context"
	"errors"
	"sync"
)

// DetachedRuns is the in-flight registry for one bridge's detached hook
// runs.
type DetachedRuns struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelCauseFunc
}

// NewDetachedRuns creates a DetachedRuns tracker (one per bridge apply).
// Settled runs are pruned by the WaitGroup so a long-lived session does not
// accumulate them.
func NewDetachedRuns() *DetachedRuns {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &DetachedRuns{ctx: ctx, cancel: cancel}
}

// Ctx is the context every tracked run must hand to RunHook. Drain fires it
// so a still-running hook process is killed rather than awaited out to its
// timeout (default 10 minutes).
func (d *DetachedRuns) Ctx() context.Context {
	return d.ctx
}

// Track registers one detached run until it settles, launching it on its
// own goroutine (the Go shape of the official promise chain: the bridge
// passes the FULL continuation — an inject, a warn — so Drain waits for the
// side effects, not just the process exit). A panicking run is contained
// here (settlement bookkeeping only).
func (d *DetachedRuns) Track(run func(ctx context.Context)) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		run(d.ctx)
	}()
}

// Drain cancels the tracker context, then blocks once every tracked run has
// settled. The bridge registers this as its disposer tail, so disposal
// resolving means the bridge's detached work is quiescent. A run tracked
// AFTER Drain returns is awaited by no one — by then the bridge's listeners
// are disposed, so nothing can start one (the official re-check loop covers
// chains tracked while a wave settles; in Go the listeners that Track are
// disposed before the drain disposer runs, so no late Track can occur).
func (d *DetachedRuns) Drain() {
	d.cancel(errors.New("hook bridge disposed"))
	d.wg.Wait()
}
