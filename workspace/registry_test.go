package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/storagedomain"
	"dshgo/storagejson"
)

// fakePersistence serves an in-memory stored-history listing.
type fakePersistence struct {
	mu      sync.Mutex
	headers []session.SessionHeader
	fail    bool
}

func (p *fakePersistence) List(ctx context.Context) ([]session.SessionHeader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return nil, errors.New("persistence unavailable")
	}
	return append([]session.SessionHeader{}, p.headers...), nil
}

func (p *fakePersistence) seed(id session.SessionID, cwd string, createdAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.headers = append(p.headers, session.SessionHeader{Version: 0, ID: id, CreatedAt: createdAt, CWD: cwd})
}

type recordingLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *recordingLogger) Info(args ...any)  {}
func (l *recordingLogger) Error(args ...any) {}
func (l *recordingLogger) Warn(args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warnings = append(l.warnings, fmt.Sprint(args...))
}

var _ cordis.Logger = (*recordingLogger)(nil)

// newRegistry builds a registry over a fresh json-backed facility. The
// persistence root is shared with the cwd roots so the same directory the
// headers name also exists. The root is canonicalized (the registry stores
// realpath-normalized paths) so short-form temp dirs — e.g. Windows 8.3
// TEMP — do not break path equality assertions.
func newRegistry(t *testing.T, host RegistryHost) (*Registry, string) {
	t.Helper()
	if host.Logger == nil {
		host.Logger = &recordingLogger{}
	}
	root := canonicalTempDir(t)
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json"},
		map[string]storagedomain.Backend{"json": storagejson.NewJsonStorageBackend(root)},
		nil)
	registry, dispose, err := NewRegistry(context.Background(), host, facility)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(dispose)
	return registry, root
}

// canonicalTempDir returns a realpath-normalized temp dir: the registry
// stores EvalSymlinks-expanded paths, so fixtures built from raw
// t.TempDir() paths break when TEMP is an 8.3 short form (Windows expands
// short names to long ones).
func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	return root
}

