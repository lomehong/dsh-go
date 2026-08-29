package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/sdk/protocol"
	"dshgo/sdk/server"
	"dshgo/session"
)

// runtimePair connects a client and a real SDK server over in-process pipes
// (the stdio pairing minus the process).
func runtimePair(t *testing.T, options server.Options) (*Client, *server.Server, *agent.AgentRegistry) {
	t.Helper()
	clientToServerReader, clientToServerWriter := io.Pipe()
	serverToClientReader, serverToClientWriter := io.Pipe()
	clientTransport := protocol.NewLineTransport(clientToServerReader, serverToClientWriter)
	serverTransport := protocol.NewLineTransport(serverToClientReader, clientToServerWriter)
	clientTransport.Start()
	serverTransport.Start()
	t.Cleanup(func() {
		clientTransport.Close()
		serverTransport.Close()
	})
	registry := agent.NewAgentRegistry(nil, nil)
	store := session.NewStore(discardLogger{})
	agents := &stubAgents{registry: registry, store: store}
	srv := server.New(server.Deps{
		Registry:       registry,
		Store:          store,
		SubagentEvents: registry.Events(),
		Agents:         agents,
		LLM:            &stubRouter{providers: map[string]bool{"deepseek-official": true}},
	}, serverTransport, options)
	srv.Serve(serverTransport)
	t.Cleanup(func() { _ = srv.Shutdown() })
	return NewOverTransport(clientTransport, 5*time.Second), srv, registry
}

type stubAgents struct {
	registry *agent.AgentRegistry
	store    *session.Store
}

