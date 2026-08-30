package sessiontitle

import (
	"errors"
	"sync"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
)

type testLogger struct {
	mu    sync.Mutex
	warns []string
	cordis.Logger
}

func (l *testLogger) Warn(args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(args) == 1 {
		if text, ok := args[0].(string); ok {
			l.warns = append(l.warns, text)
			return
		}
	}
	l.warns = append(l.warns, "unrendered")
}

func (l *testLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.warns...)
}

type fakeProvider struct {
	mu        sync.Mutex
	id        string
	automatic string
	calls     []string
	err       error
}

func (p *fakeProvider) ID() string        { return p.id }
func (p *fakeProvider) Automatic() string { return p.automatic }

func (p *fakeProvider) Generate(request ProviderRequest) (ProviderResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, request.Messages[0].Text)
	p.mu.Unlock()
	if p.err != nil {
		return ProviderResult{}, p.err
	}
	route := request.Route
	model := (*sessionquery.SessionTitleModelProvenance)(nil)
	if route != nil {
		model = route
	}
	return ProviderResult{
		Title:       "titled: " + request.Messages[0].Text,
		MessageSeqs: []int64{request.Messages[0].Seq},
		Model:       model,
	}, nil
}

func (p *fakeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func newTestService(t *testing.T, config Config) (*Service, *session.Store, *testLogger) {
	t.Helper()
	logger := &testLogger{}
	store := session.NewStore(discardSessionLogger{})
	service, err := NewService(store, config, logger)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	t.Cleanup(service.Dispose)
	return service, store, logger
}

type discardSessionLogger struct{}

func (discardSessionLogger) Warn(string) {}

func createSession(t *testing.T, store *session.Store, id, parent string) *session.Session {
	t.Helper()
	header := session.SessionHeader{CreatedAt: 30}
	if parent != "" {
		header.ParentSession = parent
	}
	sess, err := store.Create(id, session.CreateOptions{HeaderMetadata: header})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return sess
}

func appendUserMessage(t *testing.T, sess *session.Session, id, text string) session.Event {
	t.Helper()
	event, err := sess.Append(session.EventUserMessage, llm.Message{
		ID:      llm.MessageID(id),
		Role:    llm.RoleUser,
		Source:  llm.MessageSource{Kind: llm.SourceUser},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}})
	if err != nil {
		t.Fatalf("append user message: %v", err)
	}
	return event
}

func appendRequestHeader(t *testing.T, sess *session.Session, provider, model string) session.Event {
	t.Helper()
	event, err := sess.Append(session.EventRequestHeader, map[string]any{
		"header": map[string]any{
			"config": map[string]any{"provider": provider, "model": model},
		},
		"reason": "initial",
	}, nil)
	if err != nil {
		t.Fatalf("append request header: %v", err)
	}
	return event
}

func waitForTitle(t *testing.T, get func() *sessionquery.SessionTitleSnapshot) *sessionquery.SessionTitleSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapshot := get(); snapshot != nil {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("title snapshot never appeared")
	return nil
}

func waitForCalls(t *testing.T, provider *fakeProvider, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if provider.callCount() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provider calls: got %d, want >= %d", provider.callCount(), want)
}

func TestConfigValidation(t *testing.T) {
	store := session.NewStore(discardSessionLogger{})
	cases := []struct {
		name   string
		config Config
	}{
		{"zero words", Config{FallbackMaxWords: 0, FallbackMaxBytes: 10, MaxTitleBytes: 20}},
		{"zero bytes", Config{FallbackMaxWords: 4, FallbackMaxBytes: 0, MaxTitleBytes: 20}},
		{"zero title bytes", Config{FallbackMaxWords: 4, FallbackMaxBytes: 10, MaxTitleBytes: 0}},
		{"fallback exceeds max", Config{FallbackMaxWords: 4, FallbackMaxBytes: 30, MaxTitleBytes: 20}},
	}
	for _, testCase := range cases {
		if _, err := NewService(store, testCase.config, &testLogger{}); err == nil {
			t.Fatalf("%s: invalid config accepted", testCase.name)
		}
	}
}

func TestFallbackTitleOnFirstUserMessage(t *testing.T) {
	service, store, _ := newTestService(t, Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128})
	sess := createSession(t, store, "a", "")
	appendUserMessage(t, sess, "u1", "hello   wide\tworld of titles")
	snapshot := waitForTitle(t, func() *sessionquery.SessionTitleSnapshot { return service.Get(sess) })
	if snapshot.Title != "hello wide world" {
		t.Fatalf("fallback title = %q", snapshot.Title)
	}
	if snapshot.Source.Kind != sessionquery.TitleSourceFallback {
		t.Fatalf("fallback source = %+v", snapshot.Source)
	}
	if len(snapshot.MessageSeqs) != 1 || snapshot.MessageSeqs[0] != 0 {
		t.Fatalf("fallback message seqs = %v", snapshot.MessageSeqs)
	}
}

