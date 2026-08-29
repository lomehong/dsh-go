package agentloop

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
)

// Factory mechanics: prepared-but-unpublished agent resources sharing one
// memoized teardown. Port of AgentLoop.prepare/createAgent/resume in
// packages/core/agent-loop/src/index.ts. Go adaptations: the owner fiber's
// unload-following effect becomes the factory's own teardown signal plus the
// caller's cancellation context; setup runs synchronously before publication
// (the Go AgentSetup contract); a racing disposal is memoized with sync.Once;
// the sessions registry (`ctx.sessions.enter/announce`) is out of this
// slice's surface, so publication enters and announces the agent registry.

// newUUID mints a random RFC 4122 v4 id for derived fresh identities.
func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("uuid: %v", err))
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// preparedAgent is the constructed driver, scope, and one memoized reverse
// teardown for a new agent. The teardown is registered with the factory
// BEFORE publication, so a mid-setup unload rolls everything back.
type preparedAgent struct {
	agent   *agent.Agent
	driver  *ReactLoopAgent
	signal  context.Context // fused caller + factory-teardown cancellation
	publish func(source agent.SessionStartSource) (agent.AgentHandle, error)
	dispose func() error
}

// loopNotifications late-binds the driver so the Inbox can be built before
// the Agent exists.
type loopNotifications struct {
	mu     sync.Mutex
	driver *ReactLoopAgent
}

func (n *loopNotifications) bound() *ReactLoopAgent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.driver
}

func (n *loopNotifications) Inserted(message llm.Message) {
	if driver := n.bound(); driver != nil {
		driver.Events().Emit(agent.EventInboxInserted, driver.Scope, agent.AgentMessagePayload{Agent: driver.Agent, Message: message})
	}
}

func (n *loopNotifications) Discarded(message llm.Message) {
	if driver := n.bound(); driver != nil {
		driver.Events().Emit(agent.EventInboxDiscarded, driver.Scope, agent.AgentMessagePayload{Agent: driver.Agent, Message: message})
	}
}

func (n *loopNotifications) Claimed(message llm.Message, turn int64) {
	if driver := n.bound(); driver != nil {
		driver.Events().Emit(agent.EventInboxClaimed, driver.Scope, agent.AgentClaimedPayload{Agent: driver.Agent, Message: message, Turn: turn})
	}
}

// prepare constructs the driver, scope, and memoized teardown for a new
// agent over an already-acquired session.
func (l *AgentLoop) prepare(id session.SessionID, options agent.AgentOptions, sess *session.Session, callerSignal context.Context) (*preparedAgent, error) {
	if err := assertAgentOptions(options); err != nil {
		return nil, err
	}
	if !l.ownership.isActive() {
		return nil, fmt.Errorf("agent loop is not active")
	}
	if callerSignal != nil && callerSignal.Err() != nil {
		return nil, fmt.Errorf("agent %q creation aborted", id)
	}

	// Deactivation fuses two owners, each with its own reason: the caller's
	// cancellation signal and factory teardown. It is registered BEFORE any
	// resource exists.
	goCtx, cancel := context.WithCancelCause(l.baseCtx)
	stopFusing := l.fuseDeactivation(goCtx, cancel, id, callerSignal)

	notifications := &loopNotifications{}
	prepared := &preparedAgent{signal: goCtx}
	var (
		disposeOnce sync.Once
		untrack     func()
		detach      func()
	)
	dispose := func() error {
		disposeOnce.Do(func() {
			// Disposal IS a disposed-cause cancel followed by quiescence.
			// New work sent after this point is the sender's bug — the
			// registries are about to drop the agent.
			cancel(wrapCause(session.TurnEndCancelCause{Kind: "disposed"}))
			stopFusing()
			if prepared.driver != nil {
				prepared.driver.Cancel(session.TurnEndCancelCause{Kind: "disposed"}, agent.CancelOptions{})
				select {
				case <-prepared.driver.WhenIdle():
				case <-l.ownership.signal().Done():
				}
				_ = prepared.agent.Ctx.Dispose()
			}
			if detach != nil {
				detach()
			}
			untrack()
		})
		return nil
	}
	untrack = l.ownership.track(dispose)
	prepared.dispose = dispose

	inbox, err := agent.NewInbox(sess, notifications)
	if err != nil {
		_ = dispose()
		return nil, err
	}
	built := agent.NewAgent(agent.AgentConfig{
		ID:      id,
		Options: options,
		Session: sess,
		Inbox:   inbox,
	}, l.Registry.Events())
	prepared.agent = built
	// Publish the built agent into its own context so creation-window setup
	// closures (delegation policy seeding, per-child composition) can reach
	// the child's session and scope.
	agent.ContextService.Provide(built.Ctx, built)
	notifications.mu.Lock()
	prepared.driver = NewReactLoopAgent(l, built)
	notifications.mu.Unlock()

	// Per-agent prompt variables: the Go AssembleContext carries only the
	// scope, so the closures capture their agent and register at its scope.
	unwindVariables := l.installAgentVariables(built)

	prepared.publish = func(source agent.SessionStartSource) (agent.AgentHandle, error) {
		if goCtx.Err() != nil {
			return agent.AgentHandle{}, fmt.Errorf("agent %q creation aborted", id)
		}
		entryDetach, err := l.Registry.Enter(built, nil)
		if err != nil {
			return agent.AgentHandle{}, err
		}
		if err := l.Registry.Announce(built); err != nil {
			entryDetach()
			return agent.AgentHandle{}, err
		}
		if goCtx.Err() != nil {
			entryDetach()
			return agent.AgentHandle{}, fmt.Errorf("agent %q creation aborted", id)
		}
		l.emitSessionStart(built, source)
		if goCtx.Err() != nil {
			entryDetach()
			return agent.AgentHandle{}, fmt.Errorf("agent %q creation aborted", id)
		}
		detach = func() {
			entryDetach()
			unwindVariables()
		}
		return agent.AgentHandle{Agent: built, Dispose: dispose}, nil
	}
	return prepared, nil
}

