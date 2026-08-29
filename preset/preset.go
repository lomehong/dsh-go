// Agent-preset vocabulary shared by discovery, authoring, and consumers.
//
// Ported from packages/preset/agent-presets/src/{preset,types}.ts: the trust
// model, the id containment rule, the roster row, and the three refusal
// errors. Model- and user-visible strings are verbatim.
package preset

import (
	"errors"
	"fmt"
	"regexp"
)

// Trust kinds for a preset's origin.
const (
	// TrustSystem marks a preset that ships with the deployment.
	TrustSystem = "system"
	// TrustUser marks a preset authored locally, by a person or by an
	// agent, carrying the same trust as shell access.
	TrustUser = "user"
)

// presetIDPattern is the id rule a preset directory may use.
//
// The id becomes a path segment, so this is a containment boundary rather
// than a style rule: `..`, a separator, or an absolute-looking name would
// place the composition outside the root the deployment authorised.
// Discovery shares it: a directory whose name no copy could ever claim is
// not a preset slot. Rendered verbatim (including the JS literal slashes)
// in InvalidPresetIDError's message.
const presetIDPattern = "/^[a-z0-9][a-z0-9-]*$/"

var presetIDRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidPresetID reports whether id may name a preset directory.
func ValidPresetID(id string) bool { return presetIDRegexp.MatchString(id) }

// AgentPreset is one preset directory that carries a mountable agent
// composition.
type AgentPreset struct {
	// ID is the stable identifier; the preset directory's name.
	ID string `json:"id"`
	// Trust was recorded from the root this preset was discovered under.
	Trust string `json:"trust"`
	// Path is the absolute path of the preset's agent composition file.
	Path string `json:"path"`
	// Name is the display name from the preset's own metadata; nil falls
	// back to ID.
	Name *string `json:"name,omitempty"`
	// Description is one sentence on what this preset is for, when it
	// published one.
	Description *string `json:"description,omitempty"`
	// Order is the declared position within its group; nil sorts after
	// those that declare one.
	Order *float64 `json:"order,omitempty"`
	// Broken is why this preset cannot compose a session, nil when it can.
	// A broken preset stays on the roster — hiding it would leave its
	// directory blocking the id with nothing to see or delete — but every
	// mounting path refuses it up front with this reason.
	Broken *string `json:"broken,omitempty"`
}

// PresetRoot is one directory scanned for preset subdirectories.
type PresetRoot struct {
	// Path is the directory holding one subdirectory per preset; a leading
	// `~` expands.
	Path string `json:"path"`
	// Trust is recorded on every preset discovered under this root.
	Trust string `json:"trust"`
}

// Config is the plugin config: which preset is the default and where
// presets live.
type Config struct {
	// Default is the preset mounted when a caller names none. Missing at
	// mount time fails loud.
	Default string `json:"default"`
	// Roots are scanned in precedence order; an earlier root wins a
	// duplicate id.
	Roots []PresetRoot `json:"roots"`
	// IncludeShippedRoot prepends the package's bundled shipped presets as
	// a `system` root, before every configured root, so the shipped set
	// always mounts and wins a duplicate id.
	IncludeShippedRoot bool `json:"includeShippedRoot"`
	// IncludeUserRoot appends the harness home's user preset dir as a
	// `user` root, after every configured root.
	IncludeUserRoot bool `json:"includeUserRoot"`
}

// UnknownPresetError: no configured root supplies the requested preset.
//
// Separate from a mount failure because the two mean different things to a
// caller: an unknown id is a bad request, while an unusable composition is
// a broken preset the deployment must fix.
type UnknownPresetError struct {
	// PresetID is the id that was requested.
	PresetID string
	// Available are the ids the roster does supply, for the caller to
	// offer instead.
	Available []string
}

func (e *UnknownPresetError) Error() string {
	joined := ""
	for index, id := range e.Available {
		if index > 0 {
			joined += ", "
		}
		joined += id
	}
	if joined == "" {
		joined = "none"
	}
	return fmt.Sprintf("agent-presets: preset %q not found (available: %s)", e.PresetID, joined)
}

