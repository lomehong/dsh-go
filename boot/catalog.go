// The plugin catalog: official composition entry names resolve to Go plugin
// specs. Entry names are the official npm specifiers the bundled presets
// write, verbatim (see _dsh-official/packages/bundle/base/cordis.patch.yml
// for the authoritative 86-name set); a name without a Go implementation is
// a loud miss — "module not found" — never a silently skipped row, matching
// the official unresolvable-specifier behavior.
package boot

import (
	"context"
	"dshgo/agentdefaultmodel"
	"dshgo/attachment/local"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/agent"
	"dshgo/agentinstructions"
	"dshgo/agentloop"
	"dshgo/checkpointpolicy"
	"dshgo/commandfeedback"
	"dshgo/commandgoal"
	"dshgo/commands"
	"dshgo/compaction"
	"dshgo/compactionbasic"
	"dshgo/cordis"
	"dshgo/credentials"
	"dshgo/filereference"
	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/fsobservationpolicy"
	"dshgo/fssandbox"
	"dshgo/fssearch"
	"dshgo/gateway"
	"dshgo/gatewaystream"
	"dshgo/goal"
	"dshgo/goalrounddriver"
	"dshgo/guard"
	"dshgo/homepaths"
	"dshgo/host/webserver"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/interaction/userquestions"
	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/llm/deepseek"
	"dshgo/llmretry"
	"dshgo/messagefeedback"
	"dshgo/planmode"
	"dshgo/preset"
	"dshgo/sandbox"
	"dshgo/sandboxpolicy"
	"dshgo/sandboxshell"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/persistence/jsonl"
	"dshgo/session/persistence/sqlite"
	"dshgo/session/projection"
	"dshgo/session/projectioncache"
	"dshgo/sessionlog"
	"dshgo/sessionquery"
	"dshgo/sessionquerysqlite"
	"dshgo/sessiontelemetry"
	"dshgo/sessiontelemetryotel"
	"dshgo/sessiontitle"
	"dshgo/sessiontitlellm"
	"dshgo/settings"
	"dshgo/settings/file"
	"dshgo/shell"
	"dshgo/shelllocal"
	"dshgo/shelltool"
	"dshgo/skill"
	"dshgo/skillbadge"
	"dshgo/skillfilesystem"
	"dshgo/spill"
	"dshgo/spilllocal"
	"dshgo/spillpolicy"
	"dshgo/storage"
	"dshgo/storagedomain"
	"dshgo/storagejson"
	"dshgo/storagesqlite"
	"dshgo/strreplaceeditor"
	"dshgo/subagent"
	"dshgo/subagentcontrol"
	"dshgo/subprocess"
	"dshgo/systemprompt"
	"dshgo/todo"
	"dshgo/tokenmeter"
	"dshgo/toolfs"
	"dshgo/toolgoal"
	"dshgo/toolralph"
	"dshgo/toolresultpruner"
	"dshgo/tools"
	"dshgo/toolsessionquery"
	"dshgo/toolsjobs"
	"dshgo/toolskill"
	"dshgo/toolsubagent"
	"dshgo/toolsubagentreport"
	"dshgo/toolweb"
	"dshgo/typert"
	"dshgo/web"
	"dshgo/webfetchhttp"
	"dshgo/webhook"
	"dshgo/websearchdeepseek"
	"dshgo/workflow"
	"dshgo/workspace"
)

