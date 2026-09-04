package sessionformatv01

import (
	"path/filepath"
	"strings"

	"dshgo/sessionformat"
)

// Exported validation faces reused by the adjacent v1 -> v2 edge (official
// cross-package imports from @deepseek-ai/dsh-session-format-v1-to-v2).

// DispositionView is the exported frozen disposition view.
type DispositionView struct {
	Required []string
	Optional []string
	Opaque   []string
}

// DispositionFor returns the frozen released-v0 inventory entry for one
// event type.
func DispositionFor(eventType string) (DispositionView, bool) {
	spec, ok := releasedV0EventDispositions[eventType]
	if !ok {
		return DispositionView{}, false
	}
	return DispositionView{Required: spec.required, Optional: spec.optional, Opaque: spec.opaque}, true
}

// AssertKeys exports the exact required/optional member check.
func AssertKeys(record map[string]any, required, optional []string, label string) error {
	return assertKeys(record, required, optional, label)
}

// AssertDataKeys is AssertKeys under the payload-data name.
func AssertDataKeys(data map[string]any, required, optional []string, label string) error {
	return assertKeys(data, required, optional, label)
}

// AssertEnvelopeKeys exports the event envelope member check.
func AssertEnvelopeKeys(event sessionformat.Event, optional []string, index int) error {
	return assertEnvelopeKeys(event, optional, index)
}

// PathIsAbsolute reports whether a header cwd is absolute on any host
// (node's win32-aware isAbsolute semantics).
func PathIsAbsolute(path string) bool {
	return filepath.IsAbs(path) || isWindowsAbsolute(path)
}

// AssertReleasedPayloadSemanticsOnly validates nested payload semantics
// without the top-level member check (the official
// assertReleasedPayloadSemantics export).
func AssertReleasedPayloadSemanticsOnly(event sessionformat.Event, version int64) error {
	label := event.Type + " " + itoa(event.Seq)
	data, err := decodeData(event, label)
	if err != nil {
		return err
	}
	return assertReleasedPayloadSemantics(event, version, data, label)
}

// HasDisposition reports whether the type is in the frozen released-v0
// inventory.
func HasDisposition(eventType string) bool {
	_, ok := releasedV0EventDispositions[eventType]
	return ok
}

var _ = strings.TrimSpace
