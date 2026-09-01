package webhook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/interaction/permissionpresets"
	"dshgo/interaction/userapproval"
	"dshgo/llm"
	"dshgo/preset"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/sessiontitle"
	"dshgo/storagedomain"
	"dshgo/storagejson"
	"dshgo/workspace"
)

const testScopeService = "test.agentScope"

func testScopeOf(ctx *cordis.Context) scope.ScopeKey {
	if key, ok := ctx.Get(testScopeService).(scope.ScopeKey); ok {
		return key
	}
	return nil
}

// seedablePersistence is the stored-history seam; the fake factory seeds
// one header per created session, exactly what the persistence layer holds
// once the loop factory has durably written the new session.
type seedablePersistence struct {
	mu      sync.Mutex
	headers []session.SessionHeader
}

func (p *seedablePersistence) List(context.Context) ([]session.SessionHeader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]session.SessionHeader{}, p.headers...), nil
}

func (p *seedablePersistence) seed(header session.SessionHeader) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headers = append(p.headers, header)
}

type recordingLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *recordingLogger) record(level string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, level+": "+fmt.Sprint(args...))
}

func (l *recordingLogger) Info(args ...any)  { l.record("info", args...) }
func (l *recordingLogger) Warn(args ...any)  { l.record("warn", args...) }
func (l *recordingLogger) Error(args ...any) { l.record("error", args...) }

// fakeStanding is the assembled standing composition stand-in.
type fakeStanding struct{}

func (fakeStanding) PendingInjections() [][]string { return nil }
func (fakeStanding) Dispose() error                { return nil }

// fakeDriver records followup admissions.
type fakeDriver struct {
	mu        sync.Mutex
	followups []llm.Message
}

