// RemoteEventQueue is the Gateway-internal forwarding queue feeding one
// Client event generation: a bounded frame queue with a single waiter,
// matching the upstream Deque-backed async generator.
package gatewaystream

import (
	"sync"
)

// WireFrame is one item carried by the forwarded-event stream.
type WireFrame struct {
	// Type discriminates ready/emit/waterfall/cancel/end frames.
	Type string `json:"type"`
	// Event is the Host event name on emit/waterfall frames.
	Event string `json:"event,omitempty"`
	// Args are the Host event args on emit frames.
	Args []any `json:"args,omitempty"`
	// EventID is the pending waterfall correlation on waterfall/cancel.
	EventID string `json:"eventId,omitempty"`
	// AgentID is the scoped Agent on waterfall frames.
	AgentID string `json:"agentId,omitempty"`
	// Request is the projected waterfall request payload.
	Request map[string]any `json:"request,omitempty"`
	// ClientID and Host are on the ready frame.
	ClientID string              `json:"clientId,omitempty"`
	Host     RemoteEventHostInfo `json:"host,omitempty"`
}

// RemoteEventQueue is one forwarding queue with a single waiting consumer.
type RemoteEventQueue struct {
	mu     sync.Mutex
	frames []WireFrame
	waiter chan struct{}
	closed bool
}

// NewRemoteEventQueue builds an open queue.
func NewRemoteEventQueue() *RemoteEventQueue {
	return &RemoteEventQueue{waiter: make(chan struct{}, 1)}
}

// Push appends one frame and wakes the waiter.
func (q *RemoteEventQueue) Push(frame WireFrame) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.frames = append(q.frames, frame)
	select {
	case q.waiter <- struct{}{}:
	default:
	}
	q.mu.Unlock()
}

// End closes the queue for further appends and wakes the waiter.
func (q *RemoteEventQueue) End() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	select {
	case q.waiter <- struct{}{}:
	default:
	}
	q.mu.Unlock()
}

// Next returns the next frame or a closed signal. The first bool reports a
// frame; a false result with done=true means the queue ended.
func (q *RemoteEventQueue) Next() (frame WireFrame, done bool) {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			frame = q.frames[0]
			q.frames = q.frames[1:]
			q.mu.Unlock()
			return frame, false
		}
		if q.closed {
			q.mu.Unlock()
			return WireFrame{}, true
		}
		waiter := q.waiter
		q.mu.Unlock()
		<-waiter
	}
}