// Service names plugins publish and consume through ctx inject lists.
const (
	ServiceTools     = "tools"
	ServiceCommands  = "commands"
	ServiceSettings  = "settings"
	ServiceWebServer = "webServer"
	// ServiceWeb is the web access capability seam (official ctx.web):
	// search/fetch provider registries and provider-selecting execution.
	ServiceWeb = "web"
	// ServiceSessionQuerySQLite is the lazy SQLite FTS5 derived read model
	// for session full-text search (official ctx.sessionQuerySqlite).
	ServiceSessionQuerySQLite = "sessionQuerySqlite"
	ServiceCredential         = "credentials"
	ServiceSessions           = "sessions"
	// ServiceSessionTitle is the live session title service (log-backed
	// fold surface lives in sessionquery).
	ServiceSessionTitle = "sessionTitle"
	// ServiceGoals is the same-session goal domain (event-sourced goals
	// with process-local continuation activation).
	ServiceGoals             = "goals"
	ServiceProjections       = "projections"
	ServiceProjectionCache   = "sessionProjectionCache"
	ServiceAttachments       = "attachments"
	ServiceAgentDefaultModel = "agentDefaultModel"
	ServiceAgents            = "agents"
	ServiceTypert            = "typert"
	ServiceTypertGateway     = "typertGateway"
	ServiceLlm               = "llm"
	ServiceSessionPersist    = "sessionPersistence"
	ServiceUserQuestions     = "userQuestions"
	ServiceUserApproval      = "userApproval"
	ServicePermissionPresets = "permissionPresets"
	ServiceSystemPrompt      = "systemPrompt"
	ServiceAgentLoop         = "agentLoop"
	ServiceSubagentRuntime   = "subagentRuntime"
	// ServiceSubagentModelSelection is the user-preference owner for
	// model-selectable delegation (official 'subagentModelSelection').
	ServiceSubagentModelSelection = "subagentModelSelection"
	ServiceSkills                 = "skills"
	ServiceJobs                   = "jobs"
	ServicePlanMode               = "planMode"
	ServiceTokenMeter             = "tokenMeter"
	ServiceCompaction             = "compaction"
	ServiceStorage                = "storage"
	ServiceSpillStore             = "spillStore"
	ServiceStorageDomain          = "storageDomain"
	ServiceDeepseekExt            = "deepseekLlmApiExtensions"
	// ServiceToolResultPruner is the optional model-free tool-result prune
	// pass; compaction-basic consumes it when composed.
	ServiceToolResultPruner = "toolResultPruner"
	// ServiceSubprocess is the child-process execution seam (fs-search and
	// the shell tools consume it).
	ServiceSubprocess = "subprocess"
	// ServiceShellEnv is the managed DSH_* environment registry the shell
	// tools inject into every model shell call.
	ServiceShellEnv = "shellEnv"
	// ServiceShell is the shell execution capability (ctx.shell); exactly
	// one executor provider composes per host (mounting both fails loud).
	ServiceShell = "shell"
	ServiceFS    = "fs"
	// ServiceWorkspace is the durable Web Workspace registry (official
	// 'workspaceRegistry').
	ServiceWorkspace = "workspaceRegistry"
	// ServiceAgentPresets is the preset roster and standing-mount table
	// (official 'agentPresets').
	ServiceAgentPresets = "agentPresets"
	// ServiceWebhookRuntime is the fire-and-forget webhook rule registry
	// (official 'webhookRuntime').
	ServiceWebhookRuntime = "webhookRuntime"
	// ServiceWorkflowEngine is the Go-realm workflow engine fanning child
	// runs out through the subagent runtime (official 'workflowEngine').
	ServiceWorkflowEngine = "workflowEngine"
	// ServiceTelemetry is the session-telemetry backend (official
	// 'sessionTelemetry'); one implementation per context, duplicate load
	// fails loud.
	ServiceTelemetry = "sessionTelemetry"
	// ServiceSandbox is the process-confinement provider seam (official
	// 'sandbox'): confine returns enforcing argv or fails closed.
	ServiceSandbox = "sandbox"
	// ServiceMessageFeedback is the durable message-feedback sidecar
	// (official 'messageFeedback'); one implementation per context.
	ServiceMessageFeedback = "messageFeedback"
	// ServiceSessionQuery is the live-preferred session query engine
	// (official 'sessionQuery'); exact reads, filters, and traces are
	// concrete; full-text search requires a mounted backend.
	ServiceSessionQuery = "sessionQuery"
	// ServiceFileReference is the workspace file-reference discovery service
	// (official 'fileReferences'); one local owner per context.
	ServiceFileReference = "fileReferences"
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

// adaptPersistenceLogger adapts to the coordinator's identical minimal face.
func adaptPersistenceLogger(logger cordis.Logger) persistence.Logger {
	return sessionLogger{logger: logger}
}

func (s sessionLogger) Warn(message string) {
	if s.logger != nil {
		s.logger.Warn(message)
	}
}

// decodeConfigJSON funnels a composition row's raw config through the
// target's json shape (the settings-section shape), failing loud on a shape
// the plugin cannot read.
func decodeConfigJSON(config any, out any) error {
	if config == nil {
		return nil
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// decodeShellLocalConfig decodes the shared bash-local/pwsh-local executor
// config surface (pwsh adds pwshPath).
func decodeShellLocalConfig(config any, pwsh bool) (shelllocal.Config, error) {
	var cfg struct {
		Cwd            string `json:"cwd"`
		TimeoutMs      *int   `json:"timeoutMs"`
		MaxTimeoutMs   *int   `json:"maxTimeoutMs"`
		MaxOutputBytes *int   `json:"maxOutputBytes"`
		MaxSpillBytes  *int   `json:"maxSpillBytes"`
		GraceMs        *int   `json:"graceMs"`
		PwshPath       string `json:"pwshPath"`
	}
	if err := decodeConfigJSON(config, &cfg); err != nil {
		return shelllocal.Config{}, err
	}
	out := shelllocal.DefaultConfig()
	out.Cwd = cfg.Cwd
	if cfg.TimeoutMs != nil {
		out.TimeoutMs = *cfg.TimeoutMs
	}
	if cfg.MaxTimeoutMs != nil {
		out.MaxTimeoutMs = *cfg.MaxTimeoutMs
	}
	if cfg.MaxOutputBytes != nil {
		out.MaxOutputBytes = *cfg.MaxOutputBytes
	}
	if cfg.MaxSpillBytes != nil {
		out.MaxSpillBytes = *cfg.MaxSpillBytes
	}
	if cfg.GraceMs != nil {
		out.GraceMs = *cfg.GraceMs
	}
	if pwsh {
		out.PwshPath = cfg.PwshPath
	}
	return out, nil
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

	// The web app's command-line provider (official dsh-web-app/startup):
	// parses the `dsh web` flag family (--host/--port/--dev/--trusted-host)
	// from the launcher-provided inner args and publishes the immutable
	// `webStartup` service rows inject.
	"@deepseek-ai/dsh-web-app/startup": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{"cmdlineArgs"},
			Provide: []string{"webStartup"},
			Apply: func(ctx *cordis.Context, config any) error {
				args, _ := ctx.Get("cmdlineArgs").([]string)
				values, err := parseWebStartup(args)
				if err != nil {
					return err
				}
				ctx.Provide("webStartup", values)
				return nil
			},
		}
	},

	// The web runtime glue (official dsh-web-app): resolves the built
	// frontend dist, mounts the frontend-static fallback owner over the
	// webserver, registers the web-surface prompt section and bash runtime
	// variables, and prints the URL line. Injects webStartup for the
	// invocation-only values and httpServer for the live bind.
	"@deepseek-ai/dsh-web-app": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{"webStartup"},
			Provide: []string{"webRuntime"},
			Apply: func(ctx *cordis.Context, config any) error {
				// webRuntime resolves bind-dependent values; the frontend
				// mount and URL print land with the webserver bind (the
				// httpServer row). This row publishes the runtime facts.
				ctx.Provide("webRuntime", map[string]any{})
				return nil
			},
		}
	},

	// The web server: route registry served over HTTP (official
	// @deepseek-ai/dsh-host-webserver; the dsh-web specifier belongs to the
	// web capability seam below).
	"@deepseek-ai/dsh-host-webserver": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceWebServer},
			Apply: func(ctx *cordis.Context, config any) error {
				return webserver.AsPlugin(deps.Logger).Apply(ctx)
			},
		}
	},

	// cordis-plugin-timer: the official base bundle ships an enabled timer
	// row, but its only consumer is the patchReload live watcher — a
	// launcher mechanism the Go host does not compose (recorded
	// disposition). The row must still resolve for the shipped base bundle
	// to compose; the no-op plugin satisfies the import without mounting
	// anything.
	"@deepseek-ai/cordis-plugin-timer": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Apply: func(*cordis.Context, any) error { return nil },
		}
	},

	// The web access capability seam (official dsh-web): search/fetch
	// provider registries with execution-time provider selection. The base
	// bundle pins the shipped providers; omitting either field auto-selects
	// when exactly one usable provider is registered.
	"@deepseek-ai/dsh-web": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceWeb},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := web.Config{}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["searchProvider"].(string); ok {
						cfg.SearchProvider = raw
					}
					if raw, ok := overridden["fetchProvider"].(string); ok {
						cfg.FetchProvider = raw
					}
				}
				ctx.Provide(ServiceWeb, web.NewRuntime(ctx, cfg))
				return nil
			},
		}
	},

	// The anonymous public HTTP(S) fetch provider (official
	// dsh-web-fetch-http, provider id "http"): public-address pinning,
	// same-origin redirects, size/time caps, and text decoding, registered
	// into the web seam without owning it.
	"@deepseek-ai/dsh-web-fetch-http": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceWeb},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := webfetchhttp.Config{}
				if overridden, ok := config.(map[string]any); ok {
					intOf := func(key string) *int {
						if raw, ok := overridden[key].(float64); ok {
							value := int(raw)
							return &value
						}
						return nil
					}
					cfg.MaxResponseBytes = intOf("maxResponseBytes")
					cfg.MaxBodyChars = intOf("maxBodyChars")
					cfg.TimeoutMs = intOf("timeoutMs")
					cfg.MaxRedirects = intOf("maxRedirects")
					if raw, ok := overridden["userAgent"].(string); ok && raw != "" {
						cfg.UserAgent = raw
					}
				}
				return webfetchhttp.AsPlugin(cfg).Apply(ctx)
			},
		}
	},

	// The model-facing web tools (official dsh-tool-web): search/fetch
	// registration over the web seam. Enablement controls registration; an
	// enabled tool stays visible when its provider is unavailable and fails
	// structured at execution. Bounds mirror the official Config defaults;
	// per-tool budgets resolve here into the tools' timeout field.
	"@deepseek-ai/dsh-tool-web": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTools, ServiceWeb, ServiceSystemPrompt},
			Apply: func(ctx *cordis.Context, config any) error {
				overridden, _ := config.(map[string]any)
				boolAt := func(key string, fallback bool) bool {
					return decodeConfigBool(overridden, key, fallback)
				}
				intAt := func(key string, fallback int64) int64 {
					if overridden != nil {
						if raw, ok := overridden[key].(int64); ok && raw > 0 {
							return raw
						}
						if raw, ok := overridden[key].(int); ok && raw > 0 {
							return int64(raw)
						}
					}
					return fallback
				}
				toolsRuntime := ctx.Get(ServiceTools).(*tools.ToolRuntime)
				seam := ctx.Get(ServiceWeb).(*web.Runtime)
				prompt := ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt)
				searchEnabled := boolAt("search", true)
				fetchEnabled := boolAt("fetch", true)
				if searchEnabled {
					closer, err := toolweb.ApplyWebSearchTool(toolsRuntime, prompt, seam, toolweb.SearchOptions{
						MaxResults:   int(intAt("searchMaxResults", toolweb.WebSearchMaxResults)),
						MaxQueries:   int(intAt("searchMaxQueries", toolweb.WebSearchMaxQueries)),
						TimeoutMs:    float64(intAt("searchTimeoutMs", toolweb.DefaultWebToolTimeoutMs)),
						FetchEnabled: fetchEnabled,
					})
					if err != nil {
						return err
					}
					ctx.Effect(func() (cordis.Disposer, error) {
						return cordis.Disposer(closer), nil
					})
				}
				if fetchEnabled {
					closer, err := toolweb.ApplyWebFetchTool(toolsRuntime, prompt, seam, toolweb.FetchOptions{
						MaxOutputChars: int(intAt("fetchMaxOutputChars", toolweb.DefaultFetchMaxOutputChars)),
						TimeoutMs:      float64(intAt("fetchTimeoutMs", toolweb.DefaultWebToolTimeoutMs)),
					})
					if err != nil {
						return err
					}
					ctx.Effect(func() (cordis.Disposer, error) {
						return cordis.Disposer(closer), nil
					})
				}
				return nil
			},
		}
	},

	// The DeepSeek-backed search provider (official dsh-web-search-deepseek):
	// an Anthropic-compatible Messages call with the native
	// `web_search_20250305` server tool. It reuses DEEPSEEK_API_KEY but not
	// DEEPSEEK_BASE_URL — search speaks the Anthropic Messages API. Options
	// resolve per search through a thunk, so a config change never mixes one
	// search's key with another's endpoint.
	"@deepseek-ai/dsh-web-search-deepseek": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceWeb},
			Apply: func(ctx *cordis.Context, config any) error {
				overridden, _ := config.(map[string]any)
				textAt := func(key, fallback string) string {
					if overridden != nil {
						if raw, ok := overridden[key].(string); ok && raw != "" {
							return raw
						}
					}
					return fallback
				}
				intAt := func(key string, fallback int64) int64 {
					if overridden != nil {
						if raw, ok := overridden[key].(int64); ok && raw > 0 {
							return raw
						}
						if raw, ok := overridden[key].(int); ok && raw > 0 {
							return int64(raw)
						}
					}
					return fallback
				}
				apiKeyEnv := textAt("apiKeyEnv", websearchdeepseek.DefaultAPIKeyEnv)
				baseURL := textAt("baseURL", "")
				if baseURL == "" {
					baseURL = os.Getenv(websearchdeepseek.SearchBaseURLEnv)
				}
				if baseURL == "" {
					baseURL = websearchdeepseek.DefaultBaseURL
				}
				// The credential service is an optional seam (official
				// `ctx.get('credentials')`): without it the environment is
				// the whole credential plane.
				var resolveKey func() (string, error)
				if provider, ok := ctx.Get(ServiceCredential).(credentials.Provider); ok {
					resolveKey = websearchdeepseek.ResolveAPIKeyFromCredentials(provider, apiKeyEnv)
				} else {
					resolveKey = websearchdeepseek.ResolveAPIKeyFromEnv(apiKeyEnv)
				}
				seam := ctx.Get(ServiceWeb).(*web.Runtime)
				provider := websearchdeepseek.NewProvider(func() websearchdeepseek.Options {
					return websearchdeepseek.Options{
						APIKey:        textAt("apiKey", ""),
						ResolveAPIKey: resolveKey,
						APIKeyEnv:     apiKeyEnv,
						BaseURL:       baseURL,
						Model:         textAt("model", websearchdeepseek.DefaultModel),
						APIVersion:    textAt("apiVersion", websearchdeepseek.DefaultAPIVersion),
						MaxTokens:     int(intAt("maxTokens", websearchdeepseek.DefaultMaxTokens)),
						MaxUses:       int(intAt("maxUses", websearchdeepseek.DefaultMaxUses)),
					}
				})
				closer, err := seam.RegisterSearchProvider(provider)
				if err != nil {
					return err
				}
				ctx.Effect(func() (cordis.Disposer, error) {
					return closer, nil
				})
				return nil
			},
		}
	},

	// SQLite FTS5 derived read model for session full-text search (official
	// dsh-session-query-sqlite). The official base profile mounts it with
	// path ":memory:" and openAt "never": the database opens lazily on first
	// consumer use, never at assembly. The indexed corpus feed (revisions +
	// live events) is the engine-composition concern this store serves.
	"@deepseek-ai/dsh-session-query-sqlite": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSessionQuerySQLite},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := sessionquerysqlite.Config{Path: ":memory:", JournalMode: sessionquerysqlite.JournalWAL}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["path"].(string); ok && raw != "" {
						cfg.Path = raw
					}
					if raw, ok := overridden["journalMode"].(string); ok && raw != "" {
						cfg.JournalMode = raw
					}
				}
				lazy := sessionquerysqlite.NewLazyStore(cfg)
				ctx.Provide(ServiceSessionQuerySQLite, lazy)
				ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() {
						if err := lazy.Close(); err != nil {
							deps.Logger.Warn(fmt.Sprintf("session-query-sqlite: close: %v", err))
						}
					}), nil
				})
				return nil
			},
		}
	},

	// The session-query engine (official dsh-session-query): the unified
	// live-preferred query service. Exact reads, filters, and traces are
	// concrete; full-text search is wired to the sqlite derived index when
	// that backend composes (SEARCH_DISABLED otherwise — the honest
	// backend-absent state).
	"@deepseek-ai/dsh-session-query": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions, ServiceSessionPersist, ServiceProjections},
			Provide: []string{ServiceSessionQuery},
			Apply: func(ctx *cordis.Context, config any) error {
				store := ctx.Get(ServiceSessions).(*session.Store)
				coordinator := ctx.Get(ServiceSessionPersist).(*persistence.Coordinator)
				projections := sessionquery.RegistryProjectionSource{
					Registry: ctx.Get(ServiceProjections).(*projection.Registry),
				}
				engine, err := sessionquery.NewEngine(
					sessionquery.StoreSessions{Store: store},
					coordinator,
					projections,
					nil, // search backend: sqlite adapter composes when mounted
					&sessionquery.Config{},
				)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSessionQuery, engine)
				return nil
			},
		}
	},

	// The model-facing session-history tools (official
	// dsh-tool-session-query): workspace-authorized search and read over the
	// session-query engine.
	"@deepseek-ai/dsh-tool-session-query": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTools, ServiceSystemPrompt, ServiceSessionQuery, ServiceAgents},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					MaxSearchResults *int `json:"maxSearchResults"`
					SearchTimeoutMs  *int `json:"searchTimeoutMs"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				undo, err := toolsessionquery.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					ctx.Get(ServiceSessionQuery).(*sessionquery.Engine),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					toolsessionquery.Config{MaxSearchResults: cfg.MaxSearchResults, SearchTimeoutMs: cfg.SearchTimeoutMs},
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(undo), nil })
			},
		}
	},

	// The same-session goal domain (official dsh-goal): event-sourced goals
	// over the owning session log with compare-and-set mutations and
	// process-local activation; the `goal` projection unit child installs
	// only when a projection registry is composed.
	"@deepseek-ai/dsh-goal": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{ServiceGoals},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := goal.Config{}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["defaultMaxGoalRounds"].(float64); ok {
						value := int64(raw)
						cfg.DefaultMaxGoalRounds = &value
					}
				}
				service, err := goal.NewService(ctx, ctx.Get(ServiceAgents).(*agent.AgentRegistry), cfg)
				if err != nil {
					return err
				}
				if err := ctx.Effect(func() (cordis.Disposer, error) { return service.Dispose, nil }); err != nil {
					return err
				}
				ctx.Provide(ServiceGoals, service)
				return nil
			},
		}
	},

	// The automatic same-session continuation driver (official
	// dsh-goal-round-driver): reserves one armed goal round at a time over
	// the loop's inbox and pre-step fences, with a durability checkpoint
	// between rounds when the persistence coordinator is composed.
	"@deepseek-ai/dsh-goal-round-driver": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceAgents, ServiceGoals, ServiceSessions},
			Apply: func(ctx *cordis.Context, config any) error {
				driverConfig := goalrounddriver.Config{Logger: deps.Logger}
				if coordinator, ok := ctx.Get(ServiceSessionPersist).(*persistence.Coordinator); ok {
					driverConfig.Flusher = coordinator
				}
				_, err := goalrounddriver.New(ctx,
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					ctx.Get(ServiceGoals).(*goal.Service),
					ctx.Get(ServiceSessions).(*session.Store),
					driverConfig)
				return err
			},
		}
	},

	// The model-facing goal controls (official dsh-tool-goal): get_goal,
	// create_goal, and update_goal over the goal domain with direct-human
	// and goal-round authority fences; the shared policy section renders the
	// deployment-selected blocked threshold.
	"@deepseek-ai/dsh-tool-goal": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceAgents, ServiceGoals, ServiceTools, ServiceSystemPrompt},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := toolgoal.Config{}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["blockedAfterConsecutiveRounds"].(float64); ok {
						value := int64(raw)
						cfg.BlockedAfterConsecutiveRounds = &value
					}
				}
				return toolgoal.Apply(ctx,
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					ctx.Get(ServiceGoals).(*goal.Service),
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					cfg)
			},
		}
	},

	// The /goal human command over the goal service: the seven-form grammar
	// (bare show, create, clear, edit, pause, resume, help) with state
	// rendering verbatim.
	"@deepseek-ai/dsh-command-goal": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceCommands, ServiceGoals},
			Apply: func(ctx *cordis.Context, config any) error {
				_, err := commandgoal.Register(
					ctx.Get(ServiceCommands).(*commands.CommandRuntime),
					ctx.Get(ServiceGoals).(*goal.Service),
				)
				return err
			},
		}
	},

	// The Typert runtime registry: generated-schema and package reflection,
	// local/Remote invocation definitions, Host object lookups, and
	// Host/Client Context adapters. Consumers register lookups in their own
	// entries (session/agent).
	"@deepseek-ai/dsh-typert-registry": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceTypert},
			Apply: func(ctx *cordis.Context, config any) error {
				registry := typert.NewRegistry(ctx, deps.Logger)
				typert.ContextService.Provide(ctx, registry)
				return nil
			},
		}
	},
	// The Typert Remote gateway: carrier-independent Host dispatch over
	// strict registered definitions and the registry's lookup/Context
	// providers. Carrier adapters (Connection /api, WebSocket mux) adapt on
	// top.
	"@deepseek-ai/dsh-api-gateway": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceTypertGateway},
			Inject:  []string{ServiceTypert},
			Apply: func(ctx *cordis.Context, config any) error {
				registry, ok := typert.ContextService.From(ctx)
				if !ok {
					return errors.New("api-gateway: typert service is unavailable")
				}
				ctx.Provide(ServiceTypertGateway, gateway.New(ctx, registry))
				return nil
			},
		}
	},

	// The forwarded Remote event source (official dsh-api-remotes): bridges
	// every allowlisted Host event into the gateway's $events stream. Each
	// stream signal creates a fresh queue + listener set, detached on the
	// signal end (official remoteEventSource).
	"@deepseek-ai/dsh-api-remotes": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTypertGateway, ServiceAgents},
			Apply: func(ctx *cordis.Context, config any) error {
				value, ok := ctx.Get(ServiceTypertGateway).(*gateway.Gateway)
				if !ok {
					return errors.New("api-remotes: typertGateway service is unavailable")
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				bus := agents.Events()
				_, err := value.RegisterRemoteEvents(func(signal context.Context) gateway.RemoteEventDispatchIter {
					queue := gatewaystream.NewRemoteEventQueue()
					detach := gatewaystream.AttachForwardedEvents(queue, bus, agentScopeKey(agents))
					return &remoteEventBridgeIter{queue: queue, detach: detach, signal: signal}
				}, gatewaystream.RemoteEventHostInfo{Home: dshHome()})
				return err
			},
		}
	},
	// Delegation tools (official tool-subagent rows): spawn + fork.
	"@deepseek-ai/dsh-tool-subagent":      buildDelegationTool("spawn", "subagent"),
	"@deepseek-ai/dsh-tool-subagent-fork": buildDelegationTool("fork", "subagent_fork"),

	// The child-scoped report tool for continuable children (official
	// dsh-tool-subagent-report: installs through the activation setup
	// registry, invisible to parents and siblings).
	"@deepseek-ai/dsh-tool-subagent-report": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubagentRuntime, ServiceTools, ServiceSystemPrompt, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					ReportDelivery string `json:"reportDelivery"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				_, err := toolsubagentreport.Register(toolsubagentreport.Deps{
					Subagents:    ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
					Tools:        ctx.Get(ServiceTools).(*tools.ToolRuntime),
					Prompt:       ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					ResolveAgent: agentResolverOf(agents),
				}, toolsubagentreport.Config{ReportDelivery: subagent.SubagentReportDelivery(cfg.ReportDelivery)})
				return err
			},
		}
	},
	// The user-preference owner for model-selectable delegation (official
	// same-package named export; the web-app bundle composes it — the base
	// bundle does not).
	"@deepseek-ai/dsh-tool-subagent/model-selection-settings": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSettings},
			Provide: []string{ServiceSubagentModelSelection},
			Apply: func(ctx *cordis.Context, config any) error {
				var raw struct {
					Enabled       bool  `json:"enabled"`
					AllowedModels []any `json:"allowedModels"`
				}
				if err := decodeConfigJSON(config, &raw); err != nil {
					return err
				}
				initial := toolsubagent.ModelSelectionSettings{Enabled: raw.Enabled}
				for _, item := range raw.AllowedModels {
					entry, ok := item.(map[string]any)
					if !ok {
						return fmt.Errorf("subagent-model-selection: allowedModels entries must be objects")
					}
					section := toolsubagent.ParseModelSelectionSection(map[string]any{"allowedModels": []any{entry}})
					if len(section.AllowedModels) == 1 {
						initial.AllowedModels = append(initial.AllowedModels, section.AllowedModels[0])
					}
				}
				routesAsAny := make([]any, 0, len(initial.AllowedModels))
				for _, route := range initial.AllowedModels {
					routesAsAny = append(routesAsAny, map[string]any{"provider": route.Provider, "model": route.Model})
				}
				service, err := toolsubagent.NewModelSelectionConfig(initial)
				if err != nil {
					return err
				}
				store := ctx.Get(ServiceSettings)
				if store == nil {
					return fmt.Errorf("subagent-model-selection-settings: the settings store is required")
				}
				settingsStore := store.(*settings.Store)
				envelope, err := json.Marshal(map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled": map[string]any{"type": "boolean"},
						"allowedModels": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"provider": map[string]any{"type": "string"},
									"model":    map[string]any{"type": "string"},
								},
								"required": []string{"provider", "model"},
							},
						},
					},
				})
				if err != nil {
					return err
				}
				settingsScope, err := settingsStore.Register("subagent-model-selection", &settings.Schema{
					Envelope: envelope,
					// The composition initial, not the live source: the
					// source closure reads back through this same store,
					// and Defaults runs under the store lock. Routes stay
					// in the JSON document shape the reader parses back.
					Defaults: func() map[string]any {
						return map[string]any{"enabled": initial.Enabled, "allowedModels": routesAsAny}
					},
					Validate: func(value map[string]any) error {
						return service.Validate(toolsubagent.ParseModelSelectionSection(value))
					},
				}, map[string]any{"enabled": initial.Enabled, "allowedModels": routesAsAny})
				if err != nil {
					return err
				}
				// setSource: consumers sample at first selection, so a
				// settings update never rebuilds a running Agent's tools.
				service.SetSource(func() toolsubagent.ModelSelectionSettings {
					return toolsubagent.ParseModelSelectionSection(settingsScope.Get())
				})
				ctx.Provide(ServiceSubagentModelSelection, service)
				return nil
			},
		}
	},
	// Request deadline enforcement for tools that declare a timeoutMs
	// budget (the model-visible isError text mirrors the canonical shape).
	"@deepseek-ai/dsh-tool-call-timeout-policy": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTools},
			Apply: func(ctx *cordis.Context, config any) error {
				detach := guard.AttachTimeoutPolicy(ctx.Get(ServiceTools).(*tools.ToolRuntime))
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// Baseline + dynamic project instructions (AGENTS.md family) mounted
	// on every agent. maxBytes defaults to the base-bundle value.
	"@deepseek-ai/dsh-agent-instructions": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceAgents, ServiceTools},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := agentinstructions.Config{DSHHome: deps.Home, MaxBytes: 65536}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["maxBytes"].(float64); ok && raw > 0 {
						cfg.MaxBytes = int64(raw)
					}
				}
				detach, err := agentinstructions.Register(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					deps.Logger, cfg,
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// Filesystem skill source: discovers directory-bundle and flat
	// Markdown skills from project/custom/home roots; watcher invalidates
	// the registry catalog through ProviderControl.Invalidate.
	"@deepseek-ai/dsh-skill-filesystem": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceSkills},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg skillfilesystem.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				resolved, err := skillfilesystem.ResolveConfig(cfg)
				if err != nil {
					return err
				}
				registry := ctx.Get(ServiceSkills).(*skill.Registry)
				detach, err := registry.RegisterProviderIn(nil, func(control skill.ProviderControl) (skill.Provider, error) {
					return skillfilesystem.New(resolved, control.Invalidate, deps.Logger), nil
				})
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// Bundled badge skill (embedded assets materialized under the profile
	// home). The base bundle ships this row disabled; the entry exists so
	// enabling it in a profile resolves.
	"@deepseek-ai/dsh-skill-badge": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceSkills},
			Apply: func(ctx *cordis.Context, config any) error {
				detach, err := skillbadge.RegisterIn(
					ctx.Get(ServiceSkills).(*skill.Registry),
					nil, filepath.Join(deps.Home, "skill-badge"),
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// Semantic durability checkpoints: every model request prefix and
	// top-level tool dispatch requires the session log durable through its
	// input. A checkpoint rejection prevents adapter dispatch.
	"@deepseek-ai/dsh-session-checkpoint-policy": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceLlm, ServiceTools, ServiceAgents, ServiceSessionPersist, ServiceSessions},
			Apply: func(ctx *cordis.Context, config any) error {
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				coordinator := ctx.Get(ServiceSessionPersist).(*persistence.Coordinator)
				sessions := ctx.Get(ServiceSessions).(*session.Store)
				resolveAgent := agentResolverOf(agents)
				flusher := checkpointFlusherOf(coordinator, sessions)
				detach, err := checkpointpolicy.Attach(
					ctx.Get(ServiceLlm).(*llm.Runtime),
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					agents, flusher,
					func(key tools.ScopeKey) (string, bool) {
						resolved := resolveAgent(key)
						if resolved == nil {
							return "", false
						}
						return string(resolved.ID), true
					},
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// The DeepSeek API extensions registry: independently owned top-level
	// request fields merged into official DeepSeek requests. Optional
	// companion — llm-deepseek reads it opportunistically.
	"@deepseek-ai/dsh-deepseek-llm-api-extensions": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceDeepseekExt},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceDeepseekExt, deepseek.NewExtensionRegistry())
				return nil
			},
		}
	},

	// Incremental session-log contribution (dsh_session_log) for official
	// DeepSeek requests; config enabled defaults to false (register
	// nothing until a profile turns it on).
	"@deepseek-ai/dsh-session-log-deepseek": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceDeepseekExt, ServiceSessions},
			Apply: func(ctx *cordis.Context, config any) error {
				enabled := false
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["enabled"].(bool); ok {
						enabled = raw
					}
				}
				if !enabled {
					return nil
				}
				detach, err := sessionlog.RegisterDeepseekField(
					ctx.Get(ServiceDeepseekExt).(*deepseek.ExtensionRegistry),
					ctx.Get(ServiceSessions).(*session.Store),
					sessionlog.NewFolder(),
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},
	// The storage hub: named backend registry plus mounted data-form
	// facilities. The hub itself performs no IO.
	"@deepseek-ai/dsh-storage": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceStorage},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceStorage, storage.NewHub())
				return nil
			},
		}
	},

	// JSON storage backend: one human-readable document per unit under
	// config root (required — assemblies state the location explicitly, per
	// the official no-cwd-fallback stance).
	"@deepseek-ai/dsh-storage-json": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceStorage},
			Provide: []string{storage.StorageBackendServiceKey("json")},
			Apply: func(ctx *cordis.Context, config any) error {
				root := ""
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["root"].(string); ok {
						root = raw
					}
				}
				if root == "" {
					return errors.New("storage-json: config root is required")
				}
				hub := ctx.Get(ServiceStorage).(*storage.Hub)
				backend := storagejson.NewJsonStorageBackend(root)
				unregister, err := hub.Backend.Register("json", backend)
				if err != nil {
					return err
				}
				ctx.Provide(storage.StorageBackendServiceKey("json"), backend)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() {
						unregister()
						if err := backend.Close(); err != nil && deps.Logger != nil {
							deps.Logger.Warn(fmt.Sprintf("storage-json: backend close: %v", err))
						}
					}), nil
				})
			},
		}
	},

	// SQLite storage backend: one database file hosts every routed unit,
	// document-per-row over the pure-Go modernc driver. Config: `path`
	// (required; `:memory:` for tests) and `journalMode` (default wal).
	"@deepseek-ai/dsh-storage-sqlite": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceStorage},
			Provide: []string{storage.StorageBackendServiceKey("sqlite")},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					Path        string `json:"path"`
					JournalMode string `json:"journalMode"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				if cfg.Path == "" {
					return fmt.Errorf("storage-sqlite: config path is required")
				}
				hub := ctx.Get(ServiceStorage).(*storage.Hub)
				backend, err := storagesqlite.New(storagesqlite.Config{
					Path:        cfg.Path,
					JournalMode: storagesqlite.JournalMode(cfg.JournalMode),
				})
				if err != nil {
					return err
				}
				unregister, err := hub.Backend.Register("sqlite", backend)
				if err != nil {
					backend.Close()
					return err
				}
				ctx.Provide(storage.StorageBackendServiceKey("sqlite"), backend)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() {
						unregister()
						if err := backend.Close(); err != nil && deps.Logger != nil {
							deps.Logger.Warn(fmt.Sprintf("storage-sqlite: backend close: %v", err))
						}
					}), nil
				})
			},
		}
	},
	// Log-backed session titles: provider registration, deterministic
	// fallback, automatic scheduling over the store's post-commit feed, and
	// explicit renames. The service owns the store's single-slot event sink
	// for its lifetime (single-tap architecture note in sessiontitle). The
	// official base profile's word/byte caps are the catalog defaults;
	// explicit config overrides them.
	"@deepseek-ai/dsh-session-title": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions},
			Provide: []string{ServiceSessionTitle},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := sessiontitle.Config{FallbackMaxWords: 5, FallbackMaxBytes: 40, MaxTitleBytes: 80}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["fallbackMaxWords"].(float64); ok && raw > 0 {
						cfg.FallbackMaxWords = int(raw)
					}
					if raw, ok := overridden["fallbackMaxBytes"].(float64); ok && raw > 0 {
						cfg.FallbackMaxBytes = int(raw)
					}
					if raw, ok := overridden["maxTitleBytes"].(float64); ok && raw > 0 {
						cfg.MaxTitleBytes = int(raw)
					}
				}
				store, ok := ctx.Get(ServiceSessions).(*session.Store)
				if !ok || store == nil {
					return errors.New("session-title: the sessions store is required")
				}
				service, err := sessiontitle.NewService(store, cfg, deps.Logger)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSessionTitle, service)
				ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(service.Dispose), nil
				})
				return nil
			},
		}
	},
	// First-human-message model-backed title provider over the shared
	// session-title-llm policy (framing, byte budget, timeout, finish
	// mapping). The official base profile's caps are the defaults;
	// provider/model stay unpinned so the route resolves from the logged
	// request/header unless both are configured together.
	"@deepseek-ai/dsh-session-title-first-prompt-llm": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceSessionTitle, ServiceLlm},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := sessiontitlellm.Config{
					TargetWords:         5,
					TargetCJKCharacters: 10,
					MaxInputBytes:       4096,
					MaxOutputTokens:     64,
					TimeoutMs:           60000,
				}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["targetWords"].(float64); ok && raw > 0 {
						cfg.TargetWords = int(raw)
					}
					if raw, ok := overridden["targetCjkCharacters"].(float64); ok && raw > 0 {
						cfg.TargetCJKCharacters = int(raw)
					}
					if raw, ok := overridden["maxInputBytes"].(float64); ok && raw > 0 {
						cfg.MaxInputBytes = int(raw)
					}
					if raw, ok := overridden["maxOutputTokens"].(float64); ok && raw > 0 {
						cfg.MaxOutputTokens = int64(raw)
					}
					if raw, ok := overridden["timeoutMs"].(float64); ok && raw > 0 {
						cfg.TimeoutMs = int64(raw)
					}
					provider, hasProvider := overridden["provider"].(string)
					model, hasModel := overridden["model"].(string)
					if hasProvider && hasModel {
						cfg.Provider, cfg.Model = provider, model
					}
				}
				service, ok := ctx.Get(ServiceSessionTitle).(*sessiontitle.Service)
				if !ok || service == nil {
					return errors.New("session-title-first-prompt-llm: the session title service is required")
				}
				runtime, ok := ctx.Get(ServiceLlm).(*llm.Runtime)
				if !ok || runtime == nil {
					return errors.New("session-title-first-prompt-llm: the llm runtime is required")
				}
				closer, err := sessiontitlellm.Register(service, runtime, cfg,
					"session-title-first-prompt-llm", sessiontitle.AutomaticFirstPrompt,
					sessiontitlellm.SelectFirstPrompt)
				if err != nil {
					return err
				}
				ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(closer), nil
				})
				return nil
			},
		}
	},
	// Domain data form: schema-validated, change-emitting KV domains over
	// routed backends. The routed backend table resolves at apply because
	// the backend lifecycle service keys are injections (activation cannot
	// race backend registration); a route naming an unregistered backend
	// fails loud here instead of at first open.
	"@deepseek-ai/dsh-storage-domain": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceStorage, storage.StorageBackendServiceKey("json")},
			Provide: []string{ServiceStorageDomain},
			Apply: func(ctx *cordis.Context, config any) error {
				backendName := "json"
				routes := map[string]string{}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["backend"].(string); ok && raw != "" {
						backendName = raw
					}
					if raw, ok := overridden["routes"].(map[string]any); ok {
						for key, value := range raw {
							if name, ok := value.(string); ok {
								routes[key] = name
							}
						}
					}
				}
				hub := ctx.Get(ServiceStorage).(*storage.Hub)
				routed := map[string]storagedomain.Backend{}
				resolve := func(name string) error {
					backend, err := hub.Backend.Get(name)
					if err != nil {
						return fmt.Errorf("storage-domain: backend route %q: %w", name, err)
					}
					routed[name] = backend
					return nil
				}
				if err := resolve(backendName); err != nil {
					return err
				}
				for _, name := range routes {
					if err := resolve(name); err != nil {
						return err
					}
				}
				facility := storagedomain.NewFacility(storagedomain.Config{
					Backend: backendName,
					Routes:  routes,
				}, routed, deps.Logger)
				unmount, err := hub.Mount("domain", facility)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceStorageDomain, facility)
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(unmount), nil })
			},
		}
	},

	// Local spill backend: private session-scoped spill files plus the
	// one-shot startup cleanup sweep (Close awaits sweep quiescence).
	"@deepseek-ai/dsh-spill-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSpillStore},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := spilllocal.Config{}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["root"].(string); ok && raw != "" {
						cfg.Root = raw
					}
					if raw, ok := overridden["cleanupPeriodDays"].(float64); ok {
						cfg.CleanupPeriodDays = int(raw)
						cfg.CleanupPeriodDaysSet = true
					}
				}
				store, err := spilllocal.NewLocalSpillStore(spilllocal.ResolveConfig(cfg), deps.Logger)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSpillStore, store)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(store.Close), nil
				})
			},
		}
	},

	// Spill policy: post-execute transformer that keeps oversized
	// plain-text tool results out of model context. The store is optional
	// (official `ctx.get('spillStore')`): absent backend keeps everything
	// inline with a warning.
	"@deepseek-ai/dsh-spill-policy": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTools, ServiceAgents},
			Apply: func(ctx *cordis.Context, config any) error {
				var cap *int
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["maxInlineBytes"].(float64); ok {
						value := int(raw)
						cap = &value
					}
				}
				runtime := ctx.Get(ServiceTools).(*tools.ToolRuntime)
				var store spill.Store
				if candidate := ctx.Get(ServiceSpillStore); candidate != nil {
					store = candidate.(*spilllocal.LocalSpillStore)
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				resolveOwner := func(key tools.ScopeKey) (session.SessionID, bool) {
					resolved := agentResolverOf(agents)(key)
					if resolved == nil {
						return "", false
					}
					return resolved.ID, true
				}
				detach, err := spillpolicy.Attach(runtime, store, deps.Logger, spillpolicy.Config{MaxInlineBytes: cap}, resolveOwner)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},
	// The session store: in-memory session aggregation; persistence stays a
	// plugin concern (the jsonl backend lands as its own entry).
	"@deepseek-ai/dsh-session": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSessions},
			Inject:  []string{ServiceTypert},
			Apply: func(ctx *cordis.Context, config any) error {
				store := session.NewStore(adaptSessionLogger(deps.Logger))
				ctx.Provide(ServiceSessions, store)
				// The official core/session lookup: wire "sessionId" names
				// the live Session.
				if registry, ok := typert.ContextService.From(ctx); ok {
					disposer, err := registry.LookupRegister("session", typert.LookupProvider{
						Parameter:      "session",
						Wire:           "sessionId",
						HostTypeSymbol: "@deepseek-ai/dsh-session#Session",
						WireTypeSymbol: "@deepseek-ai/dsh-session/types#SessionId",
						Resolve: func(id any) (any, error) {
							if resolved := store.Get(session.SessionID(id.(string))); resolved != nil {
								return resolved, nil
							}
							return nil, nil
						},
					})
					if err != nil {
						return err
					}
					if err := ctx.Effect(func() (cordis.Disposer, error) { return disposer, nil }); err != nil {
						return err
					}
				}
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
				detach := registry.Attach(ctx)
				if err := ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(detach), nil }); err != nil {
					return err
				}
				ctx.Provide(ServiceProjections, registry)
				return nil
			},
		}
	},

	// The agent registry: owns the agent event bus and every created agent.
	"@deepseek-ai/dsh-agent": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceAgents},
			Inject:  []string{ServiceTypert},
			Apply: func(ctx *cordis.Context, config any) error {
				registry := agent.NewAgentRegistry(ctx, deps.Logger)
				ctx.Provide(ServiceAgents, registry)
				// The official core/agent lookups: wire "agentId" names the
				// live Agent, and the agent Host Context adapter projects a
				// live context to the owning agent's id and back.
				if typertRegistry, ok := typert.ContextService.From(ctx); ok {
					lookupDisposer, err := typertRegistry.LookupRegister("agent", typert.LookupProvider{
						Parameter:      "agent",
						Wire:           "agentId",
						HostTypeSymbol: "@deepseek-ai/dsh-agent#Agent",
						WireTypeSymbol: "@deepseek-ai/dsh-session/types#SessionId",
						Resolve: func(id any) (any, error) {
							if resolved := registry.Get(session.SessionID(id.(string))); resolved != nil {
								return resolved, nil
							}
							return nil, nil
						},
					})
					if err != nil {
						return err
					}
					if err := ctx.Effect(func() (cordis.Disposer, error) { return lookupDisposer, nil }); err != nil {
						return err
					}
					contextDisposer, err := typertRegistry.ContextRegisterHost("agent", typert.HostContextAdapter{
						Wire:           "agentId",
						WireTypeSymbol: "@deepseek-ai/dsh-session/types#SessionId",
						Identity: func(candidate any) (any, bool) {
							agentCtx, isCtx := candidate.(*cordis.Context)
							if !isCtx {
								return nil, false
							}
							if owned, found := agent.ContextService.From(agentCtx); found {
								return string(owned.ID), true
							}
							return nil, false
						},
						Resolve: func(id any) (any, bool, error) {
							if resolved := registry.Get(session.SessionID(id.(string))); resolved != nil {
								return resolved.Ctx, true, nil
							}
							return nil, false, nil
						},
					})
					if err != nil {
						return err
					}
					if err := ctx.Effect(func() (cordis.Disposer, error) { return contextDisposer, nil }); err != nil {
						return err
					}
				}
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
				pluginDeps := deepseek.PluginDeps{
					Runtime:     ctx.Get(ServiceLlm).(*llm.Runtime),
					Settings:    ctx.Get(ServiceSettings).(*settings.Store),
					Credentials: ctx.Get(ServiceCredential).(credentials.Provider),
					Logger:      deps.Logger,
				}
				if candidate := ctx.Get(ServiceDeepseekExt); candidate != nil {
					pluginDeps.Extensions = candidate.(*deepseek.ExtensionRegistry)
				}
				plugin, err := deepseek.Apply(pluginDeps, cfg)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(plugin.Dispose), nil
				})
			},
		}
	},

	// The jsonl persistence plugin: the physical session-log backend under
	// the profile home (config.root overrides), composed into the
	// coordinator that owns preparation, write-behind, and snapshots. The
	// coordinator is the service consumers inject.
	"@deepseek-ai/dsh-session-persistence-jsonl": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions},
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
				backend := jsonl.NewBackend(root, jsonl.CompressionNone)
				coordinator, err := persistence.NewCoordinator(
					backend,
					persistence.NewSessionsAdapter(ctx.Get(ServiceSessions).(*session.Store)),
					adaptPersistenceLogger(deps.Logger),
					persistence.CoordinatorOptions{},
				)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSessionPersist, coordinator)
				if err := ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() { _ = coordinator.Dispose() }), nil
				}); err != nil {
					return err
				}
				return nil
			},
		}
	},

	// SQLite session persistence: the same coordinator contract over the
	// pure-Go modernc.org/sqlite driver (official dsh-session-persistence-
	// sqlite). Config: `path` (absolute, or relative to the profile home),
	// `journalMode` (wal|delete|truncate|persist, default wal),
	// `busyTimeoutMs` (default 5000). The database opens lazily on first
	// persistence use.
	"@deepseek-ai/dsh-session-persistence-sqlite": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions},
			Provide: []string{ServiceSessionPersist},
			Apply: func(ctx *cordis.Context, config any) error {
				path := filepath.Join(deps.Home, "sessions.db")
				journalMode := ""
				busyTimeoutMs := int64(0)
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["path"].(string); ok && raw != "" {
						if filepath.IsAbs(raw) {
							path = raw
						} else {
							path = filepath.Join(deps.Home, raw)
						}
					}
					if raw, ok := overridden["journalMode"].(string); ok {
						journalMode = raw
					}
					if raw, ok := overridden["busyTimeoutMs"].(float64); ok && raw > 0 {
						busyTimeoutMs = int64(raw)
					}
				}
				backend, err := sqlite.Open(sqlite.Config{
					Path:          path,
					JournalMode:   sqlite.JournalMode(journalMode),
					BusyTimeoutMs: busyTimeoutMs,
				})
				if err != nil {
					return err
				}
				coordinator, err := persistence.NewCoordinator(
					backend,
					persistence.NewSessionsAdapter(ctx.Get(ServiceSessions).(*session.Store)),
					adaptPersistenceLogger(deps.Logger),
					persistence.CoordinatorOptions{},
				)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSessionPersist, coordinator)
				if err := ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() { _ = coordinator.Dispose() }), nil
				}); err != nil {
					return err
				}
				return nil
			},
		}
	},

	// The user-questions waterfall: ask-user questions resolve through the
	// typed request seam.
	// The user-interaction bundle row (official dsh-user-interaction):
	// composes the question and approval services in one row, matching the
	// shipped base patch which names a single `user-interaction` entry.
	"@deepseek-ai/dsh-user-interaction": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{ServiceUserQuestions, ServiceUserApproval},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceUserQuestions, userquestions.NewService(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry)))
				policy := userapproval.PolicyAsk
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["policy"].(string); ok && raw != "" {
						policy = userapproval.ApprovalPolicy(raw)
					}
				}
				cfg, err := userapproval.NewConfig(policy)
				if err != nil {
					return err
				}
				service, err := userapproval.NewService(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry), cfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceUserApproval, service)
				return nil
			},
		}
	},

	"@deepseek-ai/dsh-user-questions": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{ServiceUserQuestions},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceUserQuestions, userquestions.NewService(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry)))
				return nil
			},
		}
	},

	// The user-approval waterfall: tool approval resolves through the
	// approval decision seam. Config may pin the policy
	// (`config.policy`: ask|never); the ask default is the schema default.
	"@deepseek-ai/dsh-user-approval": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{ServiceUserApproval},
			Apply: func(ctx *cordis.Context, config any) error {
				policy := userapproval.PolicyAsk
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["policy"].(string); ok && raw != "" {
						policy = userapproval.ApprovalPolicy(raw)
					}
				}
				cfg, err := userapproval.NewConfig(policy)
				if err != nil {
					return err
				}
				service, err := userapproval.NewService(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry), cfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceUserApproval, service)
				return nil
			},
		}
	},

	// The permission-presets service: the knob table behind sandbox-mode
	// and approval-policy presets, and the sandbox-override source for
	// delegation. Config may replace the preset table (`config.presets`
	// and `config.names`, `config.sandboxDefault`); the default lands on
	// the schema-defaulted table over workspace-write. The composition
	// wires the user-settings `permission` section (defaultPreset) as the
	// live new-session default, pins the initial permission on every
	// session creation, and backfills sessions that already exist at
	// composition.
	"@deepseek-ai/dsh-permission-presets": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions, ServiceProjections},
			Provide: []string{ServicePermissionPresets},
			Apply: func(ctx *cordis.Context, config any) error {
				projections := ctx.Get(ServiceProjections).(*projection.Registry)
				presets, names := permissionpresets.DefaultPresets()
				cfg := permissionpresets.Config{
					Presets:        presets,
					Names:          names,
					SandboxDefault: permissionpresets.SandboxWorkspaceWrite,
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				service, err := permissionpresets.NewService(cfg)
				if err != nil {
					return err
				}
				if store := ctx.Get(ServiceSettings); store != nil {
					settingsStore := store.(*settings.Store)
					envelope, err := json.Marshal(map[string]any{
						"type": "object",
						"properties": map[string]any{
							"defaultPreset": map[string]any{
								"type": "enum",
								"enum": names,
							},
						},
						"required": []string{"defaultPreset"},
					})
					if err != nil {
						return err
					}
					scope, err := settingsStore.Register("permission", &settings.Schema{
						Envelope: envelope,
						Defaults: func() map[string]any {
							return map[string]any{"defaultPreset": service.DefaultPreset()}
						},
						Validate: func(value map[string]any) error {
							name, _ := value["defaultPreset"].(string)
							if _, err := service.Resolve(name); err != nil {
								return err
							}
							return nil
						},
					}, map[string]any{"defaultPreset": service.DefaultPreset()})
					if err != nil {
						return err
					}
					// setSource: the resolved section is read live at
					// session creation; no registration replacement is
					// needed on change.
					service.SetDefaultSource(func() string {
						name, _ := scope.Get()["defaultPreset"].(string)
						return name
					})
				}
				sessions := ctx.Get(ServiceSessions).(*session.Store)
				// session/created hook: veto-capable creation announcement.
				sessions.OnCreated(func(sess *session.Session) error {
					return service.PinInitialPermission(sess)
				})
				for _, id := range sessions.List() {
					if existing := sessions.Get(id); existing != nil {
						if err := service.PinInitialPermission(existing); err != nil {
							return err
						}
					}
				}
				ctx.Provide(ServicePermissionPresets, service)
				// The `permissions` projection unit: the three-knob fold
				// with the preset select derived at view time.
				if _, err := projections.Register(service.ProjectionDefinition()); err != nil {
					return err
				}
				return nil
			},
		}
	},

	// The system-prompt registry: harness-owned base sections plus scoped
	// layers. Config funnels through the prompt config json shape.
	"@deepseek-ai/dsh-system-prompt": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSystemPrompt},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg systemprompt.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				prompt, err := systemprompt.NewSystemPrompt(cfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSystemPrompt, prompt)
				return nil
			},
		}
	},

	// The agent loop: the per-agent react-loop factory over the composed
	// registries. This is also the manager's child create/resume seam.
	"@deepseek-ai/dsh-agent-loop": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents, ServiceLlm, ServiceTools, ServiceSystemPrompt, ServiceProjections},
			Provide: []string{ServiceAgentLoop},
			Apply: func(ctx *cordis.Context, config any) error {
				loop, err := agentloop.NewAgentLoop(
					ctx,
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					deps.Logger,
					ctx.Get(ServiceLlm).(*llm.Runtime),
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					ctx.Get(ServiceProjections).(*projection.Registry),
					agentloop.AgentLoopConfig{},
				)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceAgentLoop, loop)
				return nil
			},
		}
	},

	// The subagent runtime + continuation manager, production-composed:
	// the manager's extension services come from the composition (host =
	// runtime, snapshots = persistence coordinator, sandbox overrides =
	// permission presets, child world = prompt + tool registry), and the
	// child runtime installs the agent loop under this composition context
	// as the structural activation owner. Provider plugins
	// (spawn/fork-in-process) register on the provided runtime.
	"@deepseek-ai/dsh-subagent": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{
				ServiceAgents, ServiceAgentLoop, ServiceSessionPersist,
				ServiceTools, ServiceSystemPrompt, ServicePermissionPresets,
				ServiceUserApproval,
			},
			Provide: []string{ServiceSubagentRuntime},
			Apply: func(ctx *cordis.Context, config any) error {
				registry := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				runtime := subagent.NewSubagentRuntime(subagent.RuntimeConfig{
					Logger: deps.Logger,
					Events: registry.Events(),
				})
				manager := subagent.NewSubagentContinuationManager(subagent.ManagerDeps{
					Logger: deps.Logger,
					Agents: registry,
					Setup:  runtime.SetupRegistry(),
				})
				manager.SetChildRuntime(
					ctx.Get(ServiceAgentLoop).(subagent.ChildRuntime), ctx)
				manager.SetManagerExt(subagent.ManagerExt{
					Host:      runtime,
					Snapshots: ctx.Get(ServiceSessionPersist).(subagent.SnapshotLister),
					Composition: subagent.ChildCompositionDeps{
						Prompt:   ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
						Registry: ctx.Get(ServiceTools).(*tools.ToolRuntime),
					},
					Sandbox: ctx.Get(ServicePermissionPresets).(subagent.SandboxOverrideService),
					// The approval service is in the inject list: a
					// profile composing subagents composes approval.
					HasApproval: true,
					// The LLM registry backs the image-capability gate as an
					// optional read (official ctx.get('llm')): an LLM-less
					// composition leaves it nil and the gate defers to the
					// text-only projection.
					LLM: optionalLLM(ctx),
				})
				runtime.SetContinuations(manager)
				ctx.Provide(ServiceSubagentRuntime, runtime)
				return nil
			},
		}
	},

	// The in-process spawn provider: each child is a fresh child agent on
	// the same process (own session, own system prompt, zero parent
	// context). Config.providerName overrides the registry name (spawn).
	"@deepseek-ai/dsh-subagent-spawn-in-process": inProcessProviderSpec("spawn"),
	// The in-process fork provider: the child is seeded with the parent's
	// completed-turn prefix. Config.providerName overrides fork.
	"@deepseek-ai/dsh-subagent-fork-in-process": inProcessProviderSpec("fork"),
	// The shipped base bundle names the in-process delegation providers by
	// their upstream package names; both spellings resolve to the same
	// provider builder (official config providerName overrides).
	"@deepseek-ai/dsh-subagent-spawn": inProcessProviderSpec("spawn"),
	"@deepseek-ai/dsh-subagent-fork":  inProcessProviderSpec("fork"),

	// The Go-realm workflow engine: child runs fan out through the
	// subagent runtime (the official workflowEngine inject; the official
	// worker-thread realm has no Go counterpart — the engine executes
	// native programs).
	"@deepseek-ai/dsh-workflow": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubagentRuntime},
			Provide: []string{ServiceWorkflowEngine},
			Apply: func(ctx *cordis.Context, config any) error {
				engine, err := workflow.NewEngine(workflow.EngineOptions{
					Sink:     workflow.NewEventSink(sessionLogger{logger: deps.Logger}),
					Children: ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
				})
				if err != nil {
					return err
				}
				ctx.Provide(ServiceWorkflowEngine, engine)
				return nil
			},
		}
	},

	// The ralph tool: the model-facing foreground fresh-agent loop over
	// the workflow engine and the structured-output subagent seam.
	"@deepseek-ai/dsh-tool-ralph": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{ServiceTools, ServiceWorkflowEngine, ServiceSubagentRuntime, ServiceSystemPrompt, ServiceAgents},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg toolralph.Config
				if overridden, ok := config.(map[string]any); ok {
					if err := decodeConfigJSON(overridden, &cfg); err != nil {
						return err
					}
				}
				undo, err := toolralph.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					ctx.Get(ServiceWorkflowEngine).(workflow.Engine),
					ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
					cfg,
				)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(undo), nil })
			},
		}
	},
}

// inProcessProviderSpec builds one in-process provider's spec: shared
// composition, configurable registry name, registration on the runtime.
func inProcessProviderSpec(defaultName string) pluginBuilder {
	return func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{
				ServiceSubagentRuntime, ServiceAgentLoop,
				ServiceSystemPrompt, ServiceTools,
				ServicePermissionPresets, ServiceUserApproval,
			},
			Apply: func(ctx *cordis.Context, config any) error {
				name := defaultName
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["providerName"].(string); ok && raw != "" {
						name = raw
					}
				}
				runtime := ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime)
				provider, err := subagent.NewInProcessProvider(name, defaultName, subagent.InProcessProviderDeps{
					Children:    ctx.Get(ServiceAgentLoop).(subagent.ChildRuntime),
					Owner:       ctx,
					Sandbox:     ctx.Get(ServicePermissionPresets).(subagent.SandboxOverrideService),
					HasApproval: true,
					Prompt:      ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
					Registry:    ctx.Get(ServiceTools).(*tools.ToolRuntime),
				})
				if err != nil {
					return err
				}
				_, err = runtime.RegisterProvider(provider)
				return err
			},
		}
	}
}

// toolsCallerOf resolves the calling agent's session id for one execution:
// agent identity is the scope key, resolved against the live registry.
func toolsCallerOf(agents *agent.AgentRegistry) toolsjobs.CallerOf {
	return func(exec *tools.ToolExecution) string {
		if caller := agents.ByScope(exec.Agent); caller != nil {
			return string(caller.Session.ID())
		}
		return ""
	}
}

// buildDelegationTool builds the model-facing delegation tool rows (official
// dsh-tool-subagent: the `agent` spawn surface — one package, two bundle rows
// with different providers). Provider must precede this entry in the
// composition (static order replaces the official late-mount listeners).
func buildDelegationTool(defaultProvider string, defaultToolName string) pluginBuilder {
	return func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceSubagentRuntime, ServiceSystemPrompt, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg toolsubagent.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				if cfg.Provider == "" {
					cfg.Provider = defaultProvider
				}
				if cfg.ToolName == "" {
					cfg.ToolName = defaultToolName
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				prompt := (*systemprompt.SystemPrompt)(nil)
				if candidate := ctx.Get(ServiceSystemPrompt); candidate != nil {
					prompt = candidate.(*systemprompt.SystemPrompt)
				}
				jobsRegistry := (*jobs.LocalRegistry)(nil)
				if candidate := ctx.Get(ServiceJobs); candidate != nil {
					jobsRegistry = candidate.(*jobs.LocalRegistry)
				}
				llmRuntime := (*llm.Runtime)(nil)
				if candidate := ctx.Get(ServiceLlm); candidate != nil {
					llmRuntime = candidate.(*llm.Runtime)
				}
				selection := (*toolsubagent.SubagentModelSelectionConfig)(nil)
				if candidate := ctx.Get(ServiceSubagentModelSelection); candidate != nil {
					selection = candidate.(*toolsubagent.SubagentModelSelectionConfig)
				}
				_, err := toolsubagent.Register(toolsubagent.Deps{
					Runtime:      ctx.Get(ServiceTools).(*tools.ToolRuntime),
					Prompt:       prompt,
					Subagents:    ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
					Jobs:         jobsRegistry,
					Llm:          llmRuntime,
					Selection:    selection,
					ResolveAgent: agentResolverOf(agents),
				}, cfg)
				return err
			},
		}
	}
}

// checkpointFlusherOf adapts the persistence coordinator onto the
// checkpoint flusher seam: a checkpoint requires the live session's
// write-behind queue durable through its input.
func checkpointFlusherOf(coordinator *persistence.Coordinator, sessions *session.Store) checkpointpolicy.Flusher {
	return flusherFunc(func(sessionID string) error {
		sess := sessions.Get(session.SessionID(sessionID))
		if sess == nil {
			return fmt.Errorf("session-checkpoint-policy: session %q is not live", sessionID)
		}
		return coordinator.FlushSession(sess)
	})
}

// flusherFunc lifts a function into the checkpoint Flusher seam.
type flusherFunc func(sessionID string) error

func (f flusherFunc) FlushSession(sessionID string) error { return f(sessionID) }

// agentResolverOf adapts the live agent registry onto the by-scope
// resolver seam.
func agentResolverOf(agents *agent.AgentRegistry) func(tools.ScopeKey) *agent.Agent {
	return agents.ByScope
}

// buildShellTool builds the model-facing shell tool plugin spec (shared by
// the dsh-tool-bash and dsh-tool-pwsh rows): injects the tool runtime, the
// composed shell executor, the prompt, the managed DSH_* registry, and the
// optional jobs service; the executor flavor picks the tool identity.
func buildShellTool() pluginBuilder {
	return func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceShell, ServiceSystemPrompt, ServiceShellEnv, ServiceJobs, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				prompt := (*systemprompt.SystemPrompt)(nil)
				if candidate := ctx.Get(ServiceSystemPrompt); candidate != nil {
					prompt = candidate.(*systemprompt.SystemPrompt)
				}
				jobsRegistry := (*jobs.LocalRegistry)(nil)
				if candidate := ctx.Get(ServiceJobs); candidate != nil {
					jobsRegistry = candidate.(*jobs.LocalRegistry)
				}
				toolDeps := shelltool.Deps{
					Runtime: ctx.Get(ServiceTools).(*tools.ToolRuntime),
					Prompt:  prompt,
					Shell:   ctx.Get(ServiceShell).(shell.ShellExecutor),
					Env:     ctx.Get(ServiceShellEnv).(*shell.ShellEnvRegistry),
					Jobs:    jobsRegistry,
					Agents:  agentResolverOf(ctx.Get(ServiceAgents).(*agent.AgentRegistry)),
				}
				cfg := shelltool.DefaultConfig()
				cfg.BackgroundEnabled = decodeConfigBool(config, "enableRunInBackground", true)
				return ctx.Effect(func() (cordis.Disposer, error) {
					undo, err := shelltool.Register(toolDeps, cfg)
					return cordis.Disposer(undo), err
				})
			},
		}
	}
}

// decodeConfigBool reads one boolean composition field with its default.
func decodeConfigBool(config any, key string, fallback bool) bool {
	if overridden, ok := config.(map[string]any); ok {
		if raw, ok := overridden[key].(bool); ok {
			return raw
		}
	}
	return fallback
}

var batchThreeBuilders = map[string]pluginBuilder{
	// The skills registry: filesystem skill catalogs merged per cwd/provider.
	"@deepseek-ai/dsh-skill": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceSkills},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg skill.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				registry, err := skill.NewRegistry(deps.Logger, cfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSkills, registry)
				return nil
			},
		}
	},

	// The skill tool: lists and loads skills from the registry.
	"@deepseek-ai/dsh-tool-skill": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceSkills, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg toolskill.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				_, err := toolskill.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSkills).(*skill.Registry),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					deps.Logger,
					cfg,
				)
				return err
			},
		}
	},

	// The todo tool: per-session todo tracking. The parallel-in-progress
	// discipline is a required deployment choice (config.allowParallel,
	// default single-active).
	"@deepseek-ai/dsh-tool-todo": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceAgents, ServiceProjections},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				_, err := todo.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					ctx.Get(ServiceProjections).(*projection.Registry),
					todo.Config{AllowParallelInProgress: decodeConfigBool(config, "allowParallel", false)},
				)
				return err
			},
		}
	},

	// The local jobs registry: in-memory background job ownership.
	"@deepseek-ai/dsh-jobs-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceJobs},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg jobs.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				registry, err := jobs.NewLocalRegistry(cfg, deps.Logger)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceJobs, registry)
				return nil
			},
		}
	},

	// The jobs tool: job submit/status/cancel for the model.
	"@deepseek-ai/dsh-tool-jobs": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceJobs, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg toolsjobs.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				_, err := toolsjobs.RegisterTools(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceJobs).(*jobs.LocalRegistry),
					toolsCallerOf(agents),
					cfg,
				)
				return err
			},
		}
	},

	// Plan mode: the /plan command and its exit tool over one controller.
	// The plan projection unit registers here: the controller's state reads
	// go through the registry (the official require-the-registry seam).
	"@deepseek-ai/dsh-plan-mode": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceCommands, ServiceTools, ServiceUserQuestions, ServiceProjections},
			Provide: []string{ServicePlanMode},
			Apply: func(ctx *cordis.Context, config any) error {
				section := "plan"
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["section"].(string); ok && raw != "" {
						section = raw
					}
				}
				projections := ctx.Get(ServiceProjections).(*projection.Registry)
				if _, err := projections.Register(planmode.ProjectionDefinition()); err != nil {
					return err
				}
				controller, err := planmode.NewController(section, projections)
				if err != nil {
					return err
				}
				detach, err := planmode.RegisterPlanCommand(
					ctx.Get(ServiceCommands).(*commands.CommandRuntime), controller)
				if err != nil {
					return err
				}
				if _, err := planmode.RegisterExitTool(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceUserQuestions).(*userquestions.Service),
					controller,
				); err != nil {
					detach()
					return err
				}
				ctx.Provide(ServicePlanMode, controller)
				return nil
			},
		}
	},

	// The repeat-tool reminder: post-execution guidance when the model
	// repeats unproductive calls; pre-step reset per turn.
	"@deepseek-ai/dsh-repeat-tool-reminder": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg guard.RepeatConfig
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				reminder, err := guard.NewRepeatToolReminder(cfg)
				if err != nil {
					return err
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				detachTools := reminder.Attach(ctx.Get(ServiceTools).(*tools.ToolRuntime))
				detachReset := reminder.AttachPreStepReset(agents)
				if err := ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() {
						detachTools()
						detachReset()
					}), nil
				}); err != nil {
					return err
				}
				return nil
			},
		}
	},

	// Persisted projection cache (official dsh-session-projection-cache):
	// durable per-session checkpoint records on the session_projcache
	// storage domain (per-record layout), throttled write-behind, and the
	// cached listing read. Go composition: the sessions flush lives on the
	// persistence coordinator, so the Sessions view adapts store+coordinator
	// (the official sessions service carries both).
	"@deepseek-ai/dsh-session-projection-cache": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceStorageDomain, ServiceProjections, ServiceSessions, ServiceSessionPersist},
			Provide: []string{ServiceProjectionCache},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := projectioncache.Config{WriteEveryEvents: 200, WriteIntervalMs: 5000}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["writeEveryEvents"].(float64); ok && raw >= 1 {
						cfg.WriteEveryEvents = int(raw)
					}
					if raw, ok := overridden["writeIntervalMs"].(float64); ok && raw >= 1 {
						cfg.WriteIntervalMs = int64(raw)
					}
				}
				spec, err := projectioncache.DomainSpec()
				if err != nil {
					return err
				}
				facility := ctx.Get(ServiceStorageDomain).(*storagedomain.Facility)
				domain, err := facility.Open(spec)
				if err != nil {
					return err
				}
				store, err := projectioncache.NewDomainStore(domain)
				if err != nil {
					_ = domain.Close()
					return err
				}
				sessions := ctx.Get(ServiceSessions).(*session.Store)
				coordinator := ctx.Get(ServiceSessionPersist).(*persistence.Coordinator)
				service, err := projectioncache.New(store,
					ctx.Get(ServiceProjections).(*projection.Registry),
					sessionsFlushView{store: sessions, coordinator: coordinator},
					deps.Logger, cfg)
				if err != nil {
					_ = domain.Close()
					return err
				}
				detach := service.Attach(ctx)
				ctx.Provide(ServiceProjectionCache, service)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(func() {
						detach()
						_ = service.Close()
					}), nil
				})
			},
		}
	},
	// The singleton replay-aware token meter plus its three O(1)
	// projection units (usage accumulation, context occupancy, context
	// composition). Image route pricing is an optional llm seam; the
	// default composition leaves it nil so image pricing falls back to
	// estimation.
	"@deepseek-ai/dsh-token-meter": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceProjections},
			Provide: []string{ServiceTokenMeter},
			Apply: func(ctx *cordis.Context, config any) error {
				registry := ctx.Get(ServiceProjections).(*projection.Registry)
				for _, unit := range []projection.Definition{
					tokenmeter.TokenUsageUnit(),
					tokenmeter.ContextPressureUnit(),
					tokenmeter.ContextBreakdownUnit(),
				} {
					undo, err := registry.Register(unit)
					if err != nil {
						return err
					}
					if err := ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(undo), nil }); err != nil {
						return err
					}
				}
				ctx.Provide(ServiceTokenMeter, tokenmeter.NewMeter(nil))
				return nil
			},
		}
	},

	// The replay-aware compaction engine: config resolved fail-loud at
	// composition; summarization rides the llm runtime, capacity resolution
	// rides the same runtime's model info, durability rides the persistence
	// coordinator's flush. The tool-result pruner is an optional
	// composition: mount the pruner entry BEFORE this entry (cordis apply
	// order, matching the official README), or compaction composes without
	// it.
	"@deepseek-ai/dsh-compaction-basic": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceLlm, ServiceTokenMeter, ServiceSessionPersist},
			Provide: []string{ServiceCompaction},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg compactionbasic.BasicConfig
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				runtime := ctx.Get(ServiceLlm).(*llm.Runtime)
				engineConfig := compactionbasic.EngineConfig{
					LLM:       runtime,
					Meter:     ctx.Get(ServiceTokenMeter).(*tokenmeter.Meter),
					Logger:    deps.Logger,
					ModelInfo: runtime,
					Flusher:   ctx.Get(ServiceSessionPersist).(*persistence.Coordinator),
				}
				if pruner := ctx.Get(ServiceToolResultPruner); pruner != nil {
					engineConfig.Pruner = prunerAdapter{pruner.(*toolresultpruner.Pruner)}
				}
				engine, err := compactionbasic.NewEngine(cfg, engineConfig)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceCompaction, engine)
				return nil
			},
		}
	},

	// The model-free tool-result pruner: deterministic head/middle/tail
	// budgets over the current tool-result surface, replacements priced by
	// the adjacent compaction/prune shadow-price event. Pricing rides the
	// same fixed estimator the token meter's fold uses, so no service
	// dependency is declared.
	"@deepseek-ai/dsh-compaction-tool-result-pruner": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceToolResultPruner},
			Apply: func(ctx *cordis.Context, config any) error {
				decoded, err := toolresultpruner.DecodeConfig(config)
				if err != nil {
					return err
				}
				resolved, err := toolresultpruner.ResolveConfig(decoded)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceToolResultPruner, toolresultpruner.New(resolved))
				return nil
			},
		}
	},

	// The /compact command: manual compaction for the receiving agent; the
	// invocation binds the maintenance owner at dispatch time.
	"@deepseek-ai/dsh-command-compact": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{Inject: []string{ServiceCommands, ServiceCompaction},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				engine := ctx.Get(ServiceCompaction).(*compactionbasic.Engine)
				_, err := compactionbasic.RegisterCompactCommand(
					ctx.Get(ServiceCommands).(*commands.CommandRuntime),
					func(invocation commands.Invocation, signal context.Context) (*compaction.Result, error) {
						if invocation.Agent == nil {
							return nil, fmt.Errorf("/compact requires a receiving agent")
						}
						owner := maintenanceOwner{
							AgentView: compactionbasic.ViewAgent(invocation.Agent),
							driver:    invocation.Agent.Driver(),
						}
						return engine.CompactNow(owner, signal, compaction.CommandID(invocation.CommandID))
					},
				)
				return err
			},
		}
	},

	// The subagent control surface: send_message, interrupt_agent, and
	// list_agents over the runtime's followup/interrupt and the continuable
	// listing's projection fold.
	"@deepseek-ai/dsh-tool-subagent-control": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceSubagentRuntime, ServiceAgents, ServiceSessions, ServiceProjections, ServiceSessionPersist},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				_, err := subagentcontrol.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					subagentcontrol.ListingDeps{
						Store:       ctx.Get(ServiceSessions).(*session.Store),
						Projections: ctx.Get(ServiceProjections).(*projection.Registry),
						Coordinator: ctx.Get(ServiceSessionPersist).(*persistence.Coordinator),
					},
				)
				return err
			},
		}
	},

	// The str_replace_editor tool over the mounted fs backend; the fs
	// service arrives with the dsh-fs-sandbox composition, so a profile
	// listing the editor without a filesystem backend fails loud at inject
	// time (correct composition discipline, not a gap). The sandbox policy
	// service is optional at inject: required by the tool exactly when the
	// mounted backend confines.
	"@deepseek-ai/dsh-tool-str-replace-editor": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceFS, ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg strreplaceeditor.Config
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				depsEditor := strreplaceeditor.Deps{
					FS:     ctx.Get(ServiceFS).(fs.FileSystem),
					Ctx:    ctx,
					Agents: ctx.Get(ServiceAgents).(*agent.AgentRegistry),
				}
				if policy := ctx.Get(ServiceSandboxPolicy); policy != nil {
					depsEditor.Policy = &mutationPolicyResolver{
						service: policy.(*sandboxpolicy.Service),
						agents:  depsEditor.Agents,
					}
				}
				_, err := strreplaceeditor.Register(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					depsEditor,
					cfg,
				)
				return err
			},
		}
	},

	// The sandbox policy home: deployment default mode and fallback
	// workspace root. The fail-safe default is read-only; a deployment that
	// wants a workspace-writable agent opts in explicitly via config.
	"@deepseek-ai/dsh-sandbox-policy": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{},
			Provide: []string{ServiceSandboxPolicy},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					Mode          string `json:"mode"`
					WorkspaceRoot string `json:"workspaceRoot"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				service, err := sandboxpolicy.NewService(sandboxpolicy.Config{Mode: cfg.Mode, WorkspaceRoot: cfg.WorkspaceRoot}, "")
				if err != nil {
					return err
				}
				ctx.Provide(ServiceSandboxPolicy, service)
				return nil
			},
		}
	},

	// The process-confinement provider seam (official dsh-sandbox-local).
	// This composition provides the sandbox service with a fail-closed
	// provider: confine always refuses with SANDBOX_UNAVAILABLE until a
	// native enforcement runner (Windows ACL restricted-token, Linux
	// bwrap+Landlock, macOS Seatbelt) is composed — "missing or unusable
	// confinement fails closed rather than returning the original argv", the
	// official semantics. The native runner bodies are a dedicated
	// security-validated round (recorded in ROADMAP); the seam vocabulary
	// itself is fully ported in the sandbox package.
	"@deepseek-ai/dsh-sandbox-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{},
			Provide: []string{ServiceSandbox},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceSandbox, sandbox.FailClosedProvider{})
				return nil
			},
		}
	},

	// The sandbox-consuming bash executor (official dsh-bash-sandbox):
	// composes INSTEAD of dsh-bash-local; requires the sandbox provider.
	// With the fail-closed default provider every command fails with
	// SANDBOX_UNAVAILABLE (the correct behavior when no native runner is
	// available). The tool layer is unchanged.
	"@deepseek-ai/dsh-bash-sandbox": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubprocess, ServiceSandbox, ServiceShellEnv},
			Provide: []string{ServiceShell},
			Apply: func(ctx *cordis.Context, config any) error {
				executorCfg, err := decodeShellLocalConfig(config, false)
				if err != nil {
					return err
				}
				local, err := shelllocal.NewBash(ctx.Get(ServiceSubprocess).(subprocess.Runtime), executorCfg)
				if err != nil {
					return err
				}
				executor, err := sandboxshell.NewBashSandbox(local, ctx.Get(ServiceSandbox).(sandbox.Provider))
				if err != nil {
					return err
				}
				ctx.Provide(ServiceShell, executor)
				return nil
			},
		}
	},

	// The sandbox-consuming pwsh executor (official dsh-pwsh-sandbox):
	// composes INSTEAD of dsh-pwsh-local; requires the sandbox provider.
	"@deepseek-ai/dsh-pwsh-sandbox": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubprocess, ServiceSandbox, ServiceShellEnv},
			Provide: []string{ServiceShell},
			Apply: func(ctx *cordis.Context, config any) error {
				executorCfg, err := decodeShellLocalConfig(config, true)
				if err != nil {
					return err
				}
				local, err := shelllocal.NewPwsh(ctx.Get(ServiceSubprocess).(subprocess.Runtime), executorCfg)
				if err != nil {
					return err
				}
				executor, err := sandboxshell.NewPwshSandbox(local, ctx.Get(ServiceSandbox).(sandbox.Provider))
				if err != nil {
					return err
				}
				ctx.Provide(ServiceShell, executor)
				return nil
			},
		}
	},

	// The sandbox-enforcing filesystem backend: the fs service the
	// model-facing tools consume. Loads INSTEAD of the bare local backend;
	// the swap with a sandbox-policy service is the whole composition.
	"@deepseek-ai/dsh-fs-sandbox": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSandboxPolicy},
			Provide: []string{ServiceFS},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					Cwd string `json:"cwd"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				local, err := fslocal.New(fslocal.Config{Cwd: cfg.Cwd})
				if err != nil {
					return err
				}
				ctx.Provide(ServiceFS, fssandbox.New(local, ctx.Get(ServiceSandboxPolicy).(*sandboxpolicy.Service)))
				return nil
			},
		}
	},

	// The subprocess capability seam (`ctx.subprocess`): execution-world
	// fully specified managed process trees with raw or collected stdio,
	// bounded output with spill recovery, and tree-scoped termination.
	// Command defaulting, shell semantics, deadlines, protocol framing, and
	// presentation belong to consumers (fs-search, the bash executor seam).
	// The official terminal-process primitive (pty allocation) is a
	// documented deferral, not a silent gap.
	"@deepseek-ai/dsh-subprocess-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{},
			Provide: []string{ServiceSubprocess},
			Apply: func(ctx *cordis.Context, config any) error {
				ctx.Provide(ServiceSubprocess, subprocess.NewLocal())
				return nil
			},
		}
	},

	// The local bash executor (dsh-bash-local): public commands run as
	// `bash -c` in a managed process tree through the subprocess seam.
	// Command defaulting, deadlines and cause classification, the
	// model-friendly terminal environment (NO_COLOR/TERM/PAGER overrides
	// under the caller's env, under the trusted DSH_* snapshot), and the
	// model-facing stdout/stderr merge for background reads live here;
	// execution policy belongs to the pre-execute gate or a confining
	// executor. One provider of ctx.shell per host: the win32 layer swaps
	// this row for the pwsh one, and mounting both fails loud.
	"@deepseek-ai/dsh-bash-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubprocess},
			Provide: []string{ServiceShell},
			Apply: func(ctx *cordis.Context, config any) error {
				executorCfg, err := decodeShellLocalConfig(config, false)
				if err != nil {
					return err
				}
				executor, err := shelllocal.NewBash(ctx.Get(ServiceSubprocess).(subprocess.Runtime), executorCfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceShell, executor)
				return nil
			},
		}
	},

	// The local PowerShell executor (dsh-pwsh-local): the pwsh twin of the
	// bash row — UTF-8 preamble + -NoLogo -NoProfile -NonInteractive
	// -Command, executable resolved once from an explicit pwshPath or the
	// well-known Windows locations (PS7 install, PATH entries, then the
	// 5.1 fallback).
	"@deepseek-ai/dsh-pwsh-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSubprocess},
			Provide: []string{ServiceShell},
			Apply: func(ctx *cordis.Context, config any) error {
				executorCfg, err := decodeShellLocalConfig(config, true)
				if err != nil {
					return err
				}
				executor, err := shelllocal.NewPwsh(ctx.Get(ServiceSubprocess).(subprocess.Runtime), executorCfg)
				if err != nil {
					return err
				}
				ctx.Provide(ServiceShell, executor)
				return nil
			},
		}
	},

	// The managed shell environment registry (`ctx.shellEnv`): owns the
	// trusted, per-execution DSH_* variables the model-facing shell tools
	// inject. Built-in facts (DSH_HOME, DSH_SHELL, DSH_SESSION_ID) are
	// registry-owned; plugins contribute more with declared, ownership-
	// checked keys. The official load also registers the persistence
	// contributor (DSH_SESSION_JSONL); the Go session/persistence JSONL
	// path is not composed yet, so that contributor lands with it.
	"@deepseek-ai/dsh-shell-env": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{ServiceShellEnv},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					DshHome string `json:"dshHome"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				ctx.Provide(ServiceShellEnv, shell.NewShellEnvRegistry(cfg.DshHome, agentResolverOf(ctx.Get(ServiceAgents).(*agent.AgentRegistry))))
				return nil
			},
		}
	},

	// The local file-reference discovery owner (official
	// dsh-file-reference-local): one per-agent workspace index over the
	// seam, invalidated on tool results and disposed with the agent. The
	// `context:file-reference` prompt section installs host-wide.
	"@deepseek-ai/dsh-file-reference-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents, ServiceTools, ServiceSystemPrompt},
			Provide: []string{ServiceFileReference},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					MaxResults          *int     `json:"maxResults"`
					MaxEntries          *int     `json:"maxEntries"`
					ExcludedDirectories []string `json:"excludedDirectories"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				searchConfig := filereference.DefaultServiceConfig()
				if cfg.MaxResults != nil {
					searchConfig.MaxResults = *cfg.MaxResults
				}
				if cfg.MaxEntries != nil {
					searchConfig.MaxEntries = *cfg.MaxEntries
				}
				if cfg.ExcludedDirectories != nil {
					searchConfig.ExcludedDirectories = cfg.ExcludedDirectories
				}
				service, err := filereference.NewService(searchConfig)
				if err != nil {
					return err
				}
				agents := ctx.Get(ServiceAgents).(*agent.AgentRegistry)
				agents.Events().OnEmit(agent.EventAgentDisposed, nil, func(payload any) error {
					if typed, ok := payload.(agent.AgentLifecyclePayload); ok {
						service.DisposeAgent(string(typed.Agent.ID))
					}
					return nil
				})
				ctx.Get(ServiceTools).(*tools.ToolRuntime).OnResult(nil, func(exec *tools.ToolExecution, _ *tools.ToolExecutionResult) {
					if exec.Agent != nil {
						if resolved := agents.ByScope(exec.Agent); resolved != nil {
							service.InvalidateAgent(string(resolved.ID))
						}
					}
				})
				prompt := ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt)
				if _, err := prompt.Section(nil, systemprompt.PromptSection{
					Name:  "context:file-reference",
					Order: systemprompt.OrderFileReference,
					TextProvider: func(systemprompt.AssembleContext) string {
						return filereference.FileReferencePrompt
					},
				}); err != nil {
					return err
				}
				ctx.Provide(ServiceFileReference, service)
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(service.Dispose), nil })
			},
		}
	},

	// The model-facing shell tools (dsh-tool-bash + dsh-tool-pwsh): one
	// tool per composed executor flavor, sharing one surface — command/
	// description/timeoutMs/workdir, run_in_background through the jobs
	// seam when the jobs service is composed, foreground results rendered
	// with the exit-status markers. The profile composes exactly one row:
	// tool-bash on POSIX, tool-pwsh on win32.
	"@deepseek-ai/dsh-tool-bash": buildShellTool(),
	"@deepseek-ai/dsh-tool-pwsh": buildShellTool(),

	// The filesystem discovery tool suite (`glob`, `grep`): foreground
	// spawns of a ripgrep binary through the subprocess seam with fixed
	// argv templates — no shell layer, no model-visible background task.
	// The Go deployment resolves `rg` from PATH (the npm package ships
	// @vscode/ripgrep; the binary question is a deployment fact) — a
	// missing binary fails SEARCH_FAILED at the first call, not the
	// composition. Search cards (presentationMeta) stay with the
	// presentation round, as for every tool.
	"@deepseek-ai/dsh-tool-fs-search": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceSubprocess, ServiceSystemPrompt},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					GlobMaxResults           *int   `json:"globMaxResults"`
					SampleOverCapGlobResults *bool  `json:"sampleOverCapGlobResults"`
					GrepMaxMatches           *int   `json:"grepMaxMatches"`
					GrepMaxLineBytes         *int   `json:"grepMaxLineBytes"`
					RawOutputMaxBytes        *int   `json:"rawOutputMaxBytes"`
					GraceMs                  *int   `json:"graceMs"`
					TimeoutMs                *int   `json:"timeoutMs"`
					RGPath                   string `json:"rgPath"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				caps := fssearch.DefaultCaps()
				if cfg.GlobMaxResults != nil {
					caps.GlobMaxResults = *cfg.GlobMaxResults
				}
				if cfg.SampleOverCapGlobResults != nil {
					caps.SampleOverCapGlobResults = *cfg.SampleOverCapGlobResults
				}
				if cfg.GrepMaxMatches != nil {
					caps.GrepMaxMatches = *cfg.GrepMaxMatches
				}
				if cfg.GrepMaxLineBytes != nil {
					caps.GrepMaxLineBytes = *cfg.GrepMaxLineBytes
				}
				if cfg.RawOutputMaxBytes != nil {
					caps.RawOutputMaxBytes = *cfg.RawOutputMaxBytes
				}
				if cfg.GraceMs != nil {
					caps.GraceMs = *cfg.GraceMs
				}
				if cfg.TimeoutMs != nil {
					caps.TimeoutMs = *cfg.TimeoutMs
				}
				caps.RGPath = cfg.RGPath
				var prompt *systemprompt.SystemPrompt
				if candidate := ctx.Get(ServiceSystemPrompt); candidate != nil {
					prompt = candidate.(*systemprompt.SystemPrompt)
				}
				undo, err := fssearch.Register(ctx.Get(ServiceTools).(*tools.ToolRuntime), prompt, ctx, caps)
				if err != nil {
					return err
				}
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(undo), nil
				})
			},
		}
	},

	// The provider-routed request-retry policy (official dsh-llm-retry):
	// normal or unbounded recovery on the agent/request-error waterfall.
	// This executor has no config; providers own retryPolicy.
	"@deepseek-ai/dsh-llm-retry": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceAgents},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var decoded map[string]any
				if config != nil {
					if err := decodeConfigJSON(config, &decoded); err != nil {
						return err
					}
				}
				if err := llmretry.ValidateConfig(decoded); err != nil {
					return err
				}
				_, err := llmretry.Register(
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					deps.Logger,
					llmretry.Internals{},
				)
				return err
			},
		}
	},
	// The separately loadable discovery tool (official
	// tool-subagent-control/list-agents): list_agents alone, without the
	// send_message/interrupt_agent delivery surface. Deviation recorded: the
	// Go listing services are injected seams (official resolves them from
	// ctx.subagents internals).
	"@deepseek-ai/dsh-tool-subagent-control/list-agents": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceSubagentRuntime, ServiceAgents, ServiceSessions, ServiceProjections, ServiceSessionPersist},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				_, err := subagentcontrol.RegisterListAgents(
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSubagentRuntime).(*subagent.SubagentRuntime),
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					subagentcontrol.ListingDeps{
						Store:       ctx.Get(ServiceSessions).(*session.Store),
						Projections: ctx.Get(ServiceProjections).(*projection.Registry),
						Coordinator: ctx.Get(ServiceSessionPersist).(*persistence.Coordinator),
					},
				)
				return err
			},
		}
	},
	// The human-facing /feedback producer (official
	// dsh-command-feedback): one authoritative log-only event plus the
	// acknowledgement. The telemetry disclosure is "not configured" until a
	// session-telemetry backend is composed (the official optional read).
	"@deepseek-ai/dsh-command-feedback": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceCommands},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				runtime := ctx.Get(ServiceCommands).(*commands.CommandRuntime)
				_, err := commandfeedback.Register(runtime, commandfeedback.Options{
					Getenv: os.Getenv,
				})
				return err
			},
		}
	},

	// The durable message-feedback sidecar (official
	// dsh-message-feedback): lifecycle-bound feedback for finalized
	// assistant messages, inspected through the persistence coordinator and
	// stored in the storage-domain sidecar.
	"@deepseek-ai/dsh-message-feedback": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceStorageDomain, ServiceSessionPersist, ServiceSessions},
			Provide: []string{ServiceMessageFeedback},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					MaxNoteBytes *int64 `json:"maxNoteBytes"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				maxNoteBytes := int64(2000)
				if cfg.MaxNoteBytes != nil {
					maxNoteBytes = *cfg.MaxNoteBytes
				}
				service, err := messagefeedback.New(messagefeedback.Config{MaxNoteBytes: maxNoteBytes})
				if err != nil {
					return err
				}
				facility := ctx.Get(ServiceStorageDomain).(*storagedomain.Facility)
				domain, err := facility.Open(messagefeedback.Spec())
				if err != nil {
					return err
				}
				if err := service.Open(domain); err != nil {
					domain.Close()
					return err
				}
				service.SetDependencies(
					ctx.Get(ServiceSessionPersist).(*persistence.Coordinator),
					ctx.Get(ServiceSessions).(*session.Store),
				)
				ctx.Provide(ServiceMessageFeedback, service)
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(service.Close), nil })
			},
		}
	},

	// The OpenTelemetry session-telemetry backend (official
	// dsh-session-telemetry-otel): composes the OTel SDK pipeline and the
	// capture coordinator for the sharing policy. DISABLED (the default)
	// constructs no SDK state and warns when recorded feedback stays local;
	// FULL wires the live capture coordinator; FEEDBACK_ONLY wires an
	// on-demand coordinator that replays the canonical log through each
	// accepted feedback/record consent.
	"@deepseek-ai/dsh-session-telemetry-otel": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessions},
			Provide: []string{ServiceTelemetry},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := sessiontelemetryotel.Config{Mode: sessiontelemetryotel.ModeDisabled}
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["mode"].(string); ok {
						cfg.Mode = sessiontelemetryotel.Mode(raw)
					}
					if raw, ok := overridden["url"].(string); ok {
						cfg.URL = raw
					}
				}
				backend, err := sessiontelemetryotel.New(cfg)
				if err != nil {
					return err
				}
				store := ctx.Get(ServiceSessions).(*session.Store)
				var detach cordis.Disposer
				switch cfg.Mode {
				case sessiontelemetryotel.ModeFull:
					coord := sessiontelemetry.NewCoordinator(store, nil, deps.Logger, backend, nil)
					detach = func() { coord.Shutdown() }
				case sessiontelemetryotel.ModeFeedbackOnly:
					coord := sessiontelemetry.NewOnDemandCoordinator(store, deps.Logger, backend, nil)
					store.OnEvent(func(sess *session.Session, event session.Event) {
						if event.Type != "feedback/record" {
							return
						}
						coord.CaptureSession(sess, event.Seq)
					})
					detach = func() { coord.Shutdown() }
				default:
					// DISABLED: no SDK state, no coordinator. The backend
					// still owns the sharing disclosure.
					detach = func() {}
				}
				ctx.Provide(ServiceTelemetry, backend)
				return ctx.Effect(func() (cordis.Disposer, error) { return detach, nil })
			},
		}
	},

	// The default model selection owner (official dsh-agent-default-model):
	// settings-backed live user layer over the composition entry; usable
	// without a settings provider.
	"@deepseek-ai/dsh-agent-default-model": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSettings},
			Provide: []string{ServiceAgentDefaultModel},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				store := ctx.Get(ServiceSettings)
				if store == nil {
					return fmt.Errorf("agent-default-model: the settings store is required")
				}
				service, _, err := agentdefaultmodel.RegisterSection(store.(*settings.Store), agentdefaultmodel.Settings{
					Provider: cfg.Provider,
					Model:    cfg.Model,
				})
				if err != nil {
					return err
				}
				ctx.Provide(ServiceAgentDefaultModel, service)
				return nil
			},
		}
	},

	// The Web Workspace registry (official dsh-workspace): durable
	// workspace rows over the storage domain, bootstrapped from the stored
	// session history and live against the session store.
	"@deepseek-ai/dsh-workspace": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceSessionPersist, ServiceSessions, ServiceStorageDomain},
			Provide: []string{ServiceWorkspace},
			Apply: func(ctx *cordis.Context, config any) error {
				registry, dispose, err := workspace.NewRegistry(context.Background(), workspace.RegistryHost{
					Persistence: persistenceListAdapter{coordinator: ctx.Get(ServiceSessionPersist).(*persistence.Coordinator)},
					Sessions:    liveSessionsAdapter{store: ctx.Get(ServiceSessions).(*session.Store)},
					Logger:      deps.Logger,
				}, ctx.Get(ServiceStorageDomain).(*storagedomain.Facility))
				if err != nil {
					return err
				}
				ctx.Provide(ServiceWorkspace, registry)
				return ctx.Effect(func() (cordis.Disposer, error) { return cordis.Disposer(dispose), nil })
			},
		}
	},

	// The agent preset roster and standing-mount table (official
	// dsh-agent-presets): shipped/user root discovery, authoring copies,
	// and one standing composition per mounted preset. Config names the
	// default and any deployment roots (base-bundle shape: `default`);
	// `shippedRoot` is the Go seam for the bundled preset directory the
	// official resolves from its own package. A composed settings store
	// carries the live user default override, exactly like the official
	// optional settings injection.
	"@deepseek-ai/dsh-agent-presets": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServiceAgentPresets},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					Default            string              `json:"default"`
					Roots              []preset.PresetRoot `json:"roots"`
					IncludeShippedRoot *bool               `json:"includeShippedRoot"`
					IncludeUserRoot    *bool               `json:"includeUserRoot"`
					ShippedRoot        string              `json:"shippedRoot"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				includeShippedRoot := true
				if cfg.IncludeShippedRoot != nil {
					includeShippedRoot = *cfg.IncludeShippedRoot
				}
				includeUserRoot := true
				if cfg.IncludeUserRoot != nil {
					includeUserRoot = *cfg.IncludeUserRoot
				}
				var defaultOverride func() (string, bool)
				var clearDefaultOverride func()
				if store, ok := ctx.Get(ServiceSettings).(*settings.Store); ok && store != nil {
					defaultOverride = func() (string, bool) {
						value, present := store.Section(preset.SettingsNamespace)["default"].(string)
						return value, present && strings.TrimSpace(value) != ""
					}
					clearDefaultOverride = func() {
						section := store.Section(preset.SettingsNamespace)
						cleared := make(map[string]any, len(section))
						for key, value := range section {
							if key != "default" {
								cleared[key] = value
							}
						}
						_ = store.ProviderPush(preset.SettingsNamespace, cleared)
					}
				}
				var invalidateStanding func(string)
				roster := preset.NewRoster(preset.Config{
					Default:            cfg.Default,
					Roots:              cfg.Roots,
					IncludeShippedRoot: includeShippedRoot,
					IncludeUserRoot:    includeUserRoot,
				}, preset.RosterOptions{
					ShippedRoot:          cfg.ShippedRoot,
					DefaultOverride:      defaultOverride,
					ClearDefaultOverride: clearDefaultOverride,
					InvalidateStanding: func(id string) {
						if invalidateStanding != nil {
							invalidateStanding(id)
						}
					},
				})
				mounts, err := NewPresetMounts(ctx, roster, deps)
				if err != nil {
					return err
				}
				invalidateStanding = mounts.Invalidate
				ctx.Provide(ServiceAgentPresets, mounts)
				return nil
			},
		}
	},

	// The webhook rule runtime (official dsh-webhook): fire-and-forget
	// rules whose only built-in action is the workspace-backed Session
	// creation transaction, composed here over the injected services
	// exactly like the official runtime's inject list.
	"@deepseek-ai/dsh-webhook": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject: []string{
				ServiceAgents, ServiceAgentDefaultModel, ServiceAgentPresets,
				ServicePermissionPresets, ServiceSessionTitle, ServiceWorkspace,
			},
			Provide: []string{ServiceWebhookRuntime},
			Apply: func(ctx *cordis.Context, config any) error {
				sessionDeps := webhook.SessionDeps{
					Logger:            deps.Logger,
					DefaultModel:      ctx.Get(ServiceAgentDefaultModel).(*agentdefaultmodel.Config).CurrentSelection,
					PermissionPresets: ctx.Get(ServicePermissionPresets).(*permissionpresets.Service),
					Presets:           ctx.Get(ServiceAgentPresets).(*preset.Mounts),
					Workspaces:        ctx.Get(ServiceWorkspace).(*workspace.Registry),
					Agents:            ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					Titles:            ctx.Get(ServiceSessionTitle).(*sessiontitle.Service),
				}
				runtime := webhook.NewWebhookRuntime(deps.Logger, func(
					delivery webhook.VerifiedWebhookDelivery,
					ruleID webhook.WebhookRuleID,
					request webhook.WebhookSessionRequest,
					signal context.Context,
				) error {
					return webhook.CreateWebhookSession(sessionDeps, delivery, ruleID, request, signal)
				})
				ctx.Provide(ServiceWebhookRuntime, runtime)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(runtime.Dispose), nil
				})
			},
		}
	},

	// Event-only filesystem observation policy (official
	// dsh-fs-observation-policy): the fs/write-intent and fs/edit-intent
	// single-slot decisions derive from recorded fs/observed state; without
	// it tools keep the bare provider's unconditional mutation behavior.
	"@deepseek-ai/dsh-fs-observation-policy": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				detach := fsobservationpolicy.Apply(ctx)
				return ctx.Effect(func() (cordis.Disposer, error) {
					return cordis.Disposer(detach), nil
				})
			},
		}
	},
	// Private content-addressed DSH_HOME attachment storage (official
	// dsh-attachment-local): the durable image store read_image commits
	// into, rooted at <home>/attachments/v1. Limits decode from config;
	// unset fields keep the store's defaults.
	"@deepseek-ai/dsh-attachment-local": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{},
			Provide: []string{ServiceAttachments},
			Apply: func(ctx *cordis.Context, config any) error {
				cfg := local.Config{DSHHome: deps.Home}
				if overridden, ok := config.(map[string]any); ok {
					intOf := func(key string) int {
						if raw, ok := overridden[key].(float64); ok && raw >= 1 {
							return int(raw)
						}
						return 0
					}
					cfg.MaxImageBytes = intOf("maxImageBytes")
					cfg.MaxImagesPerMessage = intOf("maxImagesPerMessage")
					cfg.MaxMessageImageBytes = intOf("maxMessageImageBytes")
					cfg.MaxImagePixels = intOf("maxImagePixels")
					cfg.MaxImageDimension = intOf("maxImageDimension")
					cfg.NormalizedImageMaxPixels = intOf("normalizedImageMaxPixels")
					cfg.NormalizedImageMaxDimension = intOf("normalizedImageMaxDimension")
					cfg.NormalizedImageMaxBytes = intOf("normalizedImageMaxBytes")
				}
				ctx.Provide(ServiceAttachments, local.New(cfg))
				return nil
			},
		}
	},
	// The read/write/edit filesystem tool suite over the mounted fs
	// backend: schemas, read windows, observation events, and (under a
	// confining backend) the shared sandbox-escalation fields resolved
	// through the approval channel. read_image stays unregistered by the
	// source's own rule: it needs an attachment store, which the Go
	// composition does not mount yet.
	"@deepseek-ai/dsh-tool-fs": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceTools, ServiceFS, ServiceAgents, ServiceSystemPrompt, ServiceSandboxPolicy, ServiceUserApproval},
			Provide: []string{},
			Apply: func(ctx *cordis.Context, config any) error {
				var cfg struct {
					ReadLimit         *int `json:"readLimit"`
					ReadMaxLineLength *int `json:"readMaxLineLength"`
					ReadMaxBytes      *int `json:"readMaxBytes"`
					ReadStreamMinSize *int `json:"readStreamMinSize"`
				}
				if err := decodeConfigJSON(config, &cfg); err != nil {
					return err
				}
				caps := toolfs.DefaultCaps()
				if cfg.ReadLimit != nil {
					caps.Limit = *cfg.ReadLimit
				}
				if cfg.ReadMaxLineLength != nil {
					caps.MaxLineLength = *cfg.ReadMaxLineLength
				}
				if cfg.ReadMaxBytes != nil {
					caps.MaxBytes = *cfg.ReadMaxBytes
				}
				if cfg.ReadStreamMinSize != nil {
					caps.StreamMinSize = *cfg.ReadStreamMinSize
				}
				depsTools := toolfs.RegisterDeps{
					Backend: ctx.Get(ServiceFS).(fs.FileSystem),
					Ctx:     ctx,
					Agents:  toolfs.RegistryAgentSource{Registry: ctx.Get(ServiceAgents).(*agent.AgentRegistry)},
					PermissionFolds: func(caller *agent.Agent) string {
						if mode, ok := permissionpresets.EffectiveSandboxMode(caller.Session.Events()); ok {
							return mode
						}
						return ""
					},
				}
				if policy := ctx.Get(ServiceSandboxPolicy); policy != nil {
					depsTools.Policy = policy.(*sandboxpolicy.Service)
				}
				if approval := ctx.Get(ServiceUserApproval); approval != nil {
					depsTools.ApproverSource = approvalEscalationAdapter{service: approval.(*userapproval.Service)}
				}
				if store := ctx.Get(ServiceAttachments); store != nil {
					depsTools.Attachments = store.(toolfs.AttachmentStoreFace)
				}
				if llmRuntime := ctx.Get(ServiceLlm); llmRuntime != nil {
					depsTools.Llm = llmRuntime.(*llm.Runtime)
				}
				if prompt := ctx.Get(ServiceSystemPrompt); prompt != nil {
					system := prompt.(*systemprompt.SystemPrompt)
					sections := []systemprompt.PromptSection{
						{Name: "tool:read", Order: systemprompt.OrderToolRead, Text: "Use the read tool — not shell commands like cat — to inspect text files. Results include line numbers. Use offset and limit to continue reading large files."},
						{Name: "tool:write", Order: systemprompt.OrderToolWrite, Text: "Use the write tool to create files or completely replace file contents. Existing files are overwritten, so read an existing file first (the default fs-observation-policy requires it) and prefer edit for targeted changes."},
						{Name: "tool:edit", Order: systemprompt.OrderToolEdit, Text: "Use the edit tool for targeted changes to existing UTF-8 text files. It replaces literal old_string with new_string; by default old_string must appear exactly once. If old_string appears multiple times, provide a more specific old_string or set replace_all to true. Read the file first (the default fs-observation-policy requires it), unless you just created or edited it in this session."},
					}
					if err := ctx.Effect(func() (cordis.Disposer, error) {
						undos := make([]func(), 0, len(sections))
						for _, section := range sections {
							undo, err := system.Section(nil, section)
							if err != nil {
								return nil, err
							}
							undos = append(undos, undo)
						}
						return cordis.Disposer(func() {
							for _, undo := range undos {
								undo()
							}
						}), nil
					}); err != nil {
						return err
					}
				}
				return ctx.Effect(func() (cordis.Disposer, error) {
					undo, err := toolfs.Register(ctx.Get(ServiceTools).(*tools.ToolRuntime), depsTools, caps)
					return cordis.Disposer(undo), err
				})
			},
		}
	},
}

