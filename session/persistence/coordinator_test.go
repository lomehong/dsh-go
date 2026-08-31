package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/llm"
	"dshgo/session"
)

// memoryBackend is an in-memory PersistenceBackend: one log per id with a
// revision that changes on every durable mutation.
type memoryBackend struct {
	mu      sync.Mutex
	name    string
	logs    map[session.SessionID]*memLog
	closed  bool
	repairs []repairRecord
}

type memLog struct {
	header   session.SessionHeader
	events   []session.Event
	revision Revision
	torn     any // torn-tail marker when the tail is torn
}

type repairRecord struct {
	meta       session.SessionHeader
	tornMarker any
	closers    int
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{name: "memory", logs: map[session.SessionID]*memLog{}}
}

func (b *memoryBackend) Name() string { return b.name }

func (b *memoryBackend) LoadStored(id session.SessionID) (*StoredPrefix, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.logs[id]
	if log == nil {
		return nil, nil
	}
	return &StoredPrefix{
		Meta:       session.DeepCopyHeader(log.header),
		Events:     deepCopyEvents(log.events),
		Revision:   log.revision,
		TornMarker: log.torn,
	}, nil
}

func (b *memoryBackend) ReadStoredRevision(id session.SessionID) (Revision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.logs[id]
	if log == nil {
		return "", nil
	}
	return log.revision, nil
}

func (b *memoryBackend) CommitRepair(meta session.SessionHeader, tornMarker any, closers []session.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.logs[meta.ID]
	if log == nil {
		return fmt.Errorf("no log for %q", meta.ID)
	}
	if tornMarker != nil {
		if truncateTo, ok := tornMarker.(int); ok {
			log.events = log.events[:truncateTo]
		}
		log.torn = nil
	}
	log.events = append(log.events, closers...)
	log.revision = bumpRevision(log.revision)
	b.repairs = append(b.repairs, repairRecord{meta: meta, tornMarker: tornMarker, closers: len(closers)})
	return nil
}

func (b *memoryBackend) AppendBatch(meta session.SessionHeader, events []session.Event, materialized bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.logs[meta.ID]
	if !materialized || log == nil {
		if log == nil {
			log = &memLog{header: session.DeepCopyHeader(meta), revision: Revision(string(meta.ID) + ":0")}
			b.logs[meta.ID] = log
		}
	}
	log.events = append(log.events, deepCopyEvents(events)...)
	log.revision = bumpRevision(log.revision)
	return nil
}

func (b *memoryBackend) List() ([]session.SessionHeader, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []session.SessionHeader
	for _, log := range b.logs {
		out = append(out, session.DeepCopyHeader(log.header))
	}
	return out, nil
}

func (b *memoryBackend) ListSnapshots() ([]Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Snapshot
	for _, log := range b.logs {
		out = append(out, Snapshot{Header: session.DeepCopyHeader(log.header), Revision: log.revision})
	}
	return out, nil
}

func (b *memoryBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

func bumpRevision(r Revision) Revision { return r + "*" }

func deepCopyEvents(events []session.Event) []session.Event {
	out := make([]session.Event, len(events))
	for i, event := range events {
		out[i] = session.DeepCopyEvent(event)
	}
	return out
}

// --- fixture builders --------------------------------------------------------

func testHeader(id session.SessionID) session.SessionHeader {
	return session.SessionHeader{
		ID:        id,
		Version:   session.SESSION_FORMAT_VERSION,
		CreatedAt: 42,
		CWD:       `D:\work`,
	}
}

func mustEvent(t *testing.T, eventType string, seq int64, data any) session.Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("encode %s: %v", eventType, err)
	}
	event := session.Event{Type: eventType, Seq: seq, Time: 1, Data: raw}
	// Surface-eligible events entering a seed require their surfaceOp
	// marker, exactly as a real log carries it.
	if session.IsSurfaceEventType(eventType) {
		event.SurfaceOp = &session.SurfaceOp{Kind: session.SurfaceAppend}
	}
	return event
}

// validUserMessageData marshals one identified user message, the payload
// shape session validation requires.
func validUserMessageData(text string) any {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
}

var appendIntent = &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}

