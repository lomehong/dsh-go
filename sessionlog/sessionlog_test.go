package sessionlog

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func newSession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	return sess
}

func accept(t *testing.T, sess *session.Session, sessionID string, throughSeq int64) {
	t.Helper()
	data, err := json.Marshal(DeliveryAcceptedData{SessionID: sessionID, ThroughSeq: throughSeq})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := sess.Append(EventTypeDeliveryAccepted, json.RawMessage(data), nil); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func TestAcceptedThroughFoldsIncrementally(t *testing.T) {
	folder := NewFolder()
	sess := newSession(t, "log-1")
	if through, err := folder.AcceptedThrough(sess); err != nil || through != -1 {
		t.Fatalf("empty = %d %v", through, err)
	}
	// Seed a log so watermark targets exist: events take seq 0..12.
	for i := 0; i < 13; i++ {
		if _, err := sess.Append(session.EventUserMessage, llmUserMessage("m"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	accept(t, sess, "log-1", 5)
	if through, err := folder.AcceptedThrough(sess); err != nil || through != 5 {
		t.Fatalf("first = %d %v", through, err)
	}
	accept(t, sess, "log-1", 12)
	accept(t, sess, "log-1", 9)
	if through, err := folder.AcceptedThrough(sess); err != nil || through != 12 {
		t.Fatalf("max = %d %v", through, err)
	}
	// A different session identity's watermarks never leak across.
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("m"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	accept(t, sess, "other-session", 13)
	if through, err := folder.AcceptedThrough(sess); err != nil || through != 12 {
		t.Fatalf("cross-session = %d %v", through, err)
	}
	// A second folder folds the whole log from scratch to the same answer.
	if through, err := NewFolder().AcceptedThrough(sess); err != nil || through != 12 {
		t.Fatalf("cold fold = %d %v", through, err)
	}
}

func TestAcceptedThroughRejectsMalformedWatermarks(t *testing.T) {
	cases := map[string]DeliveryAcceptedData{
		"empty session id": {SessionID: "", ThroughSeq: 3},
		"negative seq":     {SessionID: "log-2", ThroughSeq: -1},
		"seq beyond self":  {SessionID: "log-2", ThroughSeq: 500},
	}
	for name, data := range cases {
		folder := NewFolder()
		sess := newSession(t, "log-2")
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := sess.Append(EventTypeDeliveryAccepted, json.RawMessage(raw), nil); err != nil {
			t.Fatalf("append: %v", err)
		}
		foldErr := func() error { _, e := folder.AcceptedThrough(sess); return e }()
		if foldErr == nil || !strings.Contains(foldErr.Error(), "malformed acceptance watermark at seq") {
			t.Fatalf("%s accepted: %v", name, foldErr)
		}
	}
	// Non-matching event types never trip the fold.
	folder := NewFolder()
	sess := newSession(t, "log-3")
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("hi"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if through, err := folder.AcceptedThrough(sess); err != nil || through != -1 {
		t.Fatalf("foreign event = %d %v", through, err)
	}
}

func TestPrepareBuildsSuffixAndAcceptRecordsWatermark(t *testing.T) {
	folder := NewFolder()
	sess := newSession(t, "log-4")
	// An empty log contributes nothing.
	if prepared, err := Prepare(folder, sess); err != nil || prepared != nil {
		t.Fatalf("empty prepare = %+v %v", prepared, err)
	}
	// Seed two events, then one accepted watermark.
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("one"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("two"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	accept(t, sess, "log-4", 1)

	prepared, err := Prepare(folder, sess)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared.Value.Version != 1 || prepared.Value.AfterSeq != 1 || prepared.Value.ThroughSeq != int64(len(sess.Events())-1) {
		t.Fatalf("value = %+v events=%d", prepared.Value, len(sess.Events()))
	}
	if prepared.Value.Session.ID != session.SessionID("log-4") {
		t.Fatalf("header = %+v", prepared.Value.Session)
	}
	if len(prepared.Value.Events) == 0 {
		t.Fatal("suffix empty")
	}
	// The contribution is stable across re-preparation without new events.
	again, err := Prepare(folder, sess)
	if err != nil || again.Value.AfterSeq != prepared.Value.AfterSeq || again.Value.ThroughSeq != prepared.Value.ThroughSeq {
		t.Fatalf("re-prepare = %+v", again.Value)
	}
	// Acceptance records the watermark durably; the next prepare resumes
	// after it.
	if err := prepared.Accept(); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if through, err := folder.AcceptedThrough(sess); err != nil || through != prepared.Value.ThroughSeq {
		t.Fatalf("post-accept = %d %v", through, err)
	}
	next, err := Prepare(folder, sess)
	if err != nil {
		t.Fatalf("next prepare: %v", err)
	}
	if next.Value.AfterSeq != prepared.Value.ThroughSeq {
		t.Fatalf("afterSeq = %d, want %d", next.Value.AfterSeq, prepared.Value.ThroughSeq)
	}
	if len(next.Value.Events) != 1 || next.Value.Events[0].Type != EventTypeDeliveryAccepted {
		t.Fatalf("suffix = %+v", next.Value.Events)
	}
}

// llmUserMessage builds a minimal durable user message for log seeding.
func llmUserMessage(text string) llm.Message {
	return llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourceUser},
	)
}
