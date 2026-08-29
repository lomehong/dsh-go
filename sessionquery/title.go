package sessionquery

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	session "dshgo/session"
)

// Pure port of @deepseek-ai/dsh-session-title's log-backed fold surface
// (normalize.ts + the fold/collect exports of index.ts). The async title
// service — provider registration, fallback generation, scheduling — is a
// host-composition concern deferred with the rest of the title service;
// session-query needs only the deterministic log fold.

// SessionTitleSourceKind values: the built-in fallback, a registered
// provider, or an explicit user rename.
const (
	TitleSourceFallback = "fallback"
	TitleSourceProvider = "provider"
	TitleSourceUser     = "user"
)

// SessionTitleModelProvenance is the exact auxiliary model route that
// produced a title.
type SessionTitleModelProvenance struct {
	// Provider is the registered LLM provider route.
	Provider string `json:"provider"`
	// Model is the provider model id.
	Model string `json:"model"`
}

// SessionTitleSource is the durable ownership record for an accepted
// session title.
type SessionTitleSource struct {
	// Kind is one of TitleSourceFallback, TitleSourceProvider,
	// TitleSourceUser.
	Kind string `json:"kind"`
	// Provider names the registered provider for kind=provider.
	Provider string `json:"provider,omitempty"`
	// Model is the exact route provenance, optional for kind=provider.
	Model *SessionTitleModelProvenance `json:"model,omitempty"`
}

// SessionTitleEventData is the payload of the log-only `session/title`
// event.
type SessionTitleEventData struct {
	// Title is the normalized non-empty title text.
	Title string `json:"title"`
	// MessageSeqs are the exact human user/message seqs used to derive this
	// title; empty for an explicit user rename.
	MessageSeqs []int64 `json:"messageSeqs"`
	// Source records whether the built-in fallback, a registered provider,
	// or the user supplied the title.
	Source SessionTitleSource `json:"source"`
}

// SessionTitleSnapshot is the latest folded title plus the title event's
// durable envelope facts.
type SessionTitleSnapshot struct {
	SessionTitleEventData
	// EventSeq is the seq of the latest session/title event.
	EventSeq int64 `json:"eventSeq"`
	// UpdatedAt is the timestamp of the latest session/title event.
	UpdatedAt int64 `json:"updatedAt"`
}

// SessionTitleUserMessage is one eligible human prompt for title
// generation, with its exact source seq.
type SessionTitleUserMessage struct {
	// Seq is the source user/message event seq.
	Seq int64 `json:"seq"`
	// Text is the joined text-block content.
	Text string `json:"text"`
}

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes("session/title")
}

// FoldSessionTitle folds the latest logged title without consulting mutable
// metadata: the last session/title event, or nil.
func FoldSessionTitle(events []session.Event) *SessionTitleSnapshot {
	var latest *session.Event
	for index := range events {
		if events[index].Type == "session/title" {
			latest = &events[index]
		}
	}
	if latest == nil {
		return nil
	}
	var data SessionTitleEventData
	// The payload was validated at the append boundary; a decoded log keeps
	// the same shape.
	if err := unmarshalJSON(latest.Data, &data); err != nil {
		return nil
	}
	messageSeqs := make([]int64, len(data.MessageSeqs))
	copy(messageSeqs, data.MessageSeqs)
	if messageSeqs == nil {
		messageSeqs = []int64{}
	}
	return &SessionTitleSnapshot{
		SessionTitleEventData: SessionTitleEventData{
			Title:       data.Title,
			MessageSeqs: messageSeqs,
			Source:      copySessionTitleSource(data.Source),
		},
		EventSeq:  latest.Seq,
		UpdatedAt: latest.Time,
	}
}

func copySessionTitleSource(source SessionTitleSource) SessionTitleSource {
	copied := SessionTitleSource{Kind: source.Kind, Provider: source.Provider}
	if source.Model != nil {
		model := *source.Model
		copied.Model = &model
	}
	return copied
}

// CollectSessionTitleMessages collects human text-bearing user messages in
// log order. ThroughSeq, when non-nil, is an inclusive event boundary.
func CollectSessionTitleMessages(events []session.Event, throughSeq *int64) []SessionTitleUserMessage {
	messages := []SessionTitleUserMessage{}
	for index := range events {
		event := events[index]
		if throughSeq != nil && event.Seq > *throughSeq {
			break
		}
		if event.Type != session.EventUserMessage {
			continue
		}
		message, err := session.DecodeUserMessage(event)
		if err != nil || message.Source.Kind != "user" {
			continue
		}
		text := contentText(message.Content)
		if len(NormalizeSessionTitle(text, maxSafeInteger)) == 0 {
			continue
		}
		messages = append(messages, SessionTitleUserMessage{Seq: event.Seq, Text: text})
	}
	return messages
}

// maxSafeInteger mirrors the official guard bound passed to title
// normalization during collection.
const maxSafeInteger int = 1<<53 - 1

// NormalizeSessionTitle sanitizes one accepted session title and enforces
// its UTF-8 byte budget: control and escape sequences removed, whitespace
// collapsed, truncated without splitting a code point.
func NormalizeSessionTitle(input string, maxBytes int) string {
	return trimEnd(truncateTitleUtf8(cleanTitleText(input), maxBytes))
}

// FallbackSessionTitle derives the deterministic first-prompt fallback: the
// normalized leading words within both limits.
func FallbackSessionTitle(input string, maxWords, maxBytes int) string {
	if maxWords <= 0 {
		panic("maxWords must be a positive integer")
	}
	words := strings.Split(cleanTitleText(input), " ")
	kept := make([]string, 0, len(words))
	for _, word := range words {
		if word != "" {
			kept = append(kept, word)
		}
		if len(kept) == maxWords {
			break
		}
	}
	return trimEnd(truncateTitleUtf8(strings.Join(kept, " "), maxBytes))
}

// truncateTitleUtf8 truncates to a UTF-8 byte budget without splitting a
// Unicode code point.
func truncateTitleUtf8(input string, maxBytes int) string {
	if maxBytes <= 0 {
		panic("maxBytes must be a positive integer")
	}
	if len(input) <= maxBytes {
		return input
	}
	used := 0
	for index, r := range input {
		size := utf8.RuneLen(r)
		if size < 0 || used+size > maxBytes {
			return input[:index]
		}
		used += size
	}
	return input
}

func unmarshalJSON(data []byte, target any) error {
	return json.Unmarshal(data, target)
}
