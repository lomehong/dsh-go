// The $events/result answer router: it correlates one pending Host-to-Client
// waterfall delivery with the Client's unary result POST, so a blocking
// Host answerer can select over the browser's outcome (official
// RemoteEventResult RPC + the pending-invocation registry in
// registerRemoteEvents).
package gatewaystream

import (
	"sync"
)

// ResultRouter routes Client results to the waiters of pending waterfall
// deliveries. Waiters register under one correlation id before the frame
// goes out; the unary $events/result handler delivers at most one outcome
// per id.
type ResultRouter struct {
	mu      sync.Mutex
	waiters map[string]chan RemoteEventResult
}

// NewResultRouter builds an empty router.
func NewResultRouter() *ResultRouter {
	return &ResultRouter{waiters: map[string]chan RemoteEventResult{}}
}

// Await registers one waiter for a pending event's outcome. The channel is
// buffered: a delivery that races the waiter's registration order still
// lands.
func (r *ResultRouter) Await(eventID string) <-chan RemoteEventResult {
	wait := make(chan RemoteEventResult, 1)
	r.mu.Lock()
	r.waiters[eventID] = wait
	r.mu.Unlock()
	return wait
}

// Forget drops one waiter: a late result for a settled event is dropped,
// and a caller abandoning the wait (caller cancellation) stops buffering.
func (r *ResultRouter) Forget(eventID string) {
	r.mu.Lock()
	delete(r.waiters, eventID)
	r.mu.Unlock()
}

// Deliver routes one Client result to its waiter. The second result reports
// whether a waiter was waiting; an unrouted result means the event was
// unknown (already settled or never pending).
func (r *ResultRouter) Deliver(result RemoteEventResult) bool {
	r.mu.Lock()
	wait, ok := r.waiters[result.EventID]
	if ok {
		delete(r.waiters, result.EventID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	wait <- result
	return true
}
