package session

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"dshgo/llm"
)

func userMessageEvent(text string) (Event, error) {
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
	encoded, err := json.Marshal(message)
	if err != nil {
		return Event{}, err
	}
	return Event{Type: EventUserMessage, Data: encoded, SurfaceOp: &SurfaceOp{Kind: SurfaceAppend}}, nil
}

func mustAppend(t *testing.T, s *Session, eventType string, data any, intent *SurfaceIntent) Event {
	t.Helper()
	event, err := s.Append(eventType, data, intent)
	if err != nil {
		t.Fatalf("append %s failed: %v", eventType, err)
	}
	return event
}

func TestSeedReplayAppendsEndSeedMarkerOnce(t *testing.T) {
	seed := []Event{}
	for i := 0; i < 3; i++ {
		event, err := userMessageEvent("hello")
		if err != nil {
			t.Fatalf("fixture failed: %v", err)
		}
		event.Seq = int64(i)
		seed = append(seed, event)
	}
	session, err := NewDetached("replay-1", seed, nil)
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	events := session.Events()
	if len(events) != 4 {
		t.Fatalf("seed must replay verbatim plus one end-seed marker, got %d", len(events))
	}
	if events[3].Type != EventEndSeed || events[3].Seq != 3 {
		t.Fatalf("end-seed marker wrong: %#v", events[3])
	}
	if session.FirstLiveSeq() != 3 {
		t.Fatalf("first live seq must be the seed length, got %d", session.FirstLiveSeq())
	}

	// Reopening an untouched session does not grow its log per open.
	reopened, err := NewDetached("replay-1", session.Events(), nil)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if len(reopened.Events()) != len(session.Events()) {
		t.Fatalf("reopen must not re-mark, got %d vs %d", len(reopened.Events()), len(session.Events()))
	}
}

func TestAppendEnforcesSurfaceIntentContract(t *testing.T) {
	session, err := NewDetached("s", nil, nil)
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	// Non-surface events refuse an intent.
	turnStart := map[string]any{"turn": 1}
	if _, err := session.Append(EventTurnStart, turnStart, appendIntent()); err == nil {
		t.Fatal("a non-surface event must not carry surface metadata")
	}
	mustAppend(t, session, EventTurnStart, turnStart, nil)

	// Surface events require an intent.
	if _, err := session.Append(EventUserMessage, map[string]any{}, nil); err == nil {
		t.Fatal("a message-producing event requires a surfaceOp marker")
	}
	// Malformed message payloads fail loud at the source.
	if _, err := session.Append(EventUserMessage, map[string]any{"id": ""}, appendIntent()); err == nil {
		t.Fatal("an unidentified user message must be refused")
	}
}

func TestAppendSeqContiguityAndEventFeed(t *testing.T) {
	store := NewStore(nil)
	var fed []Event
	store.OnEvent(func(_ *Session, event Event) { fed = append(fed, event) })
	depth := int64(1)
	session, err := store.Create("live-1", CreateOptions{HeaderMetadata: SessionHeader{
		CreatedAt: 1, DelegationDepth: &depth, Origin: "subagent",
	}})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if session.Header().Version != SESSION_FORMAT_VERSION {
		t.Fatal("store must stamp the format version")
	}

	first := mustAppend(t, session, EventTurnStart, map[string]any{"turn": 1}, nil)
	if first.Seq != 0 || first.Time <= 0 {
		t.Fatalf("first event wrong: %#v", first)
	}
	second := mustAppend(t, session, EventTurnEnd, map[string]any{"turn": 1, "reason": TurnEndReason{Kind: TurnEndCompleted}}, nil)
	if second.Seq != 1 {
		t.Fatalf("seq must stay contiguous, got %d", second.Seq)
	}
	if len(fed) != 2 {
		t.Fatalf("the append feed must observe committed events, got %d", len(fed))
	}
	if store.Get("live-1") != session {
		t.Fatal("the live instance must be reachable by id")
	}
	if session.RequestHeader() != nil {
		t.Fatal("no header snapshot exists before request/header")
	}
	if got := session.DeriveMessages(); len(got) != 0 {
		t.Fatalf("boundary events derive no messages, got %d", len(got))
	}
}