func (d *fakeDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (d *fakeDriver) WhenIdle() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
func (d *fakeDriver) RunMaintenance(task func(context.Context) error) error { return nil }
func (d *fakeDriver) Send(llm.Message, agent.InboxTarget, bool)             {}
func (d *fakeDriver) Followup(message llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.followups = append(d.followups, message)
}
func (d *fakeDriver) Steer(llm.Message)  {}
func (d *fakeDriver) Inject(llm.Message) {}

// fakeFactory builds one real Agent over a fresh session and runs the
// transaction's setup, exactly where the loop factory would.
type fakeFactory struct {
	host        *cordis.Context
	store       *session.Store
	bus         *agent.AgentRegistry
	persistence *seedablePersistence
	created     []*agent.Agent
	drivers     []*fakeDriver
	disposed    int
	lastMeta    agent.CreateAgentMeta
	lastOpts    agent.AgentOptions
	failDispose bool
}

func (f *fakeFactory) Create(ctx context.Context, options agent.CreateAgentOptions) (agent.AgentHandle, error) {
	f.lastMeta = options.Meta
	f.lastOpts = options.AgentOptions
	sess, err := f.store.Create(options.SessionID, session.CreateOptions{})
	if err != nil {
		return agent.AgentHandle{}, err
	}
	f.persistence.seed(session.SessionHeader{ID: options.SessionID, CreatedAt: 1, CWD: options.Meta.CWD})
	agentCtx := f.host.Child()
	agentCtx.Provide(testScopeService, scope.NewScopeKey(nil))
	built := agent.NewAgent(agent.AgentConfig{
		ID:      options.SessionID,
		Options: options.AgentOptions,
		Session: sess,
		Ctx:     agentCtx,
	}, f.bus.Events())
	agent.ContextService.Provide(agentCtx, built)
	if options.Setup != nil {
		if _, err := options.Setup(agentCtx); err != nil {
			_ = agentCtx.Dispose()
			return agent.AgentHandle{}, err
		}
	}
	driver := &fakeDriver{}
	built.SetDriver(driver)
	f.created = append(f.created, built)
	f.drivers = append(f.drivers, driver)
	return agent.AgentHandle{
		Agent: built,
		Dispose: func() error {
			if f.failDispose {
				return errors.New("disposal refused")
			}
			f.disposed++
			return agentCtx.Dispose()
		},
	}, nil
}

// sessionFixture wires the creation transaction over real services wherever
// the source reaches one: the workspace registry, the permission table, the
// title service, and the preset mount table.
type sessionFixture struct {
	t         *testing.T
	host      *cordis.Context
	store     *session.Store
	factory   *fakeFactory
	logger    *recordingLogger
	perms     *permissionpresets.Service
	titles    *sessiontitle.Service
	deps      SessionDeps
	workspace string
}

func newSessionFixture(t *testing.T) *sessionFixture {
	t.Helper()
	host := cordis.NewRoot(cordis.Discard{})
	presetRoot := t.TempDir()
	dir := filepath.Join(presetRoot, "minimal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, preset.CompositionFile), []byte("[]"), 0o644); err != nil {
		t.Fatalf("write composition: %v", err)
	}
	roster := preset.NewRoster(preset.Config{Default: "minimal", Roots: []preset.PresetRoot{{Path: presetRoot, Trust: preset.TrustUser}}}, preset.RosterOptions{})
	mounts, err := preset.NewMounts(host, roster, preset.MountOptions{
		Assemble: func(ctx *cordis.Context, p preset.AgentPreset) (preset.StandingTree, error) {
			return fakeStanding{}, nil
		},
		ScopeOf: testScopeOf,
	})
	if err != nil {
		t.Fatalf("new mounts: %v", err)
	}
	store := session.NewStore(nil)
	titles, err := sessiontitle.NewService(store, sessiontitle.Config{FallbackMaxWords: 5, FallbackMaxBytes: 40, MaxTitleBytes: 80}, cordis.Discard{})
	if err != nil {
		t.Fatalf("new title service: %v", err)
	}
	perms, err := permissionpresets.NewService(permissionpresets.Config{
		// The base bundle's preset table (base cordis.patch.yml).
		Presets: map[string]permissionpresets.PresetSpec{
			"read-only":          {Sandbox: permissionpresets.SandboxReadOnly, Approval: userapproval.PolicyAsk},
			"workspace-write":    {Sandbox: permissionpresets.SandboxWorkspaceWrite, Approval: userapproval.PolicyAsk},
			"danger-full-access": {Sandbox: permissionpresets.SandboxDangerFullAccess, Approval: userapproval.PolicyNever},
		},
		Names:          []string{"read-only", "workspace-write", "danger-full-access"},
		SandboxDefault: permissionpresets.SandboxWorkspaceWrite,
	})
	if err != nil {
		t.Fatalf("new permission service: %v", err)
	}
	persistence := &seedablePersistence{}
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json"},
		map[string]storagedomain.Backend{"json": storagejson.NewJsonStorageBackend(t.TempDir())},
		nil)
	registries, disposeRegistry, err := workspace.NewRegistry(context.Background(), workspace.RegistryHost{Persistence: persistence, Logger: cordis.Discard{}}, facility)
	if err != nil {
		t.Fatalf("new workspace registry: %v", err)
	}
	t.Cleanup(disposeRegistry)
	bus := agent.NewAgentRegistry(host, nil)
	factory := &fakeFactory{host: host, store: store, bus: bus, persistence: persistence}
	logger := &recordingLogger{}
	workspaceRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize workspace root: %v", err)
	}
	fx := &sessionFixture{
		t:         t,
		host:      host,
		store:     store,
		factory:   factory,
		logger:    logger,
		perms:     perms,
		titles:    titles,
		workspace: workspaceRoot,
	}
	fx.deps = SessionDeps{
		Logger: logger,
		DefaultModel: func() agent.ModelSelection {
			return agent.ModelSelection{Provider: "deepseek-official", Model: "deepseek-v4-flash", ReasoningEffort: "high", HasReasoningEffort: true}
		},
		PermissionPresets: perms,
		Presets:           mounts,
		Workspaces:        registries,
		Agents:            factory,
		Titles:            titles,
	}
	return fx
}

func (fx *sessionFixture) request() WebhookSessionRequest {
	return WebhookSessionRequest{
		WorkspacePath:    fx.workspace,
		Title:            "GitHub review",
		Prompt:           "Review the pull request.",
		AgentPreset:      "minimal",
		PermissionPreset: "read-only",
	}
}

func (fx *sessionFixture) delivery() VerifiedWebhookDelivery {
	return VerifiedWebhookDelivery{Kind: "github", Source: "primary-github", DeliveryID: "d-1", ReceivedAt: 42}
}

