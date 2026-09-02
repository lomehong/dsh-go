package compaction

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

func TestCompactionEventsRegistered(t *testing.T) {
	// The init registered all four event types; a detached session accepts
	// them as log-only appends between turns.
	sess, err := session.NewDetached("c1", nil, &session.SessionHeader{ID: "c1"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	turn := int64(3)
	start := StartPayload{CompactionID: "cp-1", Turn: &turn}
	if _, err := sess.Append(EventCompactionStart, start, nil); err != nil {
		t.Fatalf("append start: %v", err)
	}
	end := EndPayload{CompactionID: "cp-1", Turn: &turn, Error: "boom"}
	if _, err := sess.Append(EventCompactionEnd, end, nil); err != nil {
		t.Fatalf("append end: %v", err)
	}
	events := sess.Events()
	var decodedStart StartPayload
	if err := json.Unmarshal(events[0].Data, &decodedStart); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decodedStart.CompactionID != "cp-1" || decodedStart.Turn == nil || *decodedStart.Turn != 3 {
		t.Fatalf("start payload = %+v", decodedStart)
	}
	var decodedEnd EndPayload
	if err := json.Unmarshal(events[1].Data, &decodedEnd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decodedEnd.Error != "boom" {
		t.Fatalf("end payload = %+v", decodedEnd)
	}
}

func TestSummaryPayloadWireShape(t *testing.T) {
	maxTokens := int64(4096)
	total := int64(500)
	payload := SummaryPayload{
		CompactionID:       "cp-2",
		Summary:            []llm.ContentBlock{{Type: llm.BlockText, Text: "recap"}},
		ShadowedRange:      SeqRange{Start: 40, End: 10},
		ShadowedSeqs:       []int64{2, 3, 40},
		ShadowedTokenCount: 900,
		Provider:           "deepseek",
		Model:              "deepseek-chat",
		MaxTokens:          &maxTokens,
		Usage:              &llm.TokenUsage{InputTokens: 300, OutputTokens: 200, TotalTokens: &total},
		RawOutput:          []llm.ContentBlock{{Type: llm.BlockText, Text: "longer recap"}},
		LLMStreamCall:      true,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, needle := range []string{
		`"compactionId":"cp-2"`,
		`"shadowedRange":{"start":40,"end":10}`,
		`"shadowedSeqs":[2,3,40]`,
		`"shadowedTokenCount":900`,
		`"provider":"deepseek"`,
		`"model":"deepseek-chat"`,
		`"maxTokens":4096`,
		`"llmStreamCall":true`,
	} {
		if !strings.Contains(string(encoded), needle) {
			t.Fatalf("wire %s missing %s", encoded, needle)
		}
	}
	var roundTrip SummaryPayload
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundTrip.LLMStreamCall != true || len(roundTrip.RawOutput) != 1 || roundTrip.Usage.TotalTokens == nil || *roundTrip.Usage.TotalTokens != 500 {
		t.Fatalf("round trip = %+v", roundTrip)
	}
}

func TestCheckpointSource(t *testing.T) {
	source := CompactCheckpointSource("cp-3", "cmd-1")
	if source.Kind != "plugin" || source.Plugin != "compact" || source.CompactionID != "cp-3" || source.SourceCommandID != "cmd-1" {
		t.Fatalf("source = %+v", source)
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The marker fields are always present; the command id omits when absent.
	if !strings.Contains(string(encoded), `"kind":"plugin"`) || !strings.Contains(string(encoded), `"plugin":"compact"`) {
		t.Fatalf("wire = %s", encoded)
	}
	// The predicate runs on sources restored from the log: the persisted
	// shape round-trips into llm.MessageSource.
	var restored llm.MessageSource
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !IsCompactCheckpointSource(restored) {
		t.Fatal("the checkpoint source must recognize itself")
	}
	plain := CompactCheckpointSource("cp-4", "")
	if plain.SourceCommandID != "" {
		t.Fatal("absent command id must stay absent")
	}
	encodedPlain, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encodedPlain), "sourceCommandId") {
		t.Fatalf("wire = %s, want the absent command omitted", encodedPlain)
	}
	for _, foreign := range []llm.MessageSource{
		{Kind: "model"},
		{Kind: "plugin", Plugin: "other"},
		{},
	} {
		if IsCompactCheckpointSource(foreign) {
			t.Fatalf("foreign source %+v must not be a checkpoint", foreign)
		}
	}
}

// newSessionWithSurface builds one live agent session and drives messages
// through its surface by appending events directly (log-only between turns).
func newSessionWithSurface(t *testing.T, id string) (*session.Session, *agent.Agent) {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id)}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return sess, built
}

func assistantMessage(t *testing.T, callIDs ...string) session.Event {
	t.Helper()
	blocks := []llm.ContentBlock{{Type: llm.BlockText, Text: "doing work"}}
	for _, callID := range callIDs {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockToolCall, ID: llm.ToolCallID(callID)})
	}
	payload, err := json.Marshal(session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.Message{
			ID:      llm.NewMessageID(),
			Role:    llm.RoleAssistant,
			Content: blocks,
			Source:  llm.MessageSource{Kind: llm.SourceModel, Provider: "deepseek", Model: "deepseek-chat"},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return session.Event{Type: session.EventAssistantMsg, Data: payload}
}

func toolResult(t *testing.T, callID string) session.Event {
	t.Helper()
	payload, err := json.Marshal(session.ToolResultData{
		Turn: 1, Step: 2,
		Message: llm.Message{
			ID:      llm.NewMessageID(),
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: llm.ToolCallID(callID), Text: "ok"}},
			Source:  llm.MessageSource{Kind: llm.SourceTool, CallID: llm.ToolCallID(callID)},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return session.Event{Type: session.EventToolResult, Data: payload}
}

func TestToolPairingBalanceAcrossCuts(t *testing.T) {
	sess, _ := newSessionWithSurface(t, "pairing")
	events := []session.Event{
		assistantMessage(t),             // seq 0: text only
		assistantMessage(t, "c1", "c2"), // seq 1: two calls open
		toolResult(t, "c1"),             // seq 2: one still open
		toolResult(t, "c2"),             // seq 3: balanced again
		assistantMessage(t, "c3"),       // seq 4: open at tail
	}
	for _, event := range events {
		if _, err := sess.Append(event.Type, mustDecode(t, event), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
			t.Fatalf("append %s: %v", event.Type, err)
		}
	}
	balance := NewToolPairingBalance()
	cases := []struct {
		seq    int64
		before bool
		after  bool
	}{
		{0, true, true},
		{1, true, false}, // cut before seq 1 is balanced; after opens two calls
		{2, false, false},
		{3, false, true},
		{4, true, false}, // tail cut is unbalanced (open call)
	}
	for _, testCase := range cases {
		before, err := balance.ToolPairingBalancedBefore(sess, testCase.seq)
		if err != nil {
			t.Fatalf("before %d: %v", testCase.seq, err)
		}
		if before != testCase.before {
			t.Fatalf("seq %d before = %v, want %v", testCase.seq, before, testCase.before)
		}
		after, err := balance.ToolPairingBalancedAfter(sess, testCase.seq)
		if err != nil {
			t.Fatalf("after %d: %v", testCase.seq, err)
		}
		if after != testCase.after {
			t.Fatalf("seq %d after = %v, want %v", testCase.seq, after, testCase.after)
		}
	}
}

func TestToolPairingRejectsUnpairedResultAndUnknownSeq(t *testing.T) {
	// Unknown seq on a healthy surface.
	healthy, _ := newSessionWithSurface(t, "unknown-seq")
	if _, err := healthy.Append(session.EventAssistantMsg, mustDecode(t, assistantMessage(t)), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	balance := NewToolPairingBalance()
	if _, err := balance.ToolPairingBalancedBefore(healthy, 99); err == nil ||
		!strings.Contains(err.Error(), "surface seq 99 not found") {
		t.Fatalf("err = %v, want the unknown-seq rejection", err)
	}

	// An unpaired result poisons the fold, and the rejected fold leaves no
	// half-advanced cache: the error repeats identically.
	sess, _ := newSessionWithSurface(t, "corrupt")
	if _, err := sess.Append(session.EventToolResult, mustDecode(t, toolResult(t, "orphan")), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := balance.ToolPairingBalancedBefore(sess, 0); err == nil ||
		!strings.Contains(err.Error(), "tool/result at surface seq 0 has no matching tool-call (corrupt surface)") {
		t.Fatalf("err = %v, want the unpaired-result rejection", err)
	}
	if _, err := balance.ToolPairingBalancedBefore(sess, 0); err == nil {
		t.Fatal("the corrupt state was cached as healthy")
	}
}

func mustDecode(t *testing.T, event session.Event) any {
	t.Helper()
	var payload any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatalf("decode %s payload: %v", event.Type, err)
	}
	return payload
}
