package subagent

import (
	"encoding/json"
	"errors"
	"fmt"

	"dshgo/session"
)

// EventSubagentDescriptor is the durable identity and lifecycle mode of a
// session-backed subagent child, appended once by the establishing provider
// inside the child's initial turn, before its first request. Log-only: it
// carries no surface op, never enters model history, and survives
// compaction.
const EventSubagentDescriptor = "subagent/descriptor"

func init() {
	if err := session.RegisterEventType(EventSubagentDescriptor); err != nil {
		panic(fmt.Sprintf("subagent: %v", err))
	}
}

// SubagentDescriptorVersion is the current descriptor format version,
// stamped into every appended `subagent/descriptor` event and required
// verbatim by FoldSubagentDescriptor. Supporting another composition input
// is a deliberate version change, never an implicit extra field.
const SubagentDescriptorVersion = 3

// Subagent modes.
const (
	// ModeOneShot: a session-backed subagent that cannot be cold-resumed
	// after its run.
	ModeOneShot = "one-shot"
	// ModeContinuable: a session-backed subagent whose declared composition
	// supports cold resume.
	ModeContinuable = "continuable"
)

// ToolRestriction is a child tool scoping record: the named tools vanish
// from the child's prompt AND refuse to execute. Declare Allow and/or Deny.
// The tools-side application lands with the subagent service round; the
// descriptor owns the durable snapshot today.
type ToolRestriction struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// SubagentDescriptorData is the durable subagent identity and optional
// continuation composition. The descriptor deliberately snapshots explicit
// fields rather than the merge-extensible AgentOptions object: an unrelated
// extension value cannot make continuation fail merely because it is not
// JSON, and later composition inputs require a deliberate
// SubagentDescriptorVersion change. It omits SubagentDepth — cold resume
// trusts the persisted header's delegationDepth as the monotone floor — and
// the output schema and per-activation knobs, which budget one activation
// rather than durable child composition.
type SubagentDescriptorData struct {
	// Version is the descriptor format version.
	Version int64 `json:"version"`
	// Mode is whether the child is a terminal one-shot run or a resumable
	// conversation.
	Mode string `json:"mode"`
	// Provider is the subagents provider name that established the child.
	Provider string `json:"provider"`
	// Label is the initial delegation's short description kept as the
	// child's durable creation label, so enumeration can identify the
	// conversation without replaying parent tool results or exposing the
	// child prompt. Optional for one-shot children, required for
	// continuable ones.
	Label *string `json:"label,omitempty"`
	// AgentProvider is the resolved child agentOptions.provider, when one
	// was declared.
	AgentProvider *string `json:"agentProvider,omitempty"`
	// AgentModel is the resolved child agentOptions.model, when one was
	// declared.
	AgentModel *string `json:"agentModel,omitempty"`
	// AgentReasoningEffort is the resolved child agentOptions.reasoningEffort,
	// when one was declared.
	AgentReasoningEffort *string `json:"agentReasoningEffort,omitempty"`
	// Persona is the per-child persona that shadows the deployment persona
	// on resume.
	Persona *string `json:"persona,omitempty"`
	// ToolFilter is the child tool scoping reapplied on resume.
	ToolFilter *ToolRestriction `json:"toolFilter,omitempty"`
}

// DescriptorInput is the caller-collected composition for
// SnapshotSubagentDescriptor. Label is optional for one-shot children and
// required for continuable ones; the agent route, persona, and tool filter
// apply only to continuable children (they are the resumable composition).
type DescriptorInput struct {
	Mode     string
	Provider string
	Label    string
	HasLabel bool
	// Continuable composition.
	AgentProvider        string
	AgentModel           string
	AgentReasoningEffort string
	Persona              string
	ToolFilter           *ToolRestriction
}

// oneShotDescriptorKeys and continuableDescriptorKeys are the complete
// declared schemas; any other persisted field fails the fold.
var oneShotDescriptorKeys = map[string]bool{
	"version": true, "mode": true, "provider": true, "label": true,
}

var continuableDescriptorKeys = map[string]bool{
	"version": true, "mode": true, "provider": true, "label": true,
	"agentProvider": true, "agentModel": true, "agentReasoningEffort": true,
	"persona": true, "toolFilter": true,
}

