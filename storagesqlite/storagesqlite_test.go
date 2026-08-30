package storagesqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"dshgo/storagedomain"
)

func memoryBackend(t *testing.T, config Config) *SqliteBackend {
	t.Helper()
	backend, err := New(config)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	t.Cleanup(func() { backend.Close() })
	return backend
}

func descriptor() storagedomain.KvUnitDescriptor {
	return storagedomain.KvUnitDescriptor{
		Name:      "sessions",
		Version:   3,
		Tables:    []string{"rows", "checks"},
		HasGlobal: true,
	}
}

func TestRoundTripTablesAndGlobal(t *testing.T) {
	backend := memoryBackend(t, Config{Path: ":memory:"})
	unit, err := backend.Open(descriptor())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := unit.PutRecord("rows", "a", json.RawMessage(`{"seq":1}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := unit.PutRecord("rows", "b", json.RawMessage(`[2]`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Upsert replaces.
	if err := unit.PutRecord("rows", "a", json.RawMessage(`{"seq":9}`)); err != nil {
		t.Fatalf("reput: %v", err)
	}
	if err := unit.SetGlobal(json.RawMessage(`{"identity":7}`)); err != nil {
		t.Fatalf("global: %v", err)
	}
	if err := unit.DeleteRecord("rows", "b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tables, global, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(tables) != 2 {
		t.Fatalf("table count: %d", len(tables))
	}
	if string(tables["rows"]["a"]) != `{"seq":9}` {
		t.Fatalf("upserted row: %s", tables["rows"]["a"])
	}
	if _, live := tables["rows"]["b"]; live {
		t.Fatal("deleted record survived")
	}
	if len(tables["checks"]) != 0 {
		t.Fatal("empty table not surfaced")
	}
	if global == nil || string(global) != `{"identity":7}` {
		t.Fatalf("global: %s", global)
	}
}

func TestUnitVersionStampRejectsForeignDescriptor(t *testing.T) {
	backend := memoryBackend(t, Config{Path: ":memory:"})
	first, err := backend.Open(descriptor())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Release the live handle; the units row persists on the medium.
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A foreign descriptor version rejects against the stamp.
	foreign := descriptor()
	foreign.Version = 4
	if _, err := backend.Open(foreign); err == nil || !strings.Contains(err.Error(), "stamped version 3") {
		t.Fatalf("foreign version: %v", err)
	}
	// The matching version still opens (fresh handle over the same tables).
	same := descriptor()
	if _, err := backend.Open(same); err != nil {
		t.Fatalf("reopen same version: %v", err)
	}
}

func TestDoubleOpenRejectedAndClosedBackend(t *testing.T) {
	backend := memoryBackend(t, Config{Path: ":memory:"})
	if _, err := backend.Open(descriptor()); err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := backend.Open(descriptor()); err == nil || !strings.Contains(err.Error(), "double-open is a caller bug") {
		t.Fatalf("double-open: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := backend.Open(descriptor()); err == nil || !strings.Contains(err.Error(), "backend is closed") {
		t.Fatalf("open after close: %v", err)
	}
	// Close is idempotent.
	if err := backend.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestMalformedMediumRejectedOnLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "medium.db")
	backend := memoryBackend(t, Config{Path: path})
	unit, err := backend.Open(descriptor())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := unit.PutRecord("rows", "bad", json.RawMessage(`{"seq":`)); err != nil {
		// The backend stores JSON text verbatim; an unparsable payload can
		// only come from a corrupted medium, so simulate one by writing raw
		// text that is not JSON through the same column.
		t.Fatalf("put: %v", err)
	}
	backend.Close()
	// Corrupt the medium directly, then read through a fresh backend.
	db := openRaw(t, path)
	exec(t, db, `UPDATE u_sessions_rows SET value = '{oops' WHERE key = 'bad'`)
	closeRaw(t, db)
	reopened := memoryBackend(t, Config{Path: path})
	reopenedUnit, err := reopened.Open(descriptor())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_, _, err = reopenedUnit.LoadAll()
	if err == nil || !strings.Contains(err.Error(), "unparsable JSON") {
		t.Fatalf("malformed medium: %v", err)
	}
	var unitErr *storagedomain.UnitError
	if !errors.As(err, &unitErr) || unitErr.Code != storagedomain.CodeMalformedMedium {
		t.Fatalf("error shape: %v", err)
	}
}

func TestFileMediumPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	backend := memoryBackend(t, Config{Path: path})
	unit, err := backend.Open(descriptor())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := unit.PutRecord("rows", "keep", json.RawMessage(`1`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := unit.SetGlobal(json.RawMessage(`"g"`)); err != nil {
		t.Fatalf("global: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened := memoryBackend(t, Config{Path: path})
	reopenedUnit, err := reopened.Open(descriptor())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	tables, global, err := reopenedUnit.LoadAll()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(tables["rows"]["keep"]) != `1` || global == nil || string(global) != `"g"` {
		t.Fatalf("persisted state: %s / %s", tables["rows"]["keep"], global)
	}
}

func TestInvalidNamesAndJournalMode(t *testing.T) {
	backend := memoryBackend(t, Config{Path: ":memory:"})
	bad := descriptor()
	bad.Name = "1-invalid"
	if _, err := backend.Open(bad); err == nil || !strings.Contains(err.Error(), "invalid unit name") {
		t.Fatalf("unit name: %v", err)
	}
	badTable := descriptor()
	badTable.Tables = []string{"Rows"}
	if _, err := backend.Open(badTable); err == nil || !strings.Contains(err.Error(), "invalid table name") {
		t.Fatalf("table name: %v", err)
	}
	if _, err := New(Config{Path: ":memory:", JournalMode: "off"}); err == nil || !strings.Contains(err.Error(), "invalid journal mode") {
		t.Fatalf("journal mode: %v", err)
	}
}

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, statement string) {
	t.Helper()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("raw exec: %v", err)
	}
}

func closeRaw(t *testing.T, db *sql.DB) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}
}
