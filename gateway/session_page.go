// Session history page reads (official api-session-controller history.ts
// page): one message-aligned backwards page over a session's durable log,
// cold-capable through the query engine. The page answers follow snapshots
// whose hasMore is true — the browser pages older history without opening a
// live stream.
package gateway

import (
	"context"
	"errors"

	"dshgo/session"
	"dshgo/sessionquery"
)

const sessionPageEndpoint = "session/page"

// pageReadsThrough parses the required throughSeq (inclusive newest seq the
// page is anchored at, from a follow opening cursor; -1 for an empty log).
func pageReadsThrough(args map[string]any) (int64, error) {
	raw, ok := args["throughSeq"]
	if !ok || raw == nil {
		return 0, wrapGatewayError("gateway/arguments-invalid", sessionPageEndpoint, "throughSeq", nil, "session page requires a throughSeq")
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), nil
	case int64:
		return v, nil
	default:
		return 0, wrapGatewayError("gateway/arguments-invalid", sessionPageEndpoint, "throughSeq", nil, "session page throughSeq must be a number")
	}
}

// pageReadsBefore parses the optional exclusive older bound.
func pageReadsBefore(args map[string]any) *int64 {
	raw, ok := args["beforeSeq"]
	if !ok || raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case float64:
		value := int64(v)
		return &value
	case int64:
		value := v
		return &value
	default:
		return nil
	}
}

// Page answers one message-aligned backwards history page (official
// session/page): the events at or before throughSeq, cut at the newest
// maxMessages message events, plus whether older pages remain. The read is
// cold-capable (live store first, persisted corpus otherwise), so paging a
// not-currently-live session stays honest.
func (c *SessionController) Page(ctx context.Context, request map[string]any) (any, error) {
	sessionID, err := parseSessionAddress(request, sessionPageEndpoint)
	if err != nil {
		return nil, err
	}
	engine := c.engine()
	if engine == nil {
		return nil, wrapGatewayError("gateway/not-composed", sessionPageEndpoint, "", nil, "session page has no session query engine")
	}
	throughSeq, err := pageReadsThrough(request)
	if err != nil {
		return nil, err
	}
	beforeSeq := pageReadsBefore(request)

	snapshot, err := engine.ReadSession(ctx, session.SessionID(sessionID))
	if err != nil {
		if isSessionNotFound(err) {
			return nil, wrapGatewayError("session/not-found", sessionPageEndpoint, "address", nil, "session %q not found", sessionID)
		}
		return nil, wrapGatewayError("gateway/internal", sessionPageEndpoint, "", err, "session page read failed")
	}
	events := snapshot.Events
	sourceCursor := int64(-1)
	if len(events) > 0 {
		sourceCursor = events[len(events)-1].Seq
	}
	if throughSeq > sourceCursor {
		return nil, wrapGatewayError("gateway/bad-request", sessionPageEndpoint, "throughSeq", nil,
			"session page through seq %d is past cursor %d", throughSeq, sourceCursor)
	}
	page, hasMore := paginateHistory(events, beforeSeq, followMaxMessages(request), throughSeq)
	return map[string]any{
		"records": followRecords(page),
		"hasMore": hasMore,
	}, nil
}

// isSessionNotFound reports whether err is the query engine's not-found
// code (corpus.Load surfaces it for both live and persisted misses).
func isSessionNotFound(err error) bool {
	var queryErr *sessionquery.SessionQueryError
	return errors.As(err, &queryErr) && queryErr.Code == sessionquery.CodeSessionNotFound
}