// prunerAdapter reduces the concrete tool-result pruner's result-bearing
// method onto the compaction engine's error-only face: the engine never
// consumes the prune result itself.
type prunerAdapter struct{ pruner *toolresultpruner.Pruner }

func (a prunerAdapter) PruneSession(sess *session.Session) error {
	_, err := a.pruner.PruneSession(sess)
	return err
}

// approvalEscalationAdapter adapts the composed approval service to the
// sandbox escalation channel face.
// sessionsFlushView is the projection-cache Sessions view: live lookups on
// the session store, write-behind drains on the persistence coordinator
// (the official sessions service carries both faces).
type sessionsFlushView struct {
	store       *session.Store
	coordinator *persistence.Coordinator
}

func (v sessionsFlushView) Get(id session.SessionID) (*session.Session, bool) {
	sess := v.store.Get(id)
	return sess, sess != nil
}

func (v sessionsFlushView) Flush(sess *session.Session) error {
	return v.coordinator.FlushSession(sess)
}

type approvalEscalationAdapter struct {
	service *userapproval.Service
}

func (a approvalEscalationAdapter) EscalationApprover() sandbox.EscalationApprover {
	return approvalChannel{service: a.service}
}

type approvalChannel struct {
	service *userapproval.Service
}

func (c approvalChannel) RequestApproval(req sandbox.EscalationAsk) (string, error) {
	var caller *agent.Agent
	caller, _ = req.Agent.(*agent.Agent)
	outcome, err := c.service.Request(userapproval.ApprovalRequest{
		Agent:    caller,
		ToolName: req.ToolName,
		CallID:   req.CallID,
		Reason:   req.Reason,
		Signal:   req.Signal,
	})
	if err != nil {
		return "", err
	}
	return string(outcome), nil
}

