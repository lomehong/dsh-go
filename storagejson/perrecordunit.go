package storagejson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"dshgo/storagedomain"
)

// safeKeyPattern is the per-record key format: a key becomes a path segment.
var safeKeyPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// PerRecordJsonUnit is one opened `per-record`-layout unit. Stateless by
// design: the directory is the medium, the domain layer owns the live
// memory, and each method here is a single durable file operation. Write
// ordering belongs to the caller (the domain layer's write serialization),
// exactly like the single-layout unit.
type PerRecordJsonUnit struct {
	mu         sync.Mutex
	closed     bool
	descriptor storagedomain.KvUnitDescriptor
	dir        string
	onClose    func()
}

// OpenPerRecordUnit opens (loads or lazily creates) one per-record unit
// under root: the unit directory is `<root>/<name>` with one version-stamped
// document per record (`<table>/<key>.json`) plus a `global.json` for the
// global slot.
func OpenPerRecordUnit(descriptor storagedomain.KvUnitDescriptor, root string, onClose func()) (*PerRecordJsonUnit, error) {
	return &PerRecordJsonUnit{
		descriptor: descriptor,
		dir:        filepath.Join(root, descriptor.Name),
		onClose:    onClose,
	}, nil
}

// LoadAll re-reads the tree: the directory is the authoritative state.
func (u *PerRecordJsonUnit) LoadAll() (map[string]map[string]json.RawMessage, json.RawMessage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return nil, nil, err
	}
	state, err := u.loadState()
	if err != nil {
		return nil, nil, err
	}
	return state.Tables, state.Global, nil
}

// PutRecord durably replaces one record: its own document, atomically.
func (u *PerRecordJsonUnit) PutRecord(table string, key string, value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	dir, err := u.tableDir(table)
	if err != nil {
		return err
	}
	if err := assertSafeKey(u.descriptor.Name, key); err != nil {
		return err
	}
	return u.writeDocument(filepath.Join(dir, key+".json"), value)
}

// DeleteRecord durably deletes one record. Idempotent: a missing key is a
// no-op.
func (u *PerRecordJsonUnit) DeleteRecord(table string, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	dir, err := u.tableDir(table)
	if err != nil {
		return err
	}
	if err := assertSafeKey(u.descriptor.Name, key); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, key+".json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SetGlobal durably replaces the global singleton; only valid when declared.
func (u *PerRecordJsonUnit) SetGlobal(value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	if !u.descriptor.HasGlobal {
		return fmt.Errorf("unit '%s' does not declare a global slot", u.descriptor.Name)
	}
	return u.writeDocument(filepath.Join(u.dir, "global.json"), value)
}

// Close drains and releases the unit. Idempotent; the open-slot release runs
// exactly once.
func (u *PerRecordJsonUnit) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.closed = true
	if u.onClose != nil {
		u.onClose()
	}
	return nil
}

func (u *PerRecordJsonUnit) assertOpen() error {
	if u.closed {
		return storagedomain.NewUnitError(storagedomain.CodeClosed, "unit '%s' is closed", u.descriptor.Name)
	}
	return nil
}

// tableDir resolves a declared table's directory; an undeclared table is a
// caller bug and throws.
func (u *PerRecordJsonUnit) tableDir(table string) (string, error) {
	for _, declared := range u.descriptor.Tables {
		if declared == table {
			return filepath.Join(u.dir, table), nil
		}
	}
	return "", fmt.Errorf("unit '%s' does not declare table '%s'", u.descriptor.Name, table)
}

// writeDocument durably replaces one document, creating its parent
// directory.
func (u *PerRecordJsonUnit) writeDocument(path string, value json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content, err := SerializeRecord(u.descriptor.Version, value)
	if err != nil {
		return err
	}
	return WriteAtomic(path, content)
}

