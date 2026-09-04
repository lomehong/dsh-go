package projectioncache

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/storagedomain"
)

// memoryBackend is the seedable in-memory backend (the storagedomain
// package's test fixture shape).
type memoryBackend struct {
	unit *storagedomain.MemoryUnit
}

func (b *memoryBackend) Open(descriptor storagedomain.KvUnitDescriptor) (storagedomain.KvUnit, error) {
	return b.unit, nil
}

func newDomain(t *testing.T, seed map[string]json.RawMessage) (*storagedomain.Domain, *DomainStore) {
	t.Helper()
	spec, err := DomainSpec()
	if err != nil {
		t.Fatalf("DomainSpec: %v", err)
	}
	unit, release, err := storagedomain.OpenMemoryUnit(storagedomain.DescriptorOf(spec), nil)
	if err != nil {
		t.Fatalf("OpenMemoryUnit: %v", err)
	}
	t.Cleanup(release)
	for key, value := range seed {
		if err := unit.PutRecord("sessions", key, value); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
	facility := storagedomain.NewFacility(storagedomain.Config{Backend: "memory"}, map[string]storagedomain.Backend{
		"memory": &memoryBackend{unit: unit},
	}, cordis.Discard{})
	t.Cleanup(facility.CloseAll)
	domain, err := facility.Open(spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store, err := NewDomainStore(domain)
	if err != nil {
		t.Fatalf("NewDomainStore: %v", err)
	}
	return domain, store
}

func validRecordJSON(id string) string {
	return `{"identity":{"createdAt":1700000000000,"cwd":"D:\\work"},"rows":{"usage":{"ver":2,"seq":9,"val":{"total":42}}}}`
}

func TestDomainStoreRoundTrip(t *testing.T) {
	_, store := newDomain(t, map[string]json.RawMessage{"s-1": json.RawMessage(validRecordJSON("s-1"))})

	record, ok := store.Get("s-1")
	if !ok {
		t.Fatal("seeded record absent")
	}
	if record.Identity.CreatedAt != 1700000000000 || record.Identity.CWD != `D:\work` {
		t.Fatalf("identity: %+v", record.Identity)
	}
	row, ok := record.Rows["usage"]
	if !ok || row.Ver != 2 || row.Seq != 9 {
		t.Fatalf("usage row: %+v", row)
	}

	// Put replaces the whole record (whole-value discipline).
	record.Rows["occupancy"] = projectionRow()
	if err := store.Put("s-1", record); err != nil {
		t.Fatalf("put: %v", err)
	}
	reread, ok := store.Get("s-1")
	if !ok || len(reread.Rows) != 2 {
		t.Fatalf("reread rows: %d ok=%v", len(reread.Rows), ok)
	}

	if _, ok := store.Get("missing"); ok {
		t.Fatal("absent id read as present")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDomainStoreAbsentRecordReadsAsAbsent(t *testing.T) {
	// A stored JSON value the record schema rejects (a stale/unreadable
	// cache costs a replay, never a wrong value) reads as absent.
	_, store := newDomain(t, nil)
	if err := store.table.Put("corrupt", json.RawMessage(`{"identity":{"createdAt":"x"},"rows":{}}`)); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, ok := store.Get("corrupt"); ok {
		t.Fatal("corrupt record read as present")
	}
}

func projectionRow() (row struct {
	Ver int
	Seq int64
	Val json.RawMessage
}) {
	row.Ver = 1
	row.Seq = 0
	row.Val = json.RawMessage(`{}`)
	return row
}

func TestDomainSpecValidatesRecordsAtOpen(t *testing.T) {
	spec, err := DomainSpec()
	if err != nil {
		t.Fatalf("DomainSpec: %v", err)
	}
	bad := []string{
		`{"identity":{"createdAt":-1},"rows":{}}`,
		`{"identity":{"createdAt":1.5},"rows":{}}`,
		`{"identity":{"createdAt":1,"cwd":5},"rows":{}}`,
		`{"identity":{"createdAt":1},"rows":{"r":{"ver":-1,"seq":0}}}`,
		`{"identity":{"createdAt":1},"rows":{"r":{"ver":0,"seq":-2}}}`,
	}
	for _, raw := range bad {
		if err := spec.ValidateRecord("sessions", "k", json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := spec.ValidateRecord("sessions", "k", json.RawMessage(validRecordJSON("k"))); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	if err := spec.ValidateRecord("other", "k", json.RawMessage(validRecordJSON("k"))); err == nil || !strings.Contains(err.Error(), "unknown table") {
		t.Fatalf("wrong table: %v", err)
	}
	_ = errors.Is
}

// TestDomainSpecSalvagesInvalidRecordsAtOpen: the cache rows are disposable
// derived data, so a stored record failing validation at open is backed up
// and skipped (rc.1 backup-and-skip) instead of refusing the plugin tree.
func TestDomainSpecSalvagesInvalidRecordsAtOpen(t *testing.T) {
	spec, err := DomainSpec()
	if err != nil {
		t.Fatalf("DomainSpec: %v", err)
	}
	if spec.InvalidRecordPolicy != storagedomain.InvalidRecordsBackupAndSkip {
		t.Fatalf("policy = %q, want backup-and-skip", spec.InvalidRecordPolicy)
	}
	unit, release, err := storagedomain.OpenMemoryUnit(storagedomain.DescriptorOf(spec), nil)
	if err != nil {
		t.Fatalf("OpenMemoryUnit: %v", err)
	}
	t.Cleanup(release)
	// Seed one corrupt record plus one good one: the open must succeed with
	// the corrupt record backed up and skipped.
	if err := unit.PutRecord("sessions", "good", json.RawMessage(validRecordJSON("good"))); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	if err := unit.PutRecord("sessions", "bad", json.RawMessage(`{"identity":{"createdAt":-1},"rows":{}}`)); err != nil {
		t.Fatalf("seed bad: %v", err)
	}
	facility := storagedomain.NewFacility(storagedomain.Config{Backend: "memory"}, map[string]storagedomain.Backend{
		"memory": &memoryBackend{unit: unit},
	}, cordis.Discard{})
	t.Cleanup(facility.CloseAll)
	domain, err := facility.Open(spec)
	if err != nil {
		t.Fatalf("open with a corrupt cached record must succeed via backup-and-skip: %v", err)
	}
	store, err := NewDomainStore(domain)
	if err != nil {
		t.Fatalf("NewDomainStore: %v", err)
	}
	if record, ok := store.Get("good"); !ok || record == nil {
		t.Fatalf("good record must be served, ok=%v", ok)
	}
	if _, ok := store.Get("bad"); ok {
		t.Fatalf("backed-up corrupt record must read as absent")
	}
}