// ServiceSandboxPolicy is the sandbox policy service's cordis name (official
// ctx.sandboxPolicy).
const ServiceSandboxPolicy = "sandboxPolicy"

// mutationPolicyResolver adapts the sandbox policy service to the editor's
// per-call policy face: the calling agent's immutable session cwd is the
// workspace boundary and its last `sandbox/mode` knob is the override; an
// agentless call falls back to the deployment defaults.
type mutationPolicyResolver struct {
	service *sandboxpolicy.Service
	agents  *agent.AgentRegistry
}

func (m *mutationPolicyResolver) ResolveMutationPolicy(actor *agent.Agent) *fs.SandboxExecutionPolicy {
	var cwd, override string
	if actor != nil && actor.Session != nil {
		cwd = actor.Session.Header().CWD
		if mode, ok := permissionpresets.EffectiveSandboxMode(actor.Session.Events()); ok {
			override = mode
		}
	}
	policy := m.service.Resolve(cwd, override, "")
	return &policy
}

// maintenanceOwner adapts one receiving agent into the engine's
// MaintenanceAgent face: the session/model view rides the exported ViewAgent
// adapter and the reserved maintenance turn rides the driver.
type maintenanceOwner struct {
	compactionbasic.AgentView
	driver agent.Driver
}

func (m maintenanceOwner) RunMaintenance(task func(signal context.Context) error) error {
	return m.driver.RunMaintenance(task)
}

