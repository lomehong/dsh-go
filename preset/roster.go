// The agent-preset roster: the composition set a deployment supplies, with
// the read, copy, and remove operations its surfaces offer.
//
// Ported from packages/preset/agent-presets/src/index.ts. The roster face
// covers discovery, resolution, and authoring; the standing-mount
// machinery (per-preset cordis scopes agents join) lives beside it in
// mount.go. Go composes agents programmatically and has no cordis fiber to
// re-parent, so the recompose layer that re-links agents between presets
// stays deferred.
package preset

import (
	"os"

	"dshgo/homepaths"
)

// SettingsNamespace is the settings namespace carrying the user's chosen
// default preset.
const SettingsNamespace = "agent-presets"

// RosterOptions carries the seams the roster needs from its host.
type RosterOptions struct {
	// ShippedRoot is the bundled shipped-preset directory; empty means the
	// deployment carries no shipped set. The official package resolves its
	// own presets/ directory; a Go deployment names one explicitly.
	ShippedRoot string
	// Getenv resolves environment lookups for the harness home.
	Getenv homepaths.Getenv
	// DefaultOverride reads the user's chosen default (the settings
	// layer); nil means the deployment stores none.
	DefaultOverride func() (string, bool)
	// ClearDefaultOverride clears the user's chosen default; nil means
	// there is nothing to clear.
	ClearDefaultOverride func()
	// Now is unused today and reserved for stamp-based mount caching;
	// tests may leave it nil. Stamps themselves live on the standing
	// mounts (see mount.go).
	Now func() int64
	// InvalidateStanding drops any standing record for a preset id; the
	// boot layer wires it to the mount table. nil leaves records alone.
	InvalidateStanding func(id string)
}

// Roster is one deployment's agent-preset set.
type Roster struct {
	config  Config
	options RosterOptions
}

// NewRoster builds the roster over the given plugin config.
func NewRoster(config Config, options RosterOptions) *Roster {
	return &Roster{config: config, options: options}
}

// resolvedRoots is the root list the roster addresses: the shipped set
// first (always wins a duplicate id), then the configured roots in
// precedence order, then the derived writable user root.
func (r *Roster) resolvedRoots() []PresetRoot {
	roots := make([]PresetRoot, 0, len(r.config.Roots)+2)
	if r.config.IncludeShippedRoot {
		roots = append(roots, PresetRoot{Path: r.options.ShippedRoot, Trust: TrustSystem})
	}
	roots = append(roots, r.config.Roots...)
	if r.config.IncludeUserRoot {
		roots = append(roots, PresetRoot{
			Path:  homepaths.DshHomePath(r.options.Getenv, UserPresetDir),
			Trust: TrustUser,
		})
	}
	return roots
}

// authorable reports whether this deployment has a root locally authored
// presets go to.
func (r *Roster) authorable() bool {
	for _, root := range r.resolvedRoots() {
		if root.Trust == TrustUser {
			return true
		}
	}
	return false
}

// Authorable reports whether this deployment has a root locally authored
// presets go to.
func (r *Roster) Authorable() bool { return r.authorable() }

// DefaultID is the preset mounted when a caller names none: the user's
// stored choice when one exists, else the deployment default.
func (r *Roster) DefaultID() string {
	if r.options.DefaultOverride != nil {
		if chosen, ok := r.options.DefaultOverride(); ok {
			return chosen
		}
	}
	return r.config.Default
}

// List discovers the whole roster: every preset the configured roots
// supply, first-root-wins per id.
func (r *Roster) List() ([]AgentPreset, error) {
	return DiscoverPresets(r.resolvedRoots(), r.harnessBase())
}

// harnessBase is the directory a row's package name resolves against: the
// installed harness itself.
func (r *Roster) harnessBase() string {
	base, err := os.Getwd()
	if err != nil {
		return "."
	}
	return base
}

// resolve resolves one preset id against the roster.
func (r *Roster) resolve(id string) (AgentPreset, error) {
	presets, err := r.List()
	if err != nil {
		return AgentPreset{}, err
	}
	for _, preset := range presets {
		if preset.ID == id {
			return preset, nil
		}
	}
	available := make([]string, 0, len(presets))
	for _, preset := range presets {
		available = append(available, preset.ID)
	}
	return AgentPreset{}, &UnknownPresetError{PresetID: id, Available: available}
}

// resolveMountable resolves one preset and refuses a broken one up front:
// the refusal names the composition instead of failing deep inside a
// loader.
func (r *Roster) resolveMountable(id string) (AgentPreset, error) {
	preset, err := r.resolve(id)
	if err != nil {
		return AgentPreset{}, err
	}
	if preset.Broken != nil {
		return AgentPreset{}, &PresetMountError{PresetID: preset.ID, Reason: *preset.Broken}
	}
	return preset, nil
}