// PresetLockedError: the session's composition is fixed — its conversation
// has started, so its history was produced under the preset it runs and
// swapping the composition would leave logged tool calls the new one
// cannot make.
type PresetLockedError struct {
	// SessionID is the session whose composition is already fixed.
	SessionID string
	// PresetID is the preset that was refused.
	PresetID string
}

func (e *PresetLockedError) Error() string {
	return fmt.Sprintf("agent-presets: session %q has already started; its agent preset is fixed", e.SessionID)
}

// PresetMountError: a preset exists but its composition cannot be installed.
type PresetMountError struct {
	// PresetID is the preset whose composition failed.
	PresetID string
	// Reason is why it failed, without this package's own message prefix.
	Reason string
}

func (e *PresetMountError) Error() string {
	return fmt.Sprintf("agent-presets: preset %q failed to mount: %s", e.PresetID, e.Reason)
}

// Stable remote failure codes for preset refusals, mirroring
// AgentPresetErrorDetailsMap.
const (
	// CodeBadRequest: a required preset id is empty.
	CodeBadRequest = "bad-request"
	// CodePresetNotFound: no configured root supplies the requested id.
	CodePresetNotFound = "agent-preset-not-found"
	// CodePresetInvalid: the id is unusable, already taken, or its
	// composition cannot be installed.
	CodePresetInvalid = "agent-preset-invalid"
	// CodePresetReadOnly: the preset ships with the deployment and is not
	// the user's to change.
	CodePresetReadOnly = "agent-preset-read-only"
	// CodePresetLocked: the session's conversation has started, so its
	// composition is fixed.
	CodePresetLocked = "agent-preset-locked"
	// CodeInternal: the preset operation failed without a
	// caller-actionable classification.
	CodeInternal = "internal"
)

// PresetFailure is one stable preset refusal as a client reads it.
type PresetFailure struct {
	// Code is one of the stable Code* values.
	Code string `json:"code"`
	// Message is the human-readable refusal.
	Message string `json:"message"`
	// Details carry the code-specific payload.
	Details map[string]any `json:"details"`
}

// ClassifyFailure maps one preset rejection to its stable code, message,
// and details; nil when the error carries no preset classification and the
// caller's operation-specific fallback applies.
func ClassifyFailure(err error, agentPreset string) *PresetFailure {
	var unknown *UnknownPresetError
	if asPresetError(err, &unknown) {
		return &PresetFailure{
			Code:    CodePresetNotFound,
			Message: unknown.Error(),
			Details: map[string]any{"agentPreset": unknown.PresetID, "available": append([]string(nil), unknown.Available...)},
		}
	}
	var mount *PresetMountError
	if asPresetError(err, &mount) {
		return &PresetFailure{
			Code:    CodePresetInvalid,
			Message: mount.Error(),
			Details: map[string]any{"agentPreset": mount.PresetID, "reason": mount.Reason},
		}
	}
	var invalid *InvalidPresetIDError
	if asPresetError(err, &invalid) {
		return &PresetFailure{
			Code:    CodePresetInvalid,
			Message: invalid.Error(),
			Details: map[string]any{"agentPreset": invalid.PresetID, "reason": invalid.Error()},
		}
	}
	var exists *PresetExistsError
	if asPresetError(err, &exists) {
		return &PresetFailure{
			Code:    CodePresetInvalid,
			Message: exists.Error(),
			Details: map[string]any{"agentPreset": exists.PresetID, "reason": exists.Error()},
		}
	}
	var readOnly *PresetNotWritableError
	if asPresetError(err, &readOnly) {
		return &PresetFailure{
			Code:    CodePresetReadOnly,
			Message: readOnly.Error(),
			Details: map[string]any{"agentPreset": agentPreset, "reason": readOnly.Error()},
		}
	}
	var locked *PresetLockedError
	if asPresetError(err, &locked) {
		return &PresetFailure{
			Code:    CodePresetLocked,
			Message: fmt.Sprintf("session %q has already started; its agent preset is fixed", locked.SessionID),
			Details: map[string]any{"sessionId": locked.SessionID, "agentPreset": locked.PresetID},
		}
	}
	return nil
}

// asPresetError is errors.As restricted to the preset error family (kept
// local so the vocabulary file reads as one table).
func asPresetError(err error, target any) bool { return errors.As(err, target) }
