package gatewaystream

import (
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/scope"
)

func TestAttachForwardedEventsRoutesByMode(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	undo := AttachForwardedEvents(queue, bus, func(scope.ScopeKey) string { return "agent-1" })

	// An emit-mode event pushes its payload.
	bus.Emit("commands/change", nil, map[string]any{"changed": true})
	frame, done := queue.Next()
	if done || frame.Type != "emit" || frame.Event != "commands/change" {
		t.Fatalf("emit frame = %+v done=%v", frame, done)
	}
	// A waterfall-mode event pushes a pending invocation frame. Waterfall
	// listeners are dispatched via the bus's Waterfall method, not Emit.
	bus.Waterfall("approval/request", nil, map[string]any{"pending": true}, func(payload any) any { return payload })
	frame, done = queue.Next()
	if done || frame.Type != "waterfall" || frame.Event != "approval/request" {
		t.Fatalf("waterfall frame = %+v done=%v", frame, done)
	}
	// Unlisted events are not forwarded: no new frame arrives, and the
	// disposer still ends the queue cleanly.
	bus.Emit("brand/unlisted", nil, map[string]any{"x": 1})
	undo()
	if _, done = queue.Next(); !done {
		t.Fatal("queue must end after disposer")
	}
}

func TestAttachForwardedEventsWaterfallNextSemantics(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	// The waterfall listener chain must continue: the bridge relays the
	// delivery but never blocks the Host continuation.
	undo := AttachForwardedEvents(queue, bus, nil)
	defer undo()

	// Waterfall listeners are dispatched via the bus's Waterfall method, not Emit.
	bus.Waterfall("user-questions/request", nil, map[string]any{"q": "pick one"}, func(payload any) any { return payload })
	frame, done := queue.Next()
	if done || frame.Type != "waterfall" {
		t.Fatalf("frame = %+v done=%v", frame, done)
	}
	if frame.Request["q"] != "pick one" {
		t.Fatalf("request = %+v", frame.Request)
	}
}
