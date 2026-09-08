// The session history follow stream (official api-session-controller
// history.ts): one opening snapshot frame (wire header, cursor,
// message-aligned records, projection baseline) followed by the live
// continuation — gap-free durable events (store OnEvent, seq > cursor) and
// opted-in `agent/assistant-stream` frames relayed through the agent
// registry's global emit layer, routed to followers by the attempt id's
// session prefix.
package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/session"
	"dshgo/session/projection"
)

const sessionFollowEndpoint = "session/follow"

// followWireHeader is the v0 browser header (official SessionWireHeader):
// IsSeeded folds into seedLength, Origin into the subagent discriminator.
type followWireHeader struct {
	Version         int64  `json:"version"`
	ID              string `json:"id"`
	CreatedAt       int64  `json:"createdAt"`
	CWD             string `json:"cwd,omitempty"`
	ParentSession   string `json:"parentSession,omitempty"`
	SeedLength      int64  `json:"seedLength,omitempty"`
	Origin          string `json:"origin,omitempty"`
	DelegationDepth *int64 `json:"delegationDepth,omitempty"`
	AgentPreset     string `json:"agentPreset,omitempty"`
}

// followAddress is the request's session address; only the plain-session
// kind is served (official SessionAddress session branch).
type followAddress struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
}

// parseSessionAddress extracts the plain-session id from the decoded
// address payload. Subagent addresses answer a loud not-supported until
// that domain round. endpoint labels the caller (follow/page) in errors.
func parseSessionAddress(args map[string]any, endpoint string) (string, error) {
	raw, ok := args["address"]
	if !ok || raw == nil {
		return "", wrapGatewayError("gateway/arguments-invalid", endpoint, "address", nil, "session %s requires an address", strings.TrimPrefix(endpoint, "session/"))
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", wrapGatewayError("gateway/arguments-invalid", endpoint, "address", err, "session address is not JSON")
	}
	var address followAddress
	if err := json.Unmarshal(encoded, &address); err != nil {
		return "", wrapGatewayError("gateway/arguments-invalid", endpoint, "address", err, "session address is not decodable")
	}
	if address.Kind != "" && address.Kind != "session" {
		return "", wrapGatewayError("gateway/arguments-invalid", endpoint, "address", nil, "session address kind %q is not served yet", address.Kind)
	}
	if address.SessionID == "" {
		return "", wrapGatewayError("gateway/arguments-invalid", endpoint, "address", nil, "session address lacks a sessionId")
	}
	return address.SessionID, nil
}

// followMaxMessages reads the optional page size bound.
func followMaxMessages(args map[string]any) int {
	if raw, ok := args["maxMessages"].(float64); ok && raw > 0 {
		return int(raw)
	}
	return followDefaultMaxMessages
}

// sessionsStoreService is the composed live-session store (boot
// ServiceSessions, referenced by name to keep the layering).
const sessionsStoreService = "sessions"

// sessionStore resolves the composed live-session store, or nil when absent.
func (g *Gateway) sessionStore() *session.Store {
	if store, ok := g.ctx.Get(sessionsStoreService).(*session.Store); ok && store != nil {
		return store
	}
	return nil
}

// projections resolves the composed projection registry, or nil when absent.
func (g *Gateway) projections() *projection.Registry {
	if projections, ok := g.ctx.Get("projections").(*projection.Registry); ok && projections != nil {
		return projections
	}
	return nil
}

