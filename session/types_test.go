package session

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/llm"
)

func TestSessionFormatVersionPinnedAtZero(t *testing.T) {
	if SESSION_FORMAT_VERSION != 0 {
		t.Fatal("the unreleased harness pins the format at 0 with no compatibility promise")
	}
}

func TestSurfaceOpWireShapes(t *testing.T) {
	appendData, err := json.Marshal(SurfaceOp{Kind: SurfaceAppend})
	if err != nil || string(appendData) != `"append"` {
		t.Fatalf("append must render as the bare string, got %s %v", appendData, err)
	}
	replaceData, err := json.Marshal(SurfaceOp{Kind: SurfaceReplace, Start: 3, End: 5})
	if err != nil || string(replaceData) != `{"op":"replace","start":3,"end":5}` {
		t.Fatalf("replace must render as the official object, got %s %v", replaceData, err)
	}
	var appendBack SurfaceOp
	if err := json.Unmarshal(appendData, &appendBack); err != nil || appendBack.Kind != SurfaceAppend {
		t.Fatalf("append round trip failed: %#v %v", appendBack, err)
	}
	var replaceBack SurfaceOp
	if err := json.Unmarshal(replaceData, &replaceBack); err != nil ||
		replaceBack.Kind != SurfaceReplace || replaceBack.Start != 3 || replaceBack.End != 5 {
		t.Fatalf("replace round trip failed: %#v %v", replaceBack, err)
	}
	if _, err := json.Marshal(SurfaceOp{Kind: "bogus"}); err == nil {
		t.Fatal("unknown surface op must fail loudly")
	}
}

func TestKnownEventGuardFailsClosed(t *testing.T) {
	for _, known := range []string{
		"turn/start", "turn/end", "step/start", "step/end", "user/message",
		"assistant/chunk", "assistant/message", "tool/call", "tool/result",
		"request/header", "request/context", "session/end-seed",
	} {
		if !KnownEventType(known) {
			t.Fatalf("%q must be known", known)
		}
	}
	if KnownEventType("brand/new-thing") {
		t.Fatal("an unregistered type must be unknown")
	}
	if err := RegisterEventType("brand/new-thing"); err != nil {
		t.Fatalf("plugin merge path failed: %v", err)
	}
	if !KnownEventType("brand/new-thing") {
		t.Fatal("a registered type becomes known")
	}
	if err := RegisterEventType("brand/new-thing"); err == nil {
		t.Fatal("double registration must fail loudly")
	}
	if !IsSurfaceEventType("user/message") || IsSurfaceEventType("turn/start") {
		t.Fatal("surface eligibility must be the three message producers")
	}
}

func TestIsJsonValueCanonicalShapes(t *testing.T) {
	good := []any{
		nil, true, "text", float64(1.5), int64(3), 7,
		[]any{"nested", nil}, map[string]any{"deep": []any{int64(1)}},
	}
	for i, value := range good {
		if !IsJsonValue(value) {
			t.Fatalf("case %d must be a JSON value", i)
		}
	}
	bad := []any{make(chan int), func() {}, struct{ A int }{1}, &good}
	for i, value := range bad {
		if IsJsonValue(value) {
			t.Fatalf("case %d must be refused", i)
		}
	}
	if err := ValidateEventJSON("tool/result", struct{ X int }{1}); err == nil {
		t.Fatal("validation must refuse non-canonical payloads at the source")
	}
}

func TestEventEnvelopeRoundTripAndTypedDecoders(t *testing.T) {
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}, llm.MessageSource{})
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("payload marshal failed: %v", err)
	}
	event := Event{
		Type: EventUserMessage, Seq: 4, Time: 1700000000000, Data: data,
		SurfaceOp: &SurfaceOp{Kind: SurfaceAppend},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("envelope marshal failed: %v", err)
	}
	var back Event
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("envelope unmarshal failed: %v", err)
	}
	if back.Seq != 4 || back.SurfaceOp == nil || back.SurfaceOp.Kind != SurfaceAppend {
		t.Fatalf("envelope fields wrong: %s", encoded)
	}
	decoded, err := DecodeUserMessage(back)
	if err != nil || decoded.Content[0].Text != "hi" {
		t.Fatalf("typed decode failed: %#v %v", decoded, err)
	}

	assistant := AssistantMessageData{Turn: 1, Step: 1, Message: llm.NewAssistantMessage(nil, "p", "m", nil), Interrupted: true}
	payload, _ := json.Marshal(assistant)
	decodedAssistant, err := DecodeAssistantMessage(Event{Type: EventAssistantMsg, Seq: 5, Data: payload})
	if err != nil || !decodedAssistant.Interrupted || decodedAssistant.Turn != 1 {
		t.Fatalf("assistant decode failed: %#v %v", decodedAssistant, err)
	}

	if _, err := DecodeUserMessage(Event{Type: EventToolResult, Data: []byte("{}")}); err == nil {
		t.Fatal("decoding against the wrong type must fail loudly")
	}
}

func TestDeepCopyIsolatesLoggedState(t *testing.T) {
	header := SessionHeader{Version: SESSION_FORMAT_VERSION, ID: "s1", CreatedAt: 1}
	headerCopy := DeepCopyHeader(header)
	headerCopy.ID = "s2"
	if header.ID != "s1" {
		t.Fatal("header copy must be detached")
	}
	original := map[string]any{"nested": []any{int64(1)}}
	cloned := DeepCopyValue(original).(map[string]any)
	cloned["nested"].([]any)[0] = int64(2)
	if original["nested"].([]any)[0] != int64(1) {
		t.Fatal("deep copy must not alias logged state")
	}

	event := Event{Type: EventTurnStart, Data: json.RawMessage(`{"turn":1}`), SourceEventSeqs: []int64{1, 2}}
	eventCopy := DeepCopyEvent(event)
	eventCopy.Data[0] = 'X'
	eventCopy.SourceEventSeqs[0] = 99
	if string(event.Data) != `{"turn":1}` || event.SourceEventSeqs[0] != 1 {
		t.Fatal("event copy must detach payload and citations")
	}
}

func TestIgnorableEnvelopeWire(t *testing.T) {
	base := Event{Type: "brand/notice", Seq: 0, Time: 1, Data: json.RawMessage(`{"x":1}`)}
	wire, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(wire), "ignorable") {
		t.Fatalf("absent ignorable must not render: %s", wire)
	}
	var back Event
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Ignorable {
		t.Fatal("absent ignorable must decode as required")
	}

	marked := base
	marked.Ignorable = true
	wire, err = json.Marshal(marked)
	if err != nil {
		t.Fatalf("marshal marked: %v", err)
	}
	if !strings.Contains(string(wire), `"ignorable":true`) {
		t.Fatalf("marked ignorable must render as true: %s", wire)
	}
	if err := json.Unmarshal(wire, &back); err != nil || !back.Ignorable {
		t.Fatalf("marked round trip = %#v %v", back, err)
	}

	if err := json.Unmarshal([]byte(`{"type":"brand/notice","seq":0,"time":1,"data":{},"ignorable":false}`), &back); err == nil {
		t.Fatal("ignorable:false must be rejected — the only valid value is true")
	}
}