var toolFilterKeys = map[string]bool{"allow": true, "deny": true}

// SnapshotSubagentDescriptor validates and detaches descriptor inputs into
// the durable payload, before any Task or provider work begins — the same
// detached lossless-JSON boundary the session log itself enforces, applied
// early so a synchronous validation failure rejects the tool call without
// creating a Task.
func SnapshotSubagentDescriptor(input DescriptorInput) (SubagentDescriptorData, error) {
	if input.Mode != ModeOneShot && input.Mode != ModeContinuable {
		return SubagentDescriptorData{}, fmt.Errorf("subagent descriptor mode must be %q or %q", ModeOneShot, ModeContinuable)
	}
	if input.Provider == "" {
		return SubagentDescriptorData{}, errors.New("subagent descriptor provider is required")
	}
	candidate := SubagentDescriptorData{Version: SubagentDescriptorVersion, Mode: input.Mode, Provider: input.Provider}
	if input.HasLabel && input.Label != "" {
		label := input.Label
		candidate.Label = &label
	}
	if input.Mode == ModeContinuable {
		if input.Label == "" {
			return SubagentDescriptorData{}, errors.New("continuable subagent descriptor requires a label")
		}
		label := input.Label
		candidate.Label = &label
		if input.AgentProvider != "" {
			value := input.AgentProvider
			candidate.AgentProvider = &value
		}
		if input.AgentModel != "" {
			value := input.AgentModel
			candidate.AgentModel = &value
		}
		if input.AgentReasoningEffort != "" {
			value := input.AgentReasoningEffort
			candidate.AgentReasoningEffort = &value
		}
		if input.Persona != "" {
			value := input.Persona
			candidate.Persona = &value
		}
		if input.ToolFilter != nil {
			filter, err := snapshotToolFilter(*input.ToolFilter)
			if err != nil {
				return SubagentDescriptorData{}, err
			}
			candidate.ToolFilter = filter
		}
	} else if input.AgentProvider != "" || input.AgentModel != "" || input.AgentReasoningEffort != "" || input.Persona != "" || input.ToolFilter != nil {
		return SubagentDescriptorData{}, errors.New("one-shot subagent descriptor accepts no continuable composition fields")
	}
	// Detach through the same lossless-JSON round trip the log enforces.
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return SubagentDescriptorData{}, errors.New("subagent descriptor is not losslessly JSON-serializable")
	}
	var detached SubagentDescriptorData
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return SubagentDescriptorData{}, errors.New("subagent descriptor is not losslessly JSON-serializable")
	}
	return detached, nil
}

// snapshotToolFilter validates and detaches one tool restriction.
func snapshotToolFilter(filter ToolRestriction) (*ToolRestriction, error) {
	if len(filter.Allow) == 0 && len(filter.Deny) == 0 {
		return nil, errors.New("subagent descriptor toolFilter must declare allow and/or deny")
	}
	encoded, err := json.Marshal(filter)
	if err != nil {
		return nil, errors.New("subagent descriptor toolFilter is not losslessly JSON-serializable")
	}
	var detached ToolRestriction
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return nil, errors.New("subagent descriptor toolFilter is not losslessly JSON-serializable")
	}
	return &detached, nil
}

// FoldSubagentDescriptor folds a persisted child log to its supported
// descriptor. The first `subagent/descriptor` event is authoritative — the
// establishing provider appends exactly one, so a later same-type event
// cannot rewrite the declared composition. The descriptor pointer is nil
// when the log has none or its version is not
// SubagentDescriptorVersion (the child cannot be classified by this
// runtime). A current-version payload that does not match its complete
// declared schema is an error.
func FoldSubagentDescriptor(events []session.Event) (*SubagentDescriptorData, error) {
	for _, event := range events {
		if event.Type != EventSubagentDescriptor {
			continue
		}
		return parseSubagentDescriptor(event.Data)
	}
	return nil, nil
}

