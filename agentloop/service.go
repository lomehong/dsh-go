package agentloop

import (
	"context"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/projection"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Concrete agent-loop service: creates scoped ReactLoopAgents, publishes them
// through the agent registry, and owns their ordered teardown.
//
// Port of packages/core/agent-loop/src/index.ts. Go adaptations: the cordis
// service-injection strings become explicit constructor references; the
// session registry (`ctx.sessions.enter/announce`) is out of this slice's
// surface, so publication enters and announces the agent registry only; the
// settings-section installation and `agent-loop/config-start-failed` cordis
// event arrive with the settings/boot wiring (config startup failures log
// through the loop logger); per-agent `provider`/`model`/`cwd` prompt
// variables register at the agent's own scope inside prepare.

// AgentLoopConfig is the agent-loop plugin configuration.
type AgentLoopConfig struct {
	// MaxParallelToolCalls caps in-flight parallel-safe calls per agent step.
	// 1 is serial; nil defaults to DefaultMaxParallelToolCalls. Read per
	// scheduler group, so a committed change caps the next group.
	MaxParallelToolCalls *int
	// Agents are created or resumed at service construction.
	Agents []ConfiguredAgent
}

// ConfiguredAgent is one declarative agent entry.
type ConfiguredAgent struct {
	// ID is the stable config label used in logs and as the fresh combined-id
	// prefix.
	ID string
	// SessionID is an optional stable identity; remounts resume its
	// materialized history, while first use creates it fresh.
	SessionID session.SessionID
	// CWD is an optional workspace for a fresh session.
	CWD string
	// ResumeSessionID is a persisted session to resume instead of creating a
	// fresh session.
	ResumeSessionID session.SessionID
	// AgentOptions are the concrete loop options for the agent.
	AgentOptions agent.AgentOptions
}

// validateConfiguredAgents rejects self-contained identity conflicts before
// any configured agent starts.
func validateConfiguredAgents(agents []ConfiguredAgent) error {
	exactIdentities := map[session.SessionID]string{}
	for _, entry := range agents {
		hasResumeID := entry.ResumeSessionID != ""
		if entry.SessionID != "" && hasResumeID {
			return fmt.Errorf("agent %q: sessionId and resumeSessionId are mutually exclusive", entry.ID)
		}
		exactIdentity := entry.SessionID
		if hasResumeID {
			exactIdentity = entry.ResumeSessionID
		}
		if exactIdentity == "" {
			continue
		}
		if firstID, taken := exactIdentities[exactIdentity]; taken {
			return fmt.Errorf("agents %q and %q use duplicate exact session identity %q", firstID, entry.ID, exactIdentity)
		}
		exactIdentities[exactIdentity] = entry.ID
	}
	return nil
}

// resolveMaxParallelToolCalls resolves the deployment-wide scheduler cap at
// the owning config boundary.
func resolveMaxParallelToolCalls(value *int) (int, error) {
	if value == nil {
		return DefaultMaxParallelToolCalls, nil
	}
	if *value < 1 {
		return 0, fmt.Errorf("maxParallelToolCalls must be a positive integer")
	}
	return *value, nil
}

// assertAgentOptions rejects an output-token cap that cannot be represented
// exactly on the request wire.
func assertAgentOptions(options agent.AgentOptions) error {
	const maxSafeInteger = int64(1) << 53
	if options.MaxTokens != nil && (*options.MaxTokens <= 0 || *options.MaxTokens > maxSafeInteger) {
		return fmt.Errorf("agent maxTokens must be a positive safe integer")
	}
	return nil
}

// factoryOwnership is factory-level ownership: live agent teardowns plus
// config startup work.
type factoryOwnership struct {
	mu             sync.Mutex
	accepting      bool
	teardown       context.Context
	teardownCancel context.CancelCauseFunc
	liveAgents     map[*liveTeardown]struct{}
	startup        sync.WaitGroup
}

// liveTeardown is one tracked teardown handle (pointers are comparable map
// keys; funcs are not).
type liveTeardown struct{ dispose func() error }

func newFactoryOwnership() *factoryOwnership {
	teardown, cancel := context.WithCancelCause(context.Background())
	return &factoryOwnership{
		accepting:      true,
		teardown:       teardown,
		teardownCancel: cancel,
		liveAgents:     map[*liveTeardown]struct{}{},
	}
}

// isActive reports whether the factory still accepts new lifecycles.
func (o *factoryOwnership) isActive() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.accepting
}

