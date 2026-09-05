package gateway

import (
	"context"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/typert"
)

// TestFollowRelaysLiveEventsAndAssistantFrames proves the live
// continuation: durable events past the snapshot cursor arrive as `event`
// frames over the cordis session/event feed, and assistant-stream frames
// route by the attempt id's session prefix through the registry bus.
func TestFollowRelaysLiveEventsAndAssistantFrames(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	typertRegistry := typert.NewRegistry(root, cordis.Discard{})
	gateway := New(root, typertRegistry)
	store := session.NewStore(nil)
	root.Provide(sessionsStoreService, store)
	agentRegistry := agent.NewAgentRegistry(root, cordis.Discard{})
	root.Provide("agents", agentRegistry)
	// The store→cordis bridge (the composition mounts it in the dsh-session
	// row): follow's live feed rides the same multiplexed stream.
	store.OnEvent(func(live *session.Session, event session.Event) {
		root.Waterfall("session/event", &projection.SessionEventPayload{Session: live, Event: event})
	})

	sess, err := store.Create("session-follow-live", session.CreateOptions{
		HeaderMetadata: session.SessionHeader{Version: session.SESSION_FORMAT_VERSION, CWD: `C:\tmp`},
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := sess.Append("user/message", llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}},
		llm.MessageSource{Kind: llm.SourceUser},
	), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append prompt: %v", err)
	}

	signal, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	frames, _, err := gateway.openSessionFollow(map[string]any{
		"address": map[string]any{"kind": "session", "sessionId": "session-follow-live"},
	}, signal)
	if err != nil {
		t.Fatalf("open follow: %v", err)
	}
	snapshot := followFrame(t, frames)
	if snapshot["type"] != "snapshot" {
		t.Fatalf("want the snapshot frame, got %v", snapshot["type"])
	}

	// A live durable append rides the cordis session/event feed as an
	// event frame.
	live, err := sess.Append("turn/start", map[string]any{"turn": 1}, nil)
	if err != nil {
		t.Fatalf("live append: %v", err)
	}
	eventFrame := followFrameWithTimeout(t, frames, 2*time.Second)
	if eventFrame["type"] != "event" {
		t.Fatalf("want an event frame, got %v", eventFrame["type"])
	}
	event := eventFrame["event"].(session.Event)
	if event.Seq != live.Seq || event.Type != "turn/start" {
		t.Fatalf("relayed event: %+v", event)
	}

	// An assistant-stream frame routes by the attempt id's session prefix.
	agentBus := agentRegistry.Events()
	agentBus.Emit(agent.EventAssistantStream, nil, agent.AssistantStreamFrame{
		Type: "start", AttemptID: llm.NewLlmAttemptId("session-follow-live:1"),
		Revision: 1, Turn: 1, Step: 1,
	})
	streamFrame := followFrameWithTimeout(t, frames, 2*time.Second)
	if streamFrame["type"] != "assistant-stream" {
		t.Fatalf("want an assistant-stream frame, got %v", streamFrame["type"])
	}
	frame := streamFrame["frame"].(agent.AssistantStreamFrame)
	if frame.Type != "start" || string(frame.AttemptID) != "session-follow-live:1" {
		t.Fatalf("relayed frame: %+v", frame)
	}

	// A frame belonging to another session must not arrive.
	agentBus.Emit(agent.EventAssistantStream, nil, agent.AssistantStreamFrame{
		Type: "start", AttemptID: llm.NewLlmAttemptId("other-session:1"),
		Revision: 1, Turn: 1, Step: 1,
	})
	select {
	case frame := <-frames:
		t.Fatalf("a foreign session's frame leaked: %v", frame)
	case <-time.After(150 * time.Millisecond):
	}
}

func followFrameWithTimeout(t *testing.T, frames <-chan any, timeout time.Duration) map[string]any {
	t.Helper()
	select {
	case row := <-frames:
		mapped, ok := row.(map[string]any)
		if !ok {
			t.Fatalf("want a frame map, got %T", row)
		}
		return mapped
	case <-time.After(timeout):
		t.Fatalf("no frame arrived within %v", timeout)
		return nil
	}
}
