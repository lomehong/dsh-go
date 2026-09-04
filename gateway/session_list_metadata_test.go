package gateway

import (
	"encoding/json"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

// sessionListMetadataEvent builds a pure fold fixture (no live session).
func sessionListMetadataEvent(seq int64, eventType string, data any) session.Event {
	encoded, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return session.Event{
		Type: eventType,
		Seq:  seq,
		Time: seq,
		Data: encoded,
	}
}

// userPromptEvent is a user-authored (source.kind user) user/message.
func userPromptEvent(seq int64, text string) session.Event {
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourceUser},
	)
	return sessionListMetadataEvent(seq, session.EventUserMessage, message)
}

// nonUserPromptEvent is a user/message produced outside the user (plugin,
// webhook, goal): it must not advance lastPromptAt.
func nonUserPromptEvent(seq int64, text string) session.Event {
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourcePlugin},
	)
	return sessionListMetadataEvent(seq, session.EventUserMessage, message)
}

func TestSessionListMetadataInitIsBlank(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})
	if !state.Blank {
		t.Fatal("a brand-new session must start blank")
	}
	if state.LastPromptAt != nil {
		t.Fatalf("a brand-new session has no prompt: %+v", state.LastPromptAt)
	}
}

func TestSessionListMetadataTurnStartClearsBlank(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})

	state, changed := unit.Apply(state, sessionListMetadataEvent(0, session.EventTurnStart, session.TurnStartData{Turn: 1}))
	if !changed {
		t.Fatal("turn/start must change the metadata")
	}
	if state.Blank {
		t.Fatal("the first turn/start clears blank")
	}
	if state.LastPromptAt != nil {
		t.Fatalf("turn/start carries no prompt: %+v", state.LastPromptAt)
	}

	// A second turn/start over an already-blank-false session is unchanged.
	before := state
	after, changed := unit.Apply(before, sessionListMetadataEvent(3, session.EventTurnStart, session.TurnStartData{Turn: 2}))
	if changed {
		t.Fatal("a later turn/start over an already-cleared blank must not change state")
	}
	if after != before {
		t.Fatal("unchanged fold must return the same state value")
	}
}

func TestSessionListMetadataUserPromptAdvancesLastPromptAt(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})

	// A user-authored prompt stamps the time. Blank is untouched by a
	// prompt: the official fold keeps blank until a turn/start arrives
	// (blank = state.blank && type !== 'turn/start'), so a prompt on a
	// blank session leaves blank true.
	state, changed := unit.Apply(state, userPromptEvent(4, "hello"))
	if !changed {
		t.Fatal("a user prompt must change the metadata")
	}
	if !state.Blank {
		t.Fatal("a user prompt without a turn/start must leave the session blank")
	}
	if state.LastPromptAt == nil || *state.LastPromptAt != 4 {
		t.Fatalf("lastPromptAt = %+v, want 4", state.LastPromptAt)
	}

	// A later user prompt advances the stamp.
	state, changed = unit.Apply(state, userPromptEvent(9, "again"))
	if !changed {
		t.Fatal("a later user prompt must advance the stamp")
	}
	if state.LastPromptAt == nil || *state.LastPromptAt != 9 {
		t.Fatalf("lastPromptAt = %+v, want 9", state.LastPromptAt)
	}
}

func TestSessionListMetadataUserPromptKeepsBlankUntilTurnStart(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})

	// Prompts alone never clear blank.
	state, _ = unit.Apply(state, userPromptEvent(1, "hi"))
	if !state.Blank {
		t.Fatal("prompts must not clear blank")
	}

	// The first turn/start clears blank and the change publishes.
	state, changed := unit.Apply(state, sessionListMetadataEvent(2, session.EventTurnStart, session.TurnStartData{Turn: 1}))
	if !changed {
		t.Fatal("turn/start must clear blank")
	}
	if state.Blank {
		t.Fatal("the first turn/start clears blank")
	}
	if state.LastPromptAt == nil || *state.LastPromptAt != 1 {
		t.Fatalf("lastPromptAt = %+v, want the earlier prompt's time", state.LastPromptAt)
	}
}
func TestSessionListMetadataNonUserPromptDoesNotAdvance(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})
	state, _ = unit.Apply(state, userPromptEvent(4, "hello"))

	before := state
	after, changed := unit.Apply(before, nonUserPromptEvent(6, "plugin injected"))
	if changed {
		t.Fatal("a non-user prompt must not change the metadata")
	}
	if after != before {
		t.Fatal("unchanged fold must return the same state value")
	}
	if after.LastPromptAt == nil || *after.LastPromptAt != 4 {
		t.Fatalf("lastPromptAt must stay 4: %+v", after.LastPromptAt)
	}
}

