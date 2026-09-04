package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/typert"
)

func newFollowGateway(t *testing.T) (*Gateway, *session.Store) {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := typert.NewRegistry(root, cordis.Discard{})
	gateway := New(root, registry)
	store := session.NewStore(nil)
	root.Provide(sessionsStoreService, store)
	return gateway, store
}

func followFrame(t *testing.T, frames <-chan any) map[string]any {
	t.Helper()
	select {
	case frame := <-frames:
		row, ok := frame.(map[string]any)
		if !ok {
			t.Fatalf("want a frame map, got %T", frame)
		}
		return row
	case <-context.Background().Done():
		t.Fatal("no frame arrived")
		return nil
	}
}

func TestFollowRefusesSubagentAddressesUntilThatRound(t *testing.T) {
	gateway, _ := newFollowGateway(t)
	_, _, err := gateway.openSessionFollow(map[string]any{
		"address": map[string]any{"kind": "subagent", "parentSessionId": "p", "childSessionId": "c", "mode": "one-shot"},
	}, context.Background())
	if err == nil || !strings.Contains(err.Error(), "not served yet") {
		t.Fatalf("want the subagent-address refusal, got %v", err)
	}
}

func TestFollowRefusesUnknownSessions(t *testing.T) {
	gateway, _ := newFollowGateway(t)
	_, _, err := gateway.openSessionFollow(map[string]any{
		"address": map[string]any{"kind": "session", "sessionId": "session-missing"},
	}, context.Background())
	if err == nil || !strings.Contains(err.Error(), "not live") {
		t.Fatalf("want the not-live refusal, got %v", err)
	}
}

func TestFollowSnapshotsALiveSessionWithHeaderCursorAndBaseline(t *testing.T) {
	gateway, store := newFollowGateway(t)
	sess, err := store.Create("session-live", session.CreateOptions{
		HeaderMetadata: session.SessionHeader{CWD: `C:\tmp`, AgentPreset: "standard"},
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	_, err = sess.Append("user/message", llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}},
		llm.MessageSource{Kind: llm.SourceUser},
	), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}})
	if err != nil {
		t.Fatalf("append prompt: %v", err)
	}
	// A non-message informational event after the prompt must still ride
	// the snapshot when the message budget is not yet exhausted.
	if _, err := sess.Append("turn/start", map[string]any{"turn": float64(1)}, nil); err != nil {
		t.Fatalf("append turn start: %v", err)
	}

	signal, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	frames, _, err := gateway.openSessionFollow(map[string]any{
		"address": map[string]any{"kind": "session", "sessionId": "session-live"},
	}, signal)
	if err != nil {
		t.Fatalf("open follow: %v", err)
	}
	snapshot := followFrame(t, frames)
	if snapshot["type"] != "snapshot" {
		t.Fatalf("want the snapshot frame, got %v", snapshot["type"])
	}
	cursor, ok := snapshot["cursor"].(int64)
	if !ok || cursor != 1 {
		t.Fatalf("cursor = %v (%T), want the last event seq 1", snapshot["cursor"], snapshot["cursor"])
	}
	encoded, err := json.Marshal(snapshot["header"])
	if err != nil {
		t.Fatalf("header encode: %v", err)
	}
	var header map[string]any
	if err := json.Unmarshal(encoded, &header); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if header["id"] != "session-live" || header["cwd"] != `C:\tmp` || header["agentPreset"] != "standard" {
		t.Fatalf("wire header fields missing: %v", header)
	}
	projections, ok := snapshot["projections"].(map[string]any)
	if !ok {
		t.Fatalf("want a projections block, got %T", snapshot["projections"])
	}
	if _, hasValues := projections["values"]; !hasValues {
		t.Fatalf("projections baseline lacks values: %v", projections)
	}
	if snapshot["hasMore"] != false {
		t.Fatalf("a two-event log must not claim more pages, got %v", snapshot["hasMore"])
	}
}

func TestFollowPaginatesByMessageCount(t *testing.T) {
	gateway, store := newFollowGateway(t)
	sess, err := store.Create("session-paged", session.CreateOptions{})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	appendMessage := func(text string) {
		t.Helper()
		if _, err := sess.Append("user/message", llm.NewUserMessage(
			[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
			llm.MessageSource{Kind: llm.SourceUser},
		), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
			t.Fatalf("append %q: %v", text, err)
		}
	}
	appendMessage("one")
	appendMessage("two")
	appendMessage("three")

	frames, cancel, err := gateway.openSessionFollow(map[string]any{
		"address":     map[string]any{"kind": "session", "sessionId": "session-paged"},
		"maxMessages": float64(2),
	}, context.Background())
	if err != nil {
		t.Fatalf("open follow: %v", err)
	}
	t.Cleanup(cancel)
	snapshot := followFrame(t, frames)
	if snapshot["hasMore"] != true {
		t.Fatalf("a three-message log cut at two must claim hasMore, got %v", snapshot["hasMore"])
	}
	encoded, err := json.Marshal(snapshot["records"])
	if err != nil {
		t.Fatalf("records encode: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(encoded, &records); err != nil {
		t.Fatalf("records decode: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("want exactly the newest two records, got %d: %v", len(records), records)
	}
	first, _ := records[0]["event"].(map[string]any)
	if first == nil || first["seq"] != float64(1) {
		t.Fatalf("the page must start at the cut event, got %v", records[0])
	}
}