func (f *stubAgents) Create(sessionID string, options server.CreateAgentOptions) (*agent.Agent, error) {
	if f.registry.Get(session.SessionID(sessionID)) != nil {
		return nil, errors.New("exists")
	}
	sess, err := session.NewDetached(session.SessionID(sessionID), nil, &session.SessionHeader{ID: session.SessionID(sessionID), CWD: options.Cwd})
	if err != nil {
		return nil, err
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		return nil, err
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	built.SetDriver(&stubDriver{session: sess})
	if _, err := f.registry.Enter(built, nil); err != nil {
		return nil, err
	}
	// Bind to the store so the session/event feed flows to subscribers.
	if _, err := f.store.Enter(sess); err != nil {
		return nil, err
	}
	if err := f.store.Announce(sess); err != nil {
		return nil, err
	}
	return built, nil
}

func (f *stubAgents) Dispose(a *agent.Agent) error { return nil }

// stubDriver durably lands the user message like the loop's driver would,
// then drops it (no model is wired in tests).
type stubDriver struct {
	session *session.Session
	lastErr error
}

func (stubDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (stubDriver) WhenIdle() <-chan struct{}                              { return nil }
func (stubDriver) RunMaintenance(func(context.Context) error) error       { return nil }
func (stubDriver) Send(llm.Message, agent.InboxTarget, bool)              {}
func (stubDriver) Steer(llm.Message)                                      {}
func (stubDriver) Inject(llm.Message)                                     {}
func (d *stubDriver) Followup(message llm.Message) {
	if _, err := d.session.Append(session.EventUserMessage, message, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		d.lastErr = err
	}
}

type stubRouter struct{ providers map[string]bool }

func (r *stubRouter) HasAdapter(provider string) bool { return r.providers[provider] }
func (r *stubRouter) MountDefault() (func(), error)   { return func() {}, nil }
func (r *stubRouter) ResolveCallConfig(string, string, string, int64) error {
	return nil
}

type discardLogger struct{}

func (discardLogger) Warn(string) {}

func TestEndToEndInitializePromptNotificationsShutdown(t *testing.T) {
	client, srv, registry := runtimePair(t, server.Options{})
	sub := client.Subscribe(nil)
	defer sub.Close()
	ctx := context.Background()
	result, err := client.Initialize(ctx, protocol.InitializeParams{Cwd: "D:\\w", Provider: "deepseek-official", Model: "m"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if result.ServerInfo.Name != protocol.ServerName {
		t.Fatalf("identity = %+v", result.ServerInfo)
	}
	text, _ := json.Marshal(llm.ContentBlock{Type: llm.BlockText, Text: "hello"})
	messageID, err := client.Prompt(ctx, "sdk-e2e", []json.RawMessage{json.RawMessage(text)})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if messageID == "" {
		t.Fatal("no message id")
	}
	created := registry.Get("sdk-e2e")
	if created == nil {
		t.Fatal("session was not created")
	}
	driver := created.Driver().(*stubDriver)
	if driver.lastErr != nil {
		t.Fatalf("stub append failed: %v", driver.lastErr)
	}
	// The prompt's user turn arrives as a session.event notification for
	// the SDK session. Every Next gets a deadline: no notification within
	// the window is a failed expectation, not a hang.
	deadline := time.Now().Add(3 * time.Second)
	sawTurnStart := false
	for time.Now().Before(deadline) && !sawTurnStart {
		waitCtx, cancel := context.WithDeadline(ctx, deadline)
		notification, err := sub.Next(waitCtx)
		cancel()
		if err != nil {
			break
		}
		if notification.Method == protocol.NotifySessionEvent && notification.Params["sessionId"] == "sdk-e2e" {
			sawTurnStart = true
		}
	}
	if !sawTurnStart {
		t.Fatal("the prompt never surfaced as a session event")
	}
	// Status notifications flow from the registry tap.
	probe, err := srv.HandleRequest("session/prompt", map[string]any{
		"sessionId":     "sdk-e2e-2",
		"contentBlocks": []any{},
	})
	if err != nil || probe == nil {
		t.Fatalf("probe prompt: %v", err)
	}
	_ = registry
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInitializeEnforcesServerIdentity(t *testing.T) {
	// A peer that answers without a server identity violates the contract.
	peer := &scriptedPeer{reply: map[string]any{}}
	client := New(peer, time.Second)
	if _, err := client.Initialize(context.Background(), protocol.InitializeParams{}); err == nil ||
		!strings.Contains(err.Error(), "returned no server identity") {
		t.Fatalf("initialize = %v", err)
	}
}

func TestPromptEnforcesMessageID(t *testing.T) {
	peer := &scriptedPeer{reply: map[string]any{"unrelated": true}}
	client := New(peer, time.Second)
	if _, err := client.Prompt(context.Background(), "s", nil); err == nil ||
		!strings.Contains(err.Error(), "returned no message id") {
		t.Fatalf("prompt = %v", err)
	}
}

func TestProtocolErrorPropagatesTyped(t *testing.T) {
	peer := &scriptedPeer{err: &protocol.JsonRpcResponseError{Code: intPtr(-32000), Text: "admission refused"}}
	client := New(peer, time.Second)
	_, err := client.Request(context.Background(), "session/prompt", nil)
	var rpcErr *protocol.JsonRpcResponseError
	if !errors.As(err, &rpcErr) || rpcErr.Text != "admission refused" {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestTimeoutMapsToTypedError(t *testing.T) {
	peer := &hangingPeer{}
	client := New(peer, 30*time.Millisecond)
	start := time.Now()
	_, err := client.Request(context.Background(), "session/prompt", nil)
	if err == nil {
		t.Fatal("expected timeout")
	}
	var timeoutErr *RequestTimeoutError
	if !errors.As(err, &timeoutErr) || timeoutErr.Method != "session/prompt" {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout did not apply")
	}
}

func TestClosedClientFailsLoud(t *testing.T) {
	peer := &scriptedPeer{reply: map[string]any{}}
	client := New(peer, time.Second)
	sub := client.Subscribe(nil)
	cause := errors.New("runtime exited")
	client.Close(cause)
	// Requests fail with the close cause.
	if _, err := client.Request(context.Background(), "session/prompt", nil); !errors.Is(err, cause) {
		t.Fatalf("request = %v", err)
	}
	// Subscriptions are born failed.
	if _, err := sub.Next(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("next = %v", err)
	}
	// A new subscription is born failed too.
	if _, err := client.Subscribe(nil).Next(context.Background()); !errors.Is(err, cause) {
		t.Fatalf("fresh subscription = %v", err)
	}
}

func TestSubscriptionFilterAndClose(t *testing.T) {
	peer := &scriptedPeer{reply: map[string]any{}}
	client := New(peer, time.Second)
	onlyStatus := client.Subscribe(func(n Notification) bool { return n.Method == protocol.NotifySessionStatus })
	client.dispatch(Notification{Method: protocol.NotifySessionEvent, Params: map[string]any{}})
	client.dispatch(Notification{Method: protocol.NotifySessionStatus, Params: map[string]any{"status": "running"}})
	notification, ok := onlyStatus.TryNext()
	if !ok || notification.Method != protocol.NotifySessionStatus {
		t.Fatalf("tryNext = %#v %v", notification, ok)
	}
	// Close drops the queue and fails pending waiters.
	onlyStatus.Close()
	if _, ok := onlyStatus.TryNext(); ok {
		t.Fatal("closed subscription retained its queue")
	}
	if _, err := onlyStatus.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "subscription closed") {
		t.Fatalf("next after close = %v", err)
	}
}

func TestSubscriptionNextDeliversQueuedBeforeWaiting(t *testing.T) {
	peer := &scriptedPeer{reply: map[string]any{}}
	client := New(peer, time.Second)
	sub := client.Subscribe(nil)
	client.dispatch(Notification{Method: "a", Params: map[string]any{}})
	client.dispatch(Notification{Method: "b", Params: map[string]any{}})
	first, err := sub.Next(context.Background())
	if err != nil || first.Method != "a" {
		t.Fatalf("first = %#v %v", first, err)
	}
	second, err := sub.Next(context.Background())
	if err != nil || second.Method != "b" {
		t.Fatalf("second = %#v %v", second, err)
	}
}

func intPtr(value int) *int { return &value }

// scriptedPeer answers every request with a fixed reply.
type scriptedPeer struct {
	reply map[string]any
	err   error
}

func (p *scriptedPeer) Request(ctx context.Context, method string, params any) (any, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.reply, nil
}

func (p *scriptedPeer) Notify(method string, params any) {}

// hangingPeer never answers.
type hangingPeer struct{}

func (p *hangingPeer) Request(ctx context.Context, method string, params any) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (p *hangingPeer) Notify(method string, params any) {}
