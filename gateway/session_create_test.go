package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/storagedomain"
	"dshgo/storagejson"
	"dshgo/workspace"
)

// createDriver satisfies the agent Driver face without a loop.
type createDriver struct{}

func (createDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (createDriver) WhenIdle() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (createDriver) RunMaintenance(task func(context.Context) error) error { return task(context.Background()) }
func (createDriver) Send(llm.Message, agent.InboxTarget, bool)             {}
func (createDriver) Followup(llm.Message)                                  {}
func (createDriver) Steer(llm.Message)                                     {}
func (createDriver) Inject(llm.Message)                                    {}

// createFakeFactory is the loop-factory stand-in: it materializes the
// session and agent exactly where the real factory would, and records the
// creation inputs the assertions read.
type createFakeFactory struct {
	mu       sync.Mutex
	host     *cordis.Context
	store    *session.Store
	registry *agent.AgentRegistry
	err      error
	lastMeta agent.CreateAgentMeta
	ids      []string
}

func (f *createFakeFactory) Create(ctx context.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return agent.AgentHandle{}, f.err
	}
	f.lastMeta = options.Meta
	sess, err := f.store.Create(options.SessionID, session.CreateOptions{
		HeaderMetadata: session.SessionHeader{CWD: options.Meta.CWD, AgentPreset: options.Meta.AgentPreset},
	})
	if err != nil {
		return agent.AgentHandle{}, err
	}
	agentCtx := f.host.Child()
	built := agent.NewAgent(agent.AgentConfig{
		ID:      options.SessionID,
		Options: options.AgentOptions,
		Session: sess,
		Ctx:     agentCtx,
	}, f.registry.Events())
	agent.ContextService.Provide(agentCtx, built)
	if options.Setup != nil {
		if _, err := options.Setup(agentCtx); err != nil {
			_ = agentCtx.Dispose()
			return agent.AgentHandle{}, err
		}
	}
	built.SetDriver(createDriver{})
	f.ids = append(f.ids, string(options.SessionID))
	if _, err := f.registry.Register(built); err != nil {
		_ = agentCtx.Dispose()
		return agent.AgentHandle{}, err
	}
	disposed := false
	return agent.AgentHandle{Agent: built, Dispose: func() error {
		disposed = true
		_ = disposed
		return nil
	}}, nil
}

func (f *createFakeFactory) CreateAgent(_ *cordis.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	return f.Create(context.Background(), options)
}

func (f *createFakeFactory) Resume(*cordis.Context, agent.ResumeAgentOptions) (agent.AgentHandle, error) {
	return agent.AgentHandle{}, errors.New("createFakeFactory: resume is out of scope")
}

func (f *createFakeFactory) Get(id session.SessionID) *agent.Agent {
	return f.registry.Get(id)
}

// seedablePersistence is the workspace registry's stored-history seam; one
// header per session, exactly what persistence holds after a create.
type createSeedPersistence struct {
	mu      sync.Mutex
	headers []session.SessionHeader
}

func (p *createSeedPersistence) List(context.Context) ([]session.SessionHeader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]session.SessionHeader{}, p.headers...), nil
}

func newCreateWorkspaces(t *testing.T) *workspace.Registry {
	t.Helper()
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json"},
		map[string]storagedomain.Backend{"json": storagejson.NewJsonStorageBackend(t.TempDir())},
		nil)
	registry, dispose, err := workspace.NewRegistry(context.Background(),
		workspace.RegistryHost{Persistence: &createSeedPersistence{}, Logger: cordis.Discard{}}, facility)
	if err != nil {
		t.Fatalf("workspace registry: %v", err)
	}
	t.Cleanup(dispose)
	return registry
}

func newCreateController(t *testing.T) (*SessionController, *createFakeFactory, *session.Store) {
	t.Helper()
	host := cordis.NewRoot(cordis.Discard{})
	store := session.NewStore(nil)
	factory := &createFakeFactory{host: host, store: store, registry: agent.NewAgentRegistry(host, nil)}
	if _, err := factory.registry.SetFactory(factory); err != nil {
		t.Fatalf("SetFactory: %v", err)
	}
	defaultModel, err := agentdefaultmodel.New(agentdefaultmodel.Settings{Provider: "deepseek-official", Model: "deepseek-v4-flash"})
	if err != nil {
		t.Fatalf("default model: %v", err)
	}
	workspaces := newCreateWorkspaces(t)
	controller := NewSessionController(nil, nil, nil, func() any { return defaultModel })
	controller.EnableCreate(SessionCreateDeps{
		Workspaces: func() any { return workspaces },
		Agents:     func() any { return factory.registry },
		Sessions:   func() any { return store },
	})
	return controller, factory, store
}

