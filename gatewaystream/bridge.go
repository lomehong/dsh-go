// RemoteEventBridge connects the Host event bus to a RemoteEventQueue
// through the forwarded-events allowlist: emit-mode events push their
// payloads; waterfall-mode events are delivered as pending invocations. The
// single queue feeds one Client event generation over the mux's $events
// stream. Returns the disposer that unsubscribes every listener and ends
// the queue.
package gatewaystream

import (
	"dshgo/agent"
	"dshgo/apiremotes"
	"dshgo/scope"
)

// BridgeOptions wires the bridge seams.
type BridgeOptions struct {
	// Events is the agent subject event bus carrying Host events.
	Events *agent.SubjectEventBus
	// ScopeKeys resolves one scoped Agent id for waterfall deliveries.
	ScopeKeys func(scope.ScopeKey) string
}

// AttachForwardedEvents subscribes every allowlisted Host event to the
// queue, returning the disposer. Waterfall events are delivered as pending
// invocations carrying the scoped Agent id; emit events push their args.
func AttachForwardedEvents(queue *RemoteEventQueue, events *agent.SubjectEventBus, scopeKeys func(scope.ScopeKey) string) func() {
	disposers := make([]func(), 0, len(apiremotes.ForwardedEvents))
	for _, entry := range apiremotes.ForwardedEvents {
		event := entry.Event
		switch entry.Mode {
		case "waterfall":
			undo := events.OnWaterfall(event, nil, func(payload any, next func(any) any) any {
				queue.Push(WireFrame{Type: "waterfall", Event: event, Request: projectEventPayload(payload)})
				return next(payload)
			})
			disposers = append(disposers, undo)
		default:
			undo := events.OnEmit(event, nil, func(payload any) error {
				queue.Push(WireFrame{Type: "emit", Event: event, Args: []any{payload}})
				return nil
			})
			disposers = append(disposers, undo)
		}
	}
	return func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
		queue.End()
	}
}

// projectEventPayload extracts the JSON-safe request fields from a
// waterfall payload; non-record payloads pass as an opaque empty record.
func projectEventPayload(payload any) map[string]any {
	if record, ok := payload.(map[string]any); ok {
		return record
	}
	return map[string]any{}
}
