// RemoteEventBridge connects the Host event bus to a RemoteEventQueue
// through the forwarded-events allowlist: emit-mode events push their
// payloads; waterfall-mode events are delivered as pending invocations whose
// outcomes arrive through the ResultRouter (the browser answerer claims the
// Host waterfall by returning a result; next/rejected delegate onward). The
// single queue feeds one Client event generation over the mux's $events
// stream. Returns the disposer that unsubscribes every listener and ends
// the queue.
package gatewaystream

import (
	"context"
	"encoding/json"

	"dshgo/agent"
	"dshgo/apiremotes"
	"dshgo/identity"
	"dshgo/interaction/userapproval"
	"dshgo/interaction/userquestions"
	"dshgo/scope"
)

// BridgeExtras carries the answer-loop seams. The zero value keeps the
// pass-through observer behavior (push + delegate, never blocking) — the
// answer loop requires a router and a live-client probe.
type BridgeExtras struct {
	// Router routes the browser's $events/result outcomes for pending
	// waterfall deliveries.
	Router *ResultRouter
	// HasClients reports whether any $events client generation is live.
	// With no client attached, waterfall events delegate onward instead of
	// waiting for an answer that can never arrive.
	HasClients func() bool
	// ScopeKeys renders one scoped Agent id for the wire frame.
	ScopeKeys func(scope.ScopeKey) string
}

// AttachForwardedEvents subscribes every allowlisted Host event to the
// queue, returning the disposer. Waterfall events are delivered as pending
// invocations carrying the scoped Agent id; emit events push their args.
func AttachForwardedEvents(queue *RemoteEventQueue, events *agent.SubjectEventBus, scopeKeys func(scope.ScopeKey) string) func() {
	return AttachForwardedEventsWithOptions(queue, events, scopeKeys, BridgeExtras{ScopeKeys: scopeKeys})
}

