package sessiontitlellm

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/sessiontitle"
)

type discardLogger struct{}

func (discardLogger) Warn(string) {}

type fakeAdapter struct {
	llm.BaseAdapter
	mu     sync.Mutex
	chunks []llm.StreamChunk
	got    *llm.GenerateOptions
}

func (a *fakeAdapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	a.mu.Lock()
	defer a.mu.Unlock()
	captured := options
	a.got = &captured
	return llm.FromChunks(a.chunks)
}

func (a *fakeAdapter) options() *llm.GenerateOptions {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.got
}

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
		}
	}
}

func (l *testLogger) warnCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.warns)
}

func textChunks(text string) []llm.StreamChunk {
	return []llm.StreamChunk{
		{Type: llm.ChunkTextDelta, Text: text},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishStop}},
	}
}

func newFixture(t *testing.T, chunks []llm.StreamChunk) (*sessiontitle.Service, *llm.Runtime, *fakeAdapter, *session.Store, *testLogger) {
	t.Helper()
	runtime := llm.NewRuntime()
	adapter := &fakeAdapter{chunks: chunks}
	if _, err := runtime.RegisterAdapter([]string{"prov"}, adapter); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	store := session.NewStore(discardLogger{})
	logger := &testLogger{}
	service, err := sessiontitle.NewService(store, sessiontitle.Config{FallbackMaxWords: 3, FallbackMaxBytes: 64, MaxTitleBytes: 128}, logger)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	t.Cleanup(service.Dispose)
	return service, runtime, adapter, store, logger
}

func defaultConfig() Config {
	return Config{TargetWords: 5, TargetCJKCharacters: 10, MaxInputBytes: 4096, MaxOutputTokens: 64, TimeoutMs: 60000}
}

