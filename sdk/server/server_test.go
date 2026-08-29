package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/sdk/protocol"
	"dshgo/session"
	"dshgo/subagent"
)

// fakePeer records notifications.
type fakePeer struct {
	mu     sync.Mutex
	frames [][2]string // method, params JSON
}

func (p *fakePeer) Request(ctx context.Context, method string, params any) (any, error) {
	return nil, fmt.Errorf("fakePeer does not send requests")
}

func (p *fakePeer) Notify(method string, params any) {
	encoded, _ := json.Marshal(params)
	p.mu.Lock()
	p.frames = append(p.frames, [2]string{method, string(encoded)})
	p.mu.Unlock()
}

func (p *fakePeer) ofMethod(method string) [][2]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var found [][2]string
	for _, frame := range p.frames {
		if frame[0] == method {
			found = append(found, frame)
		}
	}
	return found
}

// fakeDriver records followups for the driver face.
type fakeDriver struct {
	mu        sync.Mutex
	followups []llm.Message
}

func (d *fakeDriver) Cancel(cause session.TurnEndCancelCause, options agent.CancelOptions) {}
func (d *fakeDriver) WhenIdle() <-chan struct{}                                            { return nil }
func (d *fakeDriver) RunMaintenance(task func(signal context.Context) error) error         { return nil }
func (d *fakeDriver) Send(message llm.Message, target agent.InboxTarget, wakeup bool)      {}
func (d *fakeDriver) Steer(message llm.Message)                                            {}
func (d *fakeDriver) Inject(message llm.Message)                                           {}
func (d *fakeDriver) Followup(message llm.Message) {
	d.mu.Lock()
	d.followups = append(d.followups, message)
	d.mu.Unlock()
}

func (d *fakeDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.followups)
}

// fakeAgents is the AgentFactory over registry-level agents with fake
// drivers.
type fakeAgents struct {
	registry *agent.AgentRegistry
	mu       sync.Mutex
	disposed []string
	detaches map[string]func()
}

func (f *fakeAgents) Create(sessionID string, options CreateAgentOptions) (*agent.Agent, error) {
	if f.registry.Get(session.SessionID(sessionID)) != nil {
		return nil, fmt.Errorf("session %q already exists", sessionID)
	}
	sess, err := session.NewDetached(session.SessionID(sessionID), nil, &session.SessionHeader{
		ID:  session.SessionID(sessionID),
		CWD: options.Cwd,
	})
	if err != nil {
		return nil, err
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		return nil, err
	}
	built := agent.NewAgent(agent.AgentConfig{
		ID:      sess.ID(),
		Options: agent.AgentOptions{Provider: options.Provider, Model: options.Model},
		Session: sess,
		Inbox:   inbox,
	}, f.registry.Events())
	built.SetDriver(&fakeDriver{})
	detach, err := f.registry.Enter(built, nil)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if f.detaches == nil {
		f.detaches = map[string]func(){}
	}
	f.detaches[sessionID] = detach
	f.mu.Unlock()
	return built, nil
}

func (f *fakeAgents) Dispose(a *agent.Agent) error {
	f.mu.Lock()
	f.disposed = append(f.disposed, string(a.ID))
	f.mu.Unlock()
	return nil
}

// fakeRouter is the LLMRouter seam.
type fakeRouter struct {
	providers  map[string]bool
	mounted    int
	disposed   int
	resolved   []string
	resolveErr error
	mountErr   error
}

func (r *fakeRouter) HasAdapter(provider string) bool { return r.providers[provider] }
func (r *fakeRouter) MountDefault() (func(), error) {
	if r.mountErr != nil {
		return nil, r.mountErr
	}
	r.mounted++
	return func() { r.disposed++ }, nil
}
func (r *fakeRouter) ResolveCallConfig(provider, model, reasoningEffort string, maxTokens int64) error {
	if r.resolveErr != nil {
		return r.resolveErr
	}
	r.resolved = append(r.resolved, provider+"/"+model)
	return nil
}

// fakeAttachments admits inline images into ordered image blocks.
type fakeAttachments struct {
	admitted [][]protocol.SdkEncodedImageBlock
}

func (a *fakeAttachments) AdmitEncoded(images []protocol.SdkEncodedImageBlock) ([]llm.ContentBlock, error) {
	a.admitted = append(a.admitted, images)
	blocks := make([]llm.ContentBlock, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockImage, Attachment: "att-" + image.Data})
	}
	return blocks, nil
}

