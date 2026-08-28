// Bounded per-session write batching for the shared persistence coordinator.
// Faithful port of write-behind.ts: pending queue, fixed batching deadline,
// active durable write, failure retention with automatic-pause, and an
// explicit quiescence barrier that concurrent flush callers join.
package persistence

import (
	"fmt"
	"sync"
	"time"

	"dshgo/session"
)

// WriteBehindOptions carries the dependencies and scheduling policy for one
// live session's write controller.
type WriteBehindOptions struct {
	// MaxDelay is the maximum intentional batching wait after an idle queue
	// receives work.
	MaxDelay time.Duration
	// Write persists one stable ordered prefix; it returns only after
	// backend durability.
	Write func(events []session.Event) error
	// ReportBackgroundFailure observes a detached background write failure
	// without failing the producer.
	ReportBackgroundFailure func(err error)
}

// barrier is a shared flush quiescence point concurrent flush callers join.
type barrier struct {
	done chan struct{}
	err  error
}

// SessionWriteBehind owns one live session's pending events, fixed batching
// deadline, active write, failure retention, and explicit quiescence
// barrier.
type SessionWriteBehind struct {
	options WriteBehindOptions

	mu              sync.Mutex
	pending         []session.Event
	timer           *time.Timer
	active          bool
	activeDone      chan struct{}
	barrierRef      *barrier
	deadlineExpired bool
	automaticPaused bool
	// lastForegroundErr is the failure of the most recent foreground
	// (barrier) write, observed by the barrier drain loop.
	lastForegroundErr error
}

// NewSessionWriteBehind builds one controller with a fixed policy and
// durable batch sink.
func NewSessionWriteBehind(options WriteBehindOptions) *SessionWriteBehind {
	return &SessionWriteBehind{options: options}
}

// HasWork reports whether this controller owns queued events or an active
// durable write.
func (w *SessionWriteBehind) HasWork() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.pending) > 0 || w.active
}

// Enqueue copies one event into the persistence-owned queue and starts a
// fixed deadline when the automatic path is idle. The event is deep-copied
// so the producer cannot mutate retained state (structuredClone parity).
func (w *SessionWriteBehind) Enqueue(event session.Event) {
	clone := session.DeepCopyEvent(event)
	w.mu.Lock()
	defer w.mu.Unlock()
	wasEmpty := len(w.pending) == 0
	w.pending = append(w.pending, clone)
	if w.barrierRef != nil {
		return
	}
	if w.automaticPaused {
		w.automaticPaused = false
		w.deadlineExpired = false
		w.armTimerLocked()
	} else if wasEmpty {
		w.armTimerLocked()
	}
}

// Flush cancels the batching wait and durably drains through a quiescent
// point. Concurrent callers join the same barrier. The returned error is
// the barrier's durable write failure, if any.
func (w *SessionWriteBehind) Flush() error {
	w.mu.Lock()
	if b := w.barrierRef; b != nil {
		w.mu.Unlock()
		<-b.done
		return b.err
	}
	w.cancelTimerLocked()
	w.deadlineExpired = false
	w.automaticPaused = false
	b := &barrier{done: make(chan struct{})}
	w.barrierRef = b
	w.mu.Unlock()
	w.drainBarrier(b)
	return b.err
}

// CancelAutomaticWait cancels the current automatic deadline without
// draining retained work.
func (w *SessionWriteBehind) CancelAutomaticWait() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancelTimerLocked()
	w.deadlineExpired = false
}

// armTimerLocked starts the one fixed window for the current pending
// prefix.
func (w *SessionWriteBehind) armTimerLocked() {
	w.cancelTimerLocked()
	w.timer = time.AfterFunc(w.options.MaxDelay, w.onDeadline)
}

// cancelTimerLocked cancels any pending automatic deadline.
func (w *SessionWriteBehind) cancelTimerLocked() {
	if w.timer == nil {
		return
	}
	w.timer.Stop()
	w.timer = nil
}

// onDeadline starts a background write now, or remembers that an active
// write used the budget.
func (w *SessionWriteBehind) onDeadline() {
	w.mu.Lock()
	w.timer = nil
	if w.active {
		w.deadlineExpired = true
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	// startBackground: one detached write whose failure is reported and
	// retained. The goroutine inside startWriteLocked owns continuation.
	w.mu.Lock()
	w.startWriteLocked(true)
	w.mu.Unlock()
}

// continueAutomaticLocked continues immediately after an over-budget active
// write; otherwise it keeps the timer. Called on the write goroutine after
// a successful background write.
func (w *SessionWriteBehind) continueAutomaticLocked() {
	if w.barrierRef != nil || len(w.pending) == 0 {
		return
	}
	if w.deadlineExpired {
		w.deadlineExpired = false
		w.startWriteLocked(true)
	}
}

// drainBarrier awaits overlapping work, drains to quiescence, and settles
// the shared barrier.
func (w *SessionWriteBehind) drainBarrier(b *barrier) {
	// Await any overlapping active write (Promise.allSettled parity: its
	// outcome is irrelevant, only quiescence matters).
	for {
		w.mu.Lock()
		overlapping := w.activeDone
		w.mu.Unlock()
		if overlapping == nil {
			break
		}
		<-overlapping
		w.mu.Lock()
		w.automaticPaused = false
		w.mu.Unlock()
	}
	// Drain to quiescence; the first foreground failure settles the
	// barrier with that error (the failed batch stays retained in order).
	var failErr error
	for failErr == nil {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.mu.Unlock()
			break
		}
		w.lastForegroundErr = nil
		done := w.startWriteLocked(false)
		w.mu.Unlock()
		<-done
		w.mu.Lock()
		failErr = w.lastForegroundErr
		w.lastForegroundErr = nil
		w.mu.Unlock()
	}
	// Close admission to this barrier before resolving callers: a later
	// enqueue starts its own automatic window instead of being stranded
	// behind a settled barrier.
	w.mu.Lock()
	w.barrierRef = nil
	b.err = failErr
	close(b.done)
	w.mu.Unlock()
}

// startWriteLocked starts one stable pending prefix, retaining it in order
// if durability fails. The write runs on a detached goroutine; on success
// of a background write the goroutine re-arms the over-budget continuation.
func (w *SessionWriteBehind) startWriteLocked(background bool) chan struct{} {
	batch := w.pending
	w.pending = nil
	w.cancelTimerLocked()
	w.deadlineExpired = false
	done := make(chan struct{})
	w.active = true
	w.activeDone = done
	go func() {
		err := func() (err error) {
			defer func() {
				// A panicking sink is a write failure, not a crash.
				if r := recover(); r != nil {
					err = fmt.Errorf("session write sink panicked: %v", r)
				}
			}()
			return w.options.Write(batch)
		}()
		w.mu.Lock()
		w.active = false
		w.activeDone = nil
		if err != nil {
			// Retain the batch in order and pause the automatic path until
			// a fresh enqueue resumes it.
			w.pending = append(append([]session.Event{}, batch...), w.pending...)
			w.cancelTimerLocked()
			w.deadlineExpired = false
			w.automaticPaused = true
			if !background {
				w.lastForegroundErr = err
			}
			w.mu.Unlock()
			if background && w.options.ReportBackgroundFailure != nil {
				w.options.ReportBackgroundFailure(err)
			}
			close(done)
			return
		}
		w.mu.Unlock()
		close(done)
		if background {
			w.mu.Lock()
			w.continueAutomaticLocked()
			w.mu.Unlock()
		}
	}()
	return done
}