// signal is the teardown cancellation; drivers fuse it into setup awaits.
func (o *factoryOwnership) signal() context.Context {
	return o.teardown
}

// track records one live agent's shared teardown until it has run.
func (o *factoryOwnership) track(dispose func() error) (untrack func()) {
	handle := &liveTeardown{dispose: dispose}
	o.mu.Lock()
	o.liveAgents[handle] = struct{}{}
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		delete(o.liveAgents, handle)
		o.mu.Unlock()
	}
}

// trackStartup joins config startup work that begins before an agent exists.
func (o *factoryOwnership) trackStartup() {
	o.startup.Add(1)
}

// startupDone releases one tracked startup job.
func (o *factoryOwnership) startupDone() {
	o.startup.Done()
}

// dispose stops acceptance, aborts the teardown signal, and waits for every
// live agent teardown and startup job.
func (o *factoryOwnership) dispose() error {
	o.mu.Lock()
	o.accepting = false
	o.mu.Unlock()
	o.teardownCancel(fmt.Errorf("agent loop is not active"))
	o.startup.Wait()
	for {
		o.mu.Lock()
		pending := make([]func() error, 0, len(o.liveAgents))
		for handle := range o.liveAgents {
			pending = append(pending, handle.dispose)
		}
		o.mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		for _, dispose := range pending {
			_ = dispose()
		}
	}
}

// AgentLoop is the concrete agent factory and driver service.
type AgentLoop struct {
	// LLMServices resolves model routes; Tools drives tool calls; Prompt
	// assembles per-step prompts. All three are explicit instead of
	// service-name injections.
	LLM    *llm.Runtime
	Tools  *tools.ToolRuntime
	Prompt *systemprompt.SystemPrompt
	// Persistence is the optional session store used by Resume and by
	// configured agents with a sessionId.
	Persistence *persistence.Coordinator

	// Registry receives the factory and owns publication.
	Registry *agent.AgentRegistry
	// Logger receives config-start warnings; nil discards.
	Logger cordis.Logger

	// baseCtx is the cancellation root for driver signals.
	baseCtx context.Context

	maxParallelToolCalls int
	ownership            *factoryOwnership

	publishMu sync.Mutex
}

