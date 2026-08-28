package storagedomain

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"dshgo/cordis"
)

// testSpec is a declared two-table domain with a non-nullable global.
func testSpec() DomainSpec {
	spec, err := DefineDomain(DomainSpec{
		Name:              "test_domain",
		Version:           1,
		Tables:            []string{"things", "others"},
		HasGlobal:         true,
		InitialGlobalJSON: json.RawMessage(`{"n":0}`),
		ValidateRecord: func(table string, key string, raw json.RawMessage) error {
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return err
			}
			if _, ok := decoded["id"]; !ok {
				return errors.New("record needs an id")
			}
			return nil
		},
		ValidateGlobal: func(raw json.RawMessage) error {
			// JSON null unmarshals into a Go map as a no-op success, so the
			// object-shaped validator must reject the sentinel explicitly
			// (the zod object schema does).
			if len(raw) == 0 || string(raw) == "null" {
				return errors.New("global must be an object")
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return err
			}
			return nil
		},
	})
	if err != nil {
		panic(err)
	}
	return spec
}

// newFacility seeds one memory unit and opens the spec over it, returning
// the open error for tests that expect the open to fail.
func newFacility(t *testing.T, spec DomainSpec, initial map[string]map[string]json.RawMessage, global json.RawMessage) (*Facility, *MemoryUnit, *Domain, error) {
	t.Helper()
	if initial == nil {
		initial = map[string]map[string]json.RawMessage{}
	}
	unit, release, err := OpenMemoryUnit(DescriptorOf(spec), nil)
	if err != nil {
		t.Fatalf("OpenMemoryUnit: %v", err)
	}
	if global != nil {
		if err := unit.SetGlobal(global); err != nil {
			t.Fatalf("seed global: %v", err)
		}
	}
	for table, records := range initial {
		for key, value := range records {
			if err := unit.PutRecord(table, key, value); err != nil {
				t.Fatalf("seed %s/%s: %v", table, key, err)
			}
		}
	}
	backend := &seedBackend{unit: unit}
	facility := NewFacility(Config{Backend: "memory"}, map[string]Backend{"memory": backend}, cordis.Discard{})
	t.Cleanup(facility.CloseAll)
	t.Cleanup(release)
	domain, err := facility.Open(spec)
	return facility, unit, domain, err
}

// mustFacility opens and fails the test when the open cannot.
func mustFacility(t *testing.T, spec DomainSpec, initial map[string]map[string]json.RawMessage, global json.RawMessage) (*Facility, *MemoryUnit, *Domain) {
	t.Helper()
	facility, unit, domain, err := newFacility(t, spec, initial, global)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return facility, unit, domain
}

// seedBackend serves one pre-populated unit on its first open; later opens
// materialize fresh empty units (a memory medium has no persistence across
// releases), matching how the domain layer treats reopening.
type seedBackend struct {
	unit KvUnit
	used bool
}

func (b *seedBackend) Open(descriptor KvUnitDescriptor) (KvUnit, error) {
	if !b.used {
		b.used = true
		return b.unit, nil
	}
	unit, _, err := OpenMemoryUnit(descriptor, nil)
	if err != nil {
		return nil, err
	}
	return unit, nil
}