func newTestCoordinator(t *testing.T, backend Backend) *Coordinator {
	t.Helper()
	coordinator, err := NewCoordinator(backend, nil, nil, CoordinatorOptions{WriteBatchMaxDelayMs: 40})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return coordinator
}

// --- create / append ---------------------------------------------------------

func TestCoordinatorCreateAppendAndContiguity(t *testing.T) {
	backend := newMemoryBackend()
	coordinator := newTestCoordinator(t, backend)
	id := session.SessionID("s1")
	if err := coordinator.Create(testHeader(id)); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Duplicate create rejects.
	if err := coordinator.Create(testHeader(id)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("dup create err = %v", err)
	}
	if err := coordinator.Append(id, []session.Event{mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1})}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Contiguity mismatch rejects.
	if err := coordinator.Append(id, []session.Event{mustEvent(t, session.EventTurnEnd, 5, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}})}); err == nil || !strings.Contains(err.Error(), "append seq mismatch") {
		t.Fatalf("contiguity err = %v", err)
	}
	// Legacy write-shape rejection at the shared append boundary.
	if err := coordinator.Append(id, []session.Event{mustEvent(t, "request/header-delta", 1, map[string]any{})}); err == nil || !strings.Contains(err.Error(), "request/header-delta") {
		t.Fatalf("legacy append err = %v", err)
	}
	// First append materialized the artifact (lazy creation is atomic with
	// the first batch). The bare open turn gains its interrupted closer at
	// load.
	inspection, err := coordinator.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(inspection.Events) != 2 || inspection.Events[1].Type != session.EventTurnEnd {
		t.Fatalf("events = %+v", inspection.Events)
	}
	if !backend.closed {
		// sanity: backend still open until dispose
	}
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
	if !backend.closed {
		t.Fatal("dispose did not close the backend")
	}
}

func TestCoordinatorCreateRejectsPersistedCollision(t *testing.T) {
	backend := newMemoryBackend()
	coordinator := newTestCoordinator(t, backend)
	id := session.SessionID("s1")
	if err := coordinator.Create(testHeader(id)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := coordinator.Append(id, []session.Event{mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1})}); err != nil {
		t.Fatalf("append: %v", err)
	}
	fresh := newTestCoordinator(t, backend)
	if err := fresh.Create(testHeader(id)); err == nil || !strings.Contains(err.Error(), "persisted log on disk") {
		t.Fatalf("collision err = %v", err)
	}
	// Appending through a coordinator that does not track the id fails
	// loud instead of inventing state.
	blind := newTestCoordinator(t, backend)
	if err := blind.Append(session.SessionID("ghost"), []session.Event{mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1})}); err == nil {
		t.Fatal("append on untracked id succeeded")
	}
}

// --- load / repair -----------------------------------------------------------

func TestCoordinatorLoadRepairsTornTailAndOpenTurn(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("torn")
	header := testHeader(id)
	// Seed a torn log with an open turn: 2 valid events + a torn tail.
	backend.logs[id] = &memLog{
		header: header,
		events: []session.Event{
			mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1}),
			mustEvent(t, session.EventUserMessage, 1, validUserMessageData("hi")),
		},
		revision: Revision("r1"),
		torn:     2, // truncate-to marker
	}
	coordinator := newTestCoordinator(t, backend)
	inspection, err := coordinator.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Closers: no pending tool calls here, but the open turn synthesizes an
	// interrupted turn/end only when a step is open; a bare turn with a
	// closed step closes nothing — verify via the balanced inspection.
	if len(inspection.Events) < 2 {
		t.Fatalf("events = %d", len(inspection.Events))
	}
	// The repair committed: torn marker consumed, revision bumped.
	if len(backend.repairs) == 0 {
		t.Fatal("CommitRepair was not called for the torn tail")
	}
	// A second load converges: the repaired log loads without further
	// repair.
	before := len(backend.repairs)
	if _, err := coordinator.Load(id); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(backend.repairs) != before {
		t.Fatal("converged load repaired again")
	}
}

