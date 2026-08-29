// The plugin catalog: official composition entry names resolve to Go plugin
// specs. Entry names are the official npm specifiers the bundled presets
// write, verbatim (see _dsh-official/packages/bundle/base/cordis.patch.yml
// for the authoritative 86-name set); a name without a Go implementation is
// a loud miss — "module not found" — never a silently skipped row, matching
// the official unresolvable-specifier behavior.
package boot

import (
	"fmt"
	"path/filepath"

	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/credentials"
	"dshgo/host/webserver"
	"dshgo/settings"
	"dshgo/settings/file"
	"dshgo/tools"
)

// Service names plugins publish and consume through ctx inject lists.
const (
	ServiceTools      = "tools"
	ServiceCommands   = "commands"
	ServiceSettings   = "settings"
	ServiceWebServer  = "webServer"
	ServiceCredential = "credentials"
)

// CatalogDeps carries the ambient composition inputs plugins share: the
// process logger and the resolved profile home directory.
type CatalogDeps struct {
	Logger cordis.Logger
	Home   string
}

// pluginBuilder builds one plugin's spec; builders close over the ambient
// deps instead of globals so a test home or logger plugs in cleanly.
type pluginBuilder func(deps CatalogDeps) PluginSpec

// NewCatalog builds the PluginResolver for Assemble.
func NewCatalog(deps CatalogDeps) PluginResolver {
	builtins := make(map[string]PluginSpec, len(builders))
	for name, build := range builders {
		builtins[name] = build(deps)
	}
	return func(name string) (PluginSpec, error) {
		spec, ok := builtins[name]
		if !ok {
			return PluginSpec{}, fmt.Errorf("module not found: %s", name)
		}
		return spec, nil
	}
}

var builders = map[string]pluginBuilder{
	// The shared tools runtime: tool definitions register into it at their
	// plugin's Apply time.
	"@deepseek-ai/dsh-tools": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceTools},
			Apply: func(ctx *cordis.Context, config any) error {
				runtime, err := tools.NewToolRuntime(deps.Logger, tools.Config{})
				if err != nil {
					return err
				}
				ctx.Provide(ServiceTools, runtime)
				return nil
			},
		}
	},

	// The commands runtime: /commands register into it likewise.
	"@deepseek-ai/dsh-commands": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceCommands},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceCommands, commands.NewCommandRuntime(deps.Logger))
				return nil
			},
		}
	},

	// The user-settings document store: settings.yaml under the profile
	// home, hot-reloaded. Config overrides the file path.
	"@deepseek-ai/dsh-settings-file": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSettings},
			Apply: func(ctx *cordis.Context, config any) error {
				path := filepath.Join(deps.Home, "settings.yaml")
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["path"].(string); ok && raw != "" {
						if filepath.IsAbs(raw) {
							path = raw
						} else {
							path = filepath.Join(deps.Home, raw)
						}
					}
				}
				store := settings.NewStore(deps.Logger)
				f, err := file.Open(path, store, deps.Logger)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSettings, store)
				if err := ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() { _ = f.Close() }), nil
				}); err != nil {
					return err
				}
				return nil
			},
		}
	},

	// The local credentials provider: process-memory seed today; the durable
	// source lands with the credentials store round.
	"@deepseek-ai/dsh-credentials-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceCredential},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceCredential, credentials.NewMemoryProvider(nil))
				return nil
			},
		}
	},

	// The web server: route registry served over HTTP.
	"@deepseek-ai/dsh-web": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceWebServer},
			Apply: func(ctx *cordis.Context, config any) error {
				return webserver.AsPlugin(deps.Logger).Apply(ctx)
			},
		}
	},
}
