package projectioncache

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/session/projection"
)

// --- fixtures ----------------------------------------------------------------

type memStore struct {
	mu      sync.Mutex
	records map[session.SessionID]*Record
	puts    []session.SessionID
	closed  bool
	// note observes puts in the caller's order ledger (nil = silent).
	note func(string)
}

func newMemStore(note func(string)) *memStore {
	return &memStore{records: map[session.SessionID]*Record{}, note: note}
}

func (m *memStore) Get(id session.SessionID) (*Record, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[id]
	return record, ok
}

func (m *memStore) Put(id session.SessionID, record *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[id] = record
	m.puts = append(m.puts, id)
	if m.note != nil {
		m.note("put")
	}
	return nil
}

func (m *memStore) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// recordingSessions watches the flush-before-put ordering.
type recordingSessions struct {
	mu     sync.Mutex
	live   map[session.SessionID]*session.Session
	events []string
}

func newRecordingSessions() *recordingSessions {
	return &recordingSessions{live: map[session.SessionID]*session.Session{}}
}

func (r *recordingSessions) Get(id session.SessionID) (*session.Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess, ok := r.live[id]
	return sess, ok
}

func (r *recordingSessions) Flush(sess *session.Session) error {
	r.mu.Lock()
	r.events = append(r.events, "flush")
	r.mu.Unlock()
	return nil
}

func (r *recordingSessions) note(what string) {
	r.mu.Lock()
	r.events = append(r.events, what)
	r.mu.Unlock()
}

type collectLogger struct {
	mu   sync.Mutex
	warn []string
}

func (l *collectLogger) Warn(args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warn = append(l.warn, args[0].(string))
}

func countDefinition() projection.Definition {
	return projection.Definition{
		Key:          "count",
		StateVersion: 1,
		Init:         func(session.SessionHeader) any { return 0 },
		Apply: func(state any, event session.Event) any {
			return state.(int) + 1
		},
		Wire: &projection.WireView{View: func(state any) any {
			return map[string]any{"count": state}
		}},
		DecodeState: func(raw json.RawMessage) (any, error) {
			var n int
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, err
			}
			return n, nil
		},
	}
}

func cacheHeader(id session.SessionID) session.SessionHeader {
	return session.SessionHeader{ID: id, Version: session.SESSION_FORMAT_VERSION, CreatedAt: 99, CWD: "D:\\proj"}
}

func newCacheSession(t *testing.T, id string) *session.Session {
	t.Helper()
	header := cacheHeader(session.SessionID(id))
	sess, err := session.NewDetached(session.SessionID(id), nil, &header)
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	return sess
}

// driveAll drives every committed event through the registry, mirroring the
// cordis session/event wiring.
func driveAll(registry *projection.Registry, sess *session.Session, since int64) {
	events := sess.Events()
	for index := int(since + 1); index < len(events); index++ {
		registry.Drive(sess, events[index])
	}
}

// --- write path --------------------------------------------------------------

func TestWriteCheckpointsAfterFlushBarrier(t *testing.T) {
	sessions := newRecordingSessions()
	store := newMemStore(sessions.note)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	logger := &collectLogger{}
	service, err := New(store, registry, sessions, logger, Config{WriteEveryEvents: 4, WriteIntervalMs: 50})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	sess := newCacheSession(t, "w1")
	sessions.live[sess.ID()] = sess
	appendTurnEvents(t, sess)
	driveAll(registry, sess, -1)

	if err := service.Write(sess); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The durability barrier ran before the record landed.
	sessions.mu.Lock()
	order := append([]string{}, sessions.events...)
	sessions.mu.Unlock()
	if len(order) != 2 || order[0] != "flush" || order[1] != "put" {
		t.Fatalf("order = %v (flush must precede put)", order)
	}
	record, ok := store.Get(sess.ID())
	if !ok {
		t.Fatal("record missing")
	}
	if record.Identity.CreatedAt != 99 || record.Identity.CWD != "D:\\proj" {
		t.Fatalf("identity = %+v", record.Identity)
	}
	if record.Rows["count"].Seq != int64(len(sess.Events())-1) {
		t.Fatalf("row = %+v", record.Rows["count"])
	}
	var count int
	if err := json.Unmarshal(record.Rows["count"].Val, &count); err != nil || count != len(sess.Events()) {
		t.Fatalf("val = %s (%v)", record.Rows["count"].Val, err)
	}
}