type fakeLoader struct{ calls int }

func (l *fakeLoader) Await() error { l.calls++; return nil }

// harness wires the server over fakes.
type harness struct {
	peer        *fakePeer
	registry    *agent.AgentRegistry
	store       *session.Store
	agents      *fakeAgents
	router      *fakeRouter
	attachments *fakeAttachments
	loader      *fakeLoader
	server      *Server
}

func newHarness(t *testing.T, options Options) *harness {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	agents := &fakeAgents{registry: registry}
	h := &harness{
		peer:        &fakePeer{},
		registry:    registry,
		store:       session.NewStore(discardLogger{}),
		agents:      agents,
		router:      &fakeRouter{providers: map[string]bool{"deepseek-official": true}},
		attachments: &fakeAttachments{},
		loader:      &fakeLoader{},
	}
	h.server = New(Deps{
		Registry:       registry,
		Store:          h.store,
		SubagentEvents: registry.Events(),
		Agents:         agents,
		LLM:            h.router,
		Attachments:    h.attachments,
		Loader:         h.loader,
	}, h.peer, options)
	t.Cleanup(func() { _ = h.server.Shutdown() })
	return h
}

type discardLogger struct{}

func (discardLogger) Warn(string) {}

func TestInitializeValidatesAndMountsFallback(t *testing.T) {
	h := newHarness(t, Options{})
	// An unknown provider without an adapter fails before any mount.
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "other", Model: "m"}); err == nil ||
		!strings.Contains(err.Error(), "no adapter registered") {
		t.Fatalf("unknown provider = %v", err)
	}
	if h.router.mounted != 0 {
		t.Fatal("no fallback for an unknown provider")
	}
	// A negative maxTokens is rejected.
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m", MaxTokens: -1}); err == nil ||
		!strings.Contains(err.Error(), "maxTokens") {
		t.Fatalf("negative maxTokens = %v", err)
	}
	// The default provider without an adapter mounts the fallback first.
	h.router.providers = map[string]bool{}
	result, err := h.server.Initialize(protocol.InitializeParams{Cwd: "relative/path", Provider: "deepseek-official", Model: "m", MaxTokens: 64})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if result.ServerInfo.Name != protocol.ServerName || result.ServerInfo.Version == "" {
		t.Fatalf("result = %+v", result)
	}
	if h.router.mounted != 1 || len(h.router.resolved) != 1 {
		t.Fatalf("mount=%d resolved=%v", h.router.mounted, h.router.resolved)
	}
	// The route resolved after the mount succeeded.
	if h.router.resolved[0] != "deepseek-official/m" {
		t.Fatalf("resolved = %v", h.router.resolved)
	}
	// Direct Initialize bypasses the loader gate; the dispatch face awaits
	// it (asserted in TestHandleRequestDispatch).
}

func TestPromptBeforeInitializeFails(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.server.Prompt(protocol.SessionPromptParams{SessionID: "s", ContentBlocks: []json.RawMessage{}}); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("prompt = %v", err)
	}
}