// openSessionFollow answers one session follow stream: the opening snapshot
// (header, cursor, message-aligned records, projection baseline) then an
// open, quiet hold until the caller's signal ends.
func (g *Gateway) openSessionFollow(args map[string]any, signal context.Context) (<-chan any, func(), error) {
	// The official client keys stream args by the method's parameter name
	// (follow(request, signal)): the fields ride under one "request"
	// object. Unwrap it before reading; the bare form stays tolerated.
	if wrapped, ok := args["request"].(map[string]any); ok {
		args = wrapped
	}
	sessionID, err := parseSessionAddress(args, sessionFollowEndpoint)
	if err != nil {
		return nil, nil, err
	}
	store := g.sessionStore()
	if store == nil {
		return nil, nil, wrapGatewayError("gateway/not-composed", "session/follow", "", nil, "session follow has no session store")
	}
	sess := store.Get(session.SessionID(sessionID))
	if sess == nil {
		return nil, nil, wrapGatewayError("session/not-found", "session/follow", "address", nil, "session %q is not live", sessionID)
	}

	events := sess.Events()
	cursor := int64(sess.Seq()) - 1
	page, hasMore := paginateHistory(events, nil, followMaxMessages(args), cursor)
	header := sess.Header()
	wire := followWireHeader{
		Version:         header.Version,
		ID:              string(header.ID),
		CreatedAt:       header.CreatedAt,
		CWD:             header.CWD,
		ParentSession:   string(header.ParentSession),
		Origin:          header.Origin,
		DelegationDepth: header.DelegationDepth,
		AgentPreset:     header.AgentPreset,
	}
	if header.IsSeeded {
		wire.SeedLength = int64(header.InheritedEventCount)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		select {
		case <-signal.Done():
		case <-ctx.Done():
		}
	}()
	frames := make(chan any)
	go func() {
		defer close(frames)
		// The projection baseline: every registered wire unit's view as of
		// the snapshot cursor (turnOutline, sessionStats, goal, ...). The
		// registry is an optional composition — absent, the baseline stays
		// empty exactly as before.
		values := map[string]any{}
		if projections := g.projections(); projections != nil {
			values = projections.Snapshot(sess).Values
		}
		snapshot := map[string]any{
			"type":    "snapshot",
			"header":  wire,
			"cursor":  cursor,
			"records": followRecords(page),
			"hasMore": hasMore,
			"projections": map[string]any{
				"asOfSeq": cursor,
				"values":  values,
			},
		}
		// The official client opts in via follow(request).assistantStream and
		// then fails the whole open when the snapshot omits the opening
		// baseline. Assistant frames are process-local presentation — a
		// fresh follow sees no active attempt, so the baseline is the empty
		// revision 0 (an activeAttempt reconnect prefix stays deferred until
		// the multi-client domain round).
		if requested, _ := args["assistantStream"].(bool); requested {
			snapshot["assistantStream"] = map[string]any{"revision": 0}
		}
		select {
		case frames <- snapshot:
		case <-signal.Done():
			return
		case <-ctx.Done():
			return
		}

		// Live continuation: one FIFO carrying durable events past the
		// snapshot cursor (the cordis session/event feed — the same
		// multiplexed source the projection registry drives) and
		// assistant-stream frames routed by the attempt id's session prefix
		// (official follow loop).
		live := make(chan any, 64)
		done := make(chan struct{})
		defer close(done)
		detachEvents := g.ctx.On("session/event", func(value any, next func(any) any) any {
			if payload, ok := value.(*projection.SessionEventPayload); ok {
				if payload.Session.ID() == sess.ID() && payload.Event.Seq > cursor {
					select {
					case live <- map[string]any{"type": "event", "event": payload.Event}:
					case <-done:
					case <-signal.Done():
					}
				}
			}
			return next(value)
		})
		defer detachEvents()
		detachFrames := g.relayAssistantFrames(sess.ID(), func(frame any) {
			select {
			case live <- map[string]any{"type": "assistant-stream", "frame": frame}:
			case <-done:
			case <-signal.Done():
			}
		})
		defer detachFrames()

		for {
			select {
			case entry := <-live:
				select {
				case frames <- entry:
				case <-signal.Done():
					return
				case <-ctx.Done():
					return
				}
			case <-signal.Done():
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames, cancel, nil
}

// sessionEventPayload is the cordis session/event payload (the projection
// registry drives the same shape).
type sessionEventPayload struct {
	Session *session.Session
	Event   session.Event
}

// relayAssistantStreams is the gateway-level assistant-frame relay: a
// global emit listener on the agent registry's bus (official ctx.on
// 'agent/assistant-stream'), nil when no registry is composed.
var relayAssistantStreams = struct {
	mu       sync.Mutex
	handlers map[session.SessionID][]func(any)
}{handlers: map[session.SessionID][]func(any){}}

// relayAssistantFrames registers one session-scoped assistant-frame sink.
// Frames route by the attempt id's session prefix (`<sessionId>:<n>` —
// session ids are uuid-shaped in this deployment, so the last `:` splits).
func (g *Gateway) relayAssistantFrames(id session.SessionID, sink func(any)) func() {
	registry, ok := g.ctx.Get("agents").(*agent.AgentRegistry)
	if !ok || registry == nil {
		return func() {}
	}
	relayAssistantStreams.mu.Lock()
	relayAssistantStreams.handlers[id] = append(relayAssistantStreams.handlers[id], sink)
	relayAssistantStreams.mu.Unlock()
	detach := registry.Events().OnEmit(agent.EventAssistantStream, nil, func(payload any) error {
		frame, ok := payload.(agent.AssistantStreamFrame)
		if !ok {
			return nil
		}
		target := session.SessionID(liveSessionPrefix(string(frame.AttemptID)))
		relayAssistantStreams.mu.Lock()
		handlers := append([]func(any){}, relayAssistantStreams.handlers[target]...)
		relayAssistantStreams.mu.Unlock()
		for _, handler := range handlers {
			handler(frame)
		}
		return nil
	})
	return func() {
		relayAssistantStreams.mu.Lock()
		delete(relayAssistantStreams.handlers, id)
		relayAssistantStreams.mu.Unlock()
		detach()
	}
}

// liveSessionPrefix returns the attempt id's session prefix.
func liveSessionPrefix(attemptID string) string {
	if index := strings.LastIndex(attemptID, ":"); index > 0 {
		return attemptID[:index]
	}
	return attemptID
}
