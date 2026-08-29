// A preset's display metadata: the name and description a picker shows.
//
// It lives in its own file because the composition is a top-level list of
// plugin rows — YAML cannot carry sibling keys beside it. The file carries
// display text ONLY: `id` is the directory name and `trust` comes from the
// root a preset was discovered under, so neither is writable here —
// otherwise a locally authored preset could claim to be a shipped one.
//
// Every read failure degrades to no metadata. A preset whose display text
// is missing, malformed, or unreadable still mounts: presentation is not a
// capability, and a broken name must never become an agent that cannot
// start.
package preset

import (
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// MetadataFile is the optional display-metadata file beside a preset's
// composition.
const MetadataFile = "preset.yml"

// PresetMetadata is the display text a preset may publish about itself.
type PresetMetadata struct {
	// Name is the human-facing name; falls back to the preset id when nil.
	Name *string `json:"name,omitempty"`
	// Description is one sentence on what this preset is for.
	Description *string `json:"description,omitempty"`
	// Order is the position within its group; lower comes first. A preset
	// that declares none sorts after every preset that does, then by id —
	// so the shipped set can read in capability order while authored ones
	// stay alphabetical.
	Order *float64 `json:"order,omitempty"`
}

// metadataText is a non-empty trimmed string, or nil for anything else.
// The trim mirrors JS `String.prototype.trim`, which also strips U+FEFF.
func metadataText(value any) *string {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(strings.TrimPrefix(text, "\uFEFF"))
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// ReadPresetMetadata reads one preset directory's display metadata.
//
// Absent, unparsable, and wrongly-shaped files are all the same answer —
// empty metadata — because the caller renders a picker, not a diagnostic.
func ReadPresetMetadata(directory string) PresetMetadata {
	raw, err := os.ReadFile(filepath.Join(directory, MetadataFile))
	if err != nil {
		// Absent is the common case: metadata is optional and most presets,
		// including every one authored by duplicating another, carry none.
		return PresetMetadata{}
	}
	var parsed any
	if err := yaml.Unmarshal(raw, &parsed); err != nil {
		// Malformed display text is not worth failing discovery over; the
		// picker falls back to the id, and the composition still mounts.
		return PresetMetadata{}
	}
	record, ok := parsed.(map[string]any)
	if !ok {
		return PresetMetadata{}
	}
	name := metadataText(record["name"])
	description := metadataText(record["description"])
	order := metadataOrder(record["order"])
	return PresetMetadata{Name: name, Description: description, Order: order}
}

// metadataOrder keeps a finite number, else nil. YAML decodes whole
// numbers as ints while JSON hands back float64; both count.
func metadataOrder(value any) *float64 {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return nil
		}
		return &typed
	case int:
		number := float64(typed)
		return &number
	case int64:
		number := float64(typed)
		return &number
	default:
		return nil
	}
}

// RenderPresetMetadata renders display metadata as the file's contents.
//
// Absent fields are omitted rather than written empty, so a preset with no
// description does not ship a key that reads as an intentional blank. The
// second return is false when there is nothing to store.
func RenderPresetMetadata(metadata PresetMetadata) (string, bool) {
	name := normalizeText(metadata.Name)
	description := normalizeText(metadata.Description)
	order := normalizeOrder(metadata.Order)
	if name == nil && description == nil && order == nil {
		return "", false
	}
	document := map[string]any{}
	if name != nil {
		document["name"] = *name
	}
	if description != nil {
		document["description"] = *description
	}
	if order != nil {
		document["order"] = *order
	}
	rendered, err := yaml.Marshal(document)
	if err != nil {
		// A map of three scalar values cannot fail to marshal; the guard
		// keeps the signature total without a panic path.
		return "", false
	}
	return string(rendered), true
}

// normalizeText re-runs the reader's text rule on a value the caller
// supplied directly.
func normalizeText(value *string) *string {
	if value == nil {
		return nil
	}
	return metadataText(*value)
}

// normalizeOrder re-runs the reader's order rule on a value the caller
// supplied directly.
func normalizeOrder(value *float64) *float64 {
	if value == nil {
		return nil
	}
	return metadataOrder(*value)
}
