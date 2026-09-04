// Package sessionformat re-implements the pure adjacent whole-artifact
// Session format migration machinery of @deepseek-ai/dsh-session-format
// (official tag dsh-v0.1.3-alpha.1): lossless snapshots, unique gap-free
// migration planning, header-only conversion, and whole-artifact
// composition. One profile-independent pure package owns each adjacent
// vN -> vN+1 conversion; this core owns only the shared vocabulary.
//
// Lossless-JSON discipline: header and payload trees are decoded with
// json.Number so every numeric literal survives a round-trip byte-exactly.
// Go adaptations: deep-freeze is moot (trees are passed by value and
// RawMessage bytes are treated as immutable), and JSON member order is not
// preserved through map trees (member order is not semantic).
package sessionformat

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// FormatError is raised when a durable Session artifact cannot be restored
// or migrated losslessly (official SessionFormatError).
type FormatError struct{ Message string }

func (e *FormatError) Error() string { return e.Message }

// UnsupportedMigrationError marks a readable artifact whose released source
// policy has no supported migration (official
// SessionFormatUnsupportedMigrationError).
type UnsupportedMigrationError struct{ Message string }

func (e *UnsupportedMigrationError) Error() string { return e.Message }

func formatErrorf(format string, args ...any) error {
	return &FormatError{Message: fmt.Sprintf(format, args...)}
}

func unsupportedf(format string, args ...any) error {
	return &UnsupportedMigrationError{Message: fmt.Sprintf(format, args...)}
}

// FormatErrorf builds a FormatError for edge packages.
func FormatErrorf(format string, args ...any) error {
	return formatErrorf(format, args...)
}

// UnsupportedErrorf builds an UnsupportedMigrationError for edge packages.
func UnsupportedErrorf(format string, args ...any) error {
	return unsupportedf(format, args...)
}

// Header is one logical Session header: a lossless JSON object tree. Known
// members are read through HeaderField helpers; unknown members ride
// verbatim (the format core treats the header as owner-opaque JSON beyond
// the fields it validates).
type Header map[string]any

// DecodeHeaderValue decodes one physical header JSON value into a Header
// tree, rejecting non-object values.
func DecodeHeaderValue(value json.RawMessage) (Header, error) {
	tree, err := DecodeValue(value)
	if err != nil {
		return nil, formatErrorf("Session header must be a JSON object")
	}
	header, ok := tree.(map[string]any)
	if !ok {
		return nil, formatErrorf("Session header must be a JSON object")
	}
	return Header(header), nil
}

// Encode renders the header back to lossless JSON.
func (h Header) Encode() (json.RawMessage, error) {
	return EncodeValue(map[string]any(h))
}

// Clone returns a detached deep copy of the header tree.
func (h Header) Clone() Header {
	clone, err := DecodeHeaderValue(mustEncode(map[string]any(h)))
	if err != nil {
		panic(err)
	}
	return clone
}

