package sessionreference

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/llm"
)

func TestURIConvRoundTripAndCanonicality(t *testing.T) {
	for _, sessionID := range []string{"abc-123", "with space", "中文/或符号?#", `quote"back\slash`} {
		uri := EncodeSessionReferenceURI(sessionID)
		if !strings.HasPrefix(uri, SessionReferenceScheme) {
			t.Fatalf("uri %q missing scheme", uri)
		}
		got, err := DecodeSessionReferenceURI(uri)
		if err != nil || got != sessionID {
			t.Fatalf("round trip %q → %q %v", sessionID, got, err)
		}
	}
	// Non-canonical payloads fail: different JSON escaping of the same id.
	if _, err := DecodeSessionReferenceURI(SessionReferenceScheme + "ImEtaAY"); err == nil {
		t.Fatal("non-canonical payload accepted")
	}
	// Non-base64url payload characters fail.
	if _, err := DecodeSessionReferenceURI(SessionReferenceScheme + "a+b/c="); err == nil {
		t.Fatal("padded payload accepted")
	}
	// Wrong scheme fails.
	if _, err := DecodeSessionReferenceURI("https://example.com"); err == nil {
		t.Fatal("foreign scheme accepted")
	}
	// A decoded non-string JSON payload fails.
	payload := encodePayloadFor(t, 42)
	if _, err := DecodeSessionReferenceURI(SessionReferenceScheme + payload); err == nil {
		t.Fatal("non-string payload accepted")
	}
}

func encodePayloadFor(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64RawURL(encoded)
}

func base64RawURL(raw []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var builder strings.Builder
	// Minimal base64url (no padding) encoder for test fixtures.
	for i := 0; i < len(raw); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], raw[i:])
		b := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		for j := 0; j <= n; j++ {
			builder.WriteByte(alphabet[(b>>(18-6*j))&0x3f])
		}
	}
	return builder.String()
}

func TestMentionFormatAndParse(t *testing.T) {
	mention := FormatSessionReferenceMention(Input{SessionID: "s1", Label: "my [notes]"})
	if mention != "@[my [notes\\]]("+EncodeSessionReferenceURI("s1")+")" {
		t.Fatalf("mention = %q", mention)
	}
	// A missing label falls back to the opaque session id.
	bare := FormatSessionReferenceMention(Input{SessionID: "s1"})
	if !strings.Contains(bare, "@[s1](") {
		t.Fatalf("bare = %q", bare)
	}
	parsed, err := ParseSessionReferenceText("see " + mention + " and " + bare)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Text != "see @my [notes] and @s1" {
		t.Fatalf("text = %q", parsed.Text)
	}
	if len(parsed.References) != 2 || parsed.References[0].SessionID != "s1" || parsed.References[1].Label != "s1" {
		t.Fatalf("references = %+v", parsed.References)
	}
	// A malformed Markdown URI fails loud.
	broken := "@[x](dsh-session:!!!)"
	if _, err := ParseSessionReferenceText("look " + broken); err == nil {
		t.Fatal("malformed mention accepted")
	}
	// A bare canonical URI in plain text is recognized.
	parsed, err = ParseSessionReferenceText("plain " + EncodeSessionReferenceURI("s2"))
	if err != nil || len(parsed.References) != 1 || parsed.References[0].SessionID != "s2" {
		t.Fatalf("bare parse = %+v %v", parsed, err)
	}
}

func TestStringifyTagSafeJSON(t *testing.T) {
	value := map[string]any{"tag": "<script>", "gt": ">", "amp": "&"}
	serialized, err := StringifyTagSafeJSON(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(serialized, "<") {
		t.Fatalf("literal < survived: %s", serialized)
	}
	// Only `<` is rewritten; `>` and `&` stay literal.
	if !strings.Contains(serialized, `\u003cscript>`) || !strings.Contains(serialized, `">"`) || !strings.Contains(serialized, `"&"`) {
		t.Fatalf("escaping drifted: %s", serialized)
	}
	// The parse result is unchanged.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(serialized), &decoded); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if decoded["gt"] != ">" || decoded["amp"] != "&" {
		t.Fatalf("round trip = %v", decoded)
	}
}

func userEvent(text string) SurfaceEvent {
	return SurfaceEvent{Type: EventUserMessage, User: &SurfaceUserMessage{
		Source:  llm.MessageSource{Kind: "user"},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}}
}

func checkpointEvent(text string) SurfaceEvent {
	return SurfaceEvent{Type: EventUserMessage, User: &SurfaceUserMessage{
		Source:  llm.MessageSource{Kind: "plugin", Plugin: "compact"},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}}
}

func assistantEvent(text string) SurfaceEvent {
	return SurfaceEvent{Type: EventAssistantMessage, Assistant: &SurfaceAssistantMessage{
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}}
}

