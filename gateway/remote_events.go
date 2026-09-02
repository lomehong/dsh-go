// The forwarded Remote event runtime (official dsh-api-gateway
// registerRemoteEvents + $events stream): the Gateway's sole application
// event source, per-Client queues, and the broadcast/start dispatch that
// turns Host event dispatches into wire frames. The frame vocabulary
// ($events/$events/result, ready/emit/invocation/cancellation) lives in
// gatewaystream/protocol.go (r58 port); this file owns the lifecycle.
package gateway

import (
	"context"
	"fmt"
	"sync"

	"dshgo/gatewaystream"
	"dshgo/llm"
)

// RemoteEventSource is the application-selected forwarded-event stream
// factory (official TypertRemoteEventSource): called once per Gateway
// lifetime with the registration signal; it yields dispatches until the
// signal aborts.
type RemoteEventSource func(signal context.Context) RemoteEventDispatchIter

// RemoteEventDispatchIter iterates Host event dispatches: a context-bearing
// dispatch starts an agent-scoped waterfall, a plain frame broadcasts. The
// iterator owns the source's per-signal lifecycle (the official source
// factory attaches listeners on creation and detaches them on signal
// abort); Dispose releases those resources.
type RemoteEventDispatchIter interface {
	// Next yields one dispatch; ok=false ends the source.
	Next() (RemoteEventDispatch, bool)
	// Dispose releases the per-signal resources (detaches listeners,
	// closes the backing queue).
	Dispose()
}

// RemoteEventDispatch is one item the source yields: either a
// context-bearing waterfall start or a plain broadcast frame.
type RemoteEventDispatch struct {
	// AgentScope identifies the owning agent for context dispatches; empty
	// means broadcast.
	AgentScope string
	// Frame is the wire frame (emit/invocation/cancellation) to deliver.
	Frame gatewaystream.WireFrame
	// Release cancels a pending context dispatch when the source aborts
	// (the official dispatch.reject(signal.reason)).
	Release func()
}

// RemoteEventClient is one $events stream: its queue plus the pending
// invocation deliveries bound to it.
type RemoteEventClient struct {
	ID    string
	Queue *gatewaystream.RemoteEventQueue
}

// pendingRemoteEvent is one agent-scoped invocation awaiting client
// acknowledgement, retained so joining clients receive it.
type pendingRemoteEvent struct {
	id         string
	frame      gatewaystream.WireFrame
	deliveries map[*RemoteEventClient]struct{}
	release    func()
}

// remoteEventsState is the gateway's registered source and live clients.
type remoteEventsState struct {
	mu      sync.Mutex
	reg     *remoteEventRegistration
	clients map[string]*RemoteEventClient
	pending map[string]*pendingRemoteEvent
}

type remoteEventRegistration struct {
	lifetime context.Context
	cancel   context.CancelFunc
	host     gatewaystream.RemoteEventHostInfo
	done     chan struct{}
}