func TestSessionListMetadataUninterestingEventsDoNotChange(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})
	state, _ = unit.Apply(state, userPromptEvent(4, "hello"))

	for _, event := range []session.Event{
		sessionListMetadataEvent(5, session.EventAssistantChunk, struct {
			Turn int64 `json:"turn"`
			Step int64 `json:"step"`
		}{1, 1}),
		sessionListMetadataEvent(6, session.EventToolCall, session.ToolCallData{
			Turn: 1, Step: 1, Name: "bash", Arguments: `{}`,
		}),
		sessionListMetadataEvent(7, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
	} {
		before := state
		after, changed := unit.Apply(before, event)
		if changed {
			t.Fatalf("event %s must not change the metadata", event.Type)
		}
		if after != before {
			t.Fatalf("event %s must return the same state", event.Type)
		}
	}
}

func TestSessionListMetadataViewIsIdentity(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})
	state, _ = unit.Apply(state, userPromptEvent(4, "hello"))

	view, ok := unit.View(state).(SessionListMetadata)
	if !ok {
		t.Fatalf("view = %T, want SessionListMetadata", unit.View(state))
	}
	if view.Blank != state.Blank || view.LastPromptAt == nil || *view.LastPromptAt != 4 {
		t.Fatalf("view = %+v, want %+v", view, state)
	}
}

func TestSessionListMetadataDecodeState(t *testing.T) {
	unit := SessionListMetadataUnit()

	// A valid row reifies.
	decoded, err := unit.DecodeState(json.RawMessage(`{"blank":false,"lastPromptAt":42}`))
	if err != nil {
		t.Fatalf("decode valid: %v", err)
	}
	if decoded.Blank {
		t.Fatal("decoded blank must be false")
	}
	if decoded.LastPromptAt == nil || *decoded.LastPromptAt != 42 {
		t.Fatalf("decoded lastPromptAt = %+v, want 42", decoded.LastPromptAt)
	}

	// lastPromptAt null is the no-prompt row.
	decoded, err = unit.DecodeState(json.RawMessage(`{"blank":true,"lastPromptAt":null}`))
	if err != nil {
		t.Fatalf("decode null prompt: %v", err)
	}
	if !decoded.Blank || decoded.LastPromptAt != nil {
		t.Fatalf("decoded = %+v, want blank no prompt", decoded)
	}

	// Malformed rows fail loud.
	for _, raw := range []string{
		`null`,
		`{"blank":"yes","lastPromptAt":null}`,
		`{"blank":false,"lastPromptAt":-1}`,
		`{"blank":false}`,
		`[]`,
	} {
		if _, err := unit.DecodeState(json.RawMessage(raw)); err == nil {
			t.Fatalf("decode %s must fail", raw)
		}
	}
}

func TestSessionListMetadataFoldMatchesOfficialSequence(t *testing.T) {
	unit := SessionListMetadataUnit()
	state := unit.Init(session.SessionHeader{})

	state, _ = unit.Apply(state, sessionListMetadataEvent(0, session.EventTurnStart, session.TurnStartData{Turn: 1}))
	if state.Blank {
		t.Fatal("turn/start must clear blank")
	}
	state, _ = unit.Apply(state, userPromptEvent(1, "first question"))
	if state.LastPromptAt == nil || *state.LastPromptAt != 1 {
		t.Fatalf("lastPromptAt after first prompt = %+v", state.LastPromptAt)
	}
	state, _ = unit.Apply(state, sessionListMetadataEvent(2, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1, Message: llm.NewAssistantMessage(
			[]llm.ContentBlock{{Type: llm.BlockText, Text: "answer"}}, "deepseek", "deepseek-chat", nil,
		),
	}))
	if state.LastPromptAt == nil || *state.LastPromptAt != 1 {
		t.Fatalf("an assistant message must not move lastPromptAt: %+v", state.LastPromptAt)
	}
	state, _ = unit.Apply(state, nonUserPromptEvent(3, "webhook note"))
	if state.LastPromptAt == nil || *state.LastPromptAt != 1 {
		t.Fatalf("a non-user message must not move lastPromptAt: %+v", state.LastPromptAt)
	}
	state, changed := unit.Apply(state, sessionListMetadataEvent(4, session.EventTurnStart, session.TurnStartData{Turn: 2}))
	if changed {
		t.Fatal("a later turn/start over a cleared blank is a no-op")
	}
	state, _ = unit.Apply(state, userPromptEvent(5, "second question"))
	if state.LastPromptAt == nil || *state.LastPromptAt != 5 {
		t.Fatalf("lastPromptAt after second prompt = %+v", state.LastPromptAt)
	}
}