// NewAgentLoop validates the configuration, registers the factory on the
// registry's owning context, publishes the loop-owned turnBoundary
// projection unit (the loop is the authoritative driver of turn/step
// boundaries; consumers read turn numbers from it instead of scanning the
// log), and starts the configured agents. The projection registry is
// required: a composition without it cannot serve the loop's boundary facts.
func NewAgentLoop(ctx *cordis.Context, registry *agent.AgentRegistry, logger cordis.Logger, llmRuntime *llm.Runtime, toolRuntime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, projections *projection.Registry, config AgentLoopConfig) (*AgentLoop, error) {
	if logger == nil {
		logger = cordis.Discard{}
	}
	if projections == nil {
		return nil, fmt.Errorf("agent loop requires the session projection registry (register the turnBoundary unit's owner)")
	}
	maxParallel, err := resolveMaxParallelToolCalls(config.MaxParallelToolCalls)
	if err != nil {
		return nil, err
	}
	if err := validateConfiguredAgents(config.Agents); err != nil {
		return nil, err
	}
	if _, err := projections.Register(TurnBoundaryProjectionDefinition()); err != nil {
		return nil, err
	}
	loop := &AgentLoop{
		LLM:                  llmRuntime,
		Tools:                toolRuntime,
		Prompt:               prompt,
		Registry:             registry,
		Logger:               logger,
		baseCtx:              context.Background(),
		maxParallelToolCalls: maxParallel,
		ownership:            newFactoryOwnership(),
	}
	if err := ctx.Effect(func() (cordis.Disposer, error) {
		return func() { _ = loop.ownership.dispose() }, nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Effect(func() (cordis.Disposer, error) {
		return registry.SetFactory(loop)
	}); err != nil {
		return nil, err
	}
	if err := loop.startConfiguredAgents(config.Agents); err != nil {
		return nil, err
	}
	return loop, nil
}

// startConfiguredAgents creates or resumes the declarative entries. Fresh
// entries without persistence create synchronously; persisted identities
// restore-or-create through the deferred startup path.
func (l *AgentLoop) startConfiguredAgents(entries []ConfiguredAgent) error {
	for _, entry := range entries {
		meta := agent.CreateAgentMeta{}
		if entry.CWD != "" {
			meta.CWD = entry.CWD
		}
		if entry.ResumeSessionID != "" {
			l.ownership.trackStartup()
			go func(entry ConfiguredAgent) {
				defer l.ownership.startupDone()
				if _, err := l.Resume(nil, agent.ResumeAgentOptions{
					ResumeSessionID: entry.ResumeSessionID,
					AgentOptions:    entry.AgentOptions,
				}); err != nil {
					l.reportConfiguredStartupFailure(entry.ID, "resume", entry.ResumeSessionID, err)
				}
			}(entry)
			continue
		}
		if entry.SessionID == "" || l.Persistence == nil {
			// A fresh identity without persistence creates directly; the
			// configured id labels a derived fresh session id.
			id := entry.SessionID
			if id == "" {
				id = session.SessionID(fmt.Sprintf("%s-session-%s", entry.ID, newUUID()))
			}
			if _, err := l.create(id, entry.AgentOptions, meta, agent.SessionStartStartup); err != nil {
				return err
			}
			continue
		}
		l.ownership.trackStartup()
		go func(entry ConfiguredAgent, meta agent.CreateAgentMeta) {
			defer l.ownership.startupDone()
			id := entry.SessionID
			if _, err := l.Resume(nil, agent.ResumeAgentOptions{
				ResumeSessionID: id,
				AgentOptions:    entry.AgentOptions,
			}); err != nil {
				// Only a genuinely absent artifact falls back to first
				// creation; corruption and backend failures stay loud.
				exists := false
				if headers, listErr := l.Persistence.List(); listErr == nil {
					for _, header := range headers {
						if header.ID == id {
							exists = true
							break
						}
					}
				}
				if exists {
					l.reportConfiguredStartupFailure(entry.ID, "restore", id, err)
					return
				}
				if _, createErr := l.create(id, entry.AgentOptions, meta, agent.SessionStartStartup); createErr != nil {
					l.reportConfiguredStartupFailure(entry.ID, "restore", id, createErr)
				}
			}
		}(entry, meta)
	}
	return nil
}

// reportConfiguredStartupFailure logs a contained declarative-start failure.
func (l *AgentLoop) reportConfiguredStartupFailure(configID, action string, sessionID session.SessionID, err error) {
	if !l.ownership.isActive() {
		return
	}
	l.Logger.Warn(fmt.Sprintf("agent %q: config-driven %s of %q failed: %s", configID, action, sessionID, errorChainText(err)))
}

func errorChainText(err error) string {
	if err == nil {
		return ""
	}
	return llm.ErrorChain(err)
}

// create builds and publishes one fresh agent/session pair under one
// caller-supplied identity.
func (l *AgentLoop) create(id session.SessionID, options agent.AgentOptions, meta agent.CreateAgentMeta, source agent.SessionStartSource) (*agent.Agent, error) {
	handle, err := l.CreateAgent(nil, agent.CreateAgentOptions{
		SessionID:    id,
		Meta:         meta,
		AgentOptions: options,
	})
	if err != nil {
		return nil, err
	}
	l.emitSessionStart(handle.Agent, source)
	return handle.Agent, nil
}

// emitSessionStart publishes the agent/session-start event.
func (l *AgentLoop) emitSessionStart(a *agent.Agent, source agent.SessionStartSource) {
	a.Events().Emit(agent.EventAgentSessionStart, a.Scope, agent.AgentSessionStartPayload{Agent: a, Source: source})
}