// persistenceListAdapter adapts the coordinator's stored-history listing to
// the workspace registry's context-carrying persistence seam.
type persistenceListAdapter struct {
	coordinator *persistence.Coordinator
}

func (a persistenceListAdapter) List(context.Context) ([]session.SessionHeader, error) {
	return a.coordinator.List()
}

// liveSessionsAdapter adapts the live session store to the workspace
// registry's live-header seam: live sessions outrank the persisted index.
type liveSessionsAdapter struct {
	store *session.Store
}

func (a liveSessionsAdapter) Header(id session.SessionID) (session.SessionHeader, bool) {
	sess := a.store.Get(id)
	if sess == nil {
		return session.SessionHeader{}, false
	}
	return sess.Header(), true
}

func (a liveSessionsAdapter) List() []session.SessionHeader {
	ids := a.store.List()
	headers := make([]session.SessionHeader, 0, len(ids))
	for _, id := range ids {
		if sess := a.store.Get(id); sess != nil {
			headers = append(headers, sess.Header())
		}
	}
	return headers
}

// optionalLLM reads the llm service without requiring it: an absent or
// non-llm service yields nil (the official ctx.get('llm') optional seam).
func optionalLLM(ctx *cordis.Context) *llm.Runtime {
	runtime, ok := ctx.Get(ServiceLlm).(*llm.Runtime)
	if !ok || runtime == nil {
		return nil
	}
	return runtime
}

