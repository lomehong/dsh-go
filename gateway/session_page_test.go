package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

// pageFixture reuses the session-list engine wiring: the controller's page
// endpoint reads the same live store through the same engine.
type pageFixture = sessionListFixture

func newPageFixture(t *testing.T) *pageFixture {
	return newSessionListFixture(t)
}

// appendUserMessage appends one append-surface user/message event.
func (f *pageFixture) appendUserMessage(t *testing.T, sess *session.Session, text string) {
	t.Helper()
	if _, err := sess.Append(session.EventUserMessage, llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourceUser},
	), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append %q: %v", text, err)
	}
}

func pageRequest(address string, extras map[string]any) map[string]any {
	req := map[string]any{"address": map[string]any{"kind": "session", "sessionId": address}}
	for key, value := range extras {
		req[key] = value
	}
	return req
}

func TestPageRefusesUnknownSessions(t *testing.T) {
	f := newPageFixture(t)
	_, err := f.controller.Page(context.Background(), pageRequest("session-missing", map[string]any{"throughSeq": float64(-1)}))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want the not-found refusal, got %v", err)
	}
}

func TestPageRequiresThroughSeq(t *testing.T) {
	f := newPageFixture(t)
	sess, err := f.store.Create("session-page-no-through", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = sess
	_, err = f.controller.Page(context.Background(), pageRequest("session-page-no-through", nil))
	if err == nil || !strings.Contains(err.Error(), "throughSeq") {
		t.Fatalf("want the missing-throughSeq refusal, got %v", err)
	}
}

func TestPageRefusesThroughSeqPastCursor(t *testing.T) {
	f := newPageFixture(t)
	sess, err := f.store.Create("session-page-past", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.appendUserMessage(t, sess, "one")
	_, err = f.controller.Page(context.Background(), pageRequest("session-page-past", map[string]any{"throughSeq": float64(99)}))
	if err == nil || !strings.Contains(err.Error(), "past cursor") {
		t.Fatalf("want the past-cursor refusal, got %v", err)
	}
}

func TestPageReturnsMessageAlignedRecords(t *testing.T) {
	f := newPageFixture(t)
	sess, err := f.store.Create("session-page", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.appendUserMessage(t, sess, "one")
	f.appendUserMessage(t, sess, "two")
	f.appendUserMessage(t, sess, "three")
	// A non-message event after the third message stays in the tail window.
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	cursor := sess.Seq() - 1

	value, err := f.controller.Page(context.Background(), pageRequest("session-page", map[string]any{
		"throughSeq":  float64(cursor),
		"maxMessages": float64(2),
	}))
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	body := value.(map[string]any)
	if body["hasMore"] != true {
		t.Fatalf("a three-message log cut at two must claim hasMore, got %v", body["hasMore"])
	}
	encoded, err := json.Marshal(body["records"])
	if err != nil {
		t.Fatalf("records encode: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(encoded, &records); err != nil {
		t.Fatalf("records decode: %v", err)
	}
	// The cut lands on the second-newest message ("two", seq 1); the window
	// spans events[1:4] = two/three/turn-start — the trailing non-message
	// event rides the page, matching official paginate semantics.
	if len(records) != 3 {
		t.Fatalf("want the cut window (two/three/turn-start), got %d records: %v", len(records), records)
	}
	firstEvent, _ := records[0]["event"].(map[string]any)
	secondEvent, _ := records[1]["event"].(map[string]any)
	firstData, _ := firstEvent["data"].(map[string]any)
	firstContent, _ := firstData["content"].([]any)
	if len(firstContent) == 0 {
		t.Fatalf("window start message has no content: %v", firstEvent)
	}
	firstBlock, _ := firstContent[0].(map[string]any)
	if text, _ := firstBlock["text"].(string); text != "two" {
		t.Fatalf("window start text = %q, want two", text)
	}
	if secondEvent["type"] != "user/message" {
		t.Fatalf("second record type = %v, want user/message", secondEvent["type"])
	}
}

func TestPageOlderWindowRespectsBeforeSeq(t *testing.T) {
	f := newPageFixture(t)
	sess, err := f.store.Create("session-page-window", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.appendUserMessage(t, sess, "one")
	f.appendUserMessage(t, sess, "two")
	f.appendUserMessage(t, sess, "three")
	cursor := sess.Seq() - 1

	// beforeSeq=2 excludes the newest event (seq 2): the page covers seqs 0-1.
	value, err := f.controller.Page(context.Background(), pageRequest("session-page-window", map[string]any{
		"throughSeq":  float64(cursor),
		"beforeSeq":   float64(2),
		"maxMessages": float64(50),
	}))
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	body := value.(map[string]any)
	encoded, err := json.Marshal(body["records"])
	if err != nil {
		t.Fatalf("records encode: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(encoded, &records); err != nil {
		t.Fatalf("records decode: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("beforeSeq window must exclude the newest event, got %d records: %v", len(records), records)
	}
}
