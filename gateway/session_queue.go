// Session content search + pending-queue mutations (official
// api-session-controller search/updateQueue): the sidebar's content search
// answers through the composed session-query engine, and queue edits
// mutate still-pending inbox occurrences in place.
package gateway

import (
	"context"
	"errors"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
)

// SearchSessions answers the cross-session content search (official
// session/search): a literal message-content query over the composed
// engine, no Agent resume. A deployment without a composed search backend
// answers the not-composed posture; a disabled backend surfaces the
// engine's stable code.
func (c *SessionController) SearchSessions(ctx context.Context, request map[string]any) (any, error) {
	engine := c.engine()
	if engine == nil {
		return nil, wrapGatewayError("gateway/not-composed", "session/search", "", nil, "session search has no session query engine")
	}
	query := requestString(request, "query")
	if strings.TrimSpace(query) == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/search", "query", nil, "session search requires a query")
	}
	page, err := engine.SearchSessions(ctx, sessionquery.SessionSearchRequest{Query: query})
	if err != nil {
		var queryErr *sessionquery.SessionQueryError
		if errors.As(err, &queryErr) {
			return nil, wrapGatewayError(GatewayErrorCode(queryErr.Code), "session/search", "", err, "%v", err)
		}
		return nil, wrapGatewayError("gateway/internal", "session/search", "", err, "session search failed")
	}
	items := make([]any, 0, len(page.Items))
	for _, hit := range page.Items {
		item := map[string]any{
			"sessionId": string(hit.Header.ID),
			"snippet":   hit.BestMatch.Snippet,
		}
		items = append(items, item)
	}
	value := map[string]any{"items": items, "hasMore": page.NextCursor != nil}
	if page.NextCursor != nil {
		value["cursor"] = *page.NextCursor
	}
	return value, nil
}

// queueAction is one decoded pending-queue mutation.
type queueAction struct {
	Kind    string
	Content []llm.ContentBlock
}

// errQueueEditNonText marks the official QUEUE_EDIT_NON_TEXT refusal: queue
// edits accept text content only.
var errQueueEditNonText = errors.New("queue edits accept text content only")

// decodeQueueAction reads the wire action: edit (text-only content),
// remove, steer.
func decodeQueueAction(request map[string]any) (queueAction, error) {
	raw, ok := request["action"].(map[string]any)
	if !ok {
		return queueAction{}, errors.New("action must be an object")
	}
	action := queueAction{Kind: requestString(raw, "kind")}
	switch action.Kind {
	case "edit":
		content, ok := raw["content"].([]any)
		if !ok {
			return queueAction{}, errors.New("queue edit requires content")
		}
		for _, block := range content {
			fields, ok := block.(map[string]any)
			if !ok {
				return queueAction{}, errors.New("queue edit content must be block objects")
			}
			if requestString(fields, "type") != llm.BlockText {
				return queueAction{}, errQueueEditNonText
			}
			text, _ := fields["text"].(string)
			action.Content = append(action.Content, llm.ContentBlock{Type: llm.BlockText, Text: text})
		}
	case "remove", "steer":
	default:
		return queueAction{}, errors.New("queue action kind must be edit, remove, or steer")
	}
	return action, nil
}

// UpdateQueue mutates one still-pending queue occurrence without resuming a
// cold Agent (official session/updateQueue): edit replaces the queued
// message's text content in place, remove drops it, steer promotes it into
// the running turn's steering.
func (c *SessionController) UpdateQueue(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/updateQueue", "", nil, "queue mutations are not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	itemID := requestString(request, "itemId")
	if sessionID == "" || itemID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/updateQueue", "", nil, "queue mutation requires a sessionId and itemId")
	}
	action, err := decodeQueueAction(request)
	if err != nil {
		if errors.Is(err, errQueueEditNonText) {
			return nil, wrapGatewayError("session/attachment-invalid", "session/updateQueue", "", err, "%v", err)
		}
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/updateQueue", "action", err, "%v", err)
	}
	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/queue-item-not-found", "session/updateQueue", "itemId", nil,
			"queued item is no longer pending")
	}
	if depth := live.Session.Header().DelegationDepth; depth != nil && *depth > 0 {
		return nil, wrapGatewayError("session/subagent-owned", "session/updateQueue", "sessionId", nil,
			"session %q is owned by a live subagent", sessionID)
	}

	// Locate the pending occurrence across both queue lanes.
	target := agent.InboxTarget("")
	index := int64(-1)
	var message llm.Message
	for candidate, list := range map[agent.InboxTarget][]llm.Message{
		agent.InboxNextTurn: live.Inbox.NextTurn(),
		agent.InboxNextStep: live.Inbox.NextStep(),
	} {
		for position, item := range list {
			if string(item.ID) == itemID {
				target, index, message = candidate, int64(position), item
				break
			}
		}
		if index >= 0 {
			break
		}
	}
	if index < 0 {
		return nil, wrapGatewayError("session/queue-item-not-found", "session/updateQueue", "itemId", nil,
			"queued item is no longer pending")
	}

	switch action.Kind {
	case "edit":
		message.Content = action.Content
		if _, err := live.Inbox.Splice(target, index, 1, []llm.Message{message}); err != nil {
			return nil, wrapGatewayError("gateway/internal", "session/updateQueue", "", err, "%v", err)
		}
	case "remove":
		if _, err := live.Inbox.Splice(target, index, 1, nil); err != nil {
			return nil, wrapGatewayError("gateway/internal", "session/updateQueue", "", err, "%v", err)
		}
	case "steer":
		if target != agent.InboxNextTurn || live.Status() != agent.AgentRunning {
			return nil, wrapGatewayError("session/steer-unavailable", "session/updateQueue", "itemId", nil,
				"current turn no longer accepts steering")
		}
		if _, err := live.Inbox.Splice(target, index, 1, nil); err != nil {
			return nil, wrapGatewayError("gateway/internal", "session/updateQueue", "", err, "%v", err)
		}
		live.Driver().Steer(message)
	}
	return map[string]any{"accepted": true}, nil
}