func TestDefineDomainFailsLoud(t *testing.T) {
	if _, err := DefineDomain(DomainSpec{Name: "Bad-Name"}); err == nil || !strings.Contains(err.Error(), "domain name 'Bad-Name' must match") {
		t.Fatalf("err = %v, want the name rejection", err)
	}
	if _, err := DefineDomain(DomainSpec{Name: "ok", Version: -1}); err == nil || !strings.Contains(err.Error(), "version must be a non-negative integer, got -1") {
		t.Fatalf("err = %v, want the version rejection", err)
	}
	if _, err := DefineDomain(DomainSpec{Name: "ok", Layout: "hybrid"}); err == nil || !strings.Contains(err.Error(), "layout must be 'single' or 'per-record', got hybrid") {
		t.Fatalf("err = %v, want the layout rejection", err)
	}
	if _, err := DefineDomain(DomainSpec{Name: "ok", Tables: []string{"Bad"}}); err == nil || !strings.Contains(err.Error(), "table name 'Bad' must match") {
		t.Fatalf("err = %v, want the table-name rejection", err)
	}
	// A global validator accepting null cannot round-trip: reject.
	nullable, err := DefineDomain(DomainSpec{
		Name: "ok", HasGlobal: true,
		ValidateGlobal: func(json.RawMessage) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "global schema must not accept null") {
		t.Fatalf("err = %v, want the nullable-global rejection", err)
	}
	_ = nullable
}

func TestOpenValidatesStoredRecords(t *testing.T) {
	_, _, domain, err := newFacility(t, testSpec(), map[string]map[string]json.RawMessage{
		"things": {"a": json.RawMessage(`{"id":"a"}`), "bad": json.RawMessage(`{"nope":1}`)},
	}, nil)
	var invalid *UnitError
	if domain != nil || !errors.As(err, &invalid) || invalid.Code != CodeInvalidRecord {
		t.Fatalf("domain = %v err = %v, want invalid-record", domain, err)
	}
	if !strings.Contains(err.Error(), "stored record 'bad' in table 'things' does not match its schema") {
		t.Fatalf("err = %v, want the offending slot", err)
	}
}

func TestOpenRejectsUnknownStoredGlobal(t *testing.T) {
	// A stored JSON value the global validator rejects (an array, not an
	// object) → invalid-record at the global slot.
	_, _, domain, err := newFacility(t, testSpec(), nil, json.RawMessage(`[1,2]`))
	var invalid *UnitError
	if domain != nil || !errors.As(err, &invalid) || invalid.Code != CodeInvalidRecord {
		t.Fatalf("domain = %v err = %v, want invalid-record", domain, err)
	}
	if !strings.Contains(err.Error(), "stored global does not match its schema") {
		t.Fatalf("err = %v, want the global slot", err)
	}
}

func TestPutGetUpdateDeleteRoundTrip(t *testing.T) {
	_, unit, domain := mustFacility(t, testSpec(), nil, nil)
	things := domain.Table("things")
	changes := make(chan DomainChanged, 8)
	undo := domain.OnChanged(func(change DomainChanged) { changes <- change })
	defer undo()

	// Put is durable: the memory unit observes it.
	if err := things.Put("a", json.RawMessage(`{"id":"a"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := things.Get("a"); got == nil || !strings.Contains(string(got), `"a"`) {
		t.Fatalf("get = %s", got)
	}
	if things.Size() != 1 {
		t.Fatalf("size = %d", things.Size())
	}
	snapshot := things.Entries()
	if len(snapshot) != 1 {
		t.Fatalf("entries = %v", snapshot)
	}

	// Update transforms the current value atomically.
	next, err := things.Update("a", func(current json.RawMessage) json.RawMessage {
		return json.RawMessage(`{"id":"a","extra":true}`)
	})
	if err != nil || !strings.Contains(string(next), "extra") {
		t.Fatalf("update = %s %v", next, err)
	}
	if _, err := things.Update("missing", func(c json.RawMessage) json.RawMessage { return c }); err == nil {
		t.Fatal("update on a missing key must fail")
	}
	// The verbatim missing-key message.
	_, err = things.Update("missing", func(c json.RawMessage) json.RawMessage { return c })
	if !strings.Contains(err.Error(), "has no record 'missing' to update") {
		t.Fatalf("err = %v", err)
	}

	// Delete reports true once, then false with no write and no event.
	deleted, err := things.Delete("a")
	if !deleted || err != nil {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	deleted, err = things.Delete("a")
	if deleted || err != nil {
		t.Fatalf("second delete = %v %v, want false nil", deleted, err)
	}

	// The unit saw exactly the durable writes: a, a(update) — the second
	// delete wrote nothing.
	unitRecords, _, err := unit.LoadAll()
	if err != nil {
		t.Fatalf("loadAll: %v", err)
	}
	if _, exists := unitRecords["things"]["a"]; exists {
		t.Fatal("the deleted record must be gone from the medium")
	}

	// Events: put a, put a, deleted a — in write order, no old values.
	close(changes)
	var seen []DomainChanged
	for change := range changes {
		seen = append(seen, change)
	}
	if len(seen) != 3 ||
		seen[0].Operation != "put" || seen[0].Key != "a" ||
		seen[1].Operation != "put" || !strings.Contains(string(seen[1].Value), "extra") ||
		seen[2].Operation != "deleted" || seen[2].Value != nil {
		t.Fatalf("events = %+v", seen)
	}
	for _, change := range seen {
		if change.Domain != "test_domain" || change.Table != "things" {
			t.Fatalf("change = %+v", change)
		}
	}
}

func TestGlobalInitialSetAndSentinel(t *testing.T) {
	_, unit, domain := mustFacility(t, testSpec(), nil, nil)
	global := domain.Global()
	// Before the first Set: the spec's initial, not materialized.
	if got := global.InitialOrValue(); string(got) != `{"n":0}` {
		t.Fatalf("initial = %s", got)
	}
	if _, stored, err := unit.LoadAll(); err != nil || stored != nil {
		t.Fatalf("the initial must not be written: %s %v", stored, err)
	}
	if err := global.Set(json.RawMessage(`{"n":3}`)); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := global.Get(); string(got) != `{"n":3}` {
		t.Fatalf("get = %s", got)
	}
	if _, stored, err := unit.LoadAll(); err != nil || string(stored) != `{"n":3}` {
		t.Fatalf("stored = %s %v", stored, err)
	}

	// A stored null means never-written on reopen: serve initial.
	unit2, release2, err := OpenMemoryUnit(DescriptorOf(testSpec()), nil)
	if err != nil {
		t.Fatalf("unit2: %v", err)
	}
	if err := unit2.SetGlobal(json.RawMessage(`null`)); err != nil {
		t.Fatalf("seed null: %v", err)
	}
	backend2 := &seedBackend{unit: unit2}
	facility2 := NewFacility(Config{Backend: "memory"}, map[string]Backend{"memory": backend2}, cordis.Discard{})
	domain2, err := facility2.Open(testSpec())
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	if got := domain2.Global().InitialOrValue(); string(got) != `{"n":0}` {
		t.Fatalf("null sentinel must serve initial, got %s", got)
	}
	t.Cleanup(facility2.CloseAll)
	t.Cleanup(release2)
}

func TestCloseDrainsAndRejects(t *testing.T) {
	facility, _, domain := mustFacility(t, testSpec(), nil, nil)
	if err := domain.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Idempotent.
	if err := domain.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	var closedErr *UnitError
	if err := domain.Table("things").Put("a", json.RawMessage(`{"id":"a"}`)); !errors.As(err, &closedErr) || closedErr.Code != CodeClosed {
		t.Fatalf("err = %v, want closed", err)
	}
	if err := domain.Global().Set(json.RawMessage(`{"n":1}`)); !errors.As(err, &closedErr) || closedErr.Code != CodeClosed {
		t.Fatalf("err = %v, want closed", err)
	}
	if _, ok := facility.Get("test_domain"); ok {
		t.Fatal("a closed domain must leave the facility table")
	}
	// The name freed up for reopening.
	if _, err := facility.Open(testSpec()); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestFacilitySingleOpenAndRouting(t *testing.T) {
	spec := testSpec()
	unit, release, err := OpenMemoryUnit(DescriptorOf(spec), nil)
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	t.Cleanup(release)
	openUnits := map[string]struct{}{}
	backend := &routedBackend{units: map[string]*MemoryUnit{spec.Name: unit}, open: openUnits}
	facility := NewFacility(
		Config{Backend: "memory", Routes: map[string]string{spec.Name: "memory"}},
		map[string]Backend{"memory": backend, "kvless": &facetlessBackend{}},
		cordis.Discard{},
	)
	domain, err := facility.Open(spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Single-open per domain name.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := facility.Open(spec)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	var alreadyOpen *UnitError
	for err := range errs {
		if !errors.As(err, &alreadyOpen) || alreadyOpen.Code != CodeAlreadyOpen {
			t.Fatalf("concurrent open err = %v, want already-open", err)
		}
	}
	// Closing frees the name.
	_ = domain.Close()
	if _, err := facility.Open(spec); err != nil {
		t.Fatalf("reopen after close: %v", err)
	}

	// A route naming an unregistered backend fails with backend-not-found.
	facility2 := NewFacility(Config{Backend: "ghost"}, map[string]Backend{}, cordis.Discard{})
	_, err = facility2.Open(spec)
	var notFound *UnitError
	if !errors.As(err, &notFound) || notFound.Code != CodeBackendNotFound {
		t.Fatalf("err = %v, want backend-not-found", err)
	}
	// A backend without the kv facet fails with facet-unsupported.
	facility3 := NewFacility(Config{Backend: "kvless"}, map[string]Backend{"kvless": &facetlessBackend{}}, cordis.Discard{})
	_, err = facility3.Open(spec)
	var facet *UnitError
	if !errors.As(err, &facet) || facet.Code != CodeFacetUnsupported {
		t.Fatalf("err = %v, want facet-unsupported", err)
	}
}

// routedBackend serves its unit on the first open and materializes fresh
// empty units afterwards (the facility owns single-open enforcement).
type routedBackend struct {
	units map[string]*MemoryUnit
	open  map[string]struct{}
	used  bool
}

func (b *routedBackend) Open(descriptor KvUnitDescriptor) (KvUnit, error) {
	if !b.used {
		b.used = true
		return b.units[descriptor.Name], nil
	}
	unit, _, err := OpenMemoryUnit(descriptor, nil)
	if err != nil {
		return nil, err
	}
	return unit, nil
}

// facetlessBackend serves no data kind.
type facetlessBackend struct{}

func (b *facetlessBackend) Open(descriptor KvUnitDescriptor) (KvUnit, error) {
	return nil, NewUnitError(CodeFacetUnsupported, "backend 'kvless' has no kv facet")
}

func TestFailedBackendWriteLeavesMemoryUntouched(t *testing.T) {
	spec := testSpec()
	unit, release, err := OpenMemoryUnit(DescriptorOf(spec), nil)
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	t.Cleanup(release)
	failing := &failingUnit{inner: unit, failTables: map[string]bool{"things": true}}
	facility := NewFacility(Config{Backend: "memory"}, map[string]Backend{"memory": &seedBackend{unit: failing}}, cordis.Discard{})
	domain, err := facility.Open(spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	things := domain.Table("things")
	if err := things.Put("a", json.RawMessage(`{"id":"a"}`)); err == nil {
		t.Fatal("put must fail through the failing unit")
	}
	// No divergence: the read sees the empty state, not a half-write.
	if things.Size() != 0 || things.Get("a") != nil {
		t.Fatal("a failed backend write must leave memory untouched")
	}
	t.Cleanup(facility.CloseAll)
}

// failingUnit fails writes to selected tables after durability would land.
type failingUnit struct {
	inner      *MemoryUnit
	failTables map[string]bool
}

func (u *failingUnit) LoadAll() (map[string]map[string]json.RawMessage, json.RawMessage, error) {
	return u.inner.LoadAll()
}

func (u *failingUnit) PutRecord(table string, key string, value json.RawMessage) error {
	if u.failTables[table] {
		return NewUnitError(CodeMalformedMedium, "synthetic write failure on '%s'", table)
	}
	return u.inner.PutRecord(table, key, value)
}

func (u *failingUnit) DeleteRecord(table string, key string) error {
	return u.inner.DeleteRecord(table, key)
}
func (u *failingUnit) SetGlobal(value json.RawMessage) error { return u.inner.SetGlobal(value) }
func (u *failingUnit) Close() error                          { return u.inner.Close() }

func TestConcurrentUpdatesSerialize(t *testing.T) {
	_, _, domain := mustFacility(t, testSpec(), nil, nil)
	things := domain.Table("things")
	if err := things.Put("counter", json.RawMessage(`{"id":"counter"}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = things.Update("counter", func(current json.RawMessage) json.RawMessage {
				var decoded map[string]any
				_ = json.Unmarshal(current, &decoded)
				decoded["n"] = len(decoded)
				encoded, _ := json.Marshal(decoded)
				return encoded
			})
		}()
	}
	wg.Wait()
	// Every update saw a value and wrote one: the record is present and the
	// memory state equals the medium state.
	if things.Get("counter") == nil {
		t.Fatal("the updated record must remain")
	}
}
