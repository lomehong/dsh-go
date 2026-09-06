// Tests for session/updateQueue (pending-queue mutations over the live
// inbox) and the search endpoint's composition posture.
package gateway

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// silentNotifications satisfies the inbox live-notification face without
// observers.
type silentNotifications struct{}

func (silentNotifications) Inserted(llm.Message)       {}
func (silentNotifications) Discarded(llm.Message)      {}
func (silentNotifications) Claimed(llm.Message, int64) {}

// queueFixture builds the controller plus a live agent whose inbox is
// bound to its session (the queued-occurrence store).
func queueFixture(t *testing.T) (*SessionController, session.SessionID, *agent.Agent) {
	t.Helper()
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	live := controller.liveAgent(sessionID)
	if live.Inbox == nil {
		inbox, err := agent.NewInbox(live.Session, silentNotifications{})
		if err != nil {
			t.Fatalf("inbox: %v", err)
		}
		live.Inbox = inbox
	}
	return controller, sessionID, live
}

// queueMessage mints one identified user message.
func queueMessage(id, text string) llm.Message {
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourceUser, RPCID: "rpc-" + id})
	message.ID = llm.MessageID(id)
	return message
}

// An edit replaces the queued message's text content in place; remove drops
// the occurrence; the queued item lookup matches by message id.
func TestUpdateQueueEditAndRemove(t *testing.T) {
	controller, sessionID, live := queueFixture(t)
	message := queueMessage("msg-q1", "original")
	if err := live.Inbox.Append("next-turn", message); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Edit rewrites the queued content in place.
	if _, err := controller.UpdateQueue(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"itemId":    "msg-q1",
		"action":    map[string]any{"kind": "edit", "content": []any{map[string]any{"type": "text", "text": "edited"}}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	queued := live.Inbox.NextTurn()
	if len(queued) != 1 || len(queued[0].Content) != 1 || queued[0].Content[0].Text != "edited" {
		t.Fatalf("queued = %+v", queued)
	}

	// Remove drops the occurrence entirely.
	if _, err := controller.UpdateQueue(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"itemId":    "msg-q1",
		"action":    map[string]any{"kind": "remove"},
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(live.Inbox.NextTurn()) != 0 {
		t.Fatalf("queued = %+v, want empty", live.Inbox.NextTurn())
	}
}

// Steering refuses for an idle agent (nothing to steer into) with the
// official session/steer-unavailable code, and keeps the item queued.
func TestUpdateQueueSteerUnavailableWhenIdle(t *testing.T) {
	controller, sessionID, live := queueFixture(t)
	message := queueMessage("msg-s1", "redirect me")
	if err := live.Inbox.Append("next-turn", message); err != nil {
		t.Fatalf("append: %v", err)
	}

	_, err := controller.UpdateQueue(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"itemId":    "msg-s1",
		"action":    map[string]any{"kind": "steer"},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/steer-unavailable" {
		t.Fatalf("err = %v, want session/steer-unavailable", err)
	}
	if len(live.Inbox.NextTurn()) != 1 {
		t.Fatalf("queued = %+v, want the item retained", live.Inbox.NextTurn())
	}
}

// An unknown or already-settled item answers the official
// session/queue-item-not-found wording.
func TestUpdateQueueAnswersItemNotFound(t *testing.T) {
	controller, sessionID, _ := queueFixture(t)
	_, err := controller.UpdateQueue(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"itemId":    "msg-gone",
		"action":    map[string]any{"kind": "remove"},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/queue-item-not-found" {
		t.Fatalf("err = %v, want session/queue-item-not-found", err)
	}
}

// Non-text queue edits refuse (official QUEUE_EDIT_NON_TEXT rule).
func TestUpdateQueueRejectsNonTextEdits(t *testing.T) {
	controller, sessionID, live := queueFixture(t)
	message := queueMessage("msg-n1", "x")
	if err := live.Inbox.Append("next-turn", message); err != nil {
		t.Fatalf("append: %v", err)
	}
	_, err := controller.UpdateQueue(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"itemId":    "msg-n1",
		"action": map[string]any{"kind": "edit", "content": []any{
			map[string]any{"type": "image", "mediaType": "image/png", "data": "aGk="},
		}},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/attachment-invalid" {
		t.Fatalf("err = %v, want session/attachment-invalid", err)
	}
}

// Without a composed engine, search answers not-composed (the engine is
// the search backend owner; the disabled-backend code rides the engine).
func TestSearchAnswersNotComposedWithoutEngine(t *testing.T) {
	controller, _, _ := queueFixture(t)
	_, err := controller.SearchSessions(context.Background(), map[string]any{"query": "anything"})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "gateway/not-composed" {
		t.Fatalf("err = %v, want gateway/not-composed", err)
	}
	if !strings.Contains(err.Error(), "search") {
		t.Fatalf("err = %v, want the search diagnostic", err)
	}
}
