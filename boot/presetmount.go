// The standing-assembly seam: preset compositions enter the catalog through
// the same Assemble path the host profile uses, rooted at a standing
// context instead of the process root.
//
// Go adaptations of the official mountPreset machinery:
//   - Bare specifiers resolve through the SAME catalog as the host (the
//     official resolves bare names against the harness base and relative
//     names against the preset directory; Go plugins are catalog names, so
//     one resolver serves both).
//   - A row naming a plugin that did not declare itself mountable fails
//     loud at assembly. The official leakedServices audit catches a subtree
//     that published into the root realm after the fact; Go rejects the
//     registration up front instead, which is strictly earlier.
package boot

import (
	"fmt"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/cordis/loader"
	"dshgo/preset"
	"dshgo/scope"
)

// AgentScopeOf is the preset.ScopeOfAgent seam: the agent factory publishes
// each agent on its own context, so the agent IS the scope carrier. nil
// means the context carries no agent, which mount reads as unscoped.
func AgentScopeOf(ctx *cordis.Context) scope.ScopeKey {
	if built, ok := agent.ContextService.From(ctx); ok && built != nil {
		return built.Scope
	}
	return nil
}

// mountableResolver rejects every catalog plugin that did not declare
// itself mountable: a preset composition may only build from plugins whose
// registrations file into the standing scope layer.
func mountableResolver(inner PluginResolver) PluginResolver {
	return func(name string) (PluginSpec, error) {
		spec, err := inner(name)
		if err != nil {
			return spec, err
		}
		if !spec.Mountable {
			return PluginSpec{}, fmt.Errorf("plugin %q is not mountable into preset compositions", name)
		}
		return spec, nil
	}
}

// StandingAssembler builds the preset.StandingAssembler over the catalog:
// composition text to entries, entries applied into the standing context
// through the mountable view of the host's own resolver.
func StandingAssembler(deps CatalogDeps) preset.StandingAssembler {
	resolver := mountableResolver(NewCatalog(deps))
	return func(standingCtx *cordis.Context, p preset.AgentPreset) (preset.StandingTree, error) {
		content, err := preset.ReadComposition(p)
		if err != nil {
			return nil, err
		}
		entries, err := loader.DecodeEntryList([]byte(content))
		if err != nil {
			return nil, fmt.Errorf("composition %q is not a valid entry list: %v", p.Path, err)
		}
		app, err := Assemble(standingCtx, entries, resolver)
		if err != nil {
			return nil, err
		}
		return app, nil
	}
}

// NewPresetMounts wires the standing machinery over the roster at the host
// composition context.
func NewPresetMounts(host *cordis.Context, roster *preset.Roster, deps CatalogDeps) (*preset.Mounts, error) {
	return preset.NewMounts(host, roster, preset.MountOptions{
		Assemble: StandingAssembler(deps),
		ScopeOf:  AgentScopeOf,
	})
}