func TestCoordinatorLoadSynthesizesInterruptedTurnClosers(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("open")
	backend.logs[id] = &memLog{
		header: testHeader(id),
		events: []session.Event{
			mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1}),
			mustEvent(t, session.EventStepStart, 1, map[string]any{"turn": 1, "step": 1}),
			mustEvent(t, session.EventUserMessage, 2, validUserMessageData("hi")),
		},
		revision: Revision("r1"),
	}
	coordinator := newTestCoordinator(t, backend)
	inspection, err := coordinator.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	last := inspection.Events[len(inspection.Events)-1]
	if last.Type != session.EventTurnEnd {
		t.Fatalf("last event = %s", last.Type)
	}
	var reason struct {
		Reason struct {
			Kind string `json:"kind"`
		} `json:"reason"`
	}
	if err := json.Unmarshal(last.Data, &reason); err != nil {
		t.Fatalf("turn/end data: %v", err)
	}
	if reason.Reason.Kind != session.TurnEndInterrupted {
		t.Fatalf("closer reason = %+v", reason)
	}
	if len(backend.repairs) == 0 || backend.repairs[0].closers == 0 {
		t.Fatalf("repairs = %+v", backend.repairs)
	}
}

func TestCoordinatorLoadFormatRefusals(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("future")
	header := testHeader(id)
	header.Version = session.SESSION_FORMAT_VERSION + 3
	backend.logs[id] = &memLog{header: header, revision: Revision("r1")}
	coordinator := newTestCoordinator(t, backend)
	_, err := coordinator.Load(id)
	var unsupported *FormatUnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(unsupported.Error(), "upgrade the harness") {
		t.Fatalf("refusal = %q", unsupported.Error())
	}

	// Unknown event type refuses.
	backend2 := newMemoryBackend()
	id2 := session.SessionID("alien")
	backend2.logs[id2] = &memLog{
		header:   testHeader(id2),
		events:   []session.Event{mustEvent(t, "brand/new-event", 0, map[string]any{"x": 1})},
		revision: Revision("r1"),
	}
	coordinator2 := newTestCoordinator(t, backend2)
	_, err = coordinator2.Load(id2)
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Error(), "unknown to this harness") {
		t.Fatalf("unknown-type err = %v", err)
	}

	// An unknown type carrying the ignorable marker is informational: the
	// reader skips it instead of refusing the log.
	ignorable := mustEvent(t, "brand/future-notice", 0, map[string]any{"x": 1})
	ignorable.Ignorable = true
	backend3 := newMemoryBackend()
	id3 := session.SessionID("future-notice")
	backend3.logs[id3] = &memLog{
		header:   testHeader(id3),
		events:   []session.Event{ignorable},
		revision: Revision("r1"),
	}
	coordinator3 := newTestCoordinator(t, backend3)
	if _, err = coordinator3.Load(id3); err != nil {
		t.Fatalf("ignorable unknown type must load, got %v", err)
	}
}

func TestCoordinatorLoadCorruptionWrapsCause(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("bad")
	// A user/message payload that fails session validation (missing id).
	backend.logs[id] = &memLog{
		header: testHeader(id),
		events: []session.Event{
			mustEvent(t, session.EventUserMessage, 0, map[string]any{"role": "user", "content": "no id", "source": map[string]any{"kind": "user"}}),
		},
		revision: Revision("r1"),
	}
	coordinator := newTestCoordinator(t, backend)
	_, err := coordinator.Load(id)
	var corruption *CorruptionError
	if !errors.As(err, &corruption) {
		t.Fatalf("err = %v", err)
	}
	if corruption.Cause == nil {
		t.Fatal("corruption lost its cause")
	}
	if strings.Contains(corruption.Error(), "corrupt") {
		t.Fatalf("corruption message must not say corrupt for validation: %q", corruption.Error())
	}
}

// --- readFrom ----------------------------------------------------------------

