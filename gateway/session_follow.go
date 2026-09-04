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

	"dshgo/session"
)

const sessionFollowEndpoint = "session/follow"

// followMessageTypes are the events paginate counts toward maxMessages
// (official MESSAGE_TYPES).
var followMessageTypes = map[string]bool{
	"user/message":      true,
	"assistant/message": true,
}

// followDefaultMaxMessages is the official DEFAULT_MAX_MESSAGES.
const followDefaultMaxMessages = 50

// followEventFrame is the wire form of one raw event (official
// SessionWireEvent): the durable event fields the browser journal renders.
type followEventFrame struct {
	Type            string          `json:"type"`
	Seq             int64           `json:"seq"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	Ignorable       bool            `json:"ignorable,omitempty"`
	SourceEventSeqs []int64         `json:"sourceEventSeqs,omitempty"`
}

// followRecord is one history-page record (official SessionHistoryRecord =
// SessionEventEntry | SessionChunkRun). Chunk packing stays a later round:
// unpacked chunk events remain valid SessionEventEntry records.
type followRecord struct {
	Type  string            `json:"type"`
	Event followEventFrame  `json:"event"`
}

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

// parseFollowAddress extracts the plain-session id from the decoded
// address. Subagent addresses answer a loud not-supported until that
// domain round.
func parseFollowAddress(args map[string]any) (string, error) {
	raw, ok := args["address"]
	if !ok || raw == nil {
		return "", wrapGatewayError("gateway/arguments-invalid", "session/follow", "address", nil, "session follow requires an address")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", wrapGatewayError("gateway/arguments-invalid", "session/follow", "address", err, "session follow address is not JSON")
	}
	var address followAddress
	if err := json.Unmarshal(encoded, &address); err != nil {
		return "", wrapGatewayError("gateway/arguments-invalid", "session/follow", "address", err, "session follow address is not decodable")
	}
	if address.Kind != "" && address.Kind != "session" {
		return "", wrapGatewayError("gateway/arguments-invalid", "session/follow", "address", nil, "session follow address kind %q is not served yet", address.Kind)
	}
	if address.SessionID == "" {
		return "", wrapGatewayError("gateway/arguments-invalid", "session/follow", "address", nil, "session follow address lacks a sessionId")
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

// followPage is the message-aligned backwards cut (official paginate): walk
// from the newest event counting message-typed surface events up to the
// bound, then slice from that cut.
func followPage(events []session.Event, maxMessages int) (page []session.Event, hasMore bool) {
	count := 0
	cut := 0
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if !followMessageTypes[event.Type] {
			continue
		}
		count++
		if count >= maxMessages {
			cut = index
			break
		}
	}
	return events[cut:], cut > 0
}

// followRecords translates events into wire records.
func followRecords(events []session.Event) []followRecord {
	records := make([]followRecord, 0, len(events))
	for _, event := range events {
		records = append(records, followRecord{
			Type: "event",
			Event: followEventFrame{
				Type:            event.Type,
				Seq:             event.Seq,
				Time:            event.Time,
				Data:            event.Data,
				Ignorable:       event.Ignorable,
				SourceEventSeqs: event.SourceEventSeqs,
			},
		})
	}
	return records
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
	sessionID, err := parseFollowAddress(args)
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
	page, hasMore := followPage(events, followMaxMessages(args))
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
