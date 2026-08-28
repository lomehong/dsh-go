package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dshgo/session"
)

// fakeHost records the projection index and serves canned headers, like the
// registry-owned seams the source entity mutates through.
type fakeHost struct {
	mu      sync.Mutex
	table   *Table
	paths   map[session.SessionID]string
	headers map[session.SessionID]session.SessionHeader
}

func newFakeHost() *fakeHost {
	return &fakeHost{table: NewTable(), paths: map[session.SessionID]string{}, headers: map[session.SessionID]session.SessionHeader{}}
}

func (h *fakeHost) Table() *Table { return h.table }

func (h *fakeHost) ReadSessionPath(id session.SessionID) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paths[id]
}

func (h *fakeHost) ReadSessionHeader(id session.SessionID) (session.SessionHeader, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	header, ok := h.headers[id]
	if !ok {
		return session.SessionHeader{}, errors.New("no persisted session with this id")
	}
	return header, nil
}

func (h *fakeHost) RememberSessionPath(id session.SessionID, path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.paths[id] = path
}

func (h *fakeHost) putHeader(id session.SessionID, cwd string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.headers[id] = session.SessionHeader{Version: 0, ID: id, CreatedAt: 1, CWD: cwd}
}

// realDir creates a distinctive existing directory and returns its
// canonical path.
func realDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	canonical, err := RealpathNormalize(path)
	if err != nil {
		t.Fatalf("realpath: %v", err)
	}
	return canonical
}

func seedEntity(t *testing.T, host *fakeHost, id WorkspaceID, path string) *Entity {
	t.Helper()
	record, err := ValidateWorkspaceRecord([]byte(`{"path":` + jsonQuote(path) + `,"title":"` + id + `","sessionIds":[],"createdAt":"2026-01-01T00:00:00.000Z","updatedAt":"2026-01-01T00:00:00.000Z"}`))
	if err != nil {
		t.Fatalf("seed record: %v", err)
	}
	host.table.Put(id, record)
	return NewEntity(host, id, record)
}

func jsonQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestRealpathNormalizeResolvesLinks(t *testing.T) {
	base := t.TempDir()
	inner := filepath.Join(base, "real-target")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// The create reject path (nonexistent input fails) holds everywhere.
	if _, err := RealpathNormalize(filepath.Join(base, "absent")); err == nil {
		t.Fatal("a nonexistent path must fail (create's reject path)")
	}
	// Dots and trailing separators resolve; symlink creation needs
	// privileges on this host, so those halves still assert without one.
	canonical, err := RealpathNormalize(filepath.Join(inner, "..", "real-target") + string(filepath.Separator))
	if err != nil {
		t.Fatalf("realpath: %v", err)
	}
	want, _ := filepath.EvalSymlinks(inner)
	if canonical != want {
		t.Fatalf("canonical = %s, want %s", canonical, want)
	}
	link := filepath.Join(base, "link-dir")
	if err := os.Symlink(inner, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	throughLink, err := RealpathNormalize(link)
	if err != nil {
		t.Fatalf("realpath(link): %v", err)
	}
	if throughLink != want {
		t.Fatalf("through link = %s, want %s", throughLink, want)
	}
}

func TestRecordAndStateValidation(t *testing.T) {
	if _, err := ValidateWorkspaceRecord([]byte(`{"title":"t","sessionIds":[],"createdAt":"a","updatedAt":"b"}`)); err == nil ||
		!strings.Contains(err.Error(), "path must be a string") {
		t.Fatalf("err = %v, want the path rejection", err)
	}
	if _, err := ValidateWorkspaceRecord([]byte(`{"path":"/p","title":"t","createdAt":"a","updatedAt":"b"}`)); err == nil ||
		!strings.Contains(err.Error(), "sessionIds must be an array") {
		t.Fatalf("err = %v, want the sessionIds rejection", err)
	}
	if _, err := ValidateWorkspaceRecord([]byte(`{"path":"/p","title":"t","sessionIds":[],"createdAt":"a","updatedAt":"b"}`)); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	state, err := ValidateDomainState([]byte(`{"initialized":true,"workspaceIds":["w1"]}`))
	if err != nil {
		t.Fatalf("state without archived field: %v", err)
	}
	if len(state.ArchivedSessionIDs) != 0 {
		t.Fatal("archivedSessionIds must default to empty")
	}
	if _, err := ValidateDomainState([]byte(`{"initialized":false,"workspaceIds":[],"pendingMutation":{"operation":"rename","workspaceId":"w1"}}`)); err == nil ||
		!strings.Contains(err.Error(), "pendingMutation.operation must be create or delete") {
		t.Fatalf("err = %v, want the operation rejection", err)
	}
	if _, err := ValidateDomainState([]byte(`{"initialized":false,"workspaceIds":[],"pendingMutation":{"operation":"create","workspaceId":"w1"}}`)); err != nil {
		t.Fatalf("valid pending mutation rejected: %v", err)
	}
	initial, err := ValidateDomainState([]byte(WorkspaceDomainSpec.InitialJSON))
	if err != nil || initial.Initialized || WorkspaceDomainSpec.Version != 2 || WorkspaceDomainSpec.Name != "workspace" {
		t.Fatalf("spec initial = %+v err = %v", initial, err)
	}
}

func TestEntitySessionIDsFiltersInvalidCandidates(t *testing.T) {
	host := newFakeHost()
	dir := realDir(t, "filter-ws")
	entity := seedEntity(t, host, "ws-filter", dir)
	host.putHeader("s-kept", dir)
	host.putHeader("s-mismatch", "D:\\elsewhere")
	host.paths["s-kept"] = dir
	host.paths["s-mismatch"] = "D:\\elsewhere"
	// A candidate whose indexed path is unknown stays durably recorded but
	// is never returned.
	host.table.Put("ws-filter", WorkspaceRecord{
		Path: dir, Title: "ws-filter",
		SessionIDs: []session.SessionID{"s-kept", "s-mismatch", "s-unknown"},
		CreatedAt:  "2026-01-01T00:00:00.000Z", UpdatedAt: "2026-01-01T00:00:00.000Z",
	})
	entity = NewEntity(host, "ws-filter", mustGet(t, host, "ws-filter"))
	got := entity.SessionIDs()
	if len(got) != 1 || got[0] != "s-kept" {
		t.Fatalf("sessionIds = %v, want only s-kept", got)
	}
}

func mustGet(t *testing.T, host *fakeHost, id WorkspaceID) WorkspaceRecord {
	t.Helper()
	record, ok := host.table.Get(id)
	if !ok {
		t.Fatalf("record %s missing", id)
	}
	return record
}

func TestAttachValidationLadder(t *testing.T) {
	host := newFakeHost()
	dir := realDir(t, "attach-ws")
	entity := seedEntity(t, host, "ws-attach", dir)

	// Unknown id: header read fails before any write.
	if err := entity.AttachSession("s-ghost"); err == nil {
		t.Fatal("unknown session id must fail")
	}
	if record, _ := host.table.Get("ws-attach"); len(record.SessionIDs) != 0 {
		t.Fatalf("failed attach wrote sessionIds = %v", record.SessionIDs)
	}

	// Missing cwd.
	host.putHeader("s-nocwd", "")
	if err := entity.AttachSession("s-nocwd"); err == nil ||
		!strings.Contains(err.Error(), "its stored header carries no cwd to validate against") {
		t.Fatalf("err = %v, want the no-cwd rejection", err)
	}
	// Non-resolving cwd.
	host.putHeader("s-badcwd", filepath.Join(dir, "no-such-dir"))
	if err := entity.AttachSession("s-badcwd"); err == nil ||
		!strings.Contains(err.Error(), "does not resolve, so it cannot be validated") {
		t.Fatalf("err = %v, want the resolve rejection", err)
	}
	// cwd resolves but mismatches the workspace path.
	other := realDir(t, "other-ws")
	host.putHeader("s-mismatch", other)
	if err := entity.AttachSession("s-mismatch"); err == nil ||
		!strings.Contains(err.Error(), "its cwd resolves to '") {
		t.Fatalf("err = %v, want the mismatch rejection", err)
	}

	// Matching cwd attaches at the head and publishes the projection.
	host.putHeader("s-good", dir)
	if err := entity.AttachSession("s-good"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if got := entity.SessionIDs(); len(got) != 1 || got[0] != "s-good" {
		t.Fatalf("sessionIds = %v", got)
	}
	if host.ReadSessionPath("s-good") != dir {
		t.Fatal("attach must publish the validated cwd to the projection")
	}
	updated := mustGet(t, host, "ws-attach")
	if updated.UpdatedAt == updated.CreatedAt {
		t.Fatal("accepted mutation must stamp updatedAt")
	}

	// Re-attach is a no-op: no write, updatedAt unchanged.
	before := mustGet(t, host, "ws-attach").UpdatedAt
	if err := entity.AttachSession("s-good"); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	if after := mustGet(t, host, "ws-attach").UpdatedAt; after != before {
		t.Fatalf("no-op attach rewrote the record: %s -> %s", before, after)
	}
	// New sessions prepend to the manual order.
	host.putHeader("s-second", dir)
	if err := entity.AttachSession("s-second"); err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	got := entity.SessionIDs()
	if len(got) != 2 || got[0] != "s-second" || got[1] != "s-good" {
		t.Fatalf("sessionIds = %v, want s-second prepended", got)
	}
}

func TestInsertBeforeAndDetach(t *testing.T) {
	host := newFakeHost()
	dir := realDir(t, "move-ws")
	entity := seedEntity(t, host, "ws-move", dir)
	for _, id := range []session.SessionID{"a", "b", "c"} {
		host.putHeader(id, dir)
		if err := entity.AttachSession(id); err != nil {
			t.Fatalf("attach %s: %v", id, err)
		}
	}

	// Prepend order after a,b,c attach is [c,b,a]. The identity move (c
	// before b, its current position) resolves without writing.
	if err := entity.InsertSessionBefore("c", "b"); err != nil {
		t.Fatalf("identity move: %v", err)
	}
	if got := entity.SessionIDs(); strings.Join(got, ",") != "c,b,a" {
		t.Fatalf("after identity move = %v, want c,b,a", got)
	}
	// Move a before c: [c,b,a] -> [a,c,b].
	if err := entity.InsertSessionBefore("a", "c"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := entity.SessionIDs(); strings.Join(got, ",") != "a,c,b" {
		t.Fatalf("after move = %v, want a,c,b", got)
	}
	// No-anchor move of c appends: [a,c,b] -> [a,b,c].
	if err := entity.InsertSessionBefore("c", ""); err != nil {
		t.Fatalf("append-move: %v", err)
	}
	if got := entity.SessionIDs(); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("after append-move = %v, want a,b,c", got)
	}
	// Unaccounted session and anchor fail typed without writing.
	before := mustGet(t, host, "ws-move")
	var moveErr *WorkspaceMoveInvalidError
	err := entity.InsertSessionBefore("ghost", "a")
	if !errors.As(err, &moveErr) || !strings.Contains(err.Error(), "the session is not accounted") {
		t.Fatalf("err = %v, want the typed unaccounted rejection", err)
	}
	err = entity.InsertSessionBefore("a", "ghost")
	if !errors.As(err, &moveErr) || !strings.Contains(err.Error(), "the anchor session is not accounted") {
		t.Fatalf("err = %v, want the typed anchor rejection", err)
	}
	// Self-anchor is a no-op (resolved without writing).
	if err := entity.InsertSessionBefore("a", "a"); err != nil {
		t.Fatalf("self-anchor: %v", err)
	}
	if after := mustGet(t, host, "ws-move"); after.UpdatedAt != before.UpdatedAt {
		t.Fatalf("no-op moves rewrote the record: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}

	// Detach is idempotent and never touches the session log; accepted
	// detach prunes filtered candidates durably.
	if err := entity.DetachSession("b"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	beforeUpdatedAt := mustGet(t, host, "ws-move").UpdatedAt
	if err := entity.DetachSession("b"); err != nil {
		t.Fatalf("re-detach: %v", err)
	}
	if after := mustGet(t, host, "ws-move").UpdatedAt; after != beforeUpdatedAt {
		t.Fatal("idempotent detach must not rewrite the record")
	}
	// A candidate whose projection vanished stays durable until the next
	// accepted mutation prunes it.
	host.mu.Lock()
	delete(host.paths, "a")
	host.mu.Unlock()
	if got := entity.SessionIDs(); strings.Join(got, ",") != "c" {
		t.Fatalf("filtered view = %v, want c (a's projection is gone)", got)
	}
	host.putHeader("d", dir)
	if err := entity.AttachSession("d"); err != nil {
		t.Fatalf("attach d: %v", err)
	}
	if record := mustGet(t, host, "ws-move"); strings.Join(record.SessionIDs, ",") != "d,c" {
		t.Fatalf("durable account = %v, want the filtered a pruned to d,c", record.SessionIDs)
	}
}

func TestStatusAndMutationRaces(t *testing.T) {
	host := newFakeHost()
	dir := realDir(t, "status-ws")
	entity := seedEntity(t, host, "ws-status", dir)
	if entity.Status() != "ok" {
		t.Fatal("existing directory must report ok")
	}
	// A missing directory never mutates the record.
	missing := filepath.Join(t.TempDir(), "moved-away")
	host.table.Put("ws-status", WorkspaceRecord{
		Path: missing, Title: "ws-status", SessionIDs: []session.SessionID{},
		CreatedAt: "2026-01-01T00:00:00.000Z", UpdatedAt: "2026-01-01T00:00:00.000Z",
	})
	entity = NewEntity(host, "ws-status", mustGet(t, host, "ws-status"))
	if entity.Status() != "missing-dir" {
		t.Fatal("missing directory must report missing-dir")
	}
	if entity.Path() != missing {
		t.Fatal("the record must keep the canonical path while missing")
	}

	// Concurrent attaches of distinct sessions all land; the account holds
	// each id exactly once.
	host2 := newFakeHost()
	dir2 := realDir(t, "race-ws")
	entity2 := seedEntity(t, host2, "ws-race", dir2)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := session.SessionID("s" + string(rune('a'+i)))
		host2.putHeader(id, dir2)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = entity2.AttachSession(id)
		}()
	}
	wg.Wait()
	account := entity2.SessionIDs()
	seen := map[session.SessionID]int{}
	for _, id := range account {
		seen[id]++
	}
	if len(account) != 8 {
		t.Fatalf("account = %v, want 8 distinct ids", account)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("id %s appeared %d times", id, count)
		}
	}
}