func TestCoordinatorReadFromSequentialFallback(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("rf")
	backend.logs[id] = &memLog{
		header: testHeader(id),
		events: []session.Event{
			mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1}),
			mustEvent(t, session.EventUserMessage, 1, validUserMessageData("a")),
			mustEvent(t, session.EventStepEnd, 2, map[string]any{"turn": 1, "step": 1}),
			mustEvent(t, session.EventTurnEnd, 3, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
		},
		revision: Revision("r1"),
	}
	coordinator := newTestCoordinator(t, backend)
	suffix, err := coordinator.ReadFrom(id, 2)
	if err != nil {
		t.Fatalf("readFrom: %v", err)
	}
	if len(suffix.Events) != 2 || suffix.Events[0].Seq != 2 {
		t.Fatalf("suffix = %+v", suffix.Events)
	}
	if _, err := coordinator.ReadFrom(id, -1); err == nil {
		t.Fatal("negative fromSeq accepted")
	}
	var notFound *NotFoundError
	if _, err := coordinator.ReadFrom(session.SessionID("absent"), 0); !errors.As(err, &notFound) {
		t.Fatalf("absent err = %v", err)
	}
}

// --- legacy migration ----------------------------------------------------------

func TestCoordinatorLegacyMigrationSteeringAndMessages(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("legacy")
	// Pre-identity user/message + steering/message + old turn/start trigger,
	// contiguous from seq 0 in log order.
	oldTurnStart := mustEvent(t, session.EventTurnStart, 0, map[string]any{
		"turn": 1, "trigger": map[string]any{"kind": "user"},
	})
	legacyUser := mustEvent(t, session.EventUserMessage, 1, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "plain"}},
		"source":  map[string]any{"kind": "user"},
	})
	legacySteering := mustEvent(t, legacySteeringType, 2, map[string]any{
		"turn": 1, "content": []any{map[string]any{"type": "text", "text": "steer"}},
		"source": map[string]any{"kind": "user"},
	})
	// The old steering event was surface-recorded; migration preserves the
	// envelope so the upgraded user/message stays surface-eligible.
	legacySteering.SurfaceOp = &session.SurfaceOp{Kind: session.SurfaceAppend}
	backend.logs[id] = &memLog{
		header:   testHeader(id),
		events:   []session.Event{oldTurnStart, legacyUser, legacySteering},
		revision: Revision("r1"),
	}
	coordinator := newTestCoordinator(t, backend)
	inspection, err := coordinator.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, event := range inspection.Events {
		if event.Type == legacySteeringType {
			t.Fatal("steering/message survived migration")
		}
	}
	// turn/start lost its trigger; both messages became identified.
	if inspection.Events[0].Type != session.EventTurnStart {
		t.Fatalf("first = %s", inspection.Events[0].Type)
	}
	if strings.Contains(string(inspection.Events[0].Data), "trigger") {
		t.Fatalf("trigger survived: %s", inspection.Events[0].Data)
	}
	userEvent := inspection.Events[1]
	if !strings.Contains(string(userEvent.Data), `"legacy-message:legacy:1"`) {
		t.Fatalf("legacy id not minted: %s", userEvent.Data)
	}
	steered := inspection.Events[2]
	if steered.Type != session.EventUserMessage || !strings.Contains(string(steered.Data), "steer") {
		t.Fatalf("steering migration = %s %s", steered.Type, steered.Data)
	}
}

// --- write path ----------------------------------------------------------------

func TestCoordinatorWritePathBatchesAndFlushes(t *testing.T) {
	backend := newMemoryBackend()
	coordinator := newTestCoordinator(t, backend)
	live, err := session.NewDetached("live1", nil, &[]session.SessionHeader{testHeader("live1")}[0])
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	coordinator.AttachLive(live)
	first, err := live.Append(session.EventUserMessage, validUserMessageData("m1"), appendIntent)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	coordinator.NotifySessionEvent(live, first)
	storedCount := func() int {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		log := backend.logs["live1"]
		if log == nil {
			return 0
		}
		return len(log.events)
	}
	// The fixed window durably writes without an explicit flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if storedCount() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if storedCount() != 1 {
		t.Fatalf("stored = %d", storedCount())
	}
	// Flush is the immediate barrier for the next buffered write.
	second, err := live.Append(session.EventTurnEnd, map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}, nil)
	if err != nil {
		t.Fatalf("append2: %v", err)
	}
	coordinator.NotifySessionEvent(live, second)
	if err := coordinator.FlushSession(live); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if storedCount() != 2 {
		t.Fatalf("after flush stored = %d", storedCount())
	}
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}