func TestCreateReusesCanonicalPathAndPrepends(t *testing.T) {
	persistence := &fakePersistence{}
	registry, root := newRegistry(t, RegistryHost{Persistence: persistence})
	dirA := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dirA, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	created, err := registry.Create(context.Background(), dirA, "Alpha")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Repeat creation resolves the same entity without a title change.
	again, err := registry.Create(context.Background(), dirA, "Other")
	if err != nil {
		t.Fatalf("re-create: %v", err)
	}
	if again.ID() != created.ID() || again.Title() != "Alpha" {
		t.Fatalf("re-create = %s/%s, want the existing record", again.ID(), again.Title())
	}
	// A second workspace prepends to the display order.
	dirB := filepath.Join(root, "beta")
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	second, err := registry.Create(context.Background(), dirB, "")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if second.Title() != "beta" {
		t.Fatalf("title = %s, want the basename fallback", second.Title())
	}
	order := registry.List()
	if len(order) != 2 || order[0].ID() != second.ID() || order[1].ID() != created.ID() {
		t.Fatalf("order = %d entries, newest-created first", len(order))
	}
	// A nonexistent path rejects with the realpath error; a file path
	// rejects as a non-directory.
	if _, err := registry.Create(context.Background(), filepath.Join(root, "ghost"), ""); err == nil {
		t.Fatal("a nonexistent path must reject")
	}
	file := filepath.Join(root, "plain.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := registry.Create(context.Background(), file, ""); err == nil ||
		err.Error() != "cannot create a workspace at '"+file+"': path is not a directory" {
		t.Fatalf("err = %v", err)
	}
	// ResolveByPath finds without creating; an owned path round-trips.
	resolved, err := registry.ResolveByPath(context.Background(), dirA)
	if err != nil || resolved == nil || resolved.ID() != created.ID() {
		t.Fatalf("resolve = %v %v", resolved, err)
	}
}

func TestHeaderValidatedMembershipAndFiltering(t *testing.T) {
	root := canonicalTempDir(t)
	dirA := filepath.Join(root, "alpha")
	dirB := filepath.Join(root, "beta")
	for _, dir := range []string{dirA, dirB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	persistence := &fakePersistence{}
	persistence.seed("s-a", dirA, 300)
	persistence.seed("s-a2", dirA, 200)
	persistence.seed("s-b", dirB, 100)
	// s-gone's cwd no longer exists; s-mismatch names another directory.
	persistence.seed("s-gone", filepath.Join(root, "vanished"), 50)
	registry, _ := newRegistry(t, RegistryHost{Persistence: persistence})

	entities := registry.List()
	if len(entities) != 2 {
		t.Fatalf("bootstrap produced %d workspaces, want 2 (grouped by cwd)", len(entities))
	}
	byPath := map[string]*Entity{}
	for _, entity := range entities {
		byPath[entity.Path()] = entity
	}
	alpha := byPath[dirA]
	if alpha == nil {
		t.Fatalf("alpha workspace not found for %s; got %d entities", dirA, len(entities))
	}
	if len(alpha.SessionIDs()) != 2 {
		t.Fatalf("alpha sessions = %v, want 2", alpha.SessionIDs())
	}
	// Newest session first inside the group.
	if alpha.SessionIDs()[0] != "s-a" || alpha.SessionIDs()[1] != "s-a2" {
		t.Fatalf("alpha order = %v", alpha.SessionIDs())
	}
	// The vanished cwd produced no workspace and no members anywhere.
	if _, ok := byPath[filepath.Join(root, "vanished")]; ok {
		t.Fatal("a vanished cwd must not own a workspace")
	}

	// A live session whose cwd mismatches its workspace is filtered with a
	// warning, and a later mutation prunes it durably.
	if err := alpha.AttachSession("s-b"); err == nil {
		t.Fatal("attaching a session whose cwd differs must fail")
	}
}

func TestBootstrapRanksByNewestThenPriorOrder(t *testing.T) {
	root := canonicalTempDir(t)
	dirs := map[string]string{}
	for _, name := range []string{"old", "new"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		dirs[name] = dir
	}
	persistence := &fakePersistence{}
	persistence.seed("s-old", dirs["old"], 100)
	persistence.seed("s-new", dirs["new"], 200)
	registry, _ := newRegistry(t, RegistryHost{Persistence: persistence})
	order := registry.List()
	if len(order) != 2 || order[0].Path() != dirs["new"] || order[1].Path() != dirs["old"] {
		t.Fatalf("order = %d entries, newest group first", len(order))
	}
	// Reopening keeps the durable order and skips re-bootstrap (initialized).
	reopened, _ := newRegistry(t, RegistryHost{Persistence: persistence})
	if len(reopened.List()) != 2 || reopened.List()[0].Path() != dirs["new"] {
		t.Fatalf("reopen order = %d entries", len(reopened.List()))
	}
}

func TestDeleteRetainsUnknownIdempotentAndRecoversMarker(t *testing.T) {
	persistence := &fakePersistence{}
	registry, root := newRegistry(t, RegistryHost{Persistence: persistence})
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	created, err := registry.Create(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Unknown ids are an idempotent no-op.
	deleted, err := registry.Delete(context.Background(), "ghost")
	if err != nil || deleted {
		t.Fatalf("delete ghost = %v %v", deleted, err)
	}
	deleted, err = registry.Delete(context.Background(), created.ID())
	if err != nil || !deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if registry.Get(created.ID()) != nil || len(registry.List()) != 0 {
		t.Fatal("the deleted workspace must leave the registry")
	}
	// Reopening after the committed delete stays empty (the marker was
	// cleared; nothing lingers to recover).
	reopened, _ := newRegistry(t, RegistryHost{Persistence: persistence})
	if len(reopened.List()) != 0 {
		t.Fatalf("reopened = %d workspaces", len(reopened.List()))
	}
}

func TestInsertBeforeAndArchive(t *testing.T) {
	persistence := &fakePersistence{}
	registry, root := newRegistry(t, RegistryHost{Persistence: persistence})
	var ids []WorkspaceID
	for _, name := range []string{"one", "two", "three"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		entity, err := registry.Create(context.Background(), dir, "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, entity.ID())
	}
	// Created order is three, two, one (each prepends). Move `three`
	// before `one`: two, three, one.
	order, err := registry.InsertBefore(context.Background(), ids[2], ids[0])
	if err != nil {
		t.Fatalf("insertBefore: %v", err)
	}
	if len(order) != 3 || order[0] != ids[1] || order[1] != ids[2] || order[2] != ids[0] {
		t.Fatalf("order = %v", order)
	}
	// Moving the id to its own anchor is a no-op.
	order, err = registry.InsertBefore(context.Background(), ids[1], ids[1])
	if err != nil || order[0] != ids[1] {
		t.Fatalf("self move = %v %v", order, err)
	}
	// Unknown source/anchor fail loud.
	if _, err := registry.InsertBefore(context.Background(), "ghost", ""); err == nil ||
		err.Error() != "cannot reorder unknown workspace 'ghost'" {
		t.Fatalf("err = %v", err)
	}

	// Archive: unknown sessions reject; known ones archive in order and an
	// already archived id resolves without writing.
	if err := registry.ArchiveSession(context.Background(), "s-none"); err == nil ||
		err.Error() != "cannot archive session 's-none': live sessions and session persistence hold no such session" {
		t.Fatalf("err = %v", err)
	}
	persistence.seed("s-live", root, 1)
	if err := registry.ArchiveSession(context.Background(), "s-live"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := registry.ArchiveSession(context.Background(), "s-live"); err != nil {
		t.Fatalf("re-archive: %v", err)
	}
	if archived := registry.ArchivedSessionIDs(); len(archived) != 1 || archived[0] != "s-live" {
		t.Fatalf("archived = %v", archived)
	}
}

func TestStartupFailsLoudWithoutPersistence(t *testing.T) {
	root := t.TempDir()
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json"},
		map[string]storagedomain.Backend{"json": storagejson.NewJsonStorageBackend(root)},
		nil)
	_, _, err := NewRegistry(context.Background(), RegistryHost{
		Persistence: &fakePersistence{fail: true},
		Logger:      &recordingLogger{},
	}, facility)
	if err == nil {
		t.Fatal("an unavailable persistence peer must fail startup, never commit initialized")
	}
}

func TestRecoveredPendingCreateMarker(t *testing.T) {
	persistence := &fakePersistence{}
	// A crashed run left a stale `create` marker for an id absent from
	// registry order (the record write itself never landed).
	staleID := NewWorkspaceID()
	stale, err := json.Marshal(DomainState{
		Initialized:        true,
		WorkspaceIDs:       []WorkspaceID{},
		ArchivedSessionIDs: []string{},
		PendingMutation:    &pendingMutation{Operation: "create", WorkspaceID: staleID},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	root := t.TempDir()
	backend := storagejson.NewJsonStorageBackend(root)
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json"},
		map[string]storagedomain.Backend{"json": backend}, nil)
	unit, err := backend.Open(storagedomain.KvUnitDescriptor{
		Name: "workspace", Version: 2, Tables: []string{"workspaces"}, HasGlobal: true,
	})
	if err != nil {
		t.Fatalf("open unit: %v", err)
	}
	if err := unit.SetGlobal(stale); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := unit.Close(); err != nil {
		t.Fatalf("close unit: %v", err)
	}
	registry, dispose, err := NewRegistry(context.Background(), RegistryHost{Persistence: persistence, Logger: &recordingLogger{}}, facility)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(dispose)
	// Recovery completed the named delete and cleared the marker: the
	// registry starts healthy, and the next create does not trip over the
	// stale marker.
	if len(registry.List()) != 0 {
		t.Fatalf("recovered = %d workspaces", len(registry.List()))
	}
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := registry.Create(context.Background(), dir, ""); err != nil {
		t.Fatalf("create after recovery: %v", err)
	}
}

func TestEntityMutationHitsDomainMedium(t *testing.T) {
	persistence := &fakePersistence{}
	registry, root := newRegistry(t, RegistryHost{Persistence: persistence})
	dir := filepath.Join(root, "alpha")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	persistence.seed("s-a", dir, 10)
	registry.indexHeadersForTest(persistence)
	created, err := registry.Create(context.Background(), dir, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := created.AttachSession("s-a"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if len(created.SessionIDs()) != 1 || created.SessionIDs()[0] != "s-a" {
		t.Fatalf("members = %v", created.SessionIDs())
	}
	// The domain-backed medium holds the record: re-open and read it back.
	backend := storagejson.NewJsonStorageBackend(root)
	unit, err := backend.Open(storagedomain.KvUnitDescriptor{
		Name: "workspace", Version: 2, Tables: []string{"workspaces"}, HasGlobal: true,
	})
	if err != nil {
		t.Fatalf("reopen unit: %v", err)
	}
	defer unit.Close()
	snapshot, _, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	encoded := snapshot["workspaces"][created.ID()]
	if encoded == nil {
		t.Fatal("the record must be durable on the medium")
	}
	var record WorkspaceRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(record.SessionIDs) != 1 || record.SessionIDs[0] != "s-a" {
		t.Fatalf("durable record = %+v", record)
	}
	if _, err := time.Parse(timeRFC3339Millis, record.UpdatedAt); err != nil {
		t.Fatalf("updatedAt = %s", record.UpdatedAt)
	}
}

// indexHeadersForTest exposes the header indexer so tests can seed the
// projection index before a create (the source indexes at startup).
func (r *Registry) indexHeadersForTest(persistence *fakePersistence) {
	headers, err := persistence.List(context.Background())
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.indexHeaders(headers)
}
