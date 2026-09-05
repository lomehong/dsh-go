package gatewaystream

import (
	"context"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/interaction/userapproval"
	"dshgo/interaction/userquestions"
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
	bus.Waterfall("approval/request", nil, userapproval.ApprovalRequest{ToolName: "write", Reason: "r"}, func(payload any) any { return payload })
	frame, done = queue.Next()
	if done || frame.Type != "waterfall" || frame.Event != "approval/request" {
		t.Fatalf("waterfall frame = %+v done=%v", frame, done)
	}
	if frame.EventID == "" || frame.Request["toolName"] != "write" {
		t.Fatalf("frame = %+v", frame)
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
	bus.Waterfall("user-questions/request", nil, userquestions.Request{
		Questions: []userquestions.AskUserQuestionItem{{ID: "q1", Question: "pick one"}},
	}, func(payload any) any { return payload })
	frame, done := queue.Next()
	if done || frame.Type != "waterfall" {
		t.Fatalf("frame = %+v done=%v", frame, done)
	}
	questions, ok := frame.Request["questions"].([]any)
	if !ok || len(questions) != 1 {
		t.Fatalf("request = %+v", frame.Request)
	}
}

// The answer loop: with a router and a live client, the waterfall listener
// blocks until the router delivers the browser's outcome, and the claimed
// result becomes the waterfall value (official remote-on answer path).
func TestBridgeAnswerLoopClaimsApprovalOutcome(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	router := NewResultRouter()
	clients := true
	extras := BridgeExtras{Router: router, HasClients: func() bool { return clients }}
	undo := AttachForwardedEventsWithOptions(queue, bus, nil, extras)
	defer undo()

	type claimed struct {
		value any
	}
	done := make(chan claimed, 1)
	go func() {
		value := bus.Waterfall("approval/request", nil, userapproval.ApprovalRequest{
			ToolName: "write", Reason: "needs a decision",
			Signal: context.Background(),
		}, func(payload any) any { return "delegated" })
		done <- claimed{value: value}
	}()

	// The pending invocation frame goes out first, carrying the correlation
	// id and the projected request.
	frame, _ := queue.Next()
	if frame.Type != "waterfall" || frame.EventID == "" || frame.Request["toolName"] != "write" {
		t.Fatalf("frame = %+v", frame)
	}
	if !router.Deliver(RemoteEventResult{ClientID: "c1", EventID: frame.EventID,
		Outcome: RemoteEventOutcome{Kind: "result", Value: "allowed-once"}}) {
		t.Fatal("deliver reported no waiter")
	}
	outcome := <-done
	if outcome.value != userapproval.OutcomeAllowedOnce {
		t.Fatalf("waterfall value = %#v, want the claimed outcome", outcome.value)
	}
}

// Without a live client the bridge must not strand the request: it
// delegates onward without pushing any frame.
func TestBridgeAnswerLoopFallsThroughWithoutClients(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	router := NewResultRouter()
	extras := BridgeExtras{Router: router, HasClients: func() bool { return false }}
	undo := AttachForwardedEventsWithOptions(queue, bus, nil, extras)
	defer undo()

	value := bus.Waterfall("approval/request", nil, userapproval.ApprovalRequest{ToolName: "write"},
		func(payload any) any { return "delegated" })
	if value != "delegated" {
		t.Fatalf("value = %#v, want the delegation", value)
	}
	if queue.Len() != 0 {
		t.Fatalf("queue = %d frames, want none without live clients", queue.Len())
	}
}

// The request's own cancellation lifetime withdraws the pending delivery: a
// cancel frame goes out and the waterfall delegates onward (the official
// RemoteEventCancellationFrame).
func TestBridgeAnswerLoopCancelsOnSignal(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	router := NewResultRouter()
	signal, cancel := context.WithCancel(context.Background())
	extras := BridgeExtras{Router: router, HasClients: func() bool { return true }}
	undo := AttachForwardedEventsWithOptions(queue, bus, nil, extras)
	defer undo()
	defer cancel()

	done := make(chan any, 1)
	go func() {
		done <- bus.Waterfall("approval/request", nil, userapproval.ApprovalRequest{
			ToolName: "write", Signal: signal,
		}, func(payload any) any { return "delegated" })
	}()

	frame, _ := queue.Next()
	if frame.Type != "waterfall" || frame.EventID == "" {
		t.Fatalf("frame = %+v", frame)
	}
	cancel()
	if value := <-done; value != "delegated" {
		t.Fatalf("value = %#v, want the delegation after cancellation", value)
	}
	cancelFrame, _ := queue.Next()
	if cancelFrame.Type != "cancel" || cancelFrame.EventID != frame.EventID {
		t.Fatalf("cancel frame = %+v", cancelFrame)
	}
}

// The questions answer decodes into the closed decision type; a
// non-decodable claim delegates onward instead of fabricating an answer.
func TestBridgeAnswerLoopDecodesQuestionAnswer(t *testing.T) {
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	bus := registry.Events()
	queue := NewRemoteEventQueue()
	router := NewResultRouter()
	extras := BridgeExtras{Router: router, HasClients: func() bool { return true }}
	undo := AttachForwardedEventsWithOptions(queue, bus, nil, extras)
	defer undo()

	done := make(chan any, 1)
	go func() {
		done <- bus.Waterfall("user-questions/request", nil, userquestions.Request{
			Questions: []userquestions.AskUserQuestionItem{{ID: "q1", Question: "pick"}},
		}, func(payload any) any { return "delegated" })
	}()

	frame, _ := queue.Next()
	if frame.Type != "waterfall" || frame.EventID == "" {
		t.Fatalf("frame = %+v", frame)
	}
	if !router.Deliver(RemoteEventResult{EventID: frame.EventID,
		Outcome: RemoteEventOutcome{Kind: "result", Value: map[string]any{
			"answers": []any{map[string]any{"id": "q1", "selected": []any{"yes"}}},
		}}}) {
		t.Fatal("deliver reported no waiter")
	}
	decision, ok := (<-done).(userquestions.QuestionDecision)
	if !ok || len(decision.Answer.Answers) != 1 || decision.Answer.Answers[0].ID != "q1" {
		t.Fatalf("decision = %#v", decision)
	}
}
