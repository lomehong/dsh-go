package sessionformatv12

import (
	"encoding/json"
	"testing"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionformat"
	"dshgo/sessionformatv01"
)

// TestGoSessionLogMigratesToV2 is the alignment cornerstone: a log written
// by the Go host (v0-stamped, released-v1 logical shape) must pass the
// frozen released-v0 codec, the identity v0->v1 edge, the cardinality-
// changing v1->v2 edge, and released-v2 target validation.
func TestGoSessionLogMigratesToV2(t *testing.T) {
	legacyVersion := int64(0)
	header := session.SessionHeader{
		// The live session is current-generation; the PHYSICAL header line
		// below simulates the historical Go writer (stamped 0), which the
		// migration chain takes to the current v2.
		Version:   session.SESSION_FORMAT_VERSION,
		ID:        "go-session-1",
		CreatedAt: 1725500000000,
		CWD:       t.TempDir(),
	}
	_ = legacyVersion
	seed := []session.Event{}
	live, err := session.NewDetached(header.ID, seed, &header, 0)
	if err != nil {
		t.Fatalf("session construct: %v", err)
	}

	appendEvent := func(eventType string, data any, intent *session.SurfaceIntent) session.Event {
		t.Helper()
		event, err := live.Append(eventType, data, intent)
		if err != nil {
			t.Fatalf("append %s: %v", eventType, err)
		}
		return event
	}

	appendEvent(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil)
	appendEvent(session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}, nil)
	appendEvent(session.EventUserMessage, llm.Message{
		ID: "u1", Role: "user",
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}},
		Source:  llm.MessageSource{Kind: "user"},
	}, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}})

	chunkSeqs := []int64{}
	for _, chunk := range []llm.StreamChunk{
		{Type: llm.ChunkTextDelta, Index: 0, Text: "he"},
		{Type: llm.ChunkTextDelta, Index: 0, Text: "llo"},
		{Type: llm.ChunkToolCallDelta, Index: 1, ID: "c1", Name: "read", ArgumentsDelta: `{"path":"x"}`},
	} {
		event := appendEvent(session.EventAssistantChunk,
			struct {
				Turn  int64           `json:"turn"`
				Step  int64           `json:"step"`
				Chunk llm.StreamChunk `json:"chunk"`
			}{1, 1, chunk}, nil)
		chunkSeqs = append(chunkSeqs, event.Seq)
	}
	message := llm.NewAssistantMessage([]llm.ContentBlock{
		{Type: llm.BlockText, Text: "hello"},
		{Type: llm.BlockToolCall, ID: "c1", Name: "read", Arguments: `{"path":"x"}`},
	}, "p", "m", nil)
	appendEvent(session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1, Message: message,
	}, &session.SurfaceIntent{
		SurfaceOp:         session.SurfaceOp{Kind: session.SurfaceAppend},
		SourceEventSeqs:   chunkSeqs,
		SourceSeqsPresent: true,
	})
	appendEvent(session.EventToolCall, session.ToolCallData{
		Turn: 1, Step: 1, CallID: "c1", Name: "read", Arguments: `{"path":"x"}`,
	}, nil)
	appendEvent(session.EventToolResult, session.ToolResultData{
		Turn: 1, Step: 1,
		Message: llm.Message{
			ID: "t1", Role: "user",
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1", Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "ok"}}}},
			Source:  llm.MessageSource{Kind: "tool", CallID: "c1"},
		},
	}, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}})
	appendEvent(session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}, nil)
	appendEvent(session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}, nil)

	// Serialize exactly as the jsonl backend would: header line + event rows.
	headerLine, err := json.Marshal(struct {
		Type            string `json:"type"`
		Version         int64  `json:"version"`
		ID              string `json:"id"`
		CreatedAt       int64  `json:"createdAt"`
		CWD             string `json:"cwd,omitempty"`
		DelegationDepth int64  `json:"delegationDepth"`
	}{"session", 0, header.ID, header.CreatedAt, header.CWD, 0})
	if err != nil {
		t.Fatal(err)
	}
	rows := []json.RawMessage{}
	for _, event := range live.Events() {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, encoded)
	}

	// 1. Decode through the frozen released-v0 codec.
	catalog, err := buildTestCatalog()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := catalog.DecodeArtifact(headerLine, rows)
	if err != nil {
		t.Fatalf("released v0 decode of a Go-written log: %v", err)
	}
	// 2. Migrate through the whole chain to v2.
	target, err := catalog.Migrate(decoded)
	if err != nil {
		t.Fatalf("migrate to v2: %v", err)
	}
	// 3. The settlement carries the compact stream; the chunks are consumed.
	var messageCount, chunkCount, attemptCount int
	for _, event := range target.Events {
		switch event.Type {
		case "assistant/message":
			messageCount++
			if !contains(event.Data, "text-chunks") {
				t.Fatal("migrated message must embed a compact text run")
			}
		case "assistant/chunk":
			chunkCount++
		case "assistant/attempt":
			attemptCount++
		}
	}
	if messageCount != 1 || chunkCount != 0 || attemptCount != 0 {
		t.Fatalf("v2 shape: messages=%d chunks=%d attempts=%d", messageCount, chunkCount, attemptCount)
	}
	if version, _ := sessionformat.HeaderVersion(target.Header); version != 2 {
		t.Fatalf("target version: %v", version)
	}
	_ = headerLine
}

func contains(data json.RawMessage, needle string) bool {
	return len(data) > 0 && stringsContains(string(data), needle)
}

func stringsContains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// buildTestCatalog compiles the complete chain with the released codecs and
// a permissive current restoration (full installed-vocabulary restoration is
// wired at the session layer).
func buildTestCatalog() (sessionformat.Catalog, error) {
	return sessionformat.CompileCatalog(sessionformat.CatalogOptions{
		CurrentVersion: 2,
		Codecs: []sessionformat.Codec{
			sessionformatv01.ReleasedV0Codec(),
			sessionformatv01.ReleasedV1Codec(),
			ReleasedV2Codec(),
		},
		Migrations: []sessionformat.Migration{
			sessionformatv01.NewMigration(),
			NewMigration(),
		},
		RestoreCurrent: func(artifact sessionformat.Artifact) (sessionformat.Artifact, error) {
			return artifact, nil
		},
		RestoreCurrentHeader: func(header sessionformat.Header) (sessionformat.Header, error) {
			return header, nil
		},
		EncodeCurrentArtifact: func(artifact sessionformat.Artifact) (sessionformat.EncodedArtifact, error) {
			return ReleasedV2Codec().EncodeArtifact(artifact)
		},
	})
}