// loadState reconstructs the authoritative state from the tree: a record
// document that is foreign (missing, malformed, or stamped with another
// version) reads as absent, per the per-record contract. An empty tree
// bootstraps from a legacy whole-unit file when present.
func (u *PerRecordJsonUnit) loadState() (UnitState, error) {
	state := UnitState{
		Version: u.descriptor.Version,
		Global:  nil,
		Tables:  map[string]map[string]json.RawMessage{},
	}
	for _, table := range u.descriptor.Tables {
		state.Tables[table] = map[string]json.RawMessage{}
	}
	entries, err := os.ReadDir(u.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return UnitState{}, err
		}
		// Missing directory = empty unit; the legacy bootstrap below still
		// runs (the fresh-upgrade shape is exactly an absent new tree).
		if err := u.bootstrapLegacy(&state); err != nil {
			return UnitState{}, err
		}
		return state, nil
	}
	hasNewDocuments := false
	for _, entry := range entries {
		if entry.IsDir() {
			if _, declared := state.Tables[entry.Name()]; declared {
				loaded, err := u.loadTableRecords(u.descriptor.Version, filepath.Join(u.dir, entry.Name()))
				if err != nil {
					return UnitState{}, err
				}
				if loaded != nil {
					state.Tables[entry.Name()] = loaded
				}
				hasNewDocuments = true
				continue
			}
		}
		if entry.Name() == "global.json" && u.descriptor.HasGlobal {
			if global, ok := u.readRecord(filepath.Join(u.dir, entry.Name()), u.descriptor.Version); ok {
				state.Global = global
			}
			hasNewDocuments = true
		}
	}
	if !hasNewDocuments {
		if err := u.bootstrapLegacy(&state); err != nil {
			return UnitState{}, err
		}
	}
	return state, nil
}

// bootstrapLegacy bootstraps an empty per-record tree from a legacy
// whole-unit file (`<root>/<name>.json`, the pre-per-record layout). Every
// declared-table record is copied into a current-version document, while the
// legacy file is retained unchanged. A missing, foreign (another unit's
// name), malformed, or non-unit legacy file is left alone; other read
// failures propagate.
func (u *PerRecordJsonUnit) bootstrapLegacy(state *UnitState) error {
	legacyPath := filepath.Join(filepath.Dir(u.dir), u.descriptor.Name+".json")
	text, err := os.ReadFile(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// The legacy document is runtime data: only `unit.name` and the tables
	// map shape are checked here — the record values are migrated as-is and
	// the domain layer's validators judge them.
	var document struct {
		Unit *struct {
			Name string `json:"name"`
		} `json:"unit"`
		Tables map[string]json.RawMessage `json:"tables"`
	}
	if err := json.Unmarshal(text, &document); err != nil {
		return nil // Malformed legacy file: not ours to interpret or delete.
	}
	if document.Unit == nil || document.Unit.Name != u.descriptor.Name {
		return nil
	}
	for table, recordsRaw := range document.Tables {
		target, declared := state.Tables[table]
		if !declared {
			continue
		}
		var records map[string]json.RawMessage
		if err := json.Unmarshal(recordsRaw, &records); err != nil || records == nil {
			continue
		}
		for key, value := range records {
			path := filepath.Join(u.dir, table, key+".json")
			if err := u.writeDocument(path, value); err != nil {
				return err
			}
			target[key] = value
		}
	}
	return nil
}

// loadTableRecords reads one declared table's record documents. It reports
// whether the table had any state: a directory with no documents reads as
// absent for the has-new-documents check.
func (u *PerRecordJsonUnit) loadTableRecords(version int, dir string) (map[string]json.RawMessage, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	records := map[string]json.RawMessage{}
	for _, file := range files {
		name := file.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if !safeKeyPattern.MatchString(key) {
			continue
		}
		if record, ok := u.readRecord(filepath.Join(dir, name), version); ok {
			records[key] = record
		}
	}
	return records, nil
}

// readRecord reads one record document; a foreign (unreadable or stale) one
// reads as absent.
func (u *PerRecordJsonUnit) readRecord(path string, version int) (json.RawMessage, bool) {
	text, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return ParseRecord(text, version)
}

// assertSafeKey rejects a record key that would be unsafe as a path segment.
func assertSafeKey(unit string, key string) error {
	if !safeKeyPattern.MatchString(key) {
		return fmt.Errorf("unit '%s': per-record key '%s' is not path-safe (must match %s)", unit, key, safeKeyPattern)
	}
	return nil
}
