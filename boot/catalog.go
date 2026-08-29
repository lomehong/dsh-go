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
	"dshgo/agentloop"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/credentials"
	"dshgo/guard"
	"dshgo/host/webserver"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/interaction/userquestions"
	"dshgo/jobs"
	"dshgo/llm"
	"dshgo/llm/deepseek"
	"dshgo/planmode"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/persistence/jsonl"
	"dshgo/session/projection"
	"dshgo/settings"
	"dshgo/settings/file"
	"dshgo/skill"
	"dshgo/subagent"
	"dshgo/systemprompt"
	"dshgo/todo"
	"dshgo/tools"
	"dshgo/toolsjobs"
	"dshgo/toolskill"
)

// Service names plugins publish and consume through ctx inject lists.
const (
	ServiceTools             = "tools"
	ServiceCommands          = "commands"
	ServiceSettings          = "settings"
	ServiceWebServer         = "webServer"
	ServiceCredential        = "credentials"
	ServiceSessions          = "sessions"
	ServiceProjections       = "projections"
	ServiceAgents            = "agents"
	ServiceLlm               = "llm"
	ServiceSessionPersist    = "sessionPersistence"
	ServiceUserQuestions     = "userQuestions"
	ServiceUserApproval      = "userApproval"
	ServicePermissionPresets = "permissionPresets"
	ServiceSystemPrompt      = "systemPrompt"
	ServiceAgentLoop         = "agentLoop"
	ServiceSubagentRuntime   = "subagentRuntime"
	ServiceSkills            = "skills"
	ServiceJobs              = "jobs"
	ServicePlanMode          = "planMode"
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
				return nil
			},
		}
	},

	// The user-questions waterfall: ask-user questions resolve through the
	// typed request seam.
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
	// the schema-defaulted table over workspace-write.
	"@deepseek-ai/dsh-permission-presets": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Provide: []string{ServicePermissionPresets},
			Apply: func(ctx *cordis.Context, config any) error {
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
				ctx.Provide(ServicePermissionPresets, service)
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
			Inject:  []string{ServiceAgents, ServiceLlm, ServiceTools, ServiceSystemPrompt},
			Provide: []string{ServiceAgentLoop},
			Apply: func(ctx *cordis.Context, config any) error {
				loop, err := agentloop.NewAgentLoop(
					ctx,
					ctx.Get(ServiceAgents).(*agent.AgentRegistry),
					deps.Logger,
					ctx.Get(ServiceLlm).(*llm.Runtime),
					ctx.Get(ServiceTools).(*tools.ToolRuntime),
					ctx.Get(ServiceSystemPrompt).(*systemprompt.SystemPrompt),
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
// agent identity is the scope key, resolved against the live registry (the
// established resolveByScope pattern).
func toolsCallerOf(agents *agent.AgentRegistry) toolsjobs.CallerOf {
	return func(exec *tools.ToolExecution) string {
		if exec.Agent == nil {
			return ""
		}
		for _, candidate := range agents.List() {
			if candidate.Scope == exec.Agent {
				return string(candidate.Session.ID())
			}
		}
		return ""
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
	"@deepseek-ai/dsh-plan-mode": func(deps CatalogDeps) PluginSpec {
		return PluginSpec{
			Inject:  []string{ServiceCommands, ServiceTools, ServiceUserQuestions},
			Provide: []string{ServicePlanMode},
			Apply: func(ctx *cordis.Context, config any) error {
				section := "plan"
				if overridden, ok := config.(map[string]any); ok {
					if raw, ok := overridden["section"].(string); ok && raw != "" {
						section = raw
					}
				}
				controller, err := planmode.NewController(section)
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
}

func init() {
	for name, build := range batchThreeBuilders {
		if _, dup := builders[name]; dup {
			panic(fmt.Sprintf("boot: duplicate catalog builder for %s", name))
		}
		builders[name] = build
	}
}
