package sessionquery

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"dshgo/llm"
	session "dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/projection"
)

// --- fakes -----------------------------------------------------------------

type testLogger struct{}

func (testLogger) Warn(string) {}

// fakeBackend is an in-memory persistence.Backend: stored prefixes are
// prepopulated by tests; nothing else is exercised.
type fakeBackend struct {
	mu      sync.Mutex
	stored  map[session.SessionID]*persistence.StoredPrefix
	listErr error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{stored: map[session.SessionID]*persistence.StoredPrefix{}}
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) LoadStored(id session.SessionID) (*persistence.StoredPrefix, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stored[id], nil
}

func (b *fakeBackend) ReadStoredRevision(id session.SessionID) (persistence.Revision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if prefix, ok := b.stored[id]; ok {
		return prefix.Revision, nil
	}
	return "", nil
}

func (b *fakeBackend) CommitRepair(meta session.SessionHeader, tornMarker any, closers []session.Event) error {
	return nil
}

func (b *fakeBackend) AppendBatch(meta session.SessionHeader, events []session.Event, materialized bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix, ok := b.stored[meta.ID]
	if !ok {
		prefix = &persistence.StoredPrefix{Meta: meta, Revision: persistence.Revision("rev-" + string(meta.ID))}
		b.stored[meta.ID] = prefix
	}
	prefix.Events = append(prefix.Events, events...)
	return nil
}

func (b *fakeBackend) List() ([]session.SessionHeader, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.listErr != nil {
		return nil, b.listErr
	}
	out := make([]session.SessionHeader, 0, len(b.stored))
	for _, prefix := range b.stored {
		out = append(out, prefix.Meta)
	}
	return out, nil
}

func (b *fakeBackend) ListSnapshots() ([]persistence.Snapshot, error) { return nil, nil }

func (b *fakeBackend) Close() error { return nil }

func (b *fakeBackend) ReadStoredFrom(id session.SessionID, fromSeq int64) (*persistence.StoredSuffix, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	prefix, ok := b.stored[id]
	if !ok {
		return nil, &persistence.NotFoundError{SessionID: id}
	}
	out := &persistence.StoredSuffix{Meta: prefix.Meta}
	for _, event := range prefix.Events {
		if event.Seq >= fromSeq {
			out.Events = append(out.Events, event)
		}
	}
	return out, nil
}

func (b *fakeBackend) Locate(meta session.SessionHeader) *persistence.Location { return nil }

func (b *fakeBackend) ReadStoredRaw(id session.SessionID) (persistence.RawArtifact, error) {
	return persistence.RawArtifact{}, &persistence.NotFoundError{SessionID: id}
}

// fakeProjections returns canned snapshots for both seams.
type fakeProjections struct {
	liveOK     bool
	liveValue  int
	hydrateErr error
	liveCalls  int
	hydCalls   int
}

func (f *fakeProjections) SnapshotLive(*session.Session) (projection.Snapshot, bool) {
	f.liveCalls++
	if !f.liveOK {
		return projection.Snapshot{}, false
	}
	return projection.Snapshot{AsOfSeq: 7, Values: map[string]any{"todos": f.liveValue}}, true
}

func (f *fakeProjections) HydratePrepared(*session.Session, session.SessionHeader, []session.Event) (projection.Snapshot, bool, error) {
	f.hydCalls++
	if f.hydrateErr != nil {
		return projection.Snapshot{}, false, f.hydrateErr
	}
	return projection.Snapshot{AsOfSeq: 7, Values: map[string]any{"todos": f.liveValue}}, true, nil
}

// coordinatorSessions adapts the live store to the persistence.Sessions seam.
type coordinatorSessions struct{ store *session.Store }

func (c coordinatorSessions) Get(id session.SessionID) (*session.Session, bool) {
	if sess := c.store.Get(id); sess != nil {
		return sess, true
	}
	return nil, false
}

