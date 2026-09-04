// Composition-row projection for the plugin-inventory surface.
//
// Port of packages/preset/agent-presets/src/composition-inventory.ts: reads a
// preset's agent.cordis.yml as a flat row list in composition order, with
// group rows expanded into their children and a group's disabled state
// combined into each descendant the way the Loader walks owning groups.
package preset

import (
	"fmt"
	"os"

	"dshgo/cordis/loader"
)

// CompositionRow is one flattened plugin row of a preset composition.
type CompositionRow struct {
	// EntryID is the row's stable id (the plugin's loader-entry id).
	EntryID *string
	// ModuleName is the package the row mounts.
	ModuleName string
	// Enabled is the row's effective enablement: true, false, or nil when a
	// disabled expression left the decision to a mount.
	Enabled *bool
}

// CompositionRows reads one preset's composition file and flattens its rows
// in composition order. A broken or unreadable preset answers its broken
// reason as the error.
func CompositionRows(preset AgentPreset) ([]CompositionRow, error) {
	if preset.Broken != nil {
		return nil, fmt.Errorf("agent-presets: preset %q is broken: %s", preset.ID, *preset.Broken)
	}
	text, err := os.ReadFile(preset.Path)
	if err != nil {
		return nil, fmt.Errorf("agent-presets: read composition of %q: %w", preset.ID, err)
	}
	entries, err := loader.DecodeEntryList(text)
	if err != nil {
		return nil, fmt.Errorf("agent-presets: composition of %q: %w", preset.ID, err)
	}
	rows := make([]CompositionRow, 0, len(entries))
	walkCompositionRows(entries, nil, &rows)
	return rows, nil
}

// enablementOf resolves one entry's disabled contribution: a literal bool is
// exact; a !!js expression is conditional (nil — a mount decides); absent is
// enabled.
func enablementOf(entry loader.Entry) *bool {
	switch value := entry.Disabled.(type) {
	case nil:
		return boolPtr(false)
	case bool:
		return boolPtr(value)
	case loader.RawExpression:
		resolved, err := loader.Evaluate(value)
		if err != nil {
			return nil
		}
		flag, ok := resolved.(bool)
		if !ok {
			return nil
		}
		return boolPtr(flag)
	default:
		return nil
	}
}

// combineEnablement folds a row's own disabled contribution into its
// ancestor's (official combineDisabled): any literal true disables; otherwise
// any conditional leaves the decision to a mount; else enabled.
func combineEnablement(outer, own *bool) *bool {
	if outer != nil && *outer {
		return boolPtr(true)
	}
	if own != nil && *own {
		return boolPtr(true)
	}
	if outer == nil || own == nil {
		return nil
	}
	return boolPtr(false)
}

// walkCompositionRows appends every non-group row, expanding group rows and
// threading each level's combined enablement into its descendants.
func walkCompositionRows(entries []loader.Entry, outer *bool, out *[]CompositionRow) {
	for _, entry := range entries {
		own := enablementOf(entry)
		combined := combineEnablement(outer, own)
		if entry.Group {
			if children, ok := entry.Config.([]loader.Entry); ok {
				walkCompositionRows(children, combined, out)
			}
			continue
		}
		row := CompositionRow{
			ModuleName: entry.Name,
			Enabled:    combined,
		}
		if entry.ID != "" {
			id := entry.ID
			row.EntryID = &id
		}
		*out = append(*out, row)
	}
}

func boolPtr(value bool) *bool { return &value }
