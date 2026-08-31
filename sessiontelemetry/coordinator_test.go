package sessiontelemetry

import (
	"sync"
	"testing"

	"dshgo/cordis"
	"dshgo/session"
)

// recordingSink captures emitted records for assertions.
type recordingSink struct {
	mu      sync.Mutex
	records []Record
}

func (s *recordingSink) Emit(record Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
}
func (s *recordingSink) Flush()          {}
func (s *recordingSink) Shutdown() error { return nil }
func (s *recordingSink) snapshot() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.records...)
}

func testStore(t *testing.T) *session.Store {
	t.Helper()
	store := session.NewStore(nil)
	return store
}

func appendEvent(t *testing.T, store *session.Store, sess *session.Session, eventType string, data any) session.Event {
	t.Helper()
	var intent *session.SurfaceIntent
	if session.IsSurfaceEventType(eventType) {
		intent = &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
	}
	event, err := sess.Append(eventType, data, intent)
	if err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
	return event
}

func TestCoordinatorLiveCaptureLedgerAndChunkProjection(t *testing.T) {
	store := testStore(t)
	sink := &recordingSink{}
	coord := NewCoordinator(store, nil, cordis.Discard{}, sink, nil)
	_ = coord

	sess, err := store.Create("s1", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A user message plus two chunks of one step: only the first chunk ships.
	appendEvent(t, store, sess, session.EventUserMessage, map[string]any{
		"id": "msg-1", "role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}},
		"source": map[string]any{"kind": "user"},
	})
	appendEvent(t, store, sess, session.EventAssistantChunk, map[string]any{"turn": 1, "step": 1, "text": "a"})
	appendEvent(t, store, sess, session.EventAssistantChunk, map[string]any{"turn": 1, "step": 1, "text": "b"})

	records := sink.snapshot()
	if len(records) != 2 {
		t.Fatalf("records = %d, want user+first-chunk (2); got %#v", len(records), records)
	}
	if records[0].Channel != "ledger" || records[0].Attributes["event.type"] != session.EventUserMessage {
		t.Fatalf("first = %+v", records[0])
	}
	if records[1].Attributes["event.type"] != session.EventAssistantChunk {
		t.Fatalf("second = %+v", records[1])
	}
}

func TestCoordinatorOnDemandCaptureReplaysCanonicalLog(t *testing.T) {
	store := testStore(t)
	sink := &recordingSink{}
	coord := NewOnDemandCoordinator(store, cordis.Discard{}, sink, nil)

	sess, err := store.Create("s2", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendEvent(t, store, sess, session.EventUserMessage, map[string]any{
		"id": "msg-1", "role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}},
		"source": map[string]any{"kind": "user"},
	})
	if got := len(sink.snapshot()); got != 0 {
		t.Fatalf("on-demand coordinator must not capture on append, got %d", got)
	}
	coord.CaptureSession(sess, -1)
	if got := len(sink.snapshot()); got != 1 {
		t.Fatalf("capture after request = %d", got)
	}
}

func TestCoordinatorRedactionWaterfallTransforms(t *testing.T) {
	store := testStore(t)
	sink := &recordingSink{}
	redacted := 0
	waterfall := func(record Record) Record {
		redacted++
		record.Attributes["redacted"] = "yes"
		return record
	}
	coord := NewCoordinator(store, nil, cordis.Discard{}, sink, waterfall)
	_ = coord

	sess, err := store.Create("s3", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	appendEvent(t, store, sess, session.EventUserMessage, map[string]any{
		"id": "msg-1", "role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}},
		"source": map[string]any{"kind": "user"},
	})
	records := sink.snapshot()
	if len(records) != 1 || records[0].Attributes["redacted"] != "yes" {
		t.Fatalf("waterfall not applied: %+v", records)
	}
	if redacted != 1 {
		t.Fatalf("waterfall calls = %d", redacted)
	}
}
