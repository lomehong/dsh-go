package sessionquerysqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"dshgo/sessionquery"
)

func openMemory(t *testing.T) *Store {
	t.Helper()
	store, err := Open(Config{Path: ":memory:", JournalMode: JournalDelete})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sampleDocs() []sessionquery.SessionEventSearchDocument {
	return []sessionquery.SessionEventSearchDocument{
		{SessionEventRecord: sessionquery.SessionEventRecord{SessionID: "a", Seq: 0, Type: "user/message", Time: 100, Surface: "append"}, Text: "please refactor the parser module"},
		{SessionEventRecord: sessionquery.SessionEventRecord{SessionID: "a", Seq: 1, Type: "assistant/message", Time: 120, Surface: "append"}, Text: "the parser refactor is complete"},
	}
}

func TestRoundTripSearch(t *testing.T) {
	store := openMemory(t)
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-1", 1, sampleDocs()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	hits, err := store.SearchEvents("refactor", nil, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %+v", hits)
	}
	if !strings.Contains(hits[0].Text, "refactor") {
		t.Fatalf("hit text = %q", hits[0].Text)
	}
	// A multi-word query is one quoted phrase: only the contiguous match.
	hits, err = store.SearchEvents("parser module", nil, 10)
	if err != nil {
		t.Fatalf("phrase search: %v", err)
	}
	if len(hits) != 1 || hits[0].Seq != 0 {
		t.Fatalf("phrase hits = %+v", hits)
	}
}

func TestPhraseInertQuoting(t *testing.T) {
	store := openMemory(t)
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-1", 1, sampleDocs()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// FTS5 syntax characters stay inert data inside the quoted phrase: the
	// query cannot throw or match documents that merely contain one token.
	hits, err := store.SearchEvents(`parser" NOT (refactor)`, nil, 10)
	if err != nil {
		t.Fatalf("search with syntax characters: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("syntax characters changed semantics: %+v", hits)
	}
	hits, err = store.SearchEvents(`refactor`, nil, 10)
	if err != nil {
		t.Fatalf("plain search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("plain hits = %+v", hits)
	}
}

func TestReplaceClearsAndUpdates(t *testing.T) {
	store := openMemory(t)
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-1", 1, sampleDocs()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-2", 2, sampleDocs()[:1]); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	hits, err := store.SearchEvents("refactor", nil, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Seq != 0 {
		t.Fatalf("stale documents survived: %+v", hits)
	}
	if err := store.DeleteSession("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	hits, err = store.SearchEvents("refactor", nil, 10)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("documents survived delete: %+v", hits)
	}
}

func TestMetadataFilters(t *testing.T) {
	store := openMemory(t)
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-1", 1, sampleDocs()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	from := float64(110)
	hits, err := store.SearchEvents("refactor", []sessionquery.SessionEventMetadataFilter{{Kind: "time", From: &from}}, 10)
	if err != nil {
		t.Fatalf("filtered search: %v", err)
	}
	if len(hits) != 1 || hits[0].Seq != 1 {
		t.Fatalf("time filter hits = %+v", hits)
	}
	hits, err = store.SearchEvents("refactor", []sessionquery.SessionEventMetadataFilter{{Kind: "type", Values: []string{"assistant/message"}}}, 10)
	if err != nil {
		t.Fatalf("type search: %v", err)
	}
	if len(hits) != 1 || hits[0].Seq != 1 {
		t.Fatalf("type filter hits = %+v", hits)
	}
	if _, err := store.SearchEvents("refactor", []sessionquery.SessionEventMetadataFilter{{Kind: "bogus"}}, 10); err == nil {
		t.Fatal("unknown filter kind accepted")
	}
}

func TestForeignDatabaseRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	other, err := Open(Config{Path: path, JournalMode: JournalWAL})
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	other.Close()
	// Rewrite the application id to a foreign marker.
	raw, err := Open(Config{Path: path, JournalMode: JournalWAL})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := raw.db.Exec("PRAGMA application_id = 123456"); err != nil {
		t.Fatalf("set foreign id: %v", err)
	}
	raw.Close()
	if _, err := Open(Config{Path: path, JournalMode: JournalWAL}); err == nil {
		t.Fatal("foreign application database accepted")
	}
}

func TestIncompatibleVersionResets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	store, err := Open(Config{Path: path, JournalMode: JournalWAL})
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := store.ReplaceSessionDocuments("a", 1, 30, nil, nil, nil, nil, "rev-1", 1, sampleDocs()); err != nil {
		t.Fatalf("replace: %v", err)
	}
	// A stale schema version resets the derived tables in place.
	if _, err := store.db.Exec("PRAGMA user_version = 3"); err != nil {
		t.Fatalf("stamp stale version: %v", err)
	}
	store.Close()
	reopened, err := Open(Config{Path: path, JournalMode: JournalWAL})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	hits, err := reopened.SearchEvents("refactor", nil, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("stale documents survived reset: %+v", hits)
	}
	var version int64
	if err := reopened.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("version = %d, want %d", version, SchemaVersion)
	}
}

func TestUnsupportedJournalMode(t *testing.T) {
	if _, err := Open(Config{Path: ":memory:", JournalMode: "off"}); err == nil {
		t.Fatal("unsupported journal mode accepted")
	}
	if _, err := Open(Config{Path: "", JournalMode: JournalWAL}); err == nil {
		t.Fatal("empty path accepted")
	}
}