// remoteEventBridgeIter adapts a forwarding queue into the gateway's
// dispatch iterator: every queue frame becomes a broadcast dispatch; the
// iterator ends (and detaches the bridge) when the queue closes or the
// signal aborts (official remoteEventSource per-signal lifecycle).
type remoteEventBridgeIter struct {
	queue  *gatewaystream.RemoteEventQueue
	detach func()
	signal context.Context
}

func (b *remoteEventBridgeIter) Next() (gateway.RemoteEventDispatch, bool) {
	for {
		if b.signal.Err() != nil {
			return gateway.RemoteEventDispatch{}, false
		}
		frame, done := b.queue.Next()
		if done {
			return gateway.RemoteEventDispatch{}, false
		}
		return gateway.RemoteEventDispatch{Frame: frame}, true
	}
}

func (b *remoteEventBridgeIter) Dispose() {
	b.detach()
}

// agentScopeKey maps a scope key to the agent id the waterfall dispatch
// needs (the bridge's ScopeKeys seam, via the registry's ByScope reverse
// index).
func agentScopeKey(registry *agent.AgentRegistry) func(scope.ScopeKey) string {
	return func(key scope.ScopeKey) string {
		if key == nil {
			return ""
		}
		owner := registry.ByScope(key)
		if owner == nil {
			return ""
		}
		return string(owner.ID)
	}
}