// fuseDeactivation aborts goCtx when the caller's signal or the factory
// teardown fires, each with its own reason. The returned stop releases the
// watchers when teardown never fires.
func (l *AgentLoop) fuseDeactivation(goCtx context.Context, cancel context.CancelCauseFunc, id session.SessionID, callerSignal context.Context) (stop func()) {
	var watchers sync.WaitGroup
	if callerSignal != nil {
		watchers.Add(1)
		go func() {
			defer watchers.Done()
			<-callerSignal.Done()
			if cause := context.Cause(callerSignal); cause != nil && cause != callerSignal.Err() {
				cancel(cause)
			} else if callerSignal.Err() != nil {
				cancel(fmt.Errorf("agent %q creation aborted", id))
			}
		}()
	}
	watchers.Add(1)
	go func() {
		defer watchers.Done()
		<-l.ownership.signal().Done()
		cancel(context.Cause(l.ownership.signal()))
	}()
	return func() {
		// Settle the signal watchers: a fused cancellation that never fired
		// ends quietly once goCtx is cancelled by dispose.
		go func() {
			watchers.Wait()
		}()
	}
}

// installAgentVariables registers the per-agent provider/model/cwd prompt
// variables at the agent's scope.
func (l *AgentLoop) installAgentVariables(a *agent.Agent) func() {
	var disposers []func()
	register := func(name string, provider systemprompt.VariableProvider) {
		if dispose, err := l.Prompt.Variable(a.Scope, name, provider); err == nil {
			disposers = append(disposers, dispose)
		}
	}
	register("provider", func(systemprompt.AssembleContext) (string, bool) {
		if a.Options.Provider == "" {
			return "", false
		}
		return a.Options.Provider, true
	})
	register("model", func(systemprompt.AssembleContext) (string, bool) {
		if a.Options.Model == "" {
			return "", false
		}
		return a.Options.Model, true
	})
	register("cwd", func(systemprompt.AssembleContext) (string, bool) {
		if a.Session.Header().CWD == "" {
			return "", false
		}
		return a.Session.Header().CWD, true
	})
	return func() {
		for _, dispose := range disposers {
			dispose()
		}
	}
}

// CreateAgent creates a new agent on a caller-supplied session id, awaits
// unpublished setup, publishes both records, and starts the loop.
func (l *AgentLoop) CreateAgent(owner *cordis.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	sess, err := session.NewDetached(options.SessionID, options.Seed, &session.SessionHeader{
		ID:              options.SessionID,
		CWD:             options.Meta.CWD,
		ParentSession:   options.Meta.ParentSession,
		SeedLength:      options.Meta.SeedLength,
		Origin:          options.Meta.Origin,
		DelegationDepth: options.Meta.DelegationDepth,
		AgentPreset:     options.Meta.AgentPreset,
	})
	if err != nil {
		return agent.AgentHandle{}, err
	}
	return l.setupAndPublish(owner, options.SessionID, sess, options.AgentOptions, options.Setup, agent.SessionStartStartup, nil)
}

// Resume prepares a persisted session and resumes an agent on it.
func (l *AgentLoop) Resume(owner *cordis.Context, options agent.ResumeAgentOptions) (agent.AgentHandle, error) {
	if l.Persistence == nil {
		return agent.AgentHandle{}, fmt.Errorf("cannot resume: session persistence is not configured (load a dsh-session-persistence backend)")
	}
	preparation, err := l.Persistence.Prepare(options.ResumeSessionID)
	if err != nil {
		return agent.AgentHandle{}, err
	}
	sess := preparation.Session
	preparation.Release(true)
	return l.setupAndPublish(owner, options.ResumeSessionID, sess, options.AgentOptions, options.Setup, agent.SessionStartResume, nil)
}

// setupAndPublish prepares one Agent around an acquired session, runs setup,
// and publishes it.
func (l *AgentLoop) setupAndPublish(
	owner *cordis.Context,
	id session.SessionID,
	sess *session.Session,
	agentOptions agent.AgentOptions,
	setup agent.AgentSetup,
	source agent.SessionStartSource,
	callerSignal context.Context,
) (agent.AgentHandle, error) {
	prepared, err := l.prepare(id, agentOptions, sess, callerSignal)
	if err != nil {
		return agent.AgentHandle{}, err
	}
	if setup != nil {
		commit, setupErr := setup(prepared.agent.Ctx)
		if setupErr != nil {
			_ = prepared.dispose()
			return agent.AgentHandle{}, setupErr
		}
		if commit.Commit != nil {
			if commitErr := commit.Commit(); commitErr != nil {
				_ = prepared.dispose()
				return agent.AgentHandle{}, commitErr
			}
		}
	}
	handle, err := prepared.publish(source)
	if err != nil {
		_ = prepared.dispose()
		return agent.AgentHandle{}, err
	}
	return handle, nil
}