func TestRenamePinsAndRejectsEmpty(t *testing.T) {
	service, store, _ := newTestService(t, Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128})
	sess := createSession(t, store, "a", "")
	if _, err := service.Rename(sess, "   "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty rename error = %v", err)
	}
	snapshot, err := service.Rename(sess, "  my   own  title  ")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if snapshot.Title != "my own title" || snapshot.Source.Kind != sessionquery.TitleSourceUser {
		t.Fatalf("rename snapshot = %+v", snapshot)
	}
	otherStore := session.NewStore(discardSessionLogger{})
	if _, err := service.Rename(createSession(t, otherStore, "ghost", ""), "x"); err == nil {
		t.Fatal("rename of non-live session accepted")
	}
}

func TestProviderSchedulesAndAccepts(t *testing.T) {
	service, store, _ := newTestService(t, Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128})
	provider := &fakeProvider{id: "p1", automatic: AutomaticAllPrompts}
	closer, err := service.RegisterProvider(provider)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := createSession(t, store, "a", "")
	appendUserMessage(t, sess, "u1", "first question")
	appendRequestHeader(t, sess, "deepseek", "deepseek-chat")
	// The fallback may land first; wait for the provider revision to win.
	waitForTitle(t, func() *sessionquery.SessionTitleSnapshot {
		if snapshot := service.Get(sess); snapshot != nil && snapshot.Source.Kind == sessionquery.TitleSourceProvider {
			return snapshot
		}
		return nil
	})
	snapshot := service.Get(sess)
	if snapshot.Title != "titled: first question" {
		t.Fatalf("provider title = %q", snapshot.Title)
	}
	if snapshot.Source.Kind != sessionquery.TitleSourceProvider || snapshot.Source.Provider != "p1" {
		t.Fatalf("provider source = %+v", snapshot.Source)
	}
	if snapshot.Source.Model == nil || snapshot.Source.Model.Provider != "deepseek" || snapshot.Source.Model.Model != "deepseek-chat" {
		t.Fatalf("route provenance = %+v", snapshot.Source.Model)
	}
	// A second prompt schedules a new generation under all-prompts.
	appendUserMessage(t, sess, "u2", "second question")
	appendRequestHeader(t, sess, "deepseek", "deepseek-chat")
	waitForCalls(t, provider, 2)
	// Closing the provider stops further scheduling; the closer is idempotent.
	closer()
	closer()
	if _, err := service.RegisterProvider(nil); err == nil {
		t.Fatal("nil provider accepted")
	}
}

func TestFirstPromptSkipsChildAndSecondMessage(t *testing.T) {
	service, store, _ := newTestService(t, Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128})
	provider := &fakeProvider{id: "p1", automatic: AutomaticFirstPrompt}
	if _, err := service.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	child := createSession(t, store, "child", "root")
	appendUserMessage(t, child, "u1", "child question")
	appendRequestHeader(t, child, "deepseek", "deepseek-chat")
	time.Sleep(80 * time.Millisecond)
	if provider.callCount() != 0 {
		t.Fatalf("child session scheduled generation (%d calls)", provider.callCount())
	}
	root := createSession(t, store, "root", "")
	appendUserMessage(t, root, "u1", "root question")
	appendRequestHeader(t, root, "deepseek", "deepseek-chat")
	waitForCalls(t, provider, 1)
	// A second message under first-prompt does not schedule again.
	appendUserMessage(t, root, "u2", "follow-up")
	appendRequestHeader(t, root, "deepseek", "deepseek-chat")
	time.Sleep(80 * time.Millisecond)
	if provider.callCount() != 1 {
		t.Fatalf("second message scheduled under first-prompt (%d calls)", provider.callCount())
	}
	// But the fallback still materialized from the first message.
	if snapshot := service.Get(root); snapshot == nil || snapshot.Source.Kind != sessionquery.TitleSourceProvider {
		t.Fatalf("root snapshot = %+v", service.Get(root))
	}
}

func TestProviderErrorContainedAndDisposeStopsWork(t *testing.T) {
	service, store, logger := newTestService(t, Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128})
	provider := &fakeProvider{id: "p1", automatic: AutomaticAllPrompts, err: errors.New("aux model offline")}
	if _, err := service.RegisterProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := createSession(t, store, "a", "")
	appendUserMessage(t, sess, "u1", "fallback stays")
	appendRequestHeader(t, sess, "deepseek", "deepseek-chat")
	snapshot := waitForTitle(t, func() *sessionquery.SessionTitleSnapshot { return service.Get(sess) })
	if snapshot.Source.Kind != sessionquery.TitleSourceFallback {
		t.Fatalf("expected fallback after provider error, got %+v", snapshot.Source)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(logger.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(logger.snapshot()) == 0 {
		t.Fatal("provider error was not logged")
	}
	service.Dispose()
	service.Dispose()
	if _, err := service.Rename(sess, "x"); err == nil {
		t.Fatal("rename after dispose accepted")
	}
	if _, err := service.RegisterProvider(provider); err == nil {
		t.Fatal("register after dispose accepted")
	}
}