func TestAppendFeedDeliversEveryConcurrentCommitExactlyOnce(t *testing.T) {
	store := NewStore(nil)
	var mu sync.Mutex
	fed := map[int64]int{}
	store.OnEvent(func(_ *Session, event Event) {
		mu.Lock()
		fed[event.Seq]++
		mu.Unlock()
	})
	depth := int64(1)
	session, err := store.Create("feed-race", CreateOptions{HeaderMetadata: SessionHeader{
		CreatedAt: 1, DelegationDepth: &depth, Origin: "subagent",
	}})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	const goroutines, perGoroutine = 8, 25
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, err := session.Append(EventTurnStart, map[string]any{"turn": g, "i": i}, nil); err != nil {
					t.Errorf("append failed: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	want := goroutines * perGoroutine
	if len(fed) != want {
		t.Fatalf("feed lost or duplicated deliveries: got %d distinct seqs, want %d", len(fed), want)
	}
	for seq, count := range fed {
		if count != 1 {
			t.Fatalf("seq %d delivered %d times, want exactly 1", seq, count)
		}
	}
	if got := len(session.Events()); got != want {
		t.Fatalf("log length = %d, want %d", got, want)
	}
}

func TestForkValidatesBoundariesAndOpenTurns(t *testing.T) {
	store := NewStore(nil)
	parent, err := store.Create("parent", CreateOptions{})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	mustAppend(t, parent, EventTurnStart, map[string]any{"turn": 1}, nil)

	if _, err := store.Fork(parent, "child", 0); err == nil {
		t.Fatal("a prefix ending inside an open turn must be refused")
	} else {
		var forkErr *ForkError
		if !errors.As(err, &forkErr) || forkErr.Code != ForkOpenTurn {
			t.Fatalf("open turn must carry its code, got %v", err)
		}
	}
	mustAppend(t, parent, EventTurnEnd, map[string]any{"turn": 1, "reason": TurnEndReason{Kind: TurnEndCompleted}}, nil)
	mustAppend(t, parent, EventUserMessage, mustMarshal(t, mustUserMessage(t, "q")), appendIntent())

	if _, err := store.Fork(parent, "child", 99); err == nil {
		t.Fatal("a non-contiguous boundary must be refused")
	}
	seed, err := store.Fork(parent, "child", 1)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}
	if len(seed) != 2 || seed[0].Type != EventTurnStart {
		t.Fatalf("fork seed wrong: %#v", seed)
	}
	child, err := store.Create("child", CreateOptions{Seed: seed, HeaderMetadata: SessionHeader{
		CreatedAt: 2, ParentSession: "parent", SeedLength: ptrInt64(int64(len(seed))),
	}})
	if err != nil {
		t.Fatalf("child create failed: %v", err)
	}
	if child.Header().ParentSession != "parent" || *child.Header().SeedLength != 2 {
		t.Fatalf("child header lineage wrong: %#v", child.Header())
	}
}

func TestDeriveMessagesAndEmptyAssistantHost(t *testing.T) {
	session, err := NewDetached("derive", nil, nil)
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	mustAppend(t, session, EventUserMessage, mustMarshal(t, mustUserMessage(t, "question")), appendIntent())
	// An empty-content assistant/message exists only to host usage and must
	// not enter the transcript. Its content is a present empty array — the
	// known empty provider stream — never null.
	usage := int64(9)
	empty := AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{}, "deepseek", "chat", nil),
		Usage:   &llm.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: &usage},
	}
	mustAppend(t, session, EventAssistantMsg, mustMarshal(t, empty), appendIntent())
	full := AssistantMessageData{
		Turn: 1, Step: 2,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "answer"}}, "deepseek", "chat", nil),
	}
	mustAppend(t, session, EventAssistantMsg, mustMarshal(t, full), appendIntent())
	result := session.DeriveMessages()
	if len(result) != 2 {
		t.Fatalf("empty assistant messages must not derive history, got %d: %#v", len(result), result)
	}
	if result[0].Content[0].Text != "question" || result[1].Content[0].Text != "answer" {
		t.Fatalf("derived history wrong: %#v", result)
	}
	// The snapshot is detached from later appends.
	before := len(result)
	mustAppend(t, session, EventUserMessage, mustMarshal(t, mustUserMessage(t, "more")), appendIntent())
	if len(result) != before {
		t.Fatal("an earlier snapshot must not grow")
	}
	if got := len(session.DeriveMessages()); got != 3 {
		t.Fatalf("derive must observe new nodes, got %d", got)
	}
}