func TestProjectionFitsUnderByteCapByDroppingOldest(t *testing.T) {
	snapshot := SessionSnapshot{
		SessionID: "s1",
		Cwd:       "/work",
		Events: []SurfaceEvent{
			userEvent("first long user message with plenty of words"),
			assistantEvent("assistant reply that is also fairly verbose"),
			userEvent("last"),
		},
	}
	retained := RetainReferencedSession(snapshot, "label", 10000)
	if retained == nil {
		t.Fatal("generous cap dropped everything")
	}
	if retained.Stats.OriginalMessages != 3 || retained.Stats.RetainedMessages != 3 || retained.Stats.Truncated {
		t.Fatalf("stats = %+v", retained.Stats)
	}
	if retained.Data.Cwd == nil || *retained.Data.Cwd != "/work" {
		t.Fatalf("cwd = %+v", retained.Data.Cwd)
	}
	// A tight cap drops the oldest non-checkpoint, non-newest message.
	retained = RetainReferencedSession(snapshot, "label", 220)
	if retained == nil {
		t.Fatal("cap could not fit")
	}
	if retained.Stats.OriginalMessages != 3 || retained.Stats.RetainedMessages != 2 ||
		retained.Stats.OmittedMessages != 1 || !retained.Stats.Truncated {
		t.Fatalf("stats = %+v", retained.Stats)
	}
	if retained.Data.Conversation[0].Text != "assistant reply that is also fairly verbose" ||
		retained.Data.Conversation[1].Text != "last" {
		t.Fatalf("conversation = %+v", retained.Data.Conversation)
	}
	// The omitted message's bytes are counted.
	if retained.Stats.OmittedBytes == 0 {
		t.Fatal("omitted bytes uncounted")
	}
}

func TestProjectionPinsCheckpointsAndNewest(t *testing.T) {
	snapshot := SessionSnapshot{
		SessionID: "s1",
		Events: []SurfaceEvent{
			checkpointEvent("compact checkpoint summary stays"),
			userEvent("ordinary message that may go"),
			userEvent("newest pinned"),
		},
	}
	retained := RetainReferencedSession(snapshot, "label", 200)
	if retained == nil {
		t.Fatal("cap could not fit")
	}
	if !retained.Stats.Compacted {
		t.Fatalf("stats = %+v", retained.Stats)
	}
	texts := make([]string, 0, len(retained.Data.Conversation))
	for _, item := range retained.Data.Conversation {
		texts = append(texts, item.Text)
	}
	if len(texts) != 2 || texts[0] != "compact checkpoint summary stays" || texts[1] != "newest pinned" {
		t.Fatalf("conversation = %v", texts)
	}
}

func TestProjectionShortensLongestMessage(t *testing.T) {
	// A single message is the newest, so whole-message dropping cannot
	// help: the byte shortening path must engage.
	long := strings.Repeat("0123456789 ", 30)
	snapshot := SessionSnapshot{
		SessionID: "s1",
		Events: []SurfaceEvent{
			userEvent(long),
		},
	}
	retained := RetainReferencedSession(snapshot, "label", 200)
	if retained == nil {
		t.Fatal("cap could not fit")
	}
	if !retained.Stats.Truncated || retained.Stats.OmittedBytes == 0 {
		t.Fatalf("stats = %+v", retained.Stats)
	}
	shortened := retained.Data.Conversation[0].Text
	if len([]byte(shortened)) > 190 || !strings.Contains(shortened, "… omitted ") {
		t.Fatalf("shortened = %q", shortened)
	}
}

func TestProjectionNilWhenFixedDataCannotFit(t *testing.T) {
	snapshot := SessionSnapshot{
		SessionID: "s1",
		Events:    []SurfaceEvent{userEvent("only message")},
	}
	// The single newest message cannot be dropped or shortened away
	// entirely: a tiny cap returns nil.
	if retained := RetainReferencedSession(snapshot, "label", 10); retained != nil {
		t.Fatalf("expected nil, got %+v", retained)
	}
}

func TestProjectionSkipsToolsAndNonUserSources(t *testing.T) {
	snapshot := SessionSnapshot{
		SessionID: "s1",
		Events: []SurfaceEvent{
			{Type: EventToolResult},
			{Type: EventUserMessage, User: &SurfaceUserMessage{
				Source:  llm.MessageSource{Kind: "plugin", Plugin: "other"},
				Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "injected context"}},
			}},
			userEvent("real"),
		},
	}
	retained := RetainReferencedSession(snapshot, "label", 10000)
	if len(retained.Data.Conversation) != 1 || retained.Data.Conversation[0].Text != "real" {
		t.Fatalf("conversation = %+v", retained.Data.Conversation)
	}
	// Empty cwd and absent seq render as nulls.
	serialized, err := StringifyTagSafeJSON(retained.Data)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.Contains(serialized, `"cwd":null`) || !strings.Contains(serialized, `"capturedThroughSeq":null`) {
		t.Fatalf("nulls = %s", serialized)
	}
}