func TestCreateRejectsWorkspaceAndCwdTogether(t *testing.T) {
	controller, _, _ := newCreateController(t)
	_, err := controller.Create(context.Background(), map[string]any{
		"workspaceId": "ws-1", "cwd": `C:\tmp`,
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want workspaceId XOR cwd refusal, got %v", err)
	}
}

func TestCreateAnswersWorkspaceNotFound(t *testing.T) {
	controller, factory, _ := newCreateController(t)
	_, err := controller.Create(context.Background(), map[string]any{"workspaceId": "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want workspace/not-found, got %v", err)
	}
	if len(factory.ids) != 0 {
		t.Fatalf("no agent may be created on a failed workspace lookup, got %v", factory.ids)
	}
}

func TestCreateAnswersNotComposedWithoutAgentPlane(t *testing.T) {
	controller := NewSessionController(nil, nil, nil, nil)
	controller.EnableCreate(SessionCreateDeps{})
	_, err := controller.Create(context.Background(), map[string]any{"cwd": `C:\tmp`})
	if err == nil || !strings.Contains(err.Error(), "no agent registry") {
		t.Fatalf("want not-composed agent plane refusal, got %v", err)
	}
}

func TestCreateAnswersNotComposedWithoutDefaultModel(t *testing.T) {
	controller := NewSessionController(nil, nil, nil, nil)
	controller.EnableCreate(SessionCreateDeps{
		Agents: func() any { return nil },
	})
	_, err := controller.Create(context.Background(), map[string]any{"cwd": `C:\tmp`})
	if err == nil {
		t.Fatalf("want an error, got none")
	}
}

func TestCreateMintsSessionIdentityAndBuildsTheAgent(t *testing.T) {
	controller, factory, store := newCreateController(t)
	value, err := controller.Create(context.Background(), map[string]any{
		"cwd": t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("want a payload map, got %T", value)
	}
	sessionID, _ := row["sessionId"].(string)
	if !strings.HasPrefix(sessionID, "session-") {
		t.Fatalf("want a session- prefixed identity, got %q", sessionID)
	}
	if _, hasPreset := row["agentPreset"]; hasPreset {
		t.Fatalf("the presetless composition must not advertise an agentPreset, got %v", row)
	}
	if len(factory.ids) != 1 || factory.ids[0] != sessionID {
		t.Fatalf("exactly one agent expected for %q, got %v", sessionID, factory.ids)
	}
	if factory.lastMeta.CWD == "" || !filepath.IsAbs(factory.lastMeta.CWD) {
		t.Fatalf("the agent meta must carry the absolute request cwd, got %q", factory.lastMeta.CWD)
	}
	if store.Get(session.SessionID(sessionID)) == nil {
		t.Fatalf("session %q must be live in the store after create", sessionID)
	}
}

func TestCreateAdoptsTheLiveAgentIdempotently(t *testing.T) {
	controller, factory, _ := newCreateController(t)
	cwd := t.TempDir()
	first, err := controller.Create(context.Background(), map[string]any{"cwd": cwd})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	second, err := controller.Create(context.Background(), map[string]any{
		"sessionId": first.(map[string]any)["sessionId"], "cwd": cwd,
	})
	if err != nil {
		t.Fatalf("adopting create: %v", err)
	}
	if first.(map[string]any)["sessionId"] != second.(map[string]any)["sessionId"] {
		t.Fatalf("adoption must return the same identity: %v vs %v", first, second)
	}
	if len(factory.ids) != 1 {
		t.Fatalf("adoption must not build a second agent, got %v", factory.ids)
	}
}

func TestCreateRefusesTheLiveCwdConflict(t *testing.T) {
	controller, _, _ := newCreateController(t)
	first, err := controller.Create(context.Background(), map[string]any{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	sessionID := first.(map[string]any)["sessionId"]
	_, err = controller.Create(context.Background(), map[string]any{
		"sessionId": sessionID, "cwd": t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "cwd") {
		t.Fatalf("want a cwd conflict refusal, got %v", err)
	}
}

func TestCreateAnswersColdIdentityUntilResumeLands(t *testing.T) {
	controller, _, store := newCreateController(t)
	cold := session.SessionID("session-cold0000")
	if _, err := store.Create(cold, session.CreateOptions{}); err != nil {
		t.Fatalf("seed cold session: %v", err)
	}
	_, err := controller.Create(context.Background(), map[string]any{"sessionId": string(cold)})
	if err == nil || !strings.Contains(err.Error(), "no live agent") {
		t.Fatalf("want the cold-identity refusal, got %v", err)
	}
}

func TestCreateDegradesTheBrokenDefaultPresetToAgentless(t *testing.T) {
	controller, factory, _ := newCreateController(t)
	controller.EnableCreate(SessionCreateDeps{
		Agents:   func() any { return factory.registry },
		Sessions: func() any { return factory.store },
		Presets:  func() any { return "not-a-mounts" },
	})
	value, err := controller.Create(context.Background(), map[string]any{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("create with an unresolvable default preset must degrade to agentless, got %v", err)
	}
	if row := value.(map[string]any); row["agentPreset"] != nil {
		t.Fatalf("the degraded create must not advertise a preset, got %v", row)
	}
	if len(factory.ids) != 1 {
		t.Fatalf("the degraded create still builds one agent, got %v", factory.ids)
	}
}
