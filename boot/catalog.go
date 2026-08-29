// The plugin catalog: official composition entry names resolve to Go plugin
// specs. Entry names are the official npm specifiers the bundled presets
// write, verbatim (see _dsh-official/packages/bundle/base/cordis.patch.yml
// for the authoritative 86-name set); a name without a Go implementation is
// a loud miss — "module not found" — never a silently skipped row, matching
// the official unresolvable-specifier behavior.
package boot

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/credentials"
	"dshgo/host/webserver"
	"dshgo/llm"
	"dshgo/llm/deepseek"
	"dshgo/session"
	"dshgo/session/persistence/jsonl"
	"dshgo/session/projection"
	"dshgo/settings"
	"dshgo/settings/file"
	"dshgo/tools"
)

// Service names plugins publish and consume through ctx inject lists.
const (
	ServiceTools          = "tools"
	ServiceCommands       = "commands"
	ServiceSettings       = "settings"
	ServiceWebServer      = "webServer"
	ServiceCredential     = "credentials"
	ServiceSessions       = "sessions"
	ServiceProjections    = "projections"
	ServiceAgents         = "agents"
	ServiceLlm            = "llm"
	ServiceSessionPersist = "sessionPersistence"
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

// sessionLogger adapts the catalog's cordis.Logger to the session store's
// minimal Warn(string) face (the doc-stated adapter, written out once here).
type sessionLogger struct{ logger cordis.Logger }

func adaptSessionLogger(logger cordis.Logger) session.Logger {
	return sessionLogger{logger: logger}
}

func (s sessionLogger) Warn(message string) {
	if s.logger != nil {
		s.logger.Warn(message)
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

	// The session store: in-memory session aggregation; persistence stays a
	// plugin concern (the jsonl backend lands as its own entry).
	"@deepseek-ai/dsh-session": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSessions},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceSessions, session.NewStore(adaptSessionLogger(deps.Logger)))
				return nil
			},
		}
	},

	// The projection registry: per-session derived-state units; Attach
	// subscribes the registry's event handlers for the context's lifetime.
	"@deepseek-ai/dsh-session-projection": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceProjections},
			Apply: func(ctx *cordis.Context, config any) error {
				registry := projection.NewRegistry()
				registry.Attach(ctx)
				ctx.Provide(ServiceProjections, registry)
				return nil
			},
		}
	},

	// The agent registry: owns the agent event bus and every created agent.
	"@deepseek-ai/dsh-agent": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceAgents},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceAgents, agent.NewAgentRegistry(ctx, deps.Logger))
				return nil
			},
		}
	},

	// The LLM runtime: model registrations and retry policies resolve here.
	"@deepseek-ai/dsh-llm": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceLlm},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceLlm, llm.NewRuntime())
				return nil
			},
		}
	},

	// The DeepSeek adapter: registers on the llm runtime; the settings
	// section (hot-reload) and managed credentials are optional deps the
	// plugin tolerates in nil form — wired through services so composition
	// order decides the production shape. Config decodes through the
	// plugin's json shape (the settings-section shape).
	"@deepseek-ai/dsh-llm-deepseek": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceLlm, ServiceSettings, ServiceCredential},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg deepseek.Config
				if config != nil {
					raw, err := json.Marshal(config)
					if err != nil {
						return err
					}
					if err := json.Unmarshal(raw, &cfg); err != nil {
						return err
					}
				}
				deps := deepseek.PluginDeps{
					Runtime:     ctx.Get(ServiceLlm).(*llm.Runtime),
					Settings:    ctx.Get(ServiceSettings).(*settings.Store),
					Credentials: ctx.Get(ServiceCredential).(credentials.Provider),
					Logger:      deps.Logger,
				}
				_, err := deepseek.Apply(deps, cfg)
				return err
			},
		}
	},

	// The jsonl persistence backend: physical session-log artifacts under
	// the profile home (config.root overrides). The store's consumption
	// contract lands with the storage-hub round; until then the backend is
	// provided as its own service.
	"@deepseek-ai/dsh-session-persistence-jsonl": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSessionPersist},
			Apply: func(ctx *cordis.Context, config any) error {
				root := filepath.Join(deps.Home, "sessions")
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["root"].(string); ok && raw != "" {
						if filepath.IsAbs(raw) {
							root = raw
						} else {
							root = filepath.Join(deps.Home, raw)
						}
					}
				}
				ctx.Provide(ServiceSessionPersist, jsonl.NewBackend(root, jsonl.CompressionNone))
				return nil
			},
		}
	},
}
