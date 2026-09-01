package gateway

import (
	"context"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/gatewaystream"
	"dshgo/typert"
)

// sliceSource is a fixed-dispatch event source for tests.
type sliceSource struct {
	dispatches []RemoteEventDispatch
}

func (s *sliceSource) Next() (RemoteEventDispatch, bool) {
	if len(s.dispatches) == 0 {
		return RemoteEventDispatch{}, false
	}
	dispatch := s.dispatches[0]
	s.dispatches = s.dispatches[1:]
	return dispatch, true
}

func (s *sliceSource) Dispose() {}

func newTestGateway(t *testing.T) *Gateway {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := typert.NewRegistry(root, cordis.Discard{})
	return New(root, registry)
}

func TestRegisterRemoteEventsSingleton(t *testing.T) {
	g := newTestGateway(t)
	source := func(context.Context) RemoteEventDispatchIter {
		return &sliceSource{}
	}
	dispose, err := g.RegisterRemoteEvents(source, gatewaystream.RemoteEventHostInfo{Home: "/home"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	if _, err := g.RegisterRemoteEvents(source, gatewaystream.RemoteEventHostInfo{}); err == nil ||
		err.Error() != "typert gateway: forwarded Remote event source is already registered" {
		t.Fatalf("second register err = %v", err)
	}
}

func TestOpenRemoteEventsReadyAndBroadcast(t *testing.T) {
	g := newTestGateway(t)
	dispose, err := g.RegisterRemoteEvents(func(ctx context.Context) RemoteEventDispatchIter {
		// Emit one frame after a tick.
		go func() {
			select {
			case <-time.After(20 * time.Millisecond):
				g.broadcastRemoteEvent(gatewaystream.WireFrame{Type: "emit", Event: "test/event", Args: []any{"x"}})
			case <-ctx.Done():
			}
		}()
		return &sliceSource{}
	}, gatewaystream.RemoteEventHostInfo{Home: "/home"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()

	frames, done, cleanup, err := g.OpenRemoteEvents(map[string]any{}, context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() {
		cleanup()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("stream never ended")
		}
	}()

	first := <-frames
	ready, ok := first.(gatewaystream.RemoteEventReadyFrame)
	if !ok || ready.ClientID == "" || ready.Host.Home != "/home" {
		t.Fatalf("first = %+v", first)
	}
	second := <-frames
	emit, ok := second.(gatewaystream.WireFrame)
	if !ok || emit.Type != "emit" || emit.Event != "test/event" {
		t.Fatalf("emit = %+v", second)
	}
	_ = done
}

func TestOpenRemoteEventsRejectsNonEmptyArgs(t *testing.T) {
	g := newTestGateway(t)
	if _, _, _, err := g.OpenRemoteEvents(map[string]any{"x": 1}, context.Background()); err == nil ||
		err.Error() != "typert gateway: $events stream requires an empty args object" {
		t.Fatalf("non-empty args err = %v", err)
	}
}

func TestOpenRemoteEventsUnavailableWithoutRegistration(t *testing.T) {
	g := newTestGateway(t)
	if _, _, _, err := g.OpenRemoteEvents(map[string]any{}, context.Background()); err == nil ||
		err.Error() != "gateway/service-unavailable: forwarded Remote event source is unavailable" {
		t.Fatalf("unregistered err = %v", err)
	}
}

func TestPendingEventBackfilledToJoiningClient(t *testing.T) {
	g := newTestGateway(t)
	dispose, err := g.RegisterRemoteEvents(func(context.Context) RemoteEventDispatchIter {
		// A pending agent-scoped event exists before any client opens.
		g.startRemoteEvent(RemoteEventDispatch{
			AgentScope: "agent-1",
			Frame:      gatewaystream.WireFrame{Type: "waterfall", Event: "agent/inbox/claimed", EventID: "evt-1", AgentID: "agent-1"},
		})
		return &sliceSource{}
	}, gatewaystream.RemoteEventHostInfo{Home: "/home"})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer dispose()
	// Let the source run (it fires synchronously on register).
	time.Sleep(10 * time.Millisecond)

	frames, _, cleanup, err := g.OpenRemoteEvents(map[string]any{}, context.Background())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer cleanup()
	<-frames // ready
	pending := <-frames
	waterfall, ok := pending.(gatewaystream.WireFrame)
	if !ok || waterfall.Type != "waterfall" || waterfall.EventID != "evt-1" || waterfall.AgentID != "agent-1" {
		t.Fatalf("backfilled pending = %+v", pending)
	}
}
