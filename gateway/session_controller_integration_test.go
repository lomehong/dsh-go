package gateway

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/session/projectioncache"
	"dshgo/sessionquery"
)

// sessionListCacheStore is an in-memory projection-cache store seam.
type sessionListCacheStore struct {
	mu      sync.Mutex
	records map[session.SessionID]*projectioncache.Record
}

func newSessionListCacheStore() *sessionListCacheStore {
	return &sessionListCacheStore{records: map[session.SessionID]*projectioncache.Record{}}
}

func (m *sessionListCacheStore) Get(id session.SessionID) (*projectioncache.Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[id]
	return record, ok
}

func (m *sessionListCacheStore) Put(id session.SessionID, record *projectioncache.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[id] = record
	return nil
}

func (m *sessionListCacheStore) Close() error { return nil }

// sessionListFixture wires a live session, its projection cache, and the
// session-query engine the controller lists through.
type sessionListFixture struct {
	store       *session.Store
	cache       *projectioncache.Service
	cacheStore  *sessionListCacheStore
	projections *projection.Registry
	engine      *sessionquery.Engine
	controller  *SessionController
}

func newSessionListFixture(t *testing.T) *sessionListFixture {
	t.Helper()
	store := session.NewStore(nil)
	projections := projection.NewRegistry()
	if _, err := projections.Register(SessionListMetadataUnit().Definition()); err != nil {
		t.Fatalf("register sessionListMetadata: %v", err)
	}
	cacheStore := newSessionListCacheStore()
	cache, err := projectioncache.New(cacheStore, projections, nil, nil, projectioncache.Config{
		WriteEveryEvents: 1, WriteIntervalMs: 60000,
	})
	if err != nil {
		t.Fatalf("projection cache: %v", err)
	}
	engine, err := sessionquery.NewEngine(
		sessionquery.StoreSessions{Store: store},
		nil,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	controller := NewSessionController(
		func() any { return engine },
		func() any { return cache },
		nil,
		nil,
	)
	return &sessionListFixture{
		store:       store,
		cache:       cache,
		cacheStore:  cacheStore,
		projections: projections,
		engine:      engine,
		controller:  controller,
	}
}

// createLiveMaterializedSession creates one live session, starts a turn, and
// drives the committed events through the projection registry so the cache
// row carries a cleared blank and a user-prompt time.
func (f *sessionListFixture) createLiveMaterializedSession(t *testing.T, id string, createdAt int64) *session.Session {
	t.Helper()
	sess, err := f.store.Create(id, session.CreateOptions{HeaderMetadata: session.SessionHeader{CreatedAt: createdAt}})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}},
		llm.MessageSource{Kind: llm.SourceUser},
	)
	if _, err := sess.Append(session.EventUserMessage, message, &session.SurfaceIntent{
		SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend},
	}); err != nil {
		t.Fatalf("user/message: %v", err)
	}
	events := sess.Events()
	for _, event := range events {
		f.projections.Drive(sess, event)
	}
	if err := f.cache.Write(sess); err != nil {
		t.Fatalf("cache write: %v", err)
	}
	return sess
}

func TestSessionListReadsProjectionMetadata(t *testing.T) {
	f := newSessionListFixture(t)
	sess := f.createLiveMaterializedSession(t, "materialized", 1000)

	value, err := f.controller.List(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	body := value.(map[string]any)
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	item := items[0].(map[string]any)
	if item["sessionId"] != string(sess.ID()) {
		t.Fatalf("sessionId = %v", item["sessionId"])
	}
	blank, ok := item["blank"].(bool)
	if !ok {
		t.Fatalf("blank = %T", item["blank"])
	}
	if blank {
		t.Fatal("a session with a turn/start must not be blank")
	}
	updatedAt, ok := item["updatedAt"].(float64)
	if !ok {
		t.Fatalf("updatedAt = %T", item["updatedAt"])
	}
	lastEvent := sess.Events()[len(sess.Events())-1]
	if int64(updatedAt) != lastEvent.Time {
		t.Fatalf("updatedAt = %v, want the user prompt time %d", updatedAt, lastEvent.Time)
	}
	if int64(updatedAt) <= 1000 {
		t.Fatalf("updatedAt must outrank the creation stamp: %v vs 1000", updatedAt)
	}
}

func TestSessionListCacheMissFallsBackToVisible(t *testing.T) {
	f := newSessionListFixture(t)
	if _, err := f.store.Create("cold", session.CreateOptions{
		HeaderMetadata: session.SessionHeader{CreatedAt: 2000},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	value, err := f.controller.List(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items := value.(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	item := items[0].(map[string]any)
	// No projection row was ever written for the session lifecycle: the
	// cache-miss posture stays visible (blank false) with the creation
	// stamp as the updatedAt floor.
	if blank, _ := item["blank"].(bool); blank {
		t.Fatal("cache-miss session must stay visible (blank false)")
	}
	updatedAt, _ := item["updatedAt"].(float64)
	if int64(updatedAt) != 2000 {
		t.Fatalf("updatedAt = %v, want creation stamp 2000", updatedAt)
	}
}

func TestSessionListNilEngineAnswersEmpty(t *testing.T) {
	controller := NewSessionController(nil, nil, nil, nil)
	value, err := controller.List(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	items := value.(map[string]any)["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

var _ = json.Marshal