// Resolve resolves one preset id against the roster.
func (r *Roster) Resolve(id string) (AgentPreset, error) { return r.resolve(id) }

// ResolveMountable resolves one preset and refuses a broken one up front.
func (r *Roster) ResolveMountable(id string) (AgentPreset, error) { return r.resolveMountable(id) }

// Read reads one preset's composition text.
func (r *Roster) Read(id string) (string, error) {
	preset, err := r.resolve(id)
	if err != nil {
		return "", err
	}
	return ReadComposition(preset)
}

// AgentPresetDocument is one preset's composition text beside the row it
// belongs to.
type AgentPresetDocument struct {
	// AgentPreset is the preset the composition belongs to.
	AgentPreset string `json:"agentPreset"`
	// Trust of the root this preset was discovered under.
	Trust string `json:"trust"`
	// Content is the composition exactly as stored.
	Content string `json:"content"`
	// Name is the display name the preset published.
	Name *string `json:"name,omitempty"`
	// Description is one sentence on what this preset is for.
	Description *string `json:"description,omitempty"`
}

// ReadDocument reads one preset's composition text beside its row.
func (r *Roster) ReadDocument(id string) (AgentPresetDocument, error) {
	preset, err := r.resolve(id)
	if err != nil {
		return AgentPresetDocument{}, err
	}
	content, err := ReadComposition(preset)
	if err != nil {
		return AgentPresetDocument{}, err
	}
	return AgentPresetDocument{
		AgentPreset: preset.ID,
		Trust:       preset.Trust,
		Content:     content,
		Name:        preset.Name,
		Description: preset.Description,
	}, nil
}

// Copy creates a preset by copying an existing one, optionally renaming the
// copy's display name.
//
// A settled mount under this id can only be stale (its preset was deleted
// from disk outside Remove); the new preset must not inherit it. Every
// session already joined keeps the generation it runs on regardless: the
// standing table keys by preset id, and this id was vacant, so no live
// generation can be attached to it.
func (r *Roster) Copy(from string, id string, name *string) error {
	if !ValidPresetID(from) {
		return &InvalidPresetIDError{PresetID: from}
	}
	if !ValidPresetID(id) {
		return &InvalidPresetIDError{PresetID: id}
	}
	source, err := r.resolve(from)
	if err != nil {
		return err
	}
	presets, err := r.List()
	if err != nil {
		return err
	}
	for _, preset := range presets {
		if preset.ID == id {
			return &PresetExistsError{PresetID: id}
		}
	}
	_, err = CopyComposition(r.resolvedRoots(), source, id, name)
	if err != nil {
		return err
	}
	if r.options.InvalidateStanding != nil {
		r.options.InvalidateStanding(id)
	}
	return nil
}

// Remove deletes a locally authored preset. Sessions on the deleted preset
// keep their standing mount; only new sessions see the roster without it.
//
// Storing a default that does not exist YET is deliberate — the roster is a
// live directory, so a name absent now may exist by the time a session asks
// for it, and Resolve reports it then. A default this call just deleted is
// not that case: nothing will ever supply it again, and left in place every
// session created without an explicit pick would fail to start. Clearing it
// exposes the deployment's own default underneath, which is the layering.
func (r *Roster) Remove(id string) error {
	preset, err := r.resolve(id)
	if err != nil {
		return err
	}
	if err := DeleteComposition(r.resolvedRoots(), preset); err != nil {
		return err
	}
	if r.options.DefaultOverride == nil {
		return nil
	}
	chosen, ok := r.options.DefaultOverride()
	if !ok || chosen != id {
		return nil
	}
	if r.options.ClearDefaultOverride != nil {
		r.options.ClearDefaultOverride()
	}
	return nil
}

// CompositionStamp is the identity of one composition file state.
type CompositionStamp struct {
	// MtimeMs is the file's modification time in milliseconds.
	MtimeMs int64 `json:"mtimeMs"`
	// Size is the file's size in bytes.
	Size int64 `json:"size"`
}

// StampComposition stats one composition file's identity; every failure
// means the file offers no identity to compare.
func StampComposition(path string) *CompositionStamp {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &CompositionStamp{MtimeMs: info.ModTime().UnixMilli(), Size: info.Size()}
}

// SameStamp reports whether two stamps name the same file state.
func SameStamp(a, b CompositionStamp) bool {
	return a.MtimeMs == b.MtimeMs && a.Size == b.Size
}