func TestResolveRequestRefusals(t *testing.T) {
	fx := newSessionFixture(t)
	absolute := fx.workspace
	cases := []struct {
		name   string
		mutate func(*WebhookSessionRequest)
		frag   string
	}{
		{"empty workspace", func(r *WebhookSessionRequest) { r.WorkspacePath = "  " }, "workspacePath must be a non-empty string"},
		{"relative workspace", func(r *WebhookSessionRequest) { r.WorkspacePath = "relative/dir" }, "workspacePath must be absolute"},
		{"empty title", func(r *WebhookSessionRequest) { r.Title = "" }, "title must be a non-empty string"},
		{"empty prompt", func(r *WebhookSessionRequest) { r.Prompt = "" }, "prompt must be a non-empty string"},
		{"empty agent preset", func(r *WebhookSessionRequest) { r.AgentPreset = "" }, "agentPreset must be a non-empty string"},
		{"empty permission preset", func(r *WebhookSessionRequest) { r.PermissionPreset = "" }, "permissionPreset must be a non-empty string"},
		{"empty model provider", func(r *WebhookSessionRequest) {
			r.WorkspacePath = absolute
			r.Model = &WebhookModelSelection{Model: "m"}
		}, "provider must be a non-empty string"},
		{"empty model id", func(r *WebhookSessionRequest) {
			r.WorkspacePath = absolute
			r.Model = &WebhookModelSelection{Provider: "p"}
		}, "model must be a non-empty string"},
		{"zero maxTokens", func(r *WebhookSessionRequest) {
			r.WorkspacePath = absolute
			zero := int64(0)
			r.Model = &WebhookModelSelection{Provider: "p", Model: "m", MaxTokens: &zero}
		}, "model.maxTokens must be a positive safe integer"},
	}
	for _, testCase := range cases {
		request := fx.request()
		testCase.mutate(&request)
		if _, err := resolveRequest(fx.deps, request); err == nil || !strings.Contains(err.Error(), testCase.frag) {
			t.Fatalf("%s: err = %v", testCase.name, err)
		}
	}
}

func TestResolveRequestModelPaths(t *testing.T) {
	fx := newSessionFixture(t)
	// Omitted model: the complete current default, effort included for the
	// creation-time selection but never in the agent options.
	resolved, err := resolveRequest(fx.deps, fx.request())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.agentOptions.Provider != "deepseek-official" || resolved.agentOptions.Model != "deepseek-v4-flash" || resolved.agentOptions.MaxTokens != nil {
		t.Fatalf("agentOptions = %+v", resolved.agentOptions)
	}
	if !resolved.modelSelection.HasReasoningEffort || resolved.modelSelection.ReasoningEffort != "high" {
		t.Fatalf("modelSelection = %+v", resolved.modelSelection)
	}
	// Explicit model: the cap forwards; the selection carries no effort.
	tokens := int64(128)
	request := fx.request()
	request.Model = &WebhookModelSelection{Provider: "p", Model: "m", MaxTokens: &tokens}
	resolved, err = resolveRequest(fx.deps, request)
	if err != nil {
		t.Fatalf("resolve explicit: %v", err)
	}
	if resolved.agentOptions.MaxTokens == nil || *resolved.agentOptions.MaxTokens != 128 {
		t.Fatalf("explicit agentOptions = %+v", resolved.agentOptions)
	}
	if resolved.modelSelection.HasReasoningEffort || resolved.modelSelection.Provider != "p" || resolved.modelSelection.Model != "m" {
		t.Fatalf("explicit modelSelection = %+v", resolved.modelSelection)
	}
}

func TestCreateWebhookSessionHappyPath(t *testing.T) {
	fx := newSessionFixture(t)
	if err := CreateWebhookSession(fx.deps, fx.delivery(), "rule-x", fx.request(), context.Background()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(fx.factory.created) != 1 {
		t.Fatalf("created = %d agents", len(fx.factory.created))
	}
	built := fx.factory.created[0]
	if !strings.HasPrefix(string(built.ID), "webhook-") {
		t.Fatalf("session id = %q", built.ID)
	}
	if fx.factory.lastMeta.CWD != fx.workspace || fx.factory.lastMeta.AgentPreset != "minimal" {
		t.Fatalf("meta = %+v", fx.factory.lastMeta)
	}
	// The workspace carries the attached session.
	entities := fx.deps.Workspaces.List()
	if len(entities) != 1 || entities[0].Path() != fx.workspace {
		t.Fatalf("workspaces = %+v", entities)
	}
	if ids := entities[0].SessionIDs(); len(ids) != 1 || ids[0] != built.ID {
		t.Fatalf("attached sessions = %v", ids)
	}
	// Permission pin and title landed on the live session.
	if current := fx.perms.Current(built.Session.Events()); current != "read-only" {
		t.Fatalf("permission = %q", current)
	}
	if snapshot := fx.titles.Get(built.Session); snapshot == nil || snapshot.Title != "GitHub review" {
		t.Fatalf("title = %+v", snapshot)
	}
	// The followup carries the delivery provenance verbatim.
	driver := fx.factory.drivers[0]
	if len(driver.followups) != 1 {
		t.Fatalf("followups = %d", len(driver.followups))
	}
	message := driver.followups[0]
	if message.Role != llm.RoleUser || len(message.Content) != 1 || message.Content[0].Type != "text" || message.Content[0].Text != "Review the pull request." {
		t.Fatalf("message = %+v", message)
	}
	source := message.Source
	wantSummary := llm.BoundContextSummary("github webhook handled by rule-x")
	if source.Kind != llm.SourceWebhook || source.Provider != "github" || source.WebhookSource != "primary-github" ||
		source.DeliveryID != "d-1" || source.RuleID != "rule-x" || source.Form != llm.FormNotice || source.Summary != wantSummary {
		t.Fatalf("source = %+v", source)
	}
	// The creation-time selection rewrites a matching first request: the
	// inherited effort clears and the selected effort lands.
	config := built.Events().Request().Dispatch(built.Scope, agent.RequestPayload{}, func(agent.RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "deepseek-official", Model: "deepseek-v4-flash", ReasoningEffort: "inherited"}
	})
	if config == nil || config.ReasoningEffort != "high" {
		t.Fatalf("selection rewrite = %+v", config)
	}
	// A route mismatch passes through untouched.
	other := built.Events().Request().Dispatch(built.Scope, agent.RequestPayload{}, func(agent.RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "elsewhere", Model: "deepseek-v4-flash", ReasoningEffort: "inherited"}
	})
	if other == nil || other.ReasoningEffort != "inherited" {
		t.Fatalf("mismatch passthrough = %+v", other)
	}
}

