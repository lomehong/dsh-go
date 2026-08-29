// A per-agent persona as a composable row.
//
// The global persona is the prompt registry's own config, registered
// unconditionally — so this row is scope-only. Mounted inside an agent
// preset it shadows the deployment persona for that one session, exactly
// like the per-child persona a subagent installs; mounted at the root scope
// it collides with the registry's own registration and fails loud.
//
// That constraint is the reason the row exists. An agent preset cannot
// mount the prompt registry itself, so without a row of its own a preset
// could change an agent's tools but never its identity.
package preset

import (
	"dshgo/scope"
	"dshgo/systemprompt"
)

// PersonaConfig is the plugin config: the persona text this composition
// contributes.
type PersonaConfig struct {
	// Text is persona prose rendered as the `deployment:persona` section.
	// A template: complete `{{…}}` groups interpolate strictly against
	// registered prompt variables. Empty text drops the section at render,
	// matching the registry.
	Text string `json:"text"`
	// Complete makes this persona the complete system prompt, suppressing
	// every other section.
	Complete bool `json:"complete,omitempty"`
	// IncludeRuntimeContext suppresses dynamic runtime-context snapshots
	// for this persona's agent scope when explicitly false.
	IncludeRuntimeContext *bool `json:"includeRuntimeContext,omitempty"`
}

// ApplyPersona registers the persona section for the mounting scope and
// returns the effect's disposer.
//
// Registration is the Go face of `ctx.effect()`: the caller owns the
// returned disposer, and disposing unwinds the section exactly as the
// official effect would. A root-scope mount collides with the prompt
// registry's own persona registration and is rejected by the registry
// itself (duplicate section).
func ApplyPersona(sp *systemprompt.SystemPrompt, scopeKey scope.ScopeKey, config PersonaConfig) (func(), error) {
	section := systemprompt.PromptSection{
		Name:  systemprompt.PERSONA_SECTION,
		Order: systemprompt.PERSONA_ORDER,
		Text:  config.Text,
	}
	if config.Complete {
		section.Complete = true
	}
	dispose, err := sp.Section(scopeKey, section)
	if err != nil {
		return nil, err
	}
	if config.IncludeRuntimeContext != nil && !*config.IncludeRuntimeContext {
		suppress, err := sp.SuppressRuntimeContext(scopeKey)
		if err != nil {
			dispose()
			return nil, err
		}
		combined := dispose
		return func() {
			suppress()
			combined()
		}, nil
	}
	return dispose, nil
}