// RegisterRemoteEvents registers the sole forwarded-event source (official
// registerRemoteEvents): a second registration fails loud; the disposer
// aborts the source lifetime and waits for the consume loop to settle.
func (g *Gateway) RegisterRemoteEvents(source RemoteEventSource, host gatewaystream.RemoteEventHostInfo) (func(), error) {
	g.remote.mu.Lock()
	if g.remote.reg != nil {
		g.remote.mu.Unlock()
		return nil, fmt.Errorf("typert gateway: forwarded Remote event source is already registered")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	reg := &remoteEventRegistration{lifetime: lifetime, cancel: cancel, host: host, done: make(chan struct{})}
	g.remote.reg = reg
	g.remote.mu.Unlock()

	go func() {
		defer close(reg.done)
		g.consumeRemoteEvents(source, lifetime)
	}()
	return func() {
		g.remote.mu.Lock()
		active := g.remote.reg == reg
		if active {
			g.remote.reg = nil
		}
		g.remote.mu.Unlock()
		if active {
			cancel()
		}
		<-reg.done
	}, nil
}

// consumeRemoteEvents iterates the source: context dispatches start a
// pending agent-scoped event, plain frames broadcast to every client. The
// source's Dispose releases its per-signal resources on every exit.
func (g *Gateway) consumeRemoteEvents(source RemoteEventSource, signal context.Context) {
	iter := source(signal)
	defer iter.Dispose()
	for {
		dispatch, ok := iter.Next()
		if !ok {
			return
		}
		if signal.Err() != nil {
			if dispatch.Release != nil {
				dispatch.Release()
			}
			return
		}
		if dispatch.AgentScope != "" {
			g.startRemoteEvent(dispatch)
		} else {
			g.broadcastRemoteEvent(dispatch.Frame)
		}
	}
}

// broadcastRemoteEvent delivers one frame to every live client queue.
func (g *Gateway) broadcastRemoteEvent(frame gatewaystream.WireFrame) {
	g.remote.mu.Lock()
	defer g.remote.mu.Unlock()
	for _, client := range g.remote.clients {
		client.Queue.Push(frame)
	}
}

// startRemoteEvent records an agent-scoped invocation and delivers it to
// every current client (and to clients that join later, which are
// backfilled on open).
func (g *Gateway) startRemoteEvent(dispatch RemoteEventDispatch) {
	g.remote.mu.Lock()
	pending := &pendingRemoteEvent{
		id:         frameID(dispatch.Frame),
		frame:      dispatch.Frame,
		deliveries: map[*RemoteEventClient]struct{}{},
		release:    dispatch.Release,
	}
	g.remote.pending[pending.id] = pending
	for _, client := range g.remote.clients {
		pending.deliveries[client] = struct{}{}
		client.Queue.Push(dispatch.Frame)
	}
	g.remote.mu.Unlock()
}

// OpenRemoteEvents handles one $events stream request (official
// openRemoteEvents): validates the empty-args contract, mints a client id,
// backfills pending events, then relays the ready frame and the client's
// queue as a frame channel. The returned done channel closes when the
// stream ends; the cleanup removes the client and its pending bindings.
func (g *Gateway) OpenRemoteEvents(payload map[string]any, signal context.Context) (<-chan any, <-chan struct{}, func(), error) {
	if len(payload) != 0 {
		return nil, nil, nil, fmt.Errorf("typert gateway: $events stream requires an empty args object")
	}
	g.remote.mu.Lock()
	reg := g.remote.reg
	if reg == nil {
		g.remote.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("gateway/service-unavailable: forwarded Remote event source is unavailable")
	}
	client := &RemoteEventClient{ID: newClientID(), Queue: gatewaystream.NewRemoteEventQueue()}
	g.remote.clients[client.ID] = client
	for _, pending := range g.remote.pending {
		pending.deliveries[client] = struct{}{}
		client.Queue.Push(pending.frame)
	}
	g.remote.mu.Unlock()

	frames := make(chan any)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case frames <- gatewaystream.RemoteEventReadyFrame{Type: "ready", ClientID: client.ID, Host: reg.host}:
		case <-signal.Done():
			return
		}
		for {
			frame, ended := client.Queue.Next()
			if ended {
				return
			}
			select {
			case frames <- frame:
			case <-signal.Done():
				return
			}
		}
	}()
	cleanup := func() {
		g.remote.mu.Lock()
		delete(g.remote.clients, client.ID)
		for _, pending := range g.remote.pending {
			delete(pending.deliveries, client)
		}
		g.remote.mu.Unlock()
		client.Queue.End()
	}
	return frames, done, cleanup, nil
}

// newClientID mints a fresh $events client id (UUID v4, official
// randomUUID).
func newClientID() string {
	return string(llm.NewMessageID())
}

// frameID extracts the pending-event correlation id from a wire frame:
// the EventID on waterfall frames, or the frame's own identity otherwise.
func frameID(frame gatewaystream.WireFrame) string {
	if frame.EventID != "" {
		return frame.EventID
	}
	return string(llm.NewMessageID())
}