// dshHome resolves the harness home for the $events ready frame.
func dshHome() string {
	return homepaths.ResolveDshHome("", nil)
}

// parseWebStartup parses the `dsh web` flag family from the launcher inner
// args (official web-app startup.ts apply): --host/--port/--no-open and
// repeatable --trusted-host. --host 0.0.0.0 is refused for safety (the
// official security rejection verbatim); --port must be numeric. The
// service carries openBrowser (default true; --no-open clears it), the
// conditionally-present host/port, and the accumulated trusted hosts.
func parseWebStartup(args []string) (map[string]any, error) {
	values := map[string]any{
		"openBrowser":  true,
		"trustedHosts": []any{},
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		next := func() (string, bool) {
			if index+1 < len(args) {
				index++
				return args[index], true
			}
			return "", false
		}
		switch {
		case arg == "--host":
			value, ok := next()
			if !ok {
				return nil, fmt.Errorf("error: --host needs a value")
			}
			if value == "0.0.0.0" {
				return nil, fmt.Errorf("error: --host 0.0.0.0 is intentionally not supported yet for safety: it would expose remote code execution to the network; use 127.0.0.1 instead")
			}
			values["host"] = value
		case arg == "--no-open":
			values["openBrowser"] = false
		case arg == "--port":
			value, ok := next()
			if !ok {
				return nil, fmt.Errorf("error: --port needs a value")
			}
			port := 0
			for _, digit := range value {
				if digit < '0' || digit > '9' {
					return nil, fmt.Errorf("error: --port must be a number, got %q", value)
				}
				port = port*10 + int(digit-'0')
			}
			values["port"] = float64(port)
		case arg == "--trusted-host":
			value, ok := next()
			if !ok {
				return nil, fmt.Errorf("error: --trusted-host needs a value")
			}
			hosts, _ := values["trustedHosts"].([]any)
			values["trustedHosts"] = append(hosts, value)
		}
	}
	return values, nil
}