func TestCoordinatorOnCreatedRejectsSeedCollision(t *testing.T) {
	backend := newMemoryBackend()
	coordinator := newTestCoordinator(t, backend)
	id := session.SessionID("coll")
	// Ownerless state via create(): one persisted event.
	if err := coordinator.Create(testHeader(id)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := coordinator.Append(id, []session.Event{mustEvent(t, session.EventUserMessage, 0, validUserMessageData("persisted"))}); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A live session with a DIFFERENT seed claiming the same id collides.
	live, err := session.NewDetached(id, []session.Event{mustEvent(t, session.EventUserMessage, 0, validUserMessageData("different"))}, &[]session.SessionHeader{testHeader(id)}[0])
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	coordinator.AttachLive(live)
	if err := coordinator.FlushSession(live); err == nil || !strings.Contains(err.Error(), "id collision") {
		t.Fatalf("collision flush err = %v", err)
	}
	// The matching seed claims the ownerless state and persists only the
	// suffix beyond the cursor.
	backend2 := newMemoryBackend()
	coordinator2 := newTestCoordinator(t, backend2)
	id2 := session.SessionID("claim")
	if err := coordinator2.Create(testHeader(id2)); err != nil {
		t.Fatalf("create2: %v", err)
	}
	first := mustEvent(t, session.EventUserMessage, 0, validUserMessageData("same"))
	if err := coordinator2.Append(id2, []session.Event{first}); err != nil {
		t.Fatalf("append2: %v", err)
	}
	seed := []session.Event{session.DeepCopyEvent(first), mustEvent(t, session.EventStepEnd, 1, map[string]any{"turn": 1, "step": 1})}
	live2, err := session.NewDetached(id2, seed, &[]session.SessionHeader{testHeader(id2)}[0])
	if err != nil {
		t.Fatalf("detached2: %v", err)
	}
	coordinator2.AttachLive(live2)
	if err := coordinator2.FlushSession(live2); err != nil {
		t.Fatalf("claim flush: %v", err)
	}
	backend2.mu.Lock()
	total := len(backend2.logs[id2].events)
	backend2.mu.Unlock()
	// The seed suffix beyond the cursor persists: the step/end plus the
	// constructor's end-seed marker (a known, replayable event).
	if total != 3 {
		t.Fatalf("total = %d", total)
	}
}

// --- prepare / release -----------------------------------------------------------

func TestCoordinatorPrepareReservesAndReleases(t *testing.T) {
	backend := newMemoryBackend()
	id := session.SessionID("prep")
	backend.logs[id] = &memLog{
		header: testHeader(id),
		events: []session.Event{
			mustEvent(t, session.EventTurnStart, 0, map[string]any{"turn": 1}),
			mustEvent(t, session.EventUserMessage, 1, validUserMessageData("resume me")),
		},
		revision: Revision("r1"),
	}
	coordinator := newTestCoordinator(t, backend)
	prep, err := coordinator.Prepare(id)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prep.Session == nil || len(prep.Inspection.Events) != 3 {
		t.Fatalf("prep events = %d", len(prep.Inspection.Events))
	}
	// The source log's open turn synthesized its interrupted closer.
	lastPrepared := prep.Inspection.Events[len(prep.Inspection.Events)-1]
	if lastPrepared.Type != session.EventTurnEnd || !strings.Contains(string(lastPrepared.Data), "interrupted") {
		t.Fatalf("last prepared = %s %s", lastPrepared.Type, lastPrepared.Data)
	}
	// While reserved, appends reject (the unpublished Session owns the id).
	if err := coordinator.Append(id, []session.Event{mustEvent(t, session.EventStepEnd, 2, map[string]any{"turn": 1, "step": 1})}); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("reserved append err = %v", err)
	}
	// Release unpublished+complete → reusable: a second prepare serves from
	// the same source without a backend read bumping anything.
	prep.Release(false)
	prep2, err := coordinator.Prepare(id)
	if err != nil {
		t.Fatalf("prepare2: %v", err)
	}
	if len(prep2.Inspection.Events) != 3 {
		t.Fatalf("prepare2 events = %d", len(prep2.Inspection.Events))
	}
	prep2.Release(false)
	if err := coordinator.Dispose(); err != nil {
		t.Fatalf("dispose: %v", err)
	}
}
