// Tests for session/fork: the completed-turn boundary resolution, the
// seeded child creation through the store, and the refusal shapes.
package gateway

import (
	"context"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

// turnEndEvent mints a completed turn/end marker at one seq (the boundary
// scan reads only the type and seq).
func turnEndEvent(seq int64) session.Event {
	return session.Event{Type: session.EventTurnEnd, Seq: seq, Time: seq * 1000}
}

// forkBoundaryTests pin the inclusive-index semantics: the last completed
// turn wins, atSeq selects the turn containing it, trailing inter-turn
// events ride along, and a turnless log refuses.
func TestForkBoundarySemantics(t *testing.T) {
	events := []session.Event{
		{Type: session.EventTurnStart, Seq: 0},
		turnEndEvent(1),
		{Type: session.EventUserMessage, Seq: 2},
		{Type: session.EventTurnStart, Seq: 3},
		turnEndEvent(4),
		{Type: session.EventUserMessage, Seq: 5},
		{Type: session.EventTurnStart, Seq: 6},
		turnEndEvent(7),
		{Type: session.EventUserMessage, Seq: 8},
	}
	boundary, err := forkBoundary(events, nil)
	if err != nil || boundary != 8 {
		t.Fatalf("boundary = %d, %v; want 8 (trailing user/message rides)", boundary, err)
	}
	at := int64(2)
	boundary, err = forkBoundary(events, &at)
	if err != nil || boundary != 5 {
		t.Fatalf("atSeq 2 boundary = %d, %v; want 5 (first turn/end at-or-after)", boundary, err)
	}
	// Past-end atSeq CLAMPS to the last completed turn (official fallback:
	// atSeq > lastSeq → findLast(turn/end)) — it does not refuse.
	future := int64(99)
	boundary, err = forkBoundary(events, &future)
	if err != nil || boundary != 8 {
		t.Fatalf("past-end atSeq boundary = %d, %v; want 8 (clamp to last completed turn)", boundary, err)
	}
	// An atSeq INSIDE the log whose containing turn never completed refuses
	// with the containing-turn message (atSeq 8: the trailing user/message
	// inside the open third turn).
	inside := int64(8)
	if _, err := forkBoundary(events, &inside); err == nil ||
		!strings.Contains(err.Error(), "not completed the turn containing event 8") {
		t.Fatalf("err = %v, want the uncompleted-turn refusal", err)
	}
	if _, err := forkBoundary(nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no completed turn") {
		t.Fatalf("err = %v, want the turnless refusal", err)
	}
}

// The full fork: a completed turn forks into a seeded child whose header
// carries the parent lineage, and the child composes live.
func TestForkCreatesSeededChild(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	live := controller.liveAgent(sessionID)

	// One completed turn plus one trailing inter-turn event: the fork cut
	// lands after the trailing event.
	sess := live.Session
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn start: %v", err)
	}
	end := session.TurnEndData{Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}
	if _, err := sess.Append(session.EventTurnEnd, end, nil); err != nil {
		t.Fatalf("turn end: %v", err)
	}
	if _, err := sess.Append(session.EventUserMessage, llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "summary"}},
		llm.MessageSource{Kind: llm.SourceUser},
	), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("trailing event: %v", err)
	}

	value, err := controller.Fork(context.Background(), map[string]any{"sessionId": string(sessionID)})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	childID := value.(map[string]any)["sessionId"].(string)
	if !strings.HasPrefix(childID, "session-") {
		t.Fatalf("child id = %q", childID)
	}

	child := controller.liveAgent(session.SessionID(childID))
	if child == nil {
		t.Fatal("the forked child has no live agent")
	}
	header := child.Session.Header()
	if header.ParentSession != sessionID || !header.IsSeeded {
		t.Fatalf("header = %+v", header)
	}
	if len(child.Session.Events()) == 0 {
		t.Fatal("the child log carries no inherited events")
	}
}

// A session with no completed turn refuses with fork-unavailable.
func TestForkRefusesTurnlessSession(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	_, err := controller.Fork(context.Background(), map[string]any{"sessionId": string(sessionID)})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/fork-unavailable" {
		t.Fatalf("err = %v, want session/fork-unavailable", err)
	}
}
