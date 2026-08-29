// Package sessionreference ports @deepseek-ai/dsh-session-reference:
// canonical session URIs, host-neutral Markdown mentions, tag-safe JSON
// serialization, and byte-bounded projection of another session's surface
// into cross-session context.
//
// Go adaptation: the surface snapshot arrives through the local
// SessionSnapshot seam (the session-query projection feeds it), and the
// llm.MessageSource value carries the user/checkpoint provenance directly.
package sessionreference

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SessionReferenceScheme is the URI scheme reserved for DeepSeek Harness
// session snapshots.
const SessionReferenceScheme = "dsh-session:"

// CodeInvalidReference is the error code for a malformed URI or mention.
const CodeInvalidReference = "SESSION_REFERENCE_INVALID_REFERENCE"

// ReferenceError is a session-reference failure carrying its stable code.
type ReferenceError struct {
	Message string
	Code    string
	Cause   error
}

func (e *ReferenceError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %v", e.Message, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

func (e *ReferenceError) Unwrap() error { return e.Cause }

func invalidReference(format string, args ...any) *ReferenceError {
	return &ReferenceError{Message: fmt.Sprintf(format, args...), Code: CodeInvalidReference}
}

// Input is one source session selected by a host.
type Input struct {
	// SessionID is the opaque source session identity.
	SessionID string
	// Label is the optional user-facing mention label.
	Label string
}

// encodePayload canonicalizes one session id into its base64url JSON
// payload.
func encodePayload(sessionID string) string {
	encoded, _ := json.Marshal(sessionID)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

var payloadPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// EncodeSessionReferenceURI encodes any session-id string as a canonical
// lossless URI.
func EncodeSessionReferenceURI(sessionID string) string {
	return SessionReferenceScheme + encodePayload(sessionID)
}

// DecodeSessionReferenceURI decodes and canonicalizes one session-reference
// URI: the payload must be base64url-shaped, decode to a JSON string, and
// re-encode to the exact input (canonical form only).
func DecodeSessionReferenceURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, SessionReferenceScheme) {
		return "", invalidReference("invalid session reference URI %q", uri)
	}
	payload := uri[len(SessionReferenceScheme):]
	if !payloadPattern.MatchString(payload) {
		return "", invalidReference("invalid session reference URI %q", uri)
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", invalidReference("invalid session reference URI %q", uri)
	}
	var sessionID string
	if err := json.Unmarshal(raw, &sessionID); err != nil {
		return "", invalidReference("invalid session reference URI %q", uri)
	}
	if EncodeSessionReferenceURI(sessionID) != uri {
		return "", invalidReference("invalid session reference URI %q", uri)
	}
	return sessionID, nil
}

// FormatSessionReferenceMention renders a host-neutral Markdown mention
// carrying the canonical URI: an escaped `@[label](uri)`.
func FormatSessionReferenceMention(reference Input) string {
	label := reference.Label
	if label == "" {
		label = reference.SessionID
	}
	return "@[" + escapeLabel(label) + "](" + EncodeSessionReferenceURI(reference.SessionID) + ")"
}

// ParsedText is the result of extracting canonical mentions from plain
// text.
type ParsedText struct {
	// Text has opaque tokens replaced by readable `@label` spans.
	Text string
	// References lists structured references in first-appearance order,
	// before service deduplication.
	References []Input
}

var mentionPattern = regexp.MustCompile(`@\[((?:\\.|[^\\\]])*)\]\((dsh-session:[^\s)]*)\)|(dsh-session:[A-Za-z0-9_-]+)`)

// ParseSessionReferenceText extracts Markdown mentions and bare canonical
// URIs from one text value. Explicit Markdown mentions fail on any
// malformed URI. Bare text is treated as a reference only when it has a
// non-empty base64url-shaped payload, then still fails if that candidate is
// not canonical.
func ParseSessionReferenceText(text string) (ParsedText, error) {
	parsed := ParsedText{}
	var builder strings.Builder
	cursor := 0
	for _, loc := range mentionPattern.FindAllStringSubmatchIndex(text, -1) {
		builder.WriteString(text[cursor:loc[0]])
		cursor = loc[1]
		var rawLabel, markdownURI, bareURI string
		hasLabel := loc[2] >= 0
		if hasLabel {
			rawLabel = text[loc[2]:loc[3]]
		}
		if loc[4] >= 0 {
			markdownURI = text[loc[4]:loc[5]]
		}
		if loc[6] >= 0 {
			bareURI = text[loc[6]:loc[7]]
		}
		uri := markdownURI
		if uri == "" {
			uri = bareURI
		}
		sessionID, err := DecodeSessionReferenceURI(uri)
		if err != nil {
			return ParsedText{}, err
		}
		label := sessionID
		if hasLabel {
			label = unescapeLabel(rawLabel)
		}
		parsed.References = append(parsed.References, Input{SessionID: sessionID, Label: label})
		builder.WriteString("@" + label)
	}
	builder.WriteString(text[cursor:])
	parsed.Text = builder.String()
	return parsed, nil
}

func escapeLabel(label string) string {
	var builder strings.Builder
	for _, r := range label {
		if r == '\\' || r == ']' {
			builder.WriteByte('\\')
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func unescapeLabel(label string) string {
	var builder strings.Builder
	escaped := false
	for _, r := range label {
		if escaped {
			builder.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// StringifyTagSafeJSON serializes JSON while preventing source data from
// spelling an XML-like opening tag: the parse result is unchanged and the
// data contains no literal `<`.
func StringifyTagSafeJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	serialized := buffer.String()
	serialized = strings.TrimSuffix(serialized, "\n")
	if !utf8.ValidString(serialized) {
		return "", fmt.Errorf("session-reference data is not JSON-serializable")
	}
	return strings.ReplaceAll(serialized, "<", `\u003c`), nil
}