// failingTitler refuses the rename after attach, driving the rollback path.
type failingTitler struct{ err error }

func (f failingTitler) Rename(*session.Session, string) (*sessionquery.SessionTitleSnapshot, error) {
	return nil, f.err
}

func TestCreateWebhookSessionRollsBackAfterAttach(t *testing.T) {
	fx := newSessionFixture(t)
	fx.deps.Titles = failingTitler{err: errors.New("session-title fold refused")}
	err := CreateWebhookSession(fx.deps, fx.delivery(), "rule-x", fx.request(), context.Background())
	if err == nil || err.Error() != "session-title fold refused" {
		t.Fatalf("err = %v", err)
	}
	// The original failure survives; the attach and the agent both unwind.
	if fx.factory.disposed != 1 {
		t.Fatalf("disposed = %d", fx.factory.disposed)
	}
	entities := fx.deps.Workspaces.List()
	if len(entities) != 1 {
		t.Fatalf("workspaces = %+v", entities)
	}
	if ids := entities[0].SessionIDs(); len(ids) != 0 {
		t.Fatalf("sessions after rollback = %v", ids)
	}
}

func TestCreateWebhookSessionRollbackFailuresOnlyWarn(t *testing.T) {
	fx := newSessionFixture(t)
	fx.factory.failDispose = true
	fx.deps.Titles = failingTitler{err: errors.New("session-title fold refused")}
	err := CreateWebhookSession(fx.deps, fx.delivery(), "rule-x", fx.request(), context.Background())
	if err == nil || err.Error() != "session-title fold refused" {
		t.Fatalf("the original failure must survive, got %v", err)
	}
	fx.logger.mu.Lock()
	defer fx.logger.mu.Unlock()
	found := false
	for _, line := range fx.logger.warnings {
		if strings.Contains(line, "Agent disposal for Session") && strings.Contains(line, "rollback failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v", fx.logger.warnings)
	}
}

func TestCreateWebhookSessionPreflightBeforeMutation(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*WebhookSessionRequest)
		frag   string
	}{
		{"unknown permission preset", func(r *WebhookSessionRequest) { r.PermissionPreset = "ghost" }, "ghost"},
		{"unknown agent preset", func(r *WebhookSessionRequest) { r.AgentPreset = "ghost" }, "ghost"},
	} {
		fx := newSessionFixture(t)
		request := fx.request()
		testCase.mutate(&request)
		err := CreateWebhookSession(fx.deps, fx.delivery(), "rule-x", request, context.Background())
		if err == nil || !strings.Contains(err.Error(), testCase.frag) {
			t.Fatalf("%s: err = %v", testCase.name, err)
		}
		if len(fx.deps.Workspaces.List()) != 0 || len(fx.factory.created) != 0 {
			t.Fatalf("%s: preflight failure still mutated state", testCase.name)
		}
	}
}

func TestCreateWebhookSessionHonorsTheSignal(t *testing.T) {
	fx := newSessionFixture(t)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	err := CreateWebhookSession(fx.deps, fx.delivery(), "rule-x", fx.request(), signal)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(fx.factory.created) != 0 {
		t.Fatal("an aborted signal must stop before agent creation")
	}
}