func createSession(t *testing.T, store *session.Store, id string) *session.Session {
	t.Helper()
	sess, err := store.Create(id, session.CreateOptions{HeaderMetadata: session.SessionHeader{CreatedAt: 30}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sess
}

func appendUserMessage(t *testing.T, sess *session.Session, id, text string) {
	t.Helper()
	if _, err := sess.Append(session.EventUserMessage, llm.Message{
		ID:      llm.MessageID(id),
		Role:    llm.RoleUser,
		Source:  llm.MessageSource{Kind: llm.SourceUser},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("append user message: %v", err)
	}
}

func appendRequestHeader(t *testing.T, sess *session.Session, provider, model string) {
	t.Helper()
	if _, err := sess.Append(session.EventRequestHeader, map[string]any{
		"header": map[string]any{"config": map[string]any{"provider": provider, "model": model}},
		"reason": "initial",
	}, nil); err != nil {
		t.Fatalf("append request header: %v", err)
	}
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

func mustResolve(t *testing.T, config Config) Config {
	t.Helper()
	resolved, err := ResolveConfig(config)
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	return resolved
}

func TestResolveConfig(t *testing.T) {
	if _, err := ResolveConfig(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	broken := defaultConfig()
	broken.Provider = "prov"
	if _, err := ResolveConfig(broken); err == nil {
		t.Fatal("provider without model accepted")
	}
	broken = defaultConfig()
	broken.Model = "model"
	if _, err := ResolveConfig(broken); err == nil {
		t.Fatal("model without provider accepted")
	}
	if _, err := ResolveConfig(defaultConfig()); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
}

func TestSelectFirstPrompt(t *testing.T) {
	if _, err := SelectFirstPrompt(nil); err == nil {
		t.Fatal("empty selection accepted")
	}
	selected, err := SelectFirstPrompt([]sessionquery.SessionTitleUserMessage{
		{Seq: 0, Text: "first"},
		{Seq: 4, Text: "second"},
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(selected) != 1 || selected[0].Text != "first" {
		t.Fatalf("selection = %+v", selected)
	}
}

func TestFullGenerationThroughService(t *testing.T) {
	service, runtime, adapter, store, _ := newFixture(t, textChunks("  A helpful  session title.  "))
	config := defaultConfig()
	closer, err := Register(service, runtime, config, "first-prompt", sessiontitle.AutomaticFirstPrompt, SelectFirstPrompt)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer closer()
	sess := createSession(t, store, "a")
	appendUserMessage(t, sess, "u1", "please help me refactor the parser")
	appendRequestHeader(t, sess, "prov", "model-x")
	snapshot := waitForTitle(t, func() *sessionquery.SessionTitleSnapshot { return service.Get(sess) })
	if snapshot.Title != "A helpful session title." {
		t.Fatalf("model title = %q", snapshot.Title)
	}
	if snapshot.Source.Kind != sessionquery.TitleSourceProvider || snapshot.Source.Provider != "first-prompt" {
		t.Fatalf("source = %+v", snapshot.Source)
	}
	if snapshot.Source.Model == nil || snapshot.Source.Model.Provider != "prov" || snapshot.Source.Model.Model != "model-x" {
		t.Fatalf("route = %+v", snapshot.Source.Model)
	}
	options := adapter.options()
	if options == nil {
		t.Fatal("adapter was never called")
	}
	if options.Purpose != llm.PurposeSessionTitle || options.SessionID != "a" || options.Provider != "prov" || options.Model != "model-x" {
		t.Fatalf("options = %+v", options)
	}
	if options.MaxTokens == nil || *options.MaxTokens != 64 {
		t.Fatalf("max tokens = %v", options.MaxTokens)
	}
	if !strings.Contains(options.System, "Aim for about 5 words in non-CJK languages or 10 CJK characters.") {
		t.Fatalf("system prompt = %q", options.System)
	}
	if len(options.Messages) != 1 || !strings.Contains(options.Messages[0].Content[0].Text, "please help me refactor the parser") {
		t.Fatalf("framed messages = %+v", options.Messages)
	}
}

func TestInputBudgetRejectsBeforeDispatch(t *testing.T) {
	service, runtime, adapter, store, _ := newFixture(t, textChunks("unused"))
	config := defaultConfig()
	config.MaxInputBytes = 10
	closer, err := Register(service, runtime, config, "first-prompt", sessiontitle.AutomaticFirstPrompt, SelectFirstPrompt)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer closer()
	sess := createSession(t, store, "a")
	appendUserMessage(t, sess, "u1", "a message far longer than ten bytes")
	appendRequestHeader(t, sess, "prov", "model-x")
	waitForTitle(t, func() *sessionquery.SessionTitleSnapshot { return service.Get(sess) })
	if adapter.options() != nil {
		t.Fatal("aux dispatch happened despite byte budget")
	}
	snapshot := service.Get(sess)
	if snapshot.Source.Kind != sessionquery.TitleSourceFallback {
		t.Fatalf("expected fallback, got %+v", snapshot.Source)
	}
}

func TestMaxTokensFinishRejected(t *testing.T) {
	chunks := []llm.StreamChunk{
		{Type: llm.ChunkTextDelta, Text: "partial"},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishMaxTokens}},
	}
	service, runtime, _, store, logger := newFixture(t, chunks)
	closer, err := Register(service, runtime, defaultConfig(), "first-prompt", sessiontitle.AutomaticFirstPrompt, SelectFirstPrompt)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer closer()
	sess := createSession(t, store, "a")
	appendUserMessage(t, sess, "u1", "title me")
	appendRequestHeader(t, sess, "prov", "model-x")
	snapshot := waitForTitle(t, func() *sessionquery.SessionTitleSnapshot { return service.Get(sess) })
	if snapshot.Source.Kind != sessionquery.TitleSourceFallback {
		t.Fatalf("expected fallback after max-tokens, got %+v", snapshot.Source)
	}
	deadline := time.Now().Add(2 * time.Second)
	for logger.warnCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if logger.warnCount() == 0 {
		t.Fatal("max-tokens failure was not logged")
	}
}

func TestNoRouteFailsLoudWithoutDispatch(t *testing.T) {
	service, runtime, adapter, store, _ := newFixture(t, textChunks("unused"))
	config := defaultConfig()
	config.Provider = ""
	config.Model = ""
	closer, err := Register(service, runtime, config, "first-prompt", sessiontitle.AutomaticFirstPrompt, SelectFirstPrompt)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer closer()
	sess := createSession(t, store, "a")
	// A user message with no request/header: the pending work never starts,
	// so exercise the route failure through a direct provider call instead.
	messages := []sessionquery.SessionTitleUserMessage{{Seq: 0, Text: "hello"}}
	if _, err := generateWithLlm(runtime, mustResolve(t, config), sessiontitle.ProviderRequest{
		Session:  sess,
		Messages: messages,
		Signal:   context.Background(),
	}, messages, "first-prompt"); err == nil || !strings.Contains(err.Error(), "no logged request route") {
		t.Fatalf("route failure = %v", err)
	}
	if adapter.options() != nil {
		t.Fatal("aux dispatch happened without a route")
	}
}