func TestRequestHeaderFoldAndCanonicalEquality(t *testing.T) {
	temp := 0.7
	header := EpochHeader{
		Config: llm.LlmCallConfig{Provider: "deepseek", Model: "chat", Temperature: &temp},
		System: "",
		Tools:  []llm.ToolSchema{},
	}
	canonical := CanonicalHeader(header)
	if canonical.System != "" || canonical.Tools != nil {
		t.Fatal("empty system and tools must fold to absent")
	}
	if !HeaderEquals(CanonicalHeader(header), canonical) {
		t.Fatal("canonicalization must be idempotent under equality")
	}

	session, err := NewDetached("header", nil, nil)
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	snapshot := RequestHeaderData{Header: header, Reason: HeaderReasonInitial}
	mustAppend(t, session, EventRequestHeader, mustMarshal(t, snapshot), nil)
	folded := session.RequestHeader()
	if folded == nil || folded.Config.Provider != "deepseek" || folded.System != "" {
		t.Fatalf("header fold wrong: %#v", folded)
	}
}

func TestToolResultShapeValidation(t *testing.T) {
	wrapped := ToolResultData{Turn: 1, Step: 1, Message: llm.NewToolResultMessage("call-1", []llm.ContentBlock{{Type: llm.BlockText, Text: "out"}}, false)}
	event := Event{Type: EventToolResult, Data: mustMarshal(t, wrapped), SurfaceOp: &SurfaceOp{Kind: SurfaceAppend}}
	if err := assertMessageEventShape(event); err != nil {
		t.Fatalf("valid tool result must pass: %v", err)
	}
	correlated := ToolResultData{Turn: 1, Step: 1, Message: llm.NewToolResultMessage("call-1", nil, false)}
	decoded := mustDecode[ToolResultData](t, mustMarshal(t, correlated))
	decoded.Message.Content[0].ToolCallID = "other"
	event.Data = mustMarshal(t, decoded)
	if err := assertMessageEventShape(event); err == nil || !strings.Contains(err.Error(), "mismatched") {
		t.Fatalf("mismatched call ids must be refused, got %v", err)
	}
}

func TestLegacyRequestHeaderRefusals(t *testing.T) {
	if err := assertSupportedRequestHeader("request/header-delta", json.RawMessage(`{}`)); err == nil {
		t.Fatal("legacy delta codec must be refused")
	}
	if err := assertSupportedRequestHeader(EventRequestHeader, json.RawMessage(`{"reason":"fallback"}`)); err == nil {
		t.Fatal("legacy fallback reason must be refused")
	}
	if err := assertSupportedRequestHeader(EventRequestHeader, json.RawMessage(`{"reason":"initial"}`)); err != nil {
		t.Fatalf("current vocabulary must pass: %v", err)
	}
}

func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture failed: %v", err)
	}
	return encoded
}

func mustUserMessage(t *testing.T, text string) llm.Message {
	t.Helper()
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
}

func mustDecode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode fixture failed: %v", err)
	}
	return decoded
}

func ptrInt64(v int64) *int64 { return &v }

// appendIntent is the plain append surface intent most tests use.
func appendIntent() *SurfaceIntent {
	return &SurfaceIntent{SurfaceOp: SurfaceOp{Kind: SurfaceAppend}}
}
