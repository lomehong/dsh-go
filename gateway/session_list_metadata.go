package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
)

// sessionListMetadataProjectionStateVersion guards persisted
// sessionListMetadata rows (official stateVersion 1).
const sessionListMetadataProjectionStateVersion = 1

// SessionListMetadataKey is the session-projection key this domain owns
// (official api-session-controller list projection).
const SessionListMetadataKey = "sessionListMetadata"

// SessionListMetadata is the persisted hint the session list consumes to
// summarize a session without activating it: whether the folded prefix
// contains no turn, and the latest human-authored prompt time.
type SessionListMetadata struct {
	// Blank reports whether the folded prefix contains no turn (a brand-new
	// session is blank; the first turn/start clears it for good).
	Blank bool `json:"blank"`
	// LastPromptAt is the latest human-authored (source.kind user)
	// user/message time in Unix epoch milliseconds; nil when the folded
	// prefix has none.
	LastPromptAt *int64 `json:"lastPromptAt"`
}

// ApplySessionListMetadata is the light last-wins fold of the
// `sessionListMetadata` projection unit (official applySessionListMetadata):
// blank clears on the first turn/start and stays clear; lastPromptAt
// advances to the newest user-authored prompt's time. Any other event
// leaves the metadata unchanged (same reference, zero downstream work).
func ApplySessionListMetadata(state SessionListMetadata, event session.Event) (SessionListMetadata, bool) {
	blank := state.Blank && event.Type != session.EventTurnStart
	lastPromptAt := state.LastPromptAt
	if event.Type == session.EventUserMessage {
		if message, err := session.DecodeUserMessage(event); err == nil && message.Source.Kind == llm.SourceUser {
			time := event.Time
			lastPromptAt = &time
		}
	}
	if blank == state.Blank && sameLastPromptAt(lastPromptAt, state.LastPromptAt) {
		return state, false
	}
	return SessionListMetadata{Blank: blank, LastPromptAt: lastPromptAt}, true
}

func sameLastPromptAt(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// SessionListMetadataUnit is the `sessionListMetadata` projection unit
// definition: blank/lastPromptAt fold with an identity view (the wire value
// is the persisted hint itself). The unit activates only when a projection
// registry is composed (headless assemblies stay unaffected).
func SessionListMetadataUnit() projection.Unit[SessionListMetadata] {
	return projection.Unit[SessionListMetadata]{
		Key:          SessionListMetadataKey,
		StateVersion: sessionListMetadataProjectionStateVersion,
		Init: func(session.SessionHeader) SessionListMetadata {
			return SessionListMetadata{Blank: true}
		},
		Apply: ApplySessionListMetadata,
		View: func(state SessionListMetadata) any {
			return state
		},
		DecodeState: decodeSessionListMetadata,
	}
}

// decodeSessionListMetadata validates and reifies one persisted row value
// (the official zod stateSchema role): blank must be boolean and
// lastPromptAt must be a non-negative number or null.
func decodeSessionListMetadata(raw json.RawMessage) (SessionListMetadata, error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return SessionListMetadata{}, fmt.Errorf("sessionListMetadata row must not be null")
	}
	var record struct {
		Blank        *bool           `json:"blank"`
		LastPromptAt json.RawMessage `json:"lastPromptAt"`
	}
	if err := json.Unmarshal(trimmed, &record); err != nil {
		return SessionListMetadata{}, fmt.Errorf("sessionListMetadata row is not a record: %w", err)
	}
	if record.Blank == nil {
		return SessionListMetadata{}, fmt.Errorf("sessionListMetadata row lacks a blank boolean")
	}
	if record.LastPromptAt == nil {
		return SessionListMetadata{}, fmt.Errorf("sessionListMetadata row lacks a lastPromptAt number or null")
	}
	var lastPromptAt *int64
	lastTrimmed := bytes.TrimSpace(record.LastPromptAt)
	if !bytes.Equal(lastTrimmed, []byte("null")) {
		var value int64
		if err := json.Unmarshal(lastTrimmed, &value); err != nil {
			return SessionListMetadata{}, fmt.Errorf("sessionListMetadata lastPromptAt must be a number or null")
		}
		if value < 0 {
			return SessionListMetadata{}, fmt.Errorf("sessionListMetadata lastPromptAt must be a non-negative number or null")
		}
		lastPromptAt = &value
	}
	return SessionListMetadata{Blank: *record.Blank, LastPromptAt: lastPromptAt}, nil
}
