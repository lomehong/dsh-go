package storagejson

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"dshgo/storagedomain"
)

func testDescriptor(layout string) storagedomain.KvUnitDescriptor {
	return storagedomain.KvUnitDescriptor{
		Name:      "test_unit",
		Version:   3,
		Tables:    []string{"things", "others"},
		HasGlobal: true,
		Layout:    layout,
	}
}

// jsonEqual compares two JSON documents by value. Values pass through
// serialize→parse re-formatting on both sides (the Go encoder re-indents raw
// messages exactly like the source's JSON.stringify of parsed objects), so
// byte equality is not the contract — JSON equality is.
func jsonEqual(t *testing.T, got json.RawMessage, want any) bool {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(got, &decoded); err != nil {
		return false
	}
	return reflect.DeepEqual(decoded, want)
}

func TestSingleUnitRoundTripsDurably(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenSingleUnit(testDescriptor(""), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()

	// A fresh unit serves the empty shape without materializing a file.
	tables, global, err := unit.LoadAll()
	if err != nil || global != nil || len(tables["things"]) != 0 {
		t.Fatalf("fresh = %v %s %v", tables, global, err)
	}
	if _, err := os.Stat(filepath.Join(root, "test_unit.json")); !os.IsNotExist(err) {
		t.Fatal("materialization must defer to the first write")
	}

	if err := unit.PutRecord("things", "a", json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := unit.SetGlobal(json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatalf("setGlobal: %v", err)
	}

	// The file is pretty-printed, header-stamped, and always the net state.
	text, err := os.ReadFile(filepath.Join(root, "test_unit.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(text), "\"unit\": {\n    \"name\": \"test_unit\",\n    \"version\": 3\n  }") {
		t.Fatalf("file = %s", text)
	}
	if !strings.HasSuffix(string(text), "\n") {
		t.Fatal("the document ends with a newline")
	}

	// Re-open observes the writes (durable once the call returned).
	unit2, err := OpenSingleUnit(testDescriptor(""), root, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer unit2.Close()
	tables, global, err = unit2.LoadAll()
	if err != nil || !jsonEqual(t, global, map[string]any{"n": float64(1)}) || !jsonEqual(t, tables["things"]["a"], map[string]any{"id": "a"}) {
		t.Fatalf("reopened = %v %s %v", tables, global, err)
	}

	// Version mismatch rejects with the verbatim diagnostics.
	bumped := testDescriptor("")
	bumped.Version = 4
	_, err = OpenSingleUnit(bumped, root, nil)
	var mismatch *storagedomain.UnitError
	if !errors.As(err, &mismatch) || mismatch.Code != storagedomain.CodeVersionMismatch ||
		!strings.Contains(err.Error(), "stored version 3 != expected 4") {
		t.Fatalf("err = %v, want version-mismatch", err)
	}
}

func TestSingleUnitRollsBackFailedPublish(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenSingleUnit(testDescriptor(""), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()
	if err := unit.PutRecord("things", "keep", json.RawMessage(`{"id":"keep"}`)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Sabotage the medium so the publish fails: the whole root vanishes.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("sabotage: %v", err)
	}
	if err := unit.PutRecord("things", "new", json.RawMessage(`{"id":"new"}`)); err == nil {
		t.Fatal("publish must fail")
	}
	if err := unit.PutRecord("things", "keep", json.RawMessage(`{"id":"keep2"}`)); err == nil {
		t.Fatal("publish must fail")
	}
	tables, _, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if _, exists := tables["things"]["new"]; exists {
		t.Fatal("a failed put must not survive in memory")
	}
	if !jsonEqual(t, tables["things"]["keep"], map[string]any{"id": "keep"}) {
		t.Fatalf("keep = %s, want the pre-failure value", tables["things"]["keep"])
	}
}

func TestSingleUnitClosedAndUndeclared(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenSingleUnit(testDescriptor(""), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	released := false
	unit2, err := OpenSingleUnit(testDescriptor(""), root, func() { released = true })
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	if err := unit2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !released {
		t.Fatal("the open-slot release must run on close")
	}
	if err := unit2.Close(); err != nil {
		t.Fatalf("second close must be idempotent: %v", err)
	}
	var closed *storagedomain.UnitError
	if err := unit2.PutRecord("things", "a", json.RawMessage(`{}`)); !errors.As(err, &closed) || closed.Code != storagedomain.CodeClosed {
		t.Fatalf("err = %v, want closed", err)
	}
	if err := unit.PutRecord("nope", "a", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "does not declare table 'nope'") {
		t.Fatalf("err = %v, want the undeclared-table failure", err)
	}
	// A global write on a unit without the slot fails.
	descriptor := testDescriptor("")
	descriptor.HasGlobal = false
	unit3, err := OpenSingleUnit(descriptor, root, nil)
	if err != nil {
		t.Fatalf("open3: %v", err)
	}
	defer unit3.Close()
	if err := unit3.SetGlobal(json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "does not declare a global slot") {
		t.Fatalf("err = %v, want the no-global failure", err)
	}
}

func TestPerRecordUnitDocumentsAndForeignReads(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenPerRecordUnit(testDescriptor(storagedomain.LayoutPerRecord), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()
	if err := unit.PutRecord("things", "a", json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := unit.SetGlobal(json.RawMessage(`{"n":9}`)); err != nil {
		t.Fatalf("setGlobal: %v", err)
	}

	// Each record is its own version-stamped document.
	recordText, err := os.ReadFile(filepath.Join(root, "test_unit", "things", "a.json"))
	if err != nil || !strings.Contains(string(recordText), "\"version\": 3") || !strings.Contains(string(recordText), `"record"`) {
		t.Fatalf("record doc = %s %v", recordText, err)
	}

	// A stale-version document reads as absent, not fatal.
	stale := `{"version": 2, "record": {"id":"stale"}}`
	if err := os.WriteFile(filepath.Join(root, "test_unit", "things", "stale.json"), []byte(stale), 0o600); err != nil {
		t.Fatalf("stale: %v", err)
	}
	tables, global, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if _, exists := tables["things"]["stale"]; exists {
		t.Fatal("a stale document must read as absent")
	}
	if !jsonEqual(t, tables["things"]["a"], map[string]any{"id": "a"}) || !jsonEqual(t, global, map[string]any{"n": float64(9)}) {
		t.Fatalf("state = %v %s", tables, global)
	}

	// Delete removes the document; a second delete is a no-op.
	if err := unit.DeleteRecord("things", "a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := unit.DeleteRecord("things", "a"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "test_unit", "things", "a.json")); !os.IsNotExist(err) {
		t.Fatal("the record document must be gone")
	}

	// Unsafe keys fail loud.
	if err := unit.PutRecord("things", "../escape", json.RawMessage(`{}`)); err == nil ||
		!strings.Contains(err.Error(), "per-record key '../escape' is not path-safe") {
		t.Fatalf("err = %v, want the path-safety rejection", err)
	}
	if err := unit.PutRecord("nope", "a", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "does not declare table 'nope'") {
		t.Fatalf("err = %v, want the undeclared-table failure", err)
	}
}

func TestPerRecordUnitBootstrapsLegacyWholeUnit(t *testing.T) {
	root := t.TempDir()
	legacy := `{
  "unit": { "name": "test_unit", "version": 3 },
  "global": {"n":5},
  "tables": {
    "things": { "a": {"id":"a"}, "b": {"id":"b"} },
    "undeclared": { "x": {"id":"x"} }
  }
}`
	if err := os.WriteFile(filepath.Join(root, "test_unit.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	unit, err := OpenPerRecordUnit(testDescriptor(storagedomain.LayoutPerRecord), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()
	tables, global, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	// The legacy global is NOT migrated (matching the source: only declared
	// tables are copied); only the records bootstrap.
	if global != nil || !jsonEqual(t, tables["things"]["a"], map[string]any{"id": "a"}) || !jsonEqual(t, tables["things"]["b"], map[string]any{"id": "b"}) {
		t.Fatalf("bootstrapped = %v %s", tables, global)
	}
	if _, exists := tables["undeclared"]; exists {
		t.Fatal("undeclared legacy tables must be skipped")
	}
	// Every bootstrapped record landed as a current-version document and the
	// legacy file is retained unchanged.
	for _, key := range []string{"a", "b"} {
		text, err := os.ReadFile(filepath.Join(root, "test_unit", "things", key+".json"))
		if err != nil || !strings.Contains(string(text), "\"version\": 3") {
			t.Fatalf("bootstrapped doc %s = %s %v", key, text, err)
		}
	}
	kept, err := os.ReadFile(filepath.Join(root, "test_unit.json"))
	if err != nil || string(kept) != legacy {
		t.Fatal("the legacy file must be retained unchanged")
	}

	// A foreign legacy file (another unit's name) is left alone.
	other := `{"unit": {"name": "elsewhere", "version": 1}, "tables": {}}`
	if err := os.WriteFile(filepath.Join(root, "elsewhere.json"), []byte(other), 0o600); err != nil {
		t.Fatalf("foreign: %v", err)
	}
	otherUnit, err := OpenPerRecordUnit(storagedomain.KvUnitDescriptor{
		Name: "elsewhere", Version: 1, Tables: []string{"things"},
	}, root, nil)
	if err != nil {
		t.Fatalf("open foreign: %v", err)
	}
	defer otherUnit.Close()
	foreignTables, _, err := otherUnit.LoadAll()
	if err != nil || len(foreignTables["things"]) != 0 {
		t.Fatalf("foreign bootstrap = %v %v", foreignTables, err)
	}
}

func TestBackendLifecycleAndDoubleOpen(t *testing.T) {
	root := t.TempDir()
	backend := NewJsonStorageBackend(root)
	t.Cleanup(func() { _ = backend.Close() })

	// Invalid unit and table names fail with malformed-medium.
	bad := testDescriptor("")
	bad.Name = "Bad-Name"
	var malformed *storagedomain.UnitError
	if _, err := backend.Open(bad); !errors.As(err, &malformed) || malformed.Code != storagedomain.CodeMalformedMedium {
		t.Fatalf("err = %v, want malformed-medium", err)
	}
	badTable := testDescriptor("")
	badTable.Tables = []string{"things", "Bad Table"}
	if _, err := backend.Open(badTable); !errors.As(err, &malformed) || !strings.Contains(err.Error(), "invalid table name 'Bad Table'") {
		t.Fatalf("err = %v, want the table rejection", err)
	}

	unit, err := backend.Open(testDescriptor(""))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Double-open is a caller bug.
	_, err = backend.Open(testDescriptor(""))
	if err == nil || !strings.Contains(err.Error(), "unit 'test_unit' is already open; a unit has exactly one live handle") {
		t.Fatalf("err = %v, want the double-open failure", err)
	}
	// Backend close releases the unit; the release frees the open slot.
	if err := backend.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var closed *storagedomain.UnitError
	if _, _, err := unit.LoadAll(); !errors.As(err, &closed) || closed.Code != storagedomain.CodeClosed {
		t.Fatalf("err = %v, want closed", err)
	}
	if _, err := backend.Open(testDescriptor("")); !errors.As(err, &closed) || closed.Code != storagedomain.CodeClosed {
		t.Fatalf("err = %v, want the closed backend rejection", err)
	}
}

func TestDomainOverJsonBackendEndToEnd(t *testing.T) {
	root := t.TempDir()
	backend := NewJsonStorageBackend(root)
	t.Cleanup(func() { _ = backend.Close() })
	spec, err := storagedomain.DefineDomain(storagedomain.DomainSpec{
		Name:              "test_domain",
		Version:           1,
		Tables:            []string{"things"},
		HasGlobal:         true,
		InitialGlobalJSON: json.RawMessage(`{"n":0}`),
		ValidateRecord: func(table string, key string, raw json.RawMessage) error {
			if !strings.Contains(string(raw), "id") {
				return errors.New("record needs an id")
			}
			return nil
		},
		ValidateGlobal: func(raw json.RawMessage) error {
			if string(raw) == "null" {
				return errors.New("null sentinel")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	facility := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json", Routes: map[string]string{spec.Name: "json"}},
		map[string]storagedomain.Backend{"json": backend},
		nil,
	)
	t.Cleanup(facility.CloseAll)
	domain, err := facility.Open(spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	changes := make(chan storagedomain.DomainChanged, 8)
	undo := domain.OnChanged(func(change storagedomain.DomainChanged) { changes <- change })
	defer undo()
	if err := domain.Table("things").Put("a", json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := domain.Global().Set(json.RawMessage(`{"n":2}`)); err != nil {
		t.Fatalf("set: %v", err)
	}
	close(changes)
	count := 0
	for range changes {
		count++
	}
	if count != 2 {
		t.Fatalf("events = %d, want 2", count)
	}
	// The first domain holds the unit's open slot; close it before the
	// reopen (single-open per unit name).
	if err := domain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen through a fresh facility observes the durable state.
	facility2 := storagedomain.NewFacility(
		storagedomain.Config{Backend: "json", Routes: map[string]string{spec.Name: "json"}},
		map[string]storagedomain.Backend{"json": backend},
		nil,
	)
	t.Cleanup(facility2.CloseAll)
	domain2, err := facility2.Open(spec)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := domain2.Global().Get(); !jsonEqual(t, got, map[string]any{"n": float64(2)}) {
		t.Fatalf("global = %s", got)
	}
	if got := domain2.Table("things").Get("a"); got == nil || !jsonEqual(t, got, map[string]any{"id": "a"}) {
		t.Fatalf("record = %s", got)
	}
}

func TestConcurrentSingleUnitWritesStayConsistent(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenSingleUnit(testDescriptor(""), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = unit.PutRecord("things", "shared", json.RawMessage(`{"id":"shared"}`))
			_ = unit.SetGlobal(json.RawMessage(`{"n":1}`))
		}()
	}
	wg.Wait()
	// The published file is always a complete document: parse it.
	text, err := os.ReadFile(filepath.Join(root, "test_unit.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := Parse(text, testDescriptor("")); err != nil {
		t.Fatalf("the published file must always parse: %v", err)
	}
}

func TestPerRecordUnitReadsCompatibleVersions(t *testing.T) {
	root := t.TempDir()
	// Write a v3 document through a unit stamped 3, then open a unit
	// stamped 4 that declares version 3 compatible: the record must read.
	legacy := storagedomain.KvUnitDescriptor{
		Name: "test_unit", Version: 3, Tables: []string{"things"}, HasGlobal: true, Layout: storagedomain.LayoutPerRecord,
	}
	oldUnit, err := OpenPerRecordUnit(legacy, root, nil)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	if err := oldUnit.PutRecord("things", "a", json.RawMessage(`{"v":3}`)); err != nil {
		t.Fatalf("legacy put: %v", err)
	}
	if err := oldUnit.Close(); err != nil {
		t.Fatalf("legacy close: %v", err)
	}

	current := storagedomain.KvUnitDescriptor{
		Name: "test_unit", Version: 4, CompatibleVersions: []int{3}, Tables: []string{"things"}, HasGlobal: true, Layout: storagedomain.LayoutPerRecord,
	}
	unit, err := OpenPerRecordUnit(current, root, nil)
	if err != nil {
		t.Fatalf("open current: %v", err)
	}
	defer unit.Close()
	tables, _, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if raw, ok := tables["things"]["a"]; !ok || !jsonEqual(t, raw, map[string]any{"v": float64(3)}) {
		t.Fatalf("compatible-v3 record not read: %v", tables["things"])
	}

	// A v5-stamped record (outside the accepted set) reads as absent.
	if err := os.WriteFile(filepath.Join(root, "test_unit", "things", "stale.json"),
		[]byte(`{"version":5,"record":{"v":5}}`), 0o600); err != nil {
		t.Fatalf("write stale: %v", err)
	}
	tables, _, err = unit.LoadAll()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := tables["things"]["stale"]; ok {
		t.Fatalf("stale v5 record must read as absent: %v", tables["things"])
	}
}

func TestPerRecordUnitBackupRecordMovesDocumentAside(t *testing.T) {
	root := t.TempDir()
	unit, err := OpenPerRecordUnit(testDescriptor(storagedomain.LayoutPerRecord), root, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unit.Close()
	if err := unit.PutRecord("things", "doomed", json.RawMessage(`{"bad":true}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	moved, err := unit.BackupRecord("things", "doomed")
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !strings.Contains(filepath.Base(moved), "doomed.json.bak.") {
		t.Fatalf("moved path = %q, want a .bak. stamp", moved)
	}
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("backup file must survive: %v", err)
	}
	tables, _, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := tables["things"]["doomed"]; ok {
		t.Fatalf("backed-up record must read as absent: %v", tables["things"])
	}
}
