// Patch semantics: the exact algorithm of vendor/include applyEntryPatches —
// `insert` rows append (top level or a group's children), every other field
// on a patch row REPLACES the target's field wholesale, misses are warnings
// never errors, and rows a patch inserts are indexed so later patches in the
// same list can target them. Source: packages/boot/app-boot parsePatchList
// (anchoring, fail-loud file validation) and vendor/include applyEntryPatches.
package loader

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Patch is one loader patch row: an `insert` op and/or, for a targeted row,
// whole-field overrides. Shape mirrors the official PatchOptions.
type Patch struct {
	ID string // target row id (empty for top-level insert)
	// Name guards the override: when set and the target's name differs the
	// patch is skipped with a warning.
	Name   string
	Insert []Entry
	// Overrides replace target fields wholesale — the official algorithm
	// assigns target[key] = value with no merge step.
	Overrides []FieldOverride
}

// FieldOverride replaces one target field.
type FieldOverride struct {
	Key   string
	Value any
}

// ParsePatchFile parses one cordis.patch.yml / --patch overlay: a top-level
// YAML array of patch mappings. Structural violations fail loudly; a patch
// whose target row is absent is a per-entry warning at apply time, because one
// overlay shared across surfaces need not match every tree.
func ParsePatchFile(data []byte, patchDir string) ([]Patch, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("loader: failed to parse patches: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("loader: patch document is empty")
	}
	list := root.Content[0]
	if list.Tag == "!!null" {
		return nil, nil
	}
	if list.Kind != yaml.SequenceNode {
		return nil, errors.New("loader: patches must be a top-level YAML array of loader patch entries")
	}
	patches := make([]Patch, 0, len(list.Content))
	for i, item := range list.Content {
		patch, err := decodePatch(item)
		if err != nil {
			return nil, fmt.Errorf("loader: patch entry %d: %w", i+1, err)
		}
		patches = append(patches, *patch)
	}
	anchorInsertedPluginNames(patches, patchDir)
	return patches, nil
}

func decodePatch(node *yaml.Node) (*Patch, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("must be a mapping (a loader patch entry), got %s at line %d", kindName(node), node.Line)
	}
	patch := &Patch{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valueNode := node.Content[i], node.Content[i+1]
		var err error
		switch keyNode.Value {
		case "id":
			patch.ID, err = decodeString(valueNode)
		case "name":
			patch.Name, err = decodeString(valueNode)
		case "insert":
			patch.Insert, err = decodeChildEntries(valueNode)
		default:
			var value any
			value, err = decodeValue(valueNode)
			patch.Overrides = append(patch.Overrides, FieldOverride{Key: keyNode.Value, Value: value})
		}
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", keyNode.Value, err)
		}
	}
	return patch, nil
}

// anchorInsertedPluginNames resolves relative plugin paths in one patch file's
// insert rows to file URLs against the patch file's directory, without
// touching assertion names otherwise. Source: app-boot anchorInsertedPluginNames.
func anchorInsertedPluginNames(patches []Patch, dir string) {
	var visit func(entries []Entry)
	visit = func(entries []Entry) {
		for i := range entries {
			name := entries[i].Name
			if strings.HasPrefix(name, "./") || strings.HasPrefix(name, "../") {
				entries[i].Name = fileURLOf(filepath.Join(dir, name))
			}
			if entries[i].Group {
				if children, ok := entries[i].Config.([]Entry); ok {
					visit(children)
				}
			}
		}
	}
	for i := range patches {
		visit(patches[i].Insert)
	}
}

// fileURLOf renders one absolute path as a file URL with the same shape as
// the official pathToFileURL: `file:///D:/x.ts` on Windows (three slashes,
// empty host), `file:///home/x.ts` on Unix.
func fileURLOf(path string) string {
	slash := strings.TrimPrefix(filepath.ToSlash(path), "/")
	return "file:///" + slash
}

// ApplyEntryPatches returns a detached entry list with every applicable patch
// applied, plus the per-entry warnings the official algorithm reports instead
// of failing. The input list is never mutated.
//
// The official algorithm walks object references, so its index stays valid as
// lists grow. Go slice appends may reallocate the backing array, so the id
// index is rebuilt before every patch row — same semantics, no stale pointers.
func ApplyEntryPatches(data []Entry, patches []Patch) ([]Entry, []string) {
	entries := cloneEntries(data)
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	if len(patches) == 0 {
		return entries, warnings
	}

	index := map[string]*Entry{}
	reindex := func() {
		index = map[string]*Entry{}
		var walk func(list []Entry)
		walk = func(list []Entry) {
			for i := range list {
				if list[i].ID != "" {
					index[list[i].ID] = &list[i]
				}
				if list[i].Group {
					if children, ok := list[i].Config.([]Entry); ok {
						walk(children)
					}
				}
			}
		}
		walk(entries)
	}

	for _, patch := range patches {
		reindex()
		if len(patch.Insert) > 0 {
			inserted := cloneEntries(patch.Insert)
			if patch.ID != "" {
				target := index[patch.ID]
				if target == nil {
					warn("patch insert: entry %s not found", patch.ID)
					continue
				}
				if !target.Group {
					warn("patch insert: entry %s is not a group", patch.ID)
					continue
				}
				children, _ := target.Config.([]Entry)
				children = append(children, inserted...)
				target.Config = children
			} else {
				entries = append(entries, inserted...)
			}
			continue
		}

		if patch.ID == "" {
			warn("patch: id is required for non-insert patches")
			continue
		}
		target := index[patch.ID]
		if target == nil {
			warn("patch: entry %s not found", patch.ID)
			continue
		}
		if patch.Name != "" && patch.Name != target.Name {
			warn("patch: name mismatch for %s (expected %s, got %s), skipping", patch.ID, patch.Name, target.Name)
			continue
		}
		for _, override := range patch.Overrides {
			if override.Key == "id" {
				continue
			}
			target.setField(override.Key, override.Value)
		}
	}
	return entries, warnings
}

func (e *Entry) setField(key string, value any) {
	switch key {
	case "name":
		if s, ok := value.(string); ok {
			e.Name = s
		}
	case "group":
		if b, ok := value.(bool); ok {
			e.Group = b
		}
	case "inject":
		if list, ok := value.([]any); ok {
			e.Inject = toStringList(list)
		}
	case "provide":
		if list, ok := value.([]any); ok {
			e.Provide = toStringList(list)
		}
	case "config":
		e.Config = value
	case "disabled":
		e.Disabled = value
	default:
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra[key] = value
	}
}

func toStringList(list []any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func cloneEntries(list []Entry) []Entry {
	out := make([]Entry, len(list))
	for i := range list {
		out[i] = cloneEntry(list[i])
	}
	return out
}

func cloneEntry(entry Entry) Entry {
	cloned := entry
	cloned.Inject = append([]string(nil), entry.Inject...)
	cloned.Provide = append([]string(nil), entry.Provide...)
	cloned.Config = cloneValue(entry.Config)
	cloned.Disabled = cloneValue(entry.Disabled)
	if entry.Extra == nil {
		cloned.Extra = map[string]any{}
	} else {
		cloned.Extra = cloneValue(entry.Extra).(map[string]any)
	}
	return cloned
}

func cloneValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneValue(item)
		}
		return out
	case []Entry:
		return cloneEntries(v)
	default:
		return value
	}
}
