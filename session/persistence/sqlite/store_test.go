package sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/session"
	"dshgo/session/persistence"
)

func testHeader(id session.SessionID) session.SessionHeader {
	depth := int64(0)
	seed := int64(2)
	return session.SessionHeader{
		Version:             1,
		ID:                  id,
		CreatedAt:           1700000000000,
		CWD:                 "D:\\tmp",
		ParentSession:       session.SessionID("parent-1"),
		IsSeeded:            true,
		InheritedEventCount: session.SessionLogOffset(seed),
		Origin:              "subagent",
		DelegationDepth:     &depth,
		AgentPreset:         "standard",
	}
}

func testEvents(from int64, count int64) []session.Event {
	events := make([]session.Event, 0, count)
	for seq := from; seq < from+count; seq++ {
		events = append(events, session.Event{
			Type: "user/message",
			Seq:  seq,
			Time: 1700000000000 + seq*10,
			Data: []byte(`{"text":"m"}`),
		})
	}
	return events
}

func openMemory(t *testing.T) *Store {
	t.Helper()
	store, err := Open(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.openDB(); err != nil {
		t.Fatalf("open db: %v", err)
	}
	return store
}

func TestAppendLoadRoundTripPreservesHeaderAndEvents(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("rt-1")
	first := testEvents(0, 2)
	// materialized=false: the batch creates the artifact with the header.
	if err := store.AppendBatch(meta, first, false); err != nil {
		t.Fatalf("append first: %v", err)
	}
	second := testEvents(2, 2)
	// surface-op and source-seq columns round-trip; presence semantics for
	// a known-empty provider stream ride the last event of the batch.
	second[1].SourceEventSeqs = []int64{}
	if err := store.AppendBatch(meta, second, true); err != nil {
		t.Fatalf("append second: %v", err)
	}

	prefix, err := store.LoadStored("rt-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if prefix == nil {
		t.Fatal("stored prefix missing")
	}
	if prefix.TornMarker != nil {
		t.Fatalf("clean store must have no torn marker, got %v", prefix.TornMarker)
	}
	if len(prefix.Events) != 4 {
		t.Fatalf("events = %d, want 4", len(prefix.Events))
	}
	if prefix.Events[3].SourceEventSeqs == nil || len(prefix.Events[3].SourceEventSeqs) != 0 {
		t.Fatalf("present empty source seqs must survive as [], got %v", prefix.Events[3].SourceEventSeqs)
	}
	loaded := prefix.Meta
	if loaded.ID != meta.ID || loaded.CWD != meta.CWD || loaded.ParentSession != meta.ParentSession ||
		loaded.Origin != meta.Origin || loaded.AgentPreset != meta.AgentPreset ||
		!loaded.IsSeeded || loaded.InheritedEventCount != 2 ||
		loaded.DelegationDepth == nil || *loaded.DelegationDepth != 0 {
		t.Fatalf("header round trip mismatch: %+v", loaded)
	}
	if string(prefix.Revision) == "" || !strings.HasPrefix(string(prefix.Revision), string(store.storeID)+":incarnation:") {
		t.Fatalf("revision must be source-qualified, got %q", prefix.Revision)
	}
}

func TestReadStoredRevisionEmptyWhenAbsent(t *testing.T) {
	store := openMemory(t)
	revision, err := store.ReadStoredRevision("absent")
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if revision != "" {
		t.Fatalf("absent identity must yield an empty revision, got %q", revision)
	}
}

func TestRevisionAdvancesWithAppends(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("rev-1")
	if err := store.AppendBatch(meta, testEvents(0, 1), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	snapshots, err := store.ListSnapshots()
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %d", len(snapshots))
	}
	before := snapshots[0].Revision
	if err := store.AppendBatch(meta, testEvents(1, 1), true); err != nil {
		t.Fatalf("append again: %v", err)
	}
	snapshots, err = store.ListSnapshots()
	if err != nil {
		t.Fatalf("snapshots again: %v", err)
	}
	if snapshots[0].Revision == before {
		t.Fatal("revision must change after an append")
	}
	// ReadStoredRevision agrees with the snapshot view.
	revision, err := store.ReadStoredRevision("rev-1")
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	if revision != snapshots[0].Revision {
		t.Fatalf("revision views disagree: %q vs %q", revision, snapshots[0].Revision)
	}
}

func TestContiguityEnforcedOnAppend(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("gap-1")
	if err := store.AppendBatch(meta, testEvents(0, 2), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.AppendBatch(meta, testEvents(4, 1), true); err == nil {
		t.Fatal("non-contiguous append must fail loud")
	}
}

func TestCommitRepairTruncatesTornTailAndAppendsClosers(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("repair-1")
	if err := store.AppendBatch(meta, testEvents(0, 2), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a torn physical tail: a stray row leaving a seq gap (the
	// corruption class a crashed batch could leave behind).
	if _, err := store.exec(
		"INSERT INTO events (session_id, seq, type, time, data, source_event_seqs, surface_op, is_packed) VALUES (?, ?, ?, ?, ?, ?, ?, 0)",
		1, 5, "user/message", 1700000000099, "{}", nil, nil,
	); err != nil {
		t.Fatalf("seed torn row: %v", err)
	}

	prefix, err := store.LoadStored("repair-1")
	if err != nil {
		t.Fatalf("load torn: %v", err)
	}
	// Official scanRows semantics: tornFrom is the offending row's own
	// physical seq.
	if prefix.TornMarker == nil || prefix.TornMarker.(int64) != 5 {
		t.Fatalf("torn marker = %v, want 5", prefix.TornMarker)
	}
	if len(prefix.Events) != 2 {
		t.Fatalf("preserved events = %d, want 2", len(prefix.Events))
	}

	// A stale repair (wrong marker) is refused.
	if err := store.CommitRepair(meta, int64(3), nil); err == nil {
		t.Fatal("stale repair marker must be refused")
	}
	// Repair without the current torn tail is refused.
	if err := store.CommitRepair(meta, nil, testEvents(2, 1)); err == nil {
		t.Fatal("repair omitting the current torn tail must be refused")
	}

	closers := testEvents(2, 1)
	if err := store.CommitRepair(meta, prefix.TornMarker, closers); err != nil {
		t.Fatalf("commit repair: %v", err)
	}
	repaired, err := store.LoadStored("repair-1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if repaired.TornMarker != nil {
		t.Fatalf("repaired store must be clean, got %v", repaired.TornMarker)
	}
	if len(repaired.Events) != 3 {
		t.Fatalf("events after repair = %d, want 3", len(repaired.Events))
	}
}

func TestSuffixReadScalesWithSuffix(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("suffix-1")
	if err := store.AppendBatch(meta, testEvents(0, 4), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	suffix, err := store.ReadStoredFrom("suffix-1", 2)
	if err != nil {
		t.Fatalf("read suffix: %v", err)
	}
	if suffix == nil || len(suffix.Events) != 2 || suffix.Events[0].Seq != 2 {
		t.Fatalf("suffix = %+v", suffix)
	}
	if suffix.Meta.ID != meta.ID {
		t.Fatalf("suffix header mismatch: %+v", suffix.Meta)
	}
}

func TestPackedRowsRefusedLoud(t *testing.T) {
	store := openMemory(t)
	meta := testHeader("packed-1")
	if err := store.AppendBatch(meta, testEvents(0, 1), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	// A committed turn boundary after the bad row makes the bad row part of
	// the committed prefix; an unreadable row there is hard corruption
	// (official scanRows rule), not a repairable tail.
	if _, err := store.exec(
		"INSERT INTO events (session_id, seq, type, time, data, source_event_seqs, surface_op, is_packed) VALUES (?, ?, ?, ?, ?, ?, ?, 1)",
		1, 2, "packed/run", 1700000000500, "{}", nil, nil,
	); err != nil {
		t.Fatalf("seed packed row: %v", err)
	}
	turnEnd := session.Event{Type: "turn/end", Seq: 3, Time: 1700000000020, Data: []byte(`{}`)}
	if _, err := store.exec(
		"INSERT INTO events (session_id, seq, type, time, data, source_event_seqs, surface_op, is_packed) VALUES (?, ?, ?, ?, ?, ?, ?, 0)",
		1, turnEnd.Seq, turnEnd.Type, turnEnd.Time, string(turnEnd.Data), nil, nil,
	); err != nil {
		t.Fatalf("seed turn end: %v", err)
	}
	if _, err := store.LoadStored("packed-1"); err == nil || !strings.Contains(err.Error(), "corrupt session log") {
		t.Fatalf("packed rows inside the committed prefix must be refused loud, got %v", err)
	}

	// Beyond the last committed boundary the same row is a repairable torn
	// tail, not an error (the official committed-prefix rule).
	fresh := openMemory(t)
	meta2 := testHeader("packed-2")
	if err := fresh.AppendBatch(meta2, testEvents(0, 1), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := fresh.exec(
		"INSERT INTO events (session_id, seq, type, time, data, source_event_seqs, surface_op, is_packed) VALUES (?, ?, ?, ?, ?, ?, ?, 1)",
		1, 9, "packed/run", 1700000000500, "{}", nil, nil,
	); err != nil {
		t.Fatalf("seed packed row: %v", err)
	}
	prefix, err := fresh.LoadStored("packed-2")
	if err != nil {
		t.Fatalf("packed tail must scan as torn, got %v", err)
	}
	if prefix.TornMarker == nil || prefix.TornMarker.(int64) != 9 {
		t.Fatalf("torn marker = %v, want 9", prefix.TornMarker)
	}
}

func TestListAndMaterializeHeader(t *testing.T) {
	store := openMemory(t)
	empty := testHeader("empty-1")
	empty.CWD = ""
	empty.ParentSession = ""
	empty.IsSeeded = false
	empty.InheritedEventCount = 0
	empty.DelegationDepth = nil
	empty.Origin = ""
	if err := store.MaterializeHeader(empty); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	headers, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(headers) != 1 || headers[0].ID != empty.ID {
		t.Fatalf("headers = %+v", headers)
	}
	if headers[0].CWD != "" || headers[0].IsSeeded {
		t.Fatalf("optional columns must stay null: %+v", headers[0])
	}
	// The empty artifact loads with zero events and no torn marker.
	prefix, err := store.LoadStored("empty-1")
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(prefix.Events) != 0 || prefix.TornMarker != nil {
		t.Fatalf("empty artifact = %+v", prefix)
	}
}

func TestFileBackedReopenKeepsIdentityAndData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	meta := testHeader("file-1")

	store, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.AppendBatch(meta, testEvents(0, 1), false); err != nil {
		t.Fatalf("append: %v", err)
	}
	firstSnapshots, err := store.ListSnapshots()
	if err != nil {
		t.Fatalf("snapshots: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopenedSnapshots, err := reopened.ListSnapshots()
	if err != nil {
		t.Fatalf("snapshots after reopen: %v", err)
	}
	if len(reopenedSnapshots) != 1 {
		t.Fatalf("snapshots after reopen = %d", len(reopenedSnapshots))
	}
	if reopenedSnapshots[0].Revision != firstSnapshots[0].Revision {
		t.Fatalf("store identity must survive reopen: %q vs %q", reopenedSnapshots[0].Revision, firstSnapshots[0].Revision)
	}
	prefix, err := reopened.LoadStored("file-1")
	if err != nil || prefix == nil || len(prefix.Events) != 1 {
		t.Fatalf("stored events must survive reopen: %+v err=%v", prefix, err)
	}
}

func TestOpenRefusesForeignDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := raw.Exec("CREATE TABLE alien (x INTEGER)"); err != nil {
		t.Fatalf("create alien: %v", err)
	}
	_ = raw.Close()

	store, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	err = store.openDB()
	if err == nil || !strings.Contains(err.Error(), "unversioned schema or application identity") {
		t.Fatalf("foreign database must be refused loud, got %v", err)
	}
}

func TestOpenRefusesVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.db")
	store, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.openDB(); err != nil {
		t.Fatalf("initial open: %v", err)
	}
	// Stamp a future schema version the way a newer build would.
	if _, err := store.exec(fmt.Sprintf("PRAGMA user_version = %d", SCHEMA_VERSION+1)); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(Config{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	err = reopened.openDB()
	if err == nil || !strings.Contains(err.Error(), "incompatible with this build") {
		t.Fatalf("version mismatch must be refused loud, got %v", err)
	}
}

func TestOpenRefusesRelativePath(t *testing.T) {
	store, err := Open(Config{Path: "relative.db"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if err := store.ValidatePath(); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative path must be refused, got %v", err)
	}
}

func TestNormalizeConfigDefaultsAndValidates(t *testing.T) {
	normalized, err := NormalizeConfig(Config{Path: ":memory:"})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if normalized.JournalMode != JournalWAL || normalized.BusyTimeoutMs != DefaultBusyTimeoutMs {
		t.Fatalf("defaults = %+v", normalized)
	}
	if _, err := NormalizeConfig(Config{Path: "x.db", JournalMode: "archive"}); err == nil {
		t.Fatal("unknown journal mode must fail loud")
	}
	if _, err := NormalizeConfig(Config{}); err == nil {
		t.Fatal("empty path must fail loud")
	}
}

func TestBackendSatisfiesContract(t *testing.T) {
	store := openMemory(t)
	var backend persistence.Backend = store
	if backend.Name() != BackendName {
		t.Fatalf("name = %q", backend.Name())
	}
	var suffixReader persistence.SuffixReader = store
	if suffixReader == nil {
		t.Fatal("store must provide the SuffixReader hook")
	}
	var materializer persistence.HeaderMaterializer = store
	if materializer == nil {
		t.Fatal("store must provide the HeaderMaterializer hook")
	}
	if _, err := os.Stat("."); err != nil {
		t.Fatal("unreachable")
	}
}