func appendTurnEvents(t *testing.T, sess *session.Session) {
	t.Helper()
	if _, err := sess.Append(session.EventTurnStart, map[string]any{"turn": 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	if _, err := sess.Append(session.EventTurnEnd, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
}

func TestIdentityMismatchDiscardsRecord(t *testing.T) {
	store := newMemStore(nil)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	service, err := New(store, registry, nil, nil, Config{WriteEveryEvents: 4, WriteIntervalMs: 50})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	id := session.SessionID("life")
	// A record folded from an unrelated lifecycle under the same id.
	store.records[id] = &Record{
		Identity: Identity{CreatedAt: 1, CWD: "elsewhere"},
		Rows:     projection.Checkpoint{"count": {Ver: 1, Seq: 9, Val: json.RawMessage(`99`)}},
	}
	meta := cacheHeader(id)
	if _, ok := service.CachedSnapshot(meta); ok {
		t.Fatal("unrelated record served as this lifecycle's cache")
	}
	// The matching lifecycle reads it.
	store.records[id] = &Record{
		Identity: Identity{CreatedAt: 99, CWD: "D:\\proj"},
		Rows:     projection.Checkpoint{"count": {Ver: 1, Seq: 9, Val: json.RawMessage(`9`)}},
	}
	snapshot, ok := service.CachedSnapshot(meta)
	if !ok || snapshot.AsOfSeq != 9 {
		t.Fatalf("snapshot = %+v, %v", snapshot, ok)
	}
	if snapshot.Values["count"].(map[string]any)["count"] != 9 {
		t.Fatalf("values = %+v", snapshot.Values)
	}
}

func TestWriteBehindTriggers(t *testing.T) {
	store := newMemStore(nil)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	root := cordis.NewRoot(nil)
	service, err := New(store, registry, nil, nil, Config{WriteEveryEvents: 2, WriteIntervalMs: 30})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	detach := service.Attach(root)
	defer detach()

	sess := newCacheSession(t, "wb")
	// Creation is the first mandatory point: the seed-derived cut is written
	// immediately (drive is empty; only the marker exists).
	root.Waterfall("session/created", &projection.SessionCreatedPayload{Session: sess})
	waitFor(t, 500*time.Millisecond, func() bool { _, ok := store.Get(sess.ID()); return ok })

	// turn/end is a mandatory point regardless of the counter.
	appendTurnEvents(t, sess)
	driveAll(registry, sess, -1)
	root.Waterfall("session/event", &projection.SessionEventPayload{Session: sess, Event: sess.Events()[len(sess.Events())-1]})
	waitFor(t, 500*time.Millisecond, func() bool {
		record, ok := store.Get(sess.ID())
		return ok && record.Rows["count"].Seq == int64(len(sess.Events())-1)
	})
	store.mu.Lock()
	putsAfterTurnEnd := len(store.puts)
	store.mu.Unlock()

	// The count threshold fires without turn/end.
	_, err = sess.Append(session.EventTurnStart, map[string]any{"turn": 2}, nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	event := sess.Events()[len(sess.Events())-1]
	driveAll(registry, sess, event.Seq-1)
	root.Waterfall("session/event", &projection.SessionEventPayload{Session: sess, Event: event})
	waitFor(t, 500*time.Millisecond, func() bool {
		store.mu.Lock()
		defer store.mu.Unlock()
		return len(store.puts) > putsAfterTurnEnd
	})
}

func TestDetachFlushesAndClearsDirty(t *testing.T) {
	store := newMemStore(nil)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	root := cordis.NewRoot(nil)
	service, err := New(store, registry, nil, &collectLogger{}, Config{WriteEveryEvents: 100, WriteIntervalMs: 60000})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	detach := service.Attach(root)
	sess := newCacheSession(t, "d1")
	root.Waterfall("session/created", &projection.SessionCreatedPayload{Session: sess})
	waitFor(t, 500*time.Millisecond, func() bool { _, ok := store.Get(sess.ID()); return ok })

	appendTurnEvents(t, sess)
	driveAll(registry, sess, -1)
	// One event below the threshold; the dirty counter holds one pending.
	root.Waterfall("session/event", &projection.SessionEventPayload{Session: sess, Event: sess.Events()[0]})
	// Detach (the live-to-cold moment) is the final mandatory point.
	root.Waterfall("session/disposed", &projection.SessionDisposedPayload{Session: sess})
	waitFor(t, 500*time.Millisecond, func() bool {
		record, ok := store.Get(sess.ID())
		return ok && record.Rows["count"].Seq == int64(len(sess.Events())-1)
	})
	detach()
	if err := service.Close(); err != nil || !store.closed {
		t.Fatalf("close = %v, %v", err, store.closed)
	}
}

func TestColdSnapshotWritesBackFailSoft(t *testing.T) {
	store := newMemStore(nil)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	service, err := New(store, registry, nil, nil, Config{WriteEveryEvents: 4, WriteIntervalMs: 50})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	meta := cacheHeader("cold")
	logEvents := []session.Event{
		{Type: session.EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: session.EventTurnEnd, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	snapshot, err := service.ColdSnapshot(meta, logEvents)
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if snapshot.AsOfSeq != 1 || snapshot.Values["count"].(map[string]any)["count"] != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	waitFor(t, 500*time.Millisecond, func() bool { _, ok := store.Get(meta.ID); return ok })
	// The second cold read seeds from the refreshed row (same result).
	again, err := service.ColdSnapshot(meta, logEvents)
	if err != nil || again.Values["count"].(map[string]any)["count"] != 2 {
		t.Fatalf("second cold = %+v, %v", again, err)
	}
}

func TestHydratePreparedRetriesFromExactLogOnStaleRows(t *testing.T) {
	store := newMemStore(nil)
	registry := projection.NewRegistry()
	if _, err := registry.Register(countDefinition()); err != nil {
		t.Fatalf("register: %v", err)
	}
	service, err := New(store, registry, nil, nil, Config{WriteEveryEvents: 4, WriteIntervalMs: 50})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	meta := cacheHeader("hp")
	sess := newCacheSession(t, "hp")
	logEvents := []session.Event{
		{Type: session.EventTurnStart, Seq: 0, Time: 1, Data: json.RawMessage(`{"turn":1}`)},
		{Type: session.EventTurnEnd, Seq: 1, Time: 2, Data: json.RawMessage(`{"turn":1,"reason":{"kind":"completed"}}`)},
	}
	// A poisoned row: the version matches but the value is malformed. A
	// stale schema must not make a valid session unreadable.
	store.records[meta.ID] = &Record{
		Identity: Identity{CreatedAt: 99, CWD: "D:\\proj"},
		Rows:     projection.Checkpoint{"count": {Ver: 1, Seq: 5, Val: json.RawMessage(`{"object":"not an int"}`)}},
	}
	snapshot, err := service.HydratePrepared(sess, meta, logEvents)
	if err != nil {
		t.Fatalf("hydratePrepared: %v", err)
	}
	if snapshot.AsOfSeq != 1 || snapshot.Values["count"].(map[string]any)["count"] != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not hold in time")
}
