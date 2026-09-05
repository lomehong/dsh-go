package sessionturnoutline

import (
	"encoding/json"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func mustEvent(t *testing.T, raw string) session.Event {
	t.Helper()
	var event session.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatal(err)
	}
	return event
}

func foldAll(t *testing.T, events ...session.Event) State {
	t.Helper()
	state := Projection.Init(session.SessionHeader{})
	for _, event := range events {
		state, _ = Projection.Apply(state, event)
	}
	return state
}

func TestOutlineFoldsTurnBoundariesPreviewsAndDraftCommit(t *testing.T) {
	state := foldAll(t,
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"user/message","seq":1,"time":2,"data":{"id":"u1","role":"user","content":[{"type":"text","text":"  hello   world  "}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"assistant/message","seq":2,"time":3,"data":{"turn":1,"step":1,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"the response"}],"source":{"kind":"model","provider":"p","model":"m"}}},"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"turn/end","seq":3,"time":4,"data":{"turn":1,"reason":{"kind":"completed"}}}`),
	)
	if len(state.Turns) != 1 || state.Draft != "" {
		t.Fatalf("outline: %+v", state)
	}
	entry := state.Turns[0]
	if entry.Turn != 1 || entry.Seq != 0 {
		t.Fatalf("entry header: %+v", entry)
	}
	if entry.Prompt != "hello world" {
		t.Fatalf("prompt preview: %q", entry.Prompt)
	}
	if entry.Response != "the response" {
		t.Fatalf("response preview: %q", entry.Response)
	}
}

func TestOutlineClipsWithEllipsisAndKeepsFirstPrompt(t *testing.T) {
	long := ""
	for len(long) < PromptPreviewLimit*3 {
		long += "word "
	}
	longPrompt := `{"type":"user/message","seq":1,"time":2,"data":{"id":"u1","role":"user","content":[{"type":"text","text":"` + long + `"}],"source":{"kind":"user"}},"surfaceOp":"append"}`
	steering := `{"type":"user/message","seq":2,"time":3,"data":{"id":"u2","role":"user","content":[{"type":"text","text":"steer"}],"source":{"kind":"user"}},"surfaceOp":"append"}`
	state := foldAll(t,
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, longPrompt),
		mustEvent(t, steering),
	)
	entry := state.Turns[0]
	if len([]rune(entry.Prompt)) > PromptPreviewLimit {
		t.Fatalf("prompt budget: %d", len([]rune(entry.Prompt)))
	}
	if entry.Prompt[len(entry.Prompt)-len("…"):] != "…" {
		t.Fatalf("clipped prompt must carry the ellipsis: %q", entry.Prompt)
	}
}

func TestOutlineIgnoresOutOfOrderTurnBoundary(t *testing.T) {
	state := foldAll(t,
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":2}}`),
		mustEvent(t, `{"type":"turn/end","seq":1,"time":2,"data":{"turn":2,"reason":{"kind":"completed"}}}`),
		mustEvent(t, `{"type":"turn/start","seq":2,"time":3,"data":{"turn":2}}`),
	)
	if len(state.Turns) != 1 {
		t.Fatalf("non-advancing boundary must not append: %+v", state.Turns)
	}
}

func TestOutlineDecodeStateValidatesOrder(t *testing.T) {
	if _, err := Projection.DecodeState(json.RawMessage(`{"turns":[{"turn":2,"seq":0,"prompt":"","response":""},{"turn":1,"seq":1,"prompt":"","response":""}],"draft":""}`)); err == nil {
		t.Fatal("non-increasing turns must refuse the persisted row")
	}
	if _, err := Projection.DecodeState(json.RawMessage(`{"turns":[],"draft":""}`)); err != nil {
		t.Fatalf("empty outline refused: %v", err)
	}
}

var _ = llm.SourceUser
