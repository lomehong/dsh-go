// Plugin wiring: attach Schedule to every future live root agent.
package schedule

import (
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/session"
	"dshgo/tools"
)

func init() {
	if err := session.RegisterEventType("schedule/change"); err != nil {
		panic(err)
	}
}

// Options carries the services required before future root agents can
// receive Schedule.
type Options struct {
	// Agents is the live root registry; the runtime reads liveness through
	// it.
	Agents *agent.AgentRegistry
	// Runtime receives the three management tools in each root agent scope.
	Runtime *tools.ToolRuntime
	// Flush checkpoints a session's live prefix (the official shared
	// persistence barrier); nil means the composition has no persistence
	// coordinator and every checkpoint is trivially complete.
	Flush func(*session.Session) error
	// Logger receives contained runtime warnings.
	Logger cordis.Logger
	// Now is the production wall clock; tests supply explicit samples.
	Now func() int64
}

// Register installs Schedule only for root agents published after this
// plugin loads. It returns the lifecycle disposer: stopping the created
// listener and awaiting every per-agent cleanup.
func Register(options Options) (func(), error) {
	if options.Agents == nil {
		return nil, errRequired("agents registry")
	}
	if options.Runtime == nil {
		return nil, errRequired("tool runtime")
	}
	logger := options.Logger
	if logger == nil {
		logger = cordis.Discard{}
	}
	now := options.Now
	if now == nil {
		now = defaultNow
	}

	var mu sync.Mutex
	stopping := false
	runtimes := map[*agent.Agent]func(){}

	createdDisposer := options.Agents.Events().OnEmit(agent.EventAgentCreated, nil, func(payload any) error {
		created, ok := payload.(agent.AgentLifecyclePayload)
		if !ok || created.Agent == nil {
			return nil
		}
		ag := created.Agent
		mu.Lock()
		if stopping {
			mu.Unlock()
			return nil
		}
		if _, exists := runtimes[ag]; exists {
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		if !rootsInclude(options.Agents, ag) {
			return nil
		}
		runtime := NewScheduleRuntime(options.Agents, ag, options.Flush, logger, now)
		disposeTools, err := RegisterScheduleTools(options.Runtime, ag, options.Flush, now, logger, func() {
			runtime.RequestDrive()
		})
		if err != nil {
			return err
		}
		stopStatus := options.Agents.Events().OnEmit(agent.EventAgentStatus, nil, func(statusPayload any) error {
			status, ok := statusPayload.(agent.AgentStatusPayload)
			if !ok || status.Agent != ag || status.Status != agent.AgentIdle {
				return nil
			}
			if sessionHasScheduleChange(ag.Session) {
				runtime.RequestDrive()
			}
			return nil
		})
		runtime.Start()
		cleanup := func() {
			stopStatus()
			disposeTools()
			runtime.Dispose()
			mu.Lock()
			if runtimes[ag] != nil {
				delete(runtimes, ag)
			}
			mu.Unlock()
		}
		mu.Lock()
		runtimes[ag] = cleanup
		mu.Unlock()
		return nil
	})

	active := true
	return func() {
		mu.Lock()
		if !active {
			mu.Unlock()
			return
		}
		active = false
		mu.Unlock()
		createdDisposer()
		mu.Lock()
		stopping = true
		cleanups := make([]func(), 0, len(runtimes))
		for _, cleanup := range runtimes {
			cleanups = append(cleanups, cleanup)
		}
		runtimes = map[*agent.Agent]func(){}
		mu.Unlock()
		for _, cleanup := range cleanups {
			cleanup()
		}
	}, nil
}

// rootsInclude reports whether the agent is a currently published root.
func rootsInclude(agents *agent.AgentRegistry, ag *agent.Agent) bool {
	for _, root := range agents.Roots() {
		if root == ag {
			return true
		}
	}
	return false
}

// sessionHasScheduleChange reports whether the session log already carries
// a package-owned event.
func sessionHasScheduleChange(sess *session.Session) bool {
	for _, event := range sess.Events() {
		if event.Type == "schedule/change" {
			return true
		}
	}
	return false
}
