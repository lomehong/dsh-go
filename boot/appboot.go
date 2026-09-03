// Profile-driven top-level composition: the app-boot seam that loads a
// profile, composes its patch layers into the effective entry list, and
// mounts everything through the plugin catalog. This is the programmatic
// equivalent of the official launcher's compose-and-mount front half; the
// watch/reload back half lands with the launcher round.
package boot

import (
	"sync"

	"dshgo/cordis"
	"dshgo/cordis/loader"
)

// Root exposes the composition's root context for service resolution after
// Assemble returns.
func (a *App) Root() *cordis.Context {
	return a.root
}

// AssembleProfile composes one profile and mounts it through the catalog:
// profile bundle layers in manifest order, then the profile's own patch
// layer, over an empty root. The returned warnings carry every
// skipped-patch diagnostic the composition produced.
func AssembleProfile(binName, name, installAnchor, home string, deps CatalogDeps) (*App, []string, error) {
	profile, err := LoadProfile(binName, name, installAnchor, home, true)
	if err != nil {
		return nil, nil, err
	}
	return assembleLoaded(profile, deps, nil)
}

// AssembleProfileWithCmdline composes one profile like AssembleProfile but
// provides the launcher's inner arguments as the `cmdlineArgs` service
// before any entry mounts (official dsh-cmdline provideCmdline): an app
// plugin injects cmdlineArgs and parses its own flag family.
func AssembleProfileWithCmdline(binName, name, installAnchor, home string, args []string, deps CatalogDeps) (*App, []string, error) {
	profile, err := LoadProfile(binName, name, installAnchor, home, true)
	if err != nil {
		return nil, nil, err
	}
	return assembleLoaded(profile, deps, func(ctx *cordis.Context) {
		ProvideCmdline(ctx, args)
	})
}

// composedEntriesSnapshot is the last composed profile's entry list — the
// plugin-inventory projection source (official dsh-host-plugin-inventory
// reads the live Loader table; the Go host's equivalent is the composed
// profile rows). Guarded by its mutex; replaced wholesale per composition.
var (
	composedEntriesMu       sync.Mutex
	composedEntriesSnapshot []loader.Entry
)

// ComposedEntries returns a copy of the last composed profile's rows.
func ComposedEntries() []loader.Entry {
	composedEntriesMu.Lock()
	defer composedEntriesMu.Unlock()
	out := make([]loader.Entry, len(composedEntriesSnapshot))
	copy(out, composedEntriesSnapshot)
	return out
}

// assembleLoaded shares the compose+mount tail between the cmdline and
// plain entry points; the preMount hook runs on the root before Assemble.
func assembleLoaded(profile *Profile, deps CatalogDeps, preMount func(*cordis.Context)) (*App, []string, error) {
	layers := make([][]loader.Patch, 0, len(profile.Layers)+1)
	for _, layer := range profile.Layers {
		layers = append(layers, layer.Patches)
	}
	layers = append(layers, profile.Patches)
	entries, warnings := ComposeEntries(layers...)
	composedEntriesMu.Lock()
	composedEntriesSnapshot = entries
	composedEntriesMu.Unlock()
	root := cordis.NewRoot(deps.Logger)
	if preMount != nil {
		preMount(root)
	}
	app, err := Assemble(root, entries, NewCatalog(deps))
	if err != nil {
		return nil, warnings, err
	}
	return app, warnings, nil
}

// ProvideCmdline provides the launcher's inner arguments as the
// `cmdlineArgs` service (official dsh-cmdline provideCmdline). The args
// snapshot is frozen; absent args provide an empty list.
func ProvideCmdline(ctx *cordis.Context, args []string) {
	snapshot := append([]string(nil), args...)
	ctx.Provide("cmdlineArgs", snapshot)
}