func mustEncode(value any) json.RawMessage {
	encoded, err := EncodeValue(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// HeaderField reads one member of a header tree.
func (h Header) Field(name string) (any, bool) {
	value, ok := h[name]
	return value, ok
}

// HeaderVersion reads the validated `version` member.
func HeaderVersion(h Header) (int64, error) {
	return CountField(h, "version", "Session format version")
}

// CountField reads one member as a validated non-negative safe integer.
func CountField(object map[string]any, key, label string) (int64, error) {
	value, ok := object[key]
	if !ok {
		return 0, formatErrorf("%s must be a non-negative safe integer", label)
	}
	return Count(value, label)
}

// Count validates one JSON scalar as a non-negative safe integer. Go's
// int64 domain excludes the JSON-unstable negative zero by construction.
func Count(value any, label string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, formatErrorf("%s must be a non-negative safe integer", label)
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 {
		return 0, formatErrorf("%s must be a non-negative safe integer", label)
	}
	return parsed, nil
}

// SafeInteger validates one JSON scalar as a safe integer (any sign).
func SafeInteger(value any, label string) (int64, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, formatErrorf("%s must be a safe integer", label)
	}
	parsed, err := number.Int64()
	if err != nil {
		return 0, formatErrorf("%s must be a safe integer", label)
	}
	return parsed, nil
}

// InspectVersion reads only the version required for directional dispatch
// from one untrusted physical header value.
func InspectVersion(headerValue json.RawMessage) (int64, error) {
	header, err := DecodeHeaderValue(headerValue)
	if err != nil {
		return 0, err
	}
	version, err := HeaderVersion(header)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// DecodeValue decodes lossless JSON into a generic tree (json.Number keeps
// every numeric literal exact).
func DecodeValue(value json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// EncodeValue renders a decoded tree back to lossless JSON.
func EncodeValue(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// Event is the generic format-level event envelope, independent of the
// session package's typed events: type/seq/time/data are the interpreted
// members, every other envelope member (sourceEventSeqs, surfaceOp,
// ignorable, ...) rides verbatim in Extra until an edge explicitly rewrites
// it.
type Event struct {
	Type string          `json:"type"`
	Seq  int64           `json:"seq"`
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`
	// Extra holds every other envelope member, lossless.
	Extra map[string]json.RawMessage `json:"-"`
}

// ExtraField reads one extra envelope member.
func (e Event) ExtraField(name string) (json.RawMessage, bool) {
	value, ok := e.Extra[name]
	return value, ok
}

// MarshalJSON renders the interpreted members plus extras.
func (e Event) MarshalJSON() ([]byte, error) {
	wire := map[string]json.RawMessage{
		"type": mustEncode(e.Type),
		"seq":  mustEncode(e.Seq),
		"time": mustEncode(e.Time),
		"data": json.RawMessage(e.Data),
	}
	for key, value := range e.Extra {
		wire[key] = value
	}
	return json.Marshal(wire)
}

// UnmarshalJSON reads the envelope, separating the interpreted members from
// the lossless extras.
func (e *Event) UnmarshalJSON(data []byte) error {
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	var interpreted struct {
		Type string          `json:"type"`
		Seq  int64           `json:"seq"`
		Time int64           `json:"time"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &interpreted); err != nil {
		return err
	}
	e.Type, e.Seq, e.Time, e.Data = interpreted.Type, interpreted.Seq, interpreted.Time, interpreted.Data
	e.Extra = map[string]json.RawMessage{}
	for key, value := range wire {
		switch key {
		case "type", "seq", "time", "data":
		default:
			e.Extra[key] = value
		}
	}
	return nil
}

// Artifact is one detached complete logical Session artifact.
type Artifact struct {
	Header Header
	// InheritedEventCount is the exact inherited prefix length; meaningful
	// only after a body read.
	InheritedEventCount int64
	Events              []Event
}

// SnapshotArtifact validates one complete artifact's shared coordinates and
// returns it detached: dense seqs, non-empty types, safe times, present
// data, and an inherited cut within the event count.
func SnapshotArtifact(artifact Artifact, label string) (Artifact, error) {
	if artifact.Header == nil {
		return Artifact{}, formatErrorf("%s header must be a JSON object", label)
	}
	if _, err := HeaderVersion(artifact.Header); err != nil {
		return Artifact{}, err
	}
	if artifact.InheritedEventCount < 0 {
		return Artifact{}, formatErrorf("%s inheritedEventCount must be a non-negative safe integer", label)
	}
	for index, event := range artifact.Events {
		if event.Seq != int64(index) {
			return Artifact{}, formatErrorf("%s event %d has non-dense seq %d", label, index, event.Seq)
		}
		if event.Type == "" {
			return Artifact{}, formatErrorf("%s event %d type must be a non-empty string", label, index)
		}
		if event.Data == nil {
			return Artifact{}, formatErrorf("%s event %d lacks data", label, index)
		}
	}
	if artifact.InheritedEventCount > int64(len(artifact.Events)) {
		return Artifact{}, formatErrorf("%s inheritedEventCount exceeds its event count", label)
	}
	return artifact, nil
}

// SnapshotHeader validates one logical header without inspecting an event
// body: version, string id, non-negative createdAt, boolean isSeeded, and a
// non-negative delegationDepth.
func SnapshotHeader(header Header, label string) (Header, error) {
	if header == nil {
		return nil, formatErrorf("%s must be a JSON object", label)
	}
	if _, err := HeaderVersion(header); err != nil {
		return nil, err
	}
	id, ok := header["id"]
	if !ok {
		return nil, formatErrorf("%s id must be a string", label)
	}
	if _, ok := id.(string); !ok {
		return nil, formatErrorf("%s id must be a string", label)
	}
	if _, err := CountField(header, "createdAt", label+" createdAt"); err != nil {
		return nil, err
	}
	seeded, ok := header["isSeeded"]
	if !ok {
		return nil, formatErrorf("%s isSeeded must be a boolean", label)
	}
	if _, ok := seeded.(bool); !ok {
		return nil, formatErrorf("%s isSeeded must be a boolean", label)
	}
	if _, err := CountField(header, "delegationDepth", label+" delegationDepth"); err != nil {
		return nil, err
	}
	return header, nil
}
