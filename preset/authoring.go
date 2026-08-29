// Copying, reading, and deleting locally authored presets.
//
// Authoring is confined to a `user` root: the shipped `system` set is part
// of the deployment, and letting a client rewrite it would turn "reset to a
// known preset" into something the same caller could have broken first.
//
// The only authoring write is a whole-directory copy of an existing preset.
// No caller supplies composition text: the inputs are ids the host resolves
// against its own roots plus an optional display name, so authoring grants
// no capability the copied preset did not already carry.
package preset

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"dshgo/homepaths"
)

// InvalidPresetIDError: a preset id that cannot be used as a directory name
// under a root.
type InvalidPresetIDError struct {
	// PresetID is the rejected id.
	PresetID string
}

func (e *InvalidPresetIDError) Error() string {
	return fmt.Sprintf("agent-presets: preset id %q must match %s — the id is a directory name, so anything else could escape the preset root", e.PresetID, presetIDPattern)
}

// PresetExistsError: a copy target that is already occupied — a copy never
// overwrites.
type PresetExistsError struct {
	// PresetID is the id that is already taken.
	PresetID string
}

func (e *PresetExistsError) Error() string {
	return fmt.Sprintf("agent-presets: preset %q already exists — a copy never overwrites; delete the existing preset first or choose another id", e.PresetID)
}

// PresetNotWritableError: authoring was attempted where the deployment
// allows none.
type PresetNotWritableError struct {
	// PresetID is what the caller tried to change, for the diagnostic.
	PresetID string
	// Reason is why the write was refused.
	Reason string
}

func (e *PresetNotWritableError) Error() string {
	return fmt.Sprintf("agent-presets: preset %q cannot be written: %s", e.PresetID, e.Reason)
}

// WritableRoot returns the root locally authored presets are written to:
// the absolute path of the first `user` root. An error when the deployment
// configured no writable root.
func WritableRoot(roots []PresetRoot) (string, error) {
	for _, candidate := range roots {
		if candidate.Trust == TrustUser {
			return filepath.Abs(homepaths.ExpandHomePath(candidate.Path))
		}
	}
	return "", &PresetNotWritableError{PresetID: "", Reason: "this deployment configures no user-writable preset root"}
}

// ReadComposition reads one preset's composition text exactly as stored.
func ReadComposition(preset AgentPreset) (string, error) {
	raw, err := os.ReadFile(preset.Path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// occupied reports whether anything occupies the path; every stat failure
// means the same thing here: nothing usable occupies the path, so the copy
// may claim it. The copy's own creation backstops races.
func occupied(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// tightenModes re-tightens a copied tree to owner-only. A shipped preset is
// world-readable in its install and a recursive copy preserves that; the
// copy carries the same weight as the settings document beside it, so
// group/other access is stripped. A file's owner-execute bit survives — a
// preset may ship runnable helpers.
func tightenModes(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		target := filepath.Join(dir, entry.Name())
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := tightenModes(target); err != nil {
				return err
			}
			continue
		}
		mode := info.Mode()
		fileMode := fs.FileMode(0o600)
		if mode&0o100 != 0 {
			fileMode = 0o700
		}
		if err := os.Chmod(target, fileMode); err != nil {
			return err
		}
	}
	return nil
}

// CopyComposition creates a preset by copying an existing one's whole
// directory, and returns the absolute path of the new preset directory.
//
// The copy carries everything the source directory holds — composition,
// metadata, skill directories, assets — because a preset is its directory,
// not one file. Symlinks are dereferenced so the copy is self-contained
// rather than a set of links back into the install it was copied from.
//
// The copied metadata is then rewritten: the source's description is kept
// (the file is the author's to edit afterwards), but its name and roster
// `order` are not — a copy presenting itself identically to its source, or
// sorted into the shipped set's declared order, would make the roster stop
// distinguishing them. With no name given and no description to keep, the
// file is removed so the copy publishes nothing rather than a blank.
//
// An error when the id is unusable or already occupied on disk, or the
// deployment configures no writable root. A failed copy leaves nothing.
func CopyComposition(roots []PresetRoot, source AgentPreset, id string, name *string) (string, error) {
	if !ValidPresetID(id) {
		return "", &InvalidPresetIDError{PresetID: id}
	}
	root, err := WritableRoot(roots)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, id)
	// The roster check upstream only sees discovered presets; a directory
	// with no composition file still occupies the name and deserves a
	// readable refusal rather than a filesystem error code.
	if occupied(dir) {
		return "", &PresetExistsError{PresetID: id}
	}
	if err := copyTree(filepath.Dir(source.Path), dir); err != nil {
		// A half-copied directory would be invisible to discovery at best
		// and a mountable-but-incomplete preset at worst; a failed copy
		// leaves nothing.
		os.RemoveAll(dir)
		return "", err
	}
	if err := tightenModes(dir); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	description := source.Description
	rendered, hasContent := RenderPresetMetadata(PresetMetadata{Name: name, Description: description})
	metadataPath := filepath.Join(dir, MetadataFile)
	if !hasContent {
		if err := os.Remove(metadataPath); err != nil && !os.IsNotExist(err) {
			os.RemoveAll(dir)
			return "", err
		}
	} else if err := os.WriteFile(metadataPath, []byte(rendered), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// copyTree copies one directory whole, dereferencing symlinks: every entry
// is read through its target, so the copy never references the source.
func copyTree(source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("agent-presets: preset source %s is not a directory", source)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		sourceEntry := filepath.Join(source, entry.Name())
		targetEntry := filepath.Join(target, entry.Name())
		entryInfo, err := os.Stat(sourceEntry) // dereferences symlinks
		if err != nil {
			return err
		}
		if entryInfo.IsDir() {
			if err := copyTree(sourceEntry, targetEntry); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(sourceEntry, targetEntry, entryInfo.Mode()); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, target string, mode fs.FileMode) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(target, raw, mode.Perm())
}

// DeleteComposition deletes a locally authored preset.
//
// A shipped preset is refused: it belongs to the deployment. A preset a
// live session mounted is NOT refused — the composition was read at
// creation and is never re-read, so that session keeps running exactly as
// it was.
func DeleteComposition(roots []PresetRoot, preset AgentPreset) error {
	if preset.Trust != TrustUser {
		return &PresetNotWritableError{PresetID: preset.ID, Reason: "it ships with the deployment"}
	}
	root, err := WritableRoot(roots)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, preset.ID)
	// Belt and braces over the id pattern: the resolved directory must
	// still be the one the writable root owns, whatever discovery reported.
	if !filepath.IsAbs(preset.Path) || !strings.HasPrefix(preset.Path, dir) {
		return &PresetNotWritableError{PresetID: preset.ID, Reason: "it does not live under the writable preset root"}
	}
	return os.RemoveAll(dir)
}