// AttachForwardedEventsWithOptions is AttachForwardedEvents with the
// answer-loop seams.
func AttachForwardedEventsWithOptions(queue *RemoteEventQueue, events *agent.SubjectEventBus, scopeKeys func(scope.ScopeKey) string, extras BridgeExtras) func() {
	if extras.ScopeKeys == nil {
		extras.ScopeKeys = scopeKeys
	}
	disposers := make([]func(), 0, len(apiremotes.ForwardedEvents))
	for _, entry := range apiremotes.ForwardedEvents {
		event := entry.Event
		switch entry.Mode {
		case "waterfall":
			undo := events.OnWaterfall(event, nil, func(payload any, next func(any) any) any {
				return deliverWaterfall(queue, entry, extras, payload, next)
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

// deliverWaterfall is one waterfall listener: it projects the scoped
// request, delivers the pending invocation, and selects between the
// Client's outcome and the request's own cancellation lifetime. Without the
// answer seams, an untyped request shape, or no live client, it delegates
// onward — the browser answerer is an optional participant, never a
// mandatory hop.
func deliverWaterfall(queue *RemoteEventQueue, entry apiremotes.ForwardedEventEntry, extras BridgeExtras, payload any, next func(any) any) any {
	projection, ok := projectScopedEventRequest(entry.Event, payload, extras)
	if !ok {
		return next(payload)
	}
	if extras.Router == nil || extras.HasClients == nil {
		// Pass-through observer (no answer loop composed): deliver the
		// pending frame and delegate onward.
		queue.Push(WireFrame{Type: "waterfall", Event: entry.Event, EventID: projection.eventID, AgentID: projection.agentID, Request: projection.request})
		return next(payload)
	}
	if !extras.HasClients() {
		// No browser can answer: waiting would strand the request until
		// its lifetime ends. Delegate immediately.
		return next(payload)
	}
	wait := extras.Router.Await(projection.eventID)
	defer extras.Router.Forget(projection.eventID)
	queue.Push(WireFrame{Type: "waterfall", Event: entry.Event, EventID: projection.eventID, AgentID: projection.agentID, Request: projection.request})
	if projection.signal == nil {
		return projection.apply(<-wait, next, payload)
	}
	select {
	case result := <-wait:
		return projection.apply(result, next, payload)
	case <-projection.signal:
		// The request's lifetime ended (tool-call cancellation): withdraw
		// the pending delivery and delegate onward.
		queue.Push(WireFrame{Type: "cancel", EventID: projection.eventID})
		return next(payload)
	}
}

// scopedEventProjection is one typed waterfall request prepared for the
// wire: the JSON-safe fields, the correlation identity, the scoped Agent,
// the cancellation lifetime, and the outcome-to-waterfall-value mapping.
type scopedEventProjection struct {
	eventID string
	agentID string
	request map[string]any
	signal  <-chan struct{}
	apply   func(RemoteEventResult, func(any) any, any) any
}

// projectScopedEventRequest types the two application waterfall events the
// web answerer can claim. A foreign payload delegates onward — the observer
// behavior stays the fallback, never a failed claim.
func projectScopedEventRequest(event string, payload any, extras BridgeExtras) (scopedEventProjection, bool) {
	switch event {
	case "approval/request":
		req, ok := payload.(userapproval.ApprovalRequest)
		if !ok {
			return scopedEventProjection{}, false
		}
		request := map[string]any{"toolName": req.ToolName, "reason": req.Reason}
		if req.CallID != "" {
			request["callId"] = req.CallID
		}
		agentID := ""
		if req.Agent != nil {
			agentID = extras.ScopeKeys(req.Agent.Scope)
		}
		return scopedEventProjection{
			eventID: identity.RandomUUID(),
			agentID: agentID,
			request: request,
			signal:  signalDone(req.Signal),
			apply:   applyApprovalOutcome,
		}, true
	case "user-questions/request":
		req, ok := payload.(userquestions.Request)
		if !ok {
			return scopedEventProjection{}, false
		}
		agentID := ""
		if req.Agent != nil {
			agentID = extras.ScopeKeys(req.Agent.Scope)
		}
		return scopedEventProjection{
			eventID: identity.RandomUUID(),
			agentID: agentID,
			request: map[string]any{"questions": jsonReadyValue(req.Questions)},
			signal:  signalDone(req.Signal),
			apply:   applyQuestionDecision,
		}, true
	default:
		return scopedEventProjection{}, false
	}
}

// applyApprovalOutcome maps the browser's outcome to the approval waterfall
// value: a claimed result rides verbatim (the service normalizes the closed
// vocabulary); next and rejected delegate onward, so a later answerer — or
// the fail-closed base — decides.
func applyApprovalOutcome(result RemoteEventResult, next func(any) any, payload any) any {
	if result.Outcome.Kind == "result" {
		if outcome, ok := result.Outcome.Value.(string); ok {
			return userapproval.ApprovalOutcome(outcome)
		}
		// A non-string claim is not an answer the vocabulary admits:
		// fail closed rather than delegate into an unknown shape.
		return userapproval.OutcomeUnavailable
	}
	return next(payload)
}

// applyQuestionDecision maps the browser's answer to the questions
// waterfall's closed decision type. A result whose value does not decode
// into the answer contract delegates onward (never a fabricated answer).
func applyQuestionDecision(result RemoteEventResult, next func(any) any, payload any) any {
	if result.Outcome.Kind == "result" {
		if answer, ok := decodeQuestionAnswer(result.Outcome.Value); ok {
			return userquestions.QuestionDecision{Answer: answer}
		}
	}
	return next(payload)
}

// signalDone extracts the cancellation channel from the request's context.
func signalDone(signal context.Context) <-chan struct{} {
	if signal == nil {
		return nil
	}
	return signal.Done()
}

// jsonReadyValue renders one typed value through its wire tags into the
// generic map/slice shape the frame request field carries.
func jsonReadyValue(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return []any{}
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return []any{}
	}
	return out
}

// decodeQuestionAnswer decodes the browser's answer value into the answer
// contract (json round trip — the wire value is untrusted lossless JSON).
func decodeQuestionAnswer(value any) (userquestions.AskUserQuestionAnswer, bool) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return userquestions.AskUserQuestionAnswer{}, false
	}
	var answer userquestions.AskUserQuestionAnswer
	if err := json.Unmarshal(encoded, &answer); err != nil {
		return userquestions.AskUserQuestionAnswer{}, false
	}
	return answer, true
}