// parseSubagentDescriptor validates one persisted descriptor payload for the
// current runtime, with the verbatim failure texts.
func parseSubagentDescriptor(data []byte) (*SubagentDescriptorData, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, errors.New("persisted subagent descriptor payload must be an object")
	}
	rawVersion, ok := record["version"]
	if !ok {
		return nil, errors.New("persisted subagent descriptor version must be a number")
	}
	var version float64
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return nil, errors.New("persisted subagent descriptor version must be a number")
	}
	if int64(version) != SubagentDescriptorVersion {
		return nil, nil
	}
	rawMode, ok := record["mode"]
	if !ok {
		return nil, errors.New(`persisted subagent descriptor mode must be "one-shot" or "continuable"`)
	}
	var mode string
	if err := json.Unmarshal(rawMode, &mode); err != nil || (mode != ModeOneShot && mode != ModeContinuable) {
		return nil, errors.New(`persisted subagent descriptor mode must be "one-shot" or "continuable"`)
	}
	keys := oneShotDescriptorKeys
	if mode == ModeContinuable {
		keys = continuableDescriptorKeys
	}
	for key := range record {
		if !keys[key] {
			return nil, fmt.Errorf("persisted subagent descriptor payload has unknown field %q", key)
		}
	}
	rawProvider, ok := record["provider"]
	if !ok {
		return nil, errors.New("persisted subagent descriptor provider must be a string")
	}
	var provider string
	if err := json.Unmarshal(rawProvider, &provider); err != nil {
		return nil, errors.New("persisted subagent descriptor provider must be a string")
	}
	descriptor := SubagentDescriptorData{Version: SubagentDescriptorVersion, Mode: mode, Provider: provider}
	label, err := optionalDescriptorString(record, "label", mode == ModeContinuable)
	if err != nil {
		return nil, err
	}
	if mode == ModeContinuable {
		if label == nil {
			return nil, errors.New("persisted subagent descriptor label must be a string")
		}
		descriptor.Label = label
		if err := foldContinuableFields(record, &descriptor); err != nil {
			return nil, err
		}
		return &descriptor, nil
	}
	descriptor.Label = label
	return &descriptor, nil
}

// foldContinuableFields reads the continuable composition fields.
func foldContinuableFields(record map[string]json.RawMessage, descriptor *SubagentDescriptorData) error {
	for key, target := range map[string]**string{
		"agentProvider":        &descriptor.AgentProvider,
		"agentModel":           &descriptor.AgentModel,
		"agentReasoningEffort": &descriptor.AgentReasoningEffort,
		"persona":              &descriptor.Persona,
	} {
		value, err := optionalDescriptorString(record, key, true)
		if err != nil {
			return err
		}
		*target = value
	}
	raw, present := record["toolFilter"]
	if !present {
		return nil
	}
	filter, err := parseToolFilter(raw)
	if err != nil {
		return err
	}
	descriptor.ToolFilter = filter
	return nil
}

// optionalDescriptorString reads one optional string field.
func optionalDescriptorString(record map[string]json.RawMessage, key string, required bool) (*string, error) {
	raw, present := record[key]
	if !present {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		if required && key == "label" {
			return nil, errors.New("persisted subagent descriptor label must be a string")
		}
		return nil, fmt.Errorf("persisted subagent descriptor %s must be a string", key)
	}
	return &value, nil
}

// parseToolFilter validates and reconstructs one persisted tool restriction.
func parseToolFilter(raw json.RawMessage) (*ToolRestriction, error) {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, errors.New("persisted subagent descriptor toolFilter must be an object")
	}
	for key := range record {
		if !toolFilterKeys[key] {
			return nil, fmt.Errorf("persisted subagent descriptor toolFilter has unknown field %q", key)
		}
	}
	filter := ToolRestriction{}
	for _, key := range []string{"allow", "deny"} {
		value, err := optionalStringArray(record, key)
		if err != nil {
			return nil, err
		}
		if key == "allow" && value != nil {
			filter.Allow = value
		}
		if key == "deny" && value != nil {
			filter.Deny = value
		}
	}
	if filter.Allow == nil && filter.Deny == nil {
		return nil, errors.New("persisted subagent descriptor toolFilter must declare allow and/or deny")
	}
	return &filter, nil
}

// optionalStringArray reads one optional string-array field.
func optionalStringArray(record map[string]json.RawMessage, key string) ([]string, error) {
	raw, present := record[key]
	if !present {
		return nil, nil
	}
	var items []string
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("persisted subagent descriptor toolFilter.%s must be an array of strings", key)
	}
	return items, nil
}
