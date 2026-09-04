// The session history follow stream (official api-session-controller
// history.ts): one opening snapshot frame (wire header, cursor,
// message-aligned records, projection baseline) followed by live event
// entries. The live continuation is served over the same stream; this port
// opens the snapshot and holds the stream open — the Go event bus does not
// yet relay session/event into any follow-consumable feed, so the honest
// continuation is an open, quiet stream (matching the r103 control-stream
// posture) until the event-bridge batch lands.
package gateway

import (
	"context"
	"encoding/json"
	"strings"

	"dshgo/session"
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

// openSessionFollow answers one session follow stream: the opening snapshot
// (header, cursor, message-aligned records, projection baseline) then an
// open, quiet hold until the caller's signal ends.
func (g *Gateway) openSessionFollow(args map[string]any, signal context.Context) (<-chan any, func(), error) {
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
		snapshot := map[string]any{
			"type":   "snapshot",
			"header": wire,
			"cursor": cursor,
			"records": followRecords(page),
			"hasMore": hasMore,
			"projections": map[string]any{
				"asOfSeq": cursor,
				"values":  map[string]any{},
			},
		}
		select {
		case frames <- snapshot:
		case <-signal.Done():
			return
		case <-ctx.Done():
			return
		}
		select {
		case <-signal.Done():
		case <-ctx.Done():
		}
	}()
	return frames, cancel, nil
}