func (c coordinatorSessions) List() []*session.Session {
	ids := c.store.List()
	out := make([]*session.Session, 0, len(ids))
	for _, id := range ids {
		if sess := c.store.Get(id); sess != nil {
			out = append(out, sess)
		}
	}
	return out
}

func (c coordinatorSessions) Prepare(id session.SessionID, seed []session.Event, meta session.SessionHeader) (*session.Session, error) {
	return session.NewRestored(id, seed, meta)
}

// --- fixture ---------------------------------------------------------------

type queryFixture struct {
	store       *session.Store
	backend     *fakeBackend
	projections *fakeProjections
	corpus      *SessionCorpus
	engine      *Engine
}

func newFixture(t *testing.T) *queryFixture {
	t.Helper()
	store := session.NewStore(testLogger{})
	backend := newFakeBackend()
	coordinator, err := persistence.NewCoordinator(backend, coordinatorSessions{store}, testLogger{}, persistence.CoordinatorOptions{})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	projections := &fakeProjections{liveOK: true, liveValue: 3}
	engine, err := NewEngine(StoreSessions{store}, coordinator, projections, nil, nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return &queryFixture{
		store:       store,
		backend:     backend,
		projections: projections,
		corpus:      NewSessionCorpus(StoreSessions{store}, coordinator, 4),
		engine:      engine,
	}
}

func (f *queryFixture) createSession(t *testing.T, id string, createdAt int64, parent string) *session.Session {
	t.Helper()
	header := session.SessionHeader{CreatedAt: createdAt}
	if parent != "" {
		header.ParentSession = parent
	}
	s, err := f.store.Create(id, session.CreateOptions{HeaderMetadata: header})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return s
}

func (f *queryFixture) persist(t *testing.T, s *session.Session, revision persistence.Revision) {
	t.Helper()
	f.backend.stored[s.ID()] = &persistence.StoredPrefix{
		Meta:     s.Header(),
		Events:   s.Events(),
		Revision: revision,
	}
}

// storedPrefix builds a stored prefix that never existed in the live store:
// one user/message event per text argument.
func storedPrefix(t *testing.T, id string, createdAt int64, texts ...string) *persistence.StoredPrefix {
	t.Helper()
	events := make([]session.Event, 0, len(texts))
	for index, text := range texts {
		events = append(events, session.Event{
			Seq:       int64(index),
			Time:      int64(1000 + index),
			Type:      session.EventUserMessage,
			Data:      mustJSON(t, userMessage("u"+string(rune('0'+index)), text)),
			SurfaceOp: &session.SurfaceOp{Kind: session.SurfaceAppend},
		})
	}
	return &persistence.StoredPrefix{
		Meta:     session.SessionHeader{ID: id, CreatedAt: createdAt},
		Events:   events,
		Revision: persistence.Revision("rev-" + id),
	}
}

var _ = json.Marshal
var _ = strings.Contains
var _ = llm.RoleUser

// --- corpus ----------------------------------------------------------------

func TestListSessionsOrderingAndFlags(t *testing.T) {
	f := newFixture(t)
	f.createSession(t, "a", 30, "")
	livePersisted := f.createSession(t, "c", 10, "")
	f.backend.stored["b"] = storedPrefix(t, "b", 20, "stored question")
	f.backend.stored["d"] = storedPrefix(t, "d", 20, "tie")
	f.persist(t, livePersisted, "rev-c")
	f.backend.stored["e"] = storedPrefix(t, "e", 5, "oldest")

	records, err := f.corpus.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	var ids []string
	for _, record := range records {
		ids = append(ids, record.Header.ID)
	}
	if strings.Join(ids, ",") != "a,b,d,c,e" {
		t.Fatalf("order = %v", ids)
	}
	byID := map[string]SessionRecord{}
	for _, record := range records {
		byID[record.Header.ID] = record
	}
	if !byID["a"].Live || byID["a"].Persisted {
		t.Fatalf("a flags = %+v", byID["a"])
	}
	if byID["b"].Live || !byID["b"].Persisted {
		t.Fatalf("b flags = %+v", byID["b"])
	}
	if !byID["c"].Live || !byID["c"].Persisted {
		t.Fatalf("c flags = %+v", byID["c"])
	}
}

func TestListSessionsHeaderConflict(t *testing.T) {
	f := newFixture(t)
	live := f.createSession(t, "x", 30, "")
	stored := &persistence.StoredPrefix{Meta: live.Header(), Events: live.Events(), Revision: "rev-x"}
	stored.Meta.CreatedAt = 999
	f.backend.stored["x"] = stored
	if _, err := f.corpus.ListSessions(context.Background()); err == nil {
		t.Fatal("header conflict accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSourceConflict {
			t.Fatalf("conflict error = %v", err)
		}
	}
}

func TestLoadSources(t *testing.T) {
	f := newFixture(t)
	live := f.createSession(t, "a", 30, "")
	appendUserMessage(t, live, "u1", "hello")
	f.backend.stored["b"] = storedPrefix(t, "b", 20, "stored question")

	// Live-preferred: a failing persistence listing is never touched.
	f.backend.listErr = errors.New("storage offline")
	loaded, err := f.corpus.Load(context.Background(), "a")
	if err != nil {
		t.Fatalf("live load failed: %v", err)
	}
	if loaded.Header.ID != "a" || len(loaded.Events) != 1 {
		t.Fatalf("live load = %+v", loaded)
	}
	f.backend.listErr = nil

	gotPersisted, err := f.corpus.Load(context.Background(), "b")
	if err != nil {
		t.Fatalf("persisted load failed: %v", err)
	}
	if gotPersisted.Header.ID != "b" || len(gotPersisted.Events) != 1 || gotPersisted.Events[0].Seq != 0 {
		t.Fatalf("persisted load = %+v", gotPersisted)
	}

	if _, err := f.corpus.Load(context.Background(), "missing"); err == nil {
		t.Fatal("missing session accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSessionNotFound {
			t.Fatalf("missing load error = %v", err)
		}
	}

	// A corrupt stored log (invalid message payload) is isolated as
	// CORRUPT_SESSION.
	corrupt := &persistence.StoredPrefix{
		Meta: session.SessionHeader{ID: "c", CreatedAt: 10},
		Events: []session.Event{
			{Seq: 0, Time: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{"broken":true}`)},
		},
		Revision: "rev-c",
	}
	f.backend.stored["c"] = corrupt
	_, err = f.corpus.Load(context.Background(), "c")
	if err == nil {
		t.Fatal("corrupt session accepted")
	}
	var queryErr *SessionQueryError
	if !errors.As(err, &queryErr) || queryErr.Code != CodeCorruptSession {
		t.Fatalf("corrupt error = %v", err)
	}

	// Listing failure surfaces as PERSISTENCE_FAILED.
	f.backend.listErr = errors.New("storage offline")
	if _, err := f.corpus.ListSessions(context.Background()); err == nil {
		t.Fatal("listing failure accepted")
	} else if !errors.As(err, &queryErr) || queryErr.Code != CodePersistenceFailed {
		t.Fatalf("listing error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestProjectManyOrderAndIsolation(t *testing.T) {
	f := newFixture(t)
	f.createSession(t, "a", 30, "")
	f.backend.stored["b"] = storedPrefix(t, "b", 20, "one")
	f.backend.stored["c"] = &persistence.StoredPrefix{
		Meta: session.SessionHeader{ID: "c", CreatedAt: 10},
		Events: []session.Event{
			{Seq: 0, Time: 1, Type: session.EventUserMessage, Data: json.RawMessage(`{"broken":true}`)},
		},
		Revision: "rev-c",
	}

	results, err := ProjectMany(context.Background(), f.corpus, []session.SessionID{"b", "a", "c"}, func(source LogicalSessionSource) (int, error) {
		return len(source.Events), nil
	})
	if err != nil {
		t.Fatalf("batch failed: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results = %d", len(results))
	}
	wantOrder := []string{"b", "a", "c"}
	for index, want := range wantOrder {
		if results[index].SessionID != session.SessionID(want) {
			t.Fatalf("order[%d] = %s", index, results[index].SessionID)
		}
	}
	if results[0].Fulfilled != true || results[0].Value != 1 {
		t.Fatalf("b result = %+v", results[0])
	}
	if results[1].Fulfilled != true || results[1].Value != 0 {
		t.Fatalf("a result = %+v", results[1])
	}
	if results[2].Fulfilled {
		t.Fatalf("c fulfilled: %+v", results[2])
	}
	var queryErr *SessionQueryError
	if !errors.As(results[2].Reason, &queryErr) || queryErr.Code != CodeCorruptSession {
		t.Fatalf("c reason = %v", results[2].Reason)
	}

	// A cancelled context aborts the batch.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ProjectMany(ctx, f.corpus, []session.SessionID{"a"}, func(LogicalSessionSource) (int, error) { return 0, nil })
	if err == nil {
		t.Fatal("cancelled batch accepted")
	}
}

// --- observation -----------------------------------------------------------

func TestObserveSessionLive(t *testing.T) {
	f := newFixture(t)
	live := f.createSession(t, "a", 30, "")
	appendUserMessage(t, live, "u1", "hello")
	appendUserMessage(t, live, "u2", "again")

	observation, err := f.engine.ObserveSession(context.Background(), "a", SessionObservationOptions{ProjectionMode: ProjectionModeAll})
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	if observation.Source != "live" || observation.Header.ID != "a" || observation.Cursor != 1 || observation.Revision != nil {
		t.Fatalf("observation = source:%s cursor:%d rev:%v", observation.Source, observation.Cursor, observation.Revision)
	}
	if observation.Projections == nil || observation.Projections.AsOfSeq != 7 {
		t.Fatalf("projections = %+v", observation.Projections)
	}

	// Retain duplicates the lease; both leases must be released.
	retained, err := observation.Retain()
	if err != nil {
		t.Fatalf("retain failed: %v", err)
	}
	observation.Release()
	if retained.Header.ID != "a" {
		t.Fatal("retained observation unusable after sibling release")
	}
	retained.Release()
	if _, err := observation.Retain(); err == nil {
		t.Fatal("retain after final release accepted")
	}

	// None mode leaves projections untouched.
	plain, err := f.engine.ObserveSession(context.Background(), "a", SessionObservationOptions{ProjectionMode: ProjectionModeNone})
	if err != nil {
		t.Fatalf("none observe failed: %v", err)
	}
	if plain.Projections != nil {
		t.Fatalf("none projections = %+v", plain.Projections)
	}
}

func TestObserveSessionPrepared(t *testing.T) {
	f := newFixture(t)
	f.backend.stored["b"] = storedPrefix(t, "b", 20, "stored question")

	observation, err := f.engine.ObserveSession(context.Background(), "b", SessionObservationOptions{})
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	if observation.Source != "prepared" {
		t.Fatalf("source = %q", observation.Source)
	}
	if observation.Revision == nil || *observation.Revision != persistence.Revision("rev-b") {
		t.Fatalf("revision = %v", observation.Revision)
	}
	if observation.Cursor != 0 || len(observation.Events) != 1 {
		t.Fatalf("events = %d cursor = %d", len(observation.Events), observation.Cursor)
	}
	if observation.Projections == nil || observation.Projections.Values["todos"] != 3 {
		t.Fatalf("hydrated projections = %+v", observation.Projections)
	}
	observation.Release()
	observation.Release() // idempotent

	// A mismatched prepared payload fails loudly: the coordinator rejects
	// stored identities that do not match the requested id before the
	// reader's own conflict check (defensive) can run.
	mismatch := storedPrefix(t, "b", 20, "stored question")
	mismatch.Meta.ID = "other"
	f.backend.stored["mismatch"] = mismatch
	if _, err = f.engine.ObserveSession(context.Background(), "mismatch", SessionObservationOptions{}); err == nil {
		t.Fatal("source conflict accepted")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("conflict error = %v", err)
	}

	// Hydration failure classifies as corruption.
	f.backend.stored["b"] = storedPrefix(t, "b", 20, "stored question")
	f.projections.hydrateErr = errors.New("projection cache stale")
	_, err = f.engine.ObserveSession(context.Background(), "b", SessionObservationOptions{})
	if err == nil {
		t.Fatal("hydration failure accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeCorruptSession {
			t.Fatalf("hydration error = %v", err)
		}
	}
}

// --- engine ----------------------------------------------------------------

func TestEngineReadSession(t *testing.T) {
	f := newFixture(t)
	live := f.createSession(t, "a", 30, "")
	appendUserMessage(t, live, "u1", "hello")
	persisted := f.createSession(t, "b", 20, "")
	appendUserMessage(t, persisted, "u2", "stored question")
	f.persist(t, persisted, "rev-b")

	snapshot, err := f.engine.ReadSession(context.Background(), "b")
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if snapshot.Session.ID != "b" || len(snapshot.Events) != 1 || snapshot.Events[0].Seq != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, err := f.engine.ReadSession(context.Background(), "missing"); err == nil {
		t.Fatal("missing read accepted")
	}
}

func TestEngineListAndFilterEvents(t *testing.T) {
	f := newFixture(t)
	s := f.createSession(t, "a", 30, "")
	populateLog(t, s)

	records, err := f.engine.ListEvents(context.Background(), "a")
	if err != nil {
		t.Fatalf("list events failed: %v", err)
	}
	if len(records) != 6 {
		t.Fatalf("records = %d", len(records))
	}
	documents, err := f.engine.FilterEvents(context.Background(), "a", []SessionEventResultFilter{{Kind: "text", Text: "config"}})
	if err != nil {
		t.Fatalf("filter events failed: %v", err)
	}
	if len(documents) == 0 {
		t.Fatal("text filter matched nothing")
	}
	for _, document := range documents {
		if !strings.Contains(document.Text, "config") && !strings.Contains(strings.ToLower(document.Text), "config") {
			t.Fatalf("document %d text %q lacks needle", document.Seq, document.Text)
		}
	}
}

func TestEngineReadSurface(t *testing.T) {
	f := newFixture(t)
	s := f.createSession(t, "a", 30, "")
	populateLog(t, s)
	surface, err := f.engine.ReadSurface(context.Background(), "a")
	if err != nil {
		t.Fatalf("read surface failed: %v", err)
	}
	if surface.Session.ID != "a" {
		t.Fatalf("surface session = %+v", surface.Session)
	}
	last := int64(5)
	if surface.CapturedThroughSeq == nil || *surface.CapturedThroughSeq != last {
		t.Fatalf("capturedThrough = %v", surface.CapturedThroughSeq)
	}
	if len(surface.Events) != 3 {
		t.Fatalf("surface events = %d", len(surface.Events))
	}
	f.createSession(t, "empty", 1, "")
	emptySurface, err := f.engine.ReadSurface(context.Background(), "empty")
	if err != nil {
		t.Fatalf("empty surface failed: %v", err)
	}
	if emptySurface.CapturedThroughSeq != nil || len(emptySurface.Events) != 0 {
		t.Fatalf("empty surface = %+v", emptySurface)
	}
}

func TestEngineReadEventWindow(t *testing.T) {
	f := newFixture(t)
	s := f.createSession(t, "a", 30, "")
	for index, text := range []string{"first", "second", "third"} {
		appendUserMessage(t, s, llm.MessageID("u"+string(rune('0'+index))), text)
	}
	ctx := context.Background()

	window, err := f.engine.ReadEvent(ctx, SessionEventReadRequest{SessionID: "a", Seq: 1, Before: intPtr(1), After: intPtr(1)})
	if err != nil {
		t.Fatalf("read event failed: %v", err)
	}
	if window.StartSeq != 0 || window.EndSeq != 2 || len(window.Events) != 3 || window.Target.Seq != 1 {
		t.Fatalf("window = [%d,%d] n=%d target=%d", window.StartSeq, window.EndSeq, len(window.Events), window.Target.Seq)
	}
	window, err = f.engine.ReadEvent(ctx, SessionEventReadRequest{SessionID: "a", Seq: 2, Before: intPtr(9), After: intPtr(9)})
	if err != nil {
		t.Fatalf("clamped read failed: %v", err)
	}
	if window.StartSeq != 0 || window.EndSeq != 2 {
		t.Fatalf("clamped window = [%d,%d]", window.StartSeq, window.EndSeq)
	}
	if _, err := f.engine.ReadEvent(ctx, SessionEventReadRequest{SessionID: "a", Seq: 99}); err == nil {
		t.Fatal("missing event accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeEventNotFound {
			t.Fatalf("missing event error = %v", err)
		}
	}
	if _, err := f.engine.ReadEvent(ctx, SessionEventReadRequest{SessionID: "a", Seq: 1, Before: intPtr(99)}); err == nil {
		t.Fatal("oversized window accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeInvalidWindow {
			t.Fatalf("window error = %v", err)
		}
	}
}

func intPtr(v int) *int { return &v }

func TestEngineReadTitleBatch(t *testing.T) {
	f := newFixture(t)
	s := f.createSession(t, "a", 30, "")
	appendUserMessage(t, s, "u1", "what is the plan")
	mustAppend(t, s, "session/title", titleEventData("the plan", []int64{0}, SessionTitleSource{Kind: TitleSourceFallback}), nil)
	bare := f.createSession(t, "b", 20, "")
	appendUserMessage(t, bare, "u2", "no title here")
	f.persist(t, bare, "rev-b")
	ctx := context.Background()

	title, err := f.engine.ReadTitle(ctx, "a")
	if err != nil {
		t.Fatalf("read title failed: %v", err)
	}
	if title == nil || title.Title != "the plan" || title.EventSeq <= 0 {
		t.Fatalf("title = %+v", title)
	}
	if _, err := f.engine.ReadTitle(ctx, "b"); err != nil {
		t.Fatalf("titleless read failed: %v", err)
	}

	observation, err := f.engine.ReadTitleSnapshot(ctx, "a")
	if err != nil {
		t.Fatalf("snapshot read failed: %v", err)
	}
	if observation.Session.ID != "a" || observation.Title == nil || observation.Title.Title != "the plan" {
		t.Fatalf("observation = %+v", observation)
	}

	batch, err := f.engine.ReadTitleSnapshots(ctx, []session.SessionID{"a", "b", "missing"})
	if err != nil {
		// Batch results carry per-session reasons; a total error is fatal.
		for _, result := range batch {
			if !result.Fulfilled {
				continue
			}
		}
	}
	if len(batch) != 3 {
		t.Fatalf("batch = %d", len(batch))
	}
	if !batch[0].Fulfilled || batch[0].Value == nil || batch[0].Value.Title == nil {
		t.Fatalf("batch[0] = %+v", batch[0])
	}
	if !batch[1].Fulfilled || batch[1].Value.Title != nil {
		t.Fatalf("batch[1] = %+v", batch[1])
	}
	if batch[2].Fulfilled {
		t.Fatalf("batch[2] = %+v", batch[2])
	}
	var queryErr *SessionQueryError
	if !errors.As(batch[2].Reason, &queryErr) || queryErr.Code != CodeSessionNotFound {
		t.Fatalf("batch[2] reason = %v", batch[2].Reason)
	}
}

func TestEngineSearchRequiresBackend(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.engine.SearchSessions(ctx, SessionSearchRequest{Query: "config"}); err == nil {
		t.Fatal("session search without backend accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSearchDisabled {
			t.Fatalf("session search error = %v", err)
		}
	}
	if _, err := f.engine.SearchEvents(ctx, SessionEventSearchRequest{Query: "config"}); err == nil {
		t.Fatal("event search without backend accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSearchDisabled {
			t.Fatalf("event search error = %v", err)
		}
	}
}

func TestEngineTraceAndFilterSessions(t *testing.T) {
	f := newFixture(t)
	root := f.createSession(t, "root", 1, "")
	appendUserMessage(t, root, "u0", "root start")
	child := f.createSession(t, "child", 30, "root")
	populateLog(t, child)
	f.persist(t, root, "rev-root")
	ctx := context.Background()

	trace, err := f.engine.TraceSession(ctx, "child")
	if err != nil {
		t.Fatalf("trace failed: %v", err)
	}
	if !trace.Complete || trace.Root == nil || trace.Root.Header.ID != "root" {
		t.Fatalf("trace = %+v", trace)
	}

	eventTrace, err := f.engine.TraceEvent(ctx, SessionEventTraceRequest{SessionID: "child", Seq: 2})
	if err != nil {
		t.Fatalf("event trace failed: %v", err)
	}
	if eventTrace.Session.ID != "child" || eventTrace.Target.Seq != 2 || eventTrace.ReplacedBy == nil || *eventTrace.ReplacedBy != 3 {
		t.Fatalf("event trace = %+v", eventTrace)
	}

	records, err := f.engine.FilterSessions(ctx, []SessionResultFilter{{Kind: "availability", Values: []string{AvailabilityPersisted}}})
	if err != nil {
		t.Fatalf("filter sessions failed: %v", err)
	}
	if len(records) != 1 || records[0].Header.ID != "root" {
		t.Fatalf("persisted filter = %+v", records)
	}
}

// populateLog appends the fixture replacement sequence onto an attached
// session: user -> tool/call (log-only) -> tool/result -> replacing
// tool/result -> assistant -> log-only chunk.
func populateLog(t *testing.T, s *session.Session) {
	t.Helper()
	appendUserMessage(t, s, "u1", "find the config file")
	call := mustAppend(t, s, session.EventToolCall,
		session.ToolCallData{Turn: 1, Step: 1, CallID: llm.ToolCallID("c1"), Name: "grep", Arguments: "{\"q\":\"config\"}"},
		nil)
	original := llm.Message{
		ID: "r1", Role: llm.RoleUser, Source: llm.MessageSource{Kind: llm.SourceTool, CallID: llm.ToolCallID("c1")},
		Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1",
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "raw dump"}}}},
	}
	result := mustAppend(t, s, session.EventToolResult,
		session.ToolResultData{Turn: 1, Step: 1, Message: original},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceEventSeqs: []int64{call.Seq}, SourceSeqsPresent: true})
	replacement := original
	replacement.Content = []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1",
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "trimmed config dump"}}}}
	mustAppend(t, s, session.EventToolResult,
		session.ToolResultData{Turn: 1, Step: 1, Message: replacement},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceReplace, Start: result.Seq, End: result.Seq}, SourceEventSeqs: []int64{result.Seq}, SourceSeqsPresent: true})
	mustAppend(t, s, session.EventAssistantMsg,
		session.AssistantMessageData{Turn: 1, Step: 1, Message: llm.Message{
			ID: "a1", Role: llm.RoleAssistant,
			Source:  llm.MessageSource{Kind: llm.SourceModel, Provider: "deepseek", Model: "m1"},
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "found config"}},
		}},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceSeqsPresent: true})
	mustAppend(t, s, session.EventAssistantChunk, map[string]any{"turn": 1}, nil)
}