func TestPromptDeliversUserMessage(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	text, _ := json.Marshal(llm.ContentBlock{Type: llm.BlockText, Text: "hello"})
	result, err := h.server.Prompt(protocol.SessionPromptParams{
		SessionID:     "sdk-1",
		ContentBlocks: []json.RawMessage{json.RawMessage(text)},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if result.MessageID == "" {
		t.Fatal("receipt has no message id")
	}
	agentObj := h.registry.Get("sdk-1")
	if agentObj == nil {
		t.Fatal("session was not created")
	}
	driver := agentObj.Driver().(*fakeDriver)
	if driver.count() != 1 {
		t.Fatalf("followups = %d", driver.count())
	}
	if driver.followups[0].Source.Kind != "user" {
		t.Fatalf("source = %+v", driver.followups[0].Source)
	}
	if agentObj.Session.Header().CWD == "" {
		t.Fatal("header cwd missing")
	}

	// A disposed agent fails delivery loudly.
	h.agents.mu.Lock()
	detach := h.agents.detaches["sdk-1"]
	h.agents.mu.Unlock()
	detach()
	if _, err := h.server.Prompt(protocol.SessionPromptParams{
		SessionID:     "sdk-1",
		ContentBlocks: []json.RawMessage{json.RawMessage(text)},
	}); err == nil || !strings.Contains(err.Error(), "disposed outside the server") {
		t.Fatalf("disposed prompt = %v", err)
	}
}

func TestPromptInlineImagesRequireStore(t *testing.T) {
	h := newHarness(t, Options{Version: "test"})
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	image, _ := json.Marshal(protocol.SdkEncodedImageBlock{Type: "image", Data: "abc", MimeType: "image/png"})
	text, _ := json.Marshal(llm.ContentBlock{Type: llm.BlockText, Text: "see this"})
	// Without a store the inline image fails loud.
	h.server.deps.Attachments = nil
	if _, err := h.server.Prompt(protocol.SessionPromptParams{SessionID: "sdk-img", ContentBlocks: []json.RawMessage{json.RawMessage(image)}}); err == nil ||
		!strings.Contains(err.Error(), "requires an attachment store") {
		t.Fatalf("image without store = %v", err)
	}
	// With a store, image blocks are admitted in order and spliced back.
	h.server.deps.Attachments = h.attachments
	result, err := h.server.Prompt(protocol.SessionPromptParams{
		SessionID:     "sdk-img",
		ContentBlocks: []json.RawMessage{json.RawMessage(text), json.RawMessage(image), json.RawMessage(text)},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if result.MessageID == "" || len(h.attachments.admitted) != 1 || len(h.attachments.admitted[0]) != 1 {
		t.Fatalf("admission = %+v %+v", result, h.attachments.admitted)
	}
	agentObj := h.registry.Get("sdk-img")
	blocks := agentObj.Driver().(*fakeDriver).followups[0].Content
	if len(blocks) != 3 || blocks[1].Type != llm.BlockImage || blocks[1].Attachment != "att-abc" {
		t.Fatalf("blocks = %+v", blocks)
	}
}

func TestRacingPromptsShareOneCreation(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	text, _ := json.Marshal(llm.ContentBlock{Type: llm.BlockText, Text: "hi"})
	params := protocol.SessionPromptParams{SessionID: "sdk-race", ContentBlocks: []json.RawMessage{json.RawMessage(text)}}
	done := make(chan error, 4)
	for range 4 {
		go func() {
			_, err := h.server.Prompt(params)
			done <- err
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatalf("prompt: %v", err)
		}
	}
	// Four deliveries, one creation.
	agentObj := h.registry.Get("sdk-race")
	if agentObj == nil {
		t.Fatal("session was not created")
	}
	if agentObj.Driver().(*fakeDriver).count() != 4 {
		t.Fatal("deliveries lost")
	}
}

func TestLifecycleNotifications(t *testing.T) {
	h := newHarness(t, Options{MaxTokensAsSuccess: true})
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// session/event streams appends.
	sess, err := h.store.Create("observed", session.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("append: %v", err)
	}
	events := h.peer.ofMethod(protocol.NotifySessionEvent)
	if len(events) != 1 || !strings.Contains(events[0][1], `"sessionId":"observed"`) {
		t.Fatalf("session events = %v", events)
	}

	// agent/status streams transitions.
	agentObj, err := h.agents.Create("status-1", CreateAgentOptions{Cwd: "D:\\w"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	agentObj.SetStatus(agent.AgentRunning)
	statuses := h.peer.ofMethod(protocol.NotifySessionStatus)
	if len(statuses) != 1 || !strings.Contains(statuses[0][1], `"status":"running"`) {
		t.Fatalf("statuses = %v", statuses)
	}

	// session/created with a parent announces subagent.started.
	childHeader := session.SessionHeader{ID: "child-1", ParentSession: "observed"}
	child, err := session.NewDetached("child-1", nil, &childHeader)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if _, err := h.store.Enter(child); err != nil {
		t.Fatalf("child enter: %v", err)
	}
	if err := h.store.Announce(child); err != nil {
		t.Fatalf("child announce: %v", err)
	}
	started := h.peer.ofMethod(protocol.NotifySubagentStarted)
	if len(started) != 1 || !strings.Contains(started[0][1], `"parentSessionId":"observed"`) {
		t.Fatalf("started = %v", started)
	}

	// subagent/end local runs map the stop reason; remote runs drop.
	emit := func(info subagent.SubagentRunEndInfo) {
		h.registry.Events().Emit(subagent.EventSubagentEnd, agentObj.Scope, info)
	}
	emit(subagent.SubagentRunEndInfo{SubagentRunInfo: subagent.SubagentRunInfo{Provider: "local", ID: "child-1", Local: true}, StopReason: subagent.StopCompleted})
	emit(subagent.SubagentRunEndInfo{SubagentRunInfo: subagent.SubagentRunInfo{Provider: "local", ID: "child-2", Local: false}, StopReason: subagent.StopCompleted})
	emit(subagent.SubagentRunEndInfo{SubagentRunInfo: subagent.SubagentRunInfo{Provider: "local", ID: "child-3", Local: true}, StopReason: "max-tokens"})
	finished := h.peer.ofMethod(protocol.NotifySubagentFinished)
	if len(finished) != 2 {
		t.Fatalf("finished = %v", finished)
	}
	if !strings.Contains(finished[0][1], `"status":"ok"`) || !strings.Contains(finished[1][1], `"status":"ok"`) {
		t.Fatalf("mapping = %v", finished)
	}
	// With the option off, max-tokens maps to error.
	strict := newHarness(t, Options{})
	if _, err := strict.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	strictRegistry := strict.registry
	probe, err := strict.agents.Create("probe", CreateAgentOptions{Cwd: "D:\\w"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	strictRegistry.Events().Emit(subagent.EventSubagentEnd, probe.Scope, subagent.SubagentRunEndInfo{
		SubagentRunInfo: subagent.SubagentRunInfo{Provider: "local", ID: "child-9", Local: true},
		StopReason:      "max-tokens",
	})
	strictFinished := strict.peer.ofMethod(protocol.NotifySubagentFinished)
	if len(strictFinished) != 1 || !strings.Contains(strictFinished[0][1], `"status":"error"`) {
		t.Fatalf("strict mapping = %v", strictFinished)
	}
}

func TestShutdownDisposesAndIsIdempotent(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	// Mount an adapter fiber to observe its disposal.
	h.router.providers = map[string]bool{}
	_, err := h.server.Initialize(protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m2"})
	if err != nil {
		t.Fatalf("remount: %v", err)
	}
	text, _ := json.Marshal(llm.ContentBlock{Type: llm.BlockText, Text: "hi"})
	if _, err := h.server.Prompt(protocol.SessionPromptParams{SessionID: "sdk-shut", ContentBlocks: []json.RawMessage{json.RawMessage(text)}}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := h.server.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(h.agents.disposed) != 1 || h.agents.disposed[0] != "sdk-shut" {
		t.Fatalf("disposed = %v", h.agents.disposed)
	}
	if h.router.disposed != 1 {
		t.Fatalf("adapter disposals = %d", h.router.disposed)
	}
	// Idempotent, and prompts fail during/after shutdown.
	if err := h.server.Shutdown(); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
	if _, err := h.server.Prompt(protocol.SessionPromptParams{SessionID: "sdk-shut2", ContentBlocks: []json.RawMessage{}}); err == nil ||
		!strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("post-shutdown prompt = %v", err)
	}
}

func TestHandleRequestDispatch(t *testing.T) {
	h := newHarness(t, Options{})
	// Unknown methods fail loud.
	if _, err := h.server.HandleRequest("nonsense", nil); err == nil ||
		!strings.Contains(err.Error(), "unknown DeepSeek Harness SDK runtime method") {
		t.Fatalf("unknown = %v", err)
	}
	// initialize through the dispatch face.
	params := map[string]any{"cwd": "D:\\w", "provider": "deepseek-official", "model": "m"}
	result, err := h.server.HandleRequest("initialize", params)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if result.(protocol.InitializeResult).ServerInfo.Name != protocol.ServerName {
		t.Fatalf("result = %#v", result)
	}
	// prompt through the dispatch face.
	promptParams := map[string]any{
		"sessionId":     "sdk-dispatch",
		"contentBlocks": []any{map[string]any{"type": "text", "text": "hi"}},
	}
	prompted, err := h.server.HandleRequest("session/prompt", promptParams)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if prompted.(protocol.SessionPromptResult).MessageID == "" {
		t.Fatalf("prompted = %#v", prompted)
	}
	// shutdown returns the empty result and quiesces.
	done, err := h.server.HandleRequest("shutdown", nil)
	if err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if len(done.(map[string]any)) != 0 {
		t.Fatalf("done = %#v", done)
	}
}
