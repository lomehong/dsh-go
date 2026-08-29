package boot

import (
	"fmt"

	"dshgo/cordis"
	"dshgo/cordis/loader"
)

// PluginSpec is one resolvable plugin: the source-facing view of the
// official plugin module contract (name / inject / provide / apply).
type PluginSpec struct {
	// Inject lists the services apply needs; the entry's own inject field
	// overrides it wholesale.
	Inject []string
	// Provide declares the services apply contributes (loader metadata).
	Provide []string
	// Apply mounts the plugin. Config is the entry's config value.
	Apply func(ctx *cordis.Context, config any) error
}

// PluginResolver resolves one entry's module specifier to its plugin. A miss
// fails loud: naming an unresolvable plugin is a misconfiguration.
type PluginResolver func(name string) (PluginSpec, error)

// App is one assembled plugin tree over a root context: group entries own
// child contexts, and shutdown disposes the tree to quiescence.
type App struct {
	root     *cordis.Context
	children []*cordis.Context
}

// Assemble mounts a composed entry list into ctx: disabled entries (and
// their subtrees) are skipped, group entries assemble their children into a
// child context, and every other entry resolves through the catalog and
// applies with its config once its injected services are present. The
// entry-level inject field overrides the plugin's own list wholesale.
//
// Failure wording matches the official updateError: failures are prefixed
// with the stage and the entry identity.
func Assemble(ctx *cordis.Context, entries []loader.Entry, resolver PluginResolver) (*App, error) {
	app := &App{root: ctx}
	if err := app.mount(ctx, entries, resolver, nil); err != nil {
		return nil, err
	}
	return app, nil
}

func (a *App) mount(ctx *cordis.Context, entries []loader.Entry, resolver PluginResolver, ancestors []loader.Entry) error {
	for _, entry := range entries {
		if entry.Group {
			// A group entry itself never runs and is always enabled, but
			// its disabled flag joins the ancestor chain: every child
			// checks the owning groups before activating.
			child := ctx.Child()
			a.children = append(a.children, child)
			children, _ := entry.Config.([]loader.Entry)
			if err := a.mount(child, children, resolver, append(ancestors, entry)); err != nil {
				return err
			}
			continue
		}
		disabled, err := loader.IsDisabled(entry)
		if err != nil {
			return fmt.Errorf("failed to apply loader entry %s (%s): %v", entry.ID, entry.Name, err)
		}
		if !disabled {
			for _, ancestor := range ancestors {
				ancestorDisabled, err := loader.IsDisabled(ancestor)
				if err != nil {
					return fmt.Errorf("failed to apply loader entry %s (%s): %v", entry.ID, entry.Name, err)
				}
				if ancestorDisabled {
					disabled = true
					break
				}
			}
		}
		if disabled {
			continue
		}
		spec, err := resolver(entry.Name)
		if err != nil {
			return fmt.Errorf("failed to import loader entry %s (%s): %v", entry.ID, entry.Name, err)
		}
		inject := spec.Inject
		if entry.Inject != nil {
			inject = entry.Inject
		}
		config := entry.Config
		applied := spec.Apply
		if err := ctx.Inject(inject, func(injected *cordis.Context) error {
			return applied(injected, config)
		}); err != nil {
			return fmt.Errorf("failed to apply loader entry %s (%s): %v", entry.ID, entry.Name, err)
		}
	}
	return nil
}

// Shutdown disposes the assembled tree: child contexts unwind in reverse
// mount order before the root, so descendants never outlive their owners.
func (a *App) Shutdown() error {
	var firstErr error
	for index := len(a.children) - 1; index >= 0; index-- {
		if err := a.children[index].Dispose(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	a.children = nil
	if err := a.root.Dispose(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