func init() {
	for name, build := range batchThreeBuilders {
		if _, dup := builders[name]; dup {
			panic(fmt.Sprintf("boot: duplicate catalog builder for %s", name))
		}
		builders[name] = build
	}
	// Official-name aliases: the shipped bundles name some Go-portable rows
	// by their upstream npm package names, while the Go port wired those
	// implementations under the names its remote-line rounds used. Both
	// spellings must resolve to the same spec — a composition never mixes
	// them, so the alias shares the canonical builder verbatim.
	officialNameAliases := map[string]string{
		"@deepseek-ai/dsh-tasks-local":                     "@deepseek-ai/dsh-jobs-local",
		"@deepseek-ai/dsh-settings-local":                  "@deepseek-ai/dsh-settings-file",
		"@deepseek-ai/dsh-session-title-first-message-llm": "@deepseek-ai/dsh-session-title-first-prompt-llm",
		"@deepseek-ai/dsh-repeat-tool-guard":               "@deepseek-ai/dsh-repeat-tool-reminder",
		"@deepseek-ai/dsh-bash-env":                        "@deepseek-ai/dsh-shell-env",
		"@deepseek-ai/dsh-skill-local":                     "@deepseek-ai/dsh-skill-filesystem",
		"@deepseek-ai/dsh-timeout-policy":                  "@deepseek-ai/dsh-tool-call-timeout-policy",
		"@deepseek-ai/dsh-fs-policy":                       "@deepseek-ai/dsh-fs-observation-policy",
		"@deepseek-ai/dsh-permission":                      "@deepseek-ai/dsh-permission-presets",
		"@deepseek-ai/dsh-goal-session":                    "@deepseek-ai/dsh-goal-round-driver",
		"@deepseek-ai/dsh-host-apiproxy":                   "@deepseek-ai/dsh-api-gateway",
	}
	for alias, canonical := range officialNameAliases {
		build, ok := builders[canonical]
		if !ok {
			panic(fmt.Sprintf("boot: official-name alias %s points at missing canonical %s", alias, canonical))
		}
		if _, dup := builders[alias]; dup {
			panic(fmt.Sprintf("boot: duplicate catalog builder for %s", alias))
		}
		builders[alias] = build
	}
}
