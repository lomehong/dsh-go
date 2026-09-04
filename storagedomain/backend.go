package storagedomain

import (
	"encoding/json"
	"fmt"
	"sync"
)

// Unit error codes: the closed set every unit and the domain layer route on.
const (
	CodeClosed           = "closed"
	CodeVersionMismatch  = "version-mismatch"
	CodeMalformedMedium  = "malformed-medium"
	CodeAlreadyOpen      = "already-open"
	CodeFacetUnsupported = "facet-unsupported"
	CodeBackendNotFound  = "backend-not-found"
	CodeMissingKey       = "missing-key"
	CodeInvalidRecord    = "invalid-record"
	CodeUnitAlreadyOpen  = "unit-already-open"
)

// UnitError is one routed storage failure; Code selects the closed error
// vocabulary. Detect with errors.As into *UnitError.
type UnitError struct {
	Code    string
	Message string
}

func (e *UnitError) Error() string { return e.Message }

// NewUnitError builds one routed storage failure.
func NewUnitError(code string, format string, args ...any) *UnitError {
	return &UnitError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// KvUnit is one opened unit over a backend medium. Values are opaque JSON to
// this layer: no schema, no events, no domain meaning. The unit does NOT
// serialize concurrent writes — write ordering is the caller's
// responsibility (the domain layer serializes per unit); the unit only
// guarantees that each single call is atomic on the medium and durable once
// it returns (a crash after return followed by a re-open observes the
// write). Any call after Close fails with the `closed` code.
//
// Go adaptation: the source's promise-based unit is a synchronous interface
// — a Go backend performs its durability work inline.
type KvUnit interface {
	// LoadAll reads the full current snapshot: every table's records keyed
	// by table name, plus the global singleton (nil when never written).
	LoadAll() (tables map[string]map[string]json.RawMessage, global json.RawMessage, err error)
	// PutRecord upserts one record durably; an existing key is replaced.
	PutRecord(table string, key string, value json.RawMessage) error
	// DeleteRecord deletes one record durably. Idempotent: a missing key is
	// a no-op.
	DeleteRecord(table string, key string) error
	// SetGlobal writes the global singleton durably; only valid when the
	// descriptor declared HasGlobal.
	SetGlobal(value json.RawMessage) error
	// Close drains in-flight writes and releases the unit. Idempotent.
	Close() error
}

// RecordBackuper is the optional seam a backend whose medium has a
// per-record document can move aside (official KvUnit.backupRecord): the
// invalid-record backup-and-skip policy needs it. Backends without the seam
// (single layout, a row store) keep the reject-loud path.
type RecordBackuper interface {
	// BackupRecord moves one record's stored document out of the unit's
	// readable set, preserving its bytes for inspection instead of deleting
	// them. Returns the medium location the document moved to. A later read
	// sees the key as missing.
	BackupRecord(table string, key string) (string, error)
}

// MemoryUnit is the in-memory KvUnit used by tests and composition bundles.
// It enforces the descriptor contract (unknown table/key-value shape, closed
// unit, double-open) exactly like a file backend would.
type MemoryUnit struct {
	mu         sync.Mutex
	descriptor KvUnitDescriptor
	records    map[string]map[string]json.RawMessage
	backups    map[string]map[string]json.RawMessage
	global     json.RawMessage
	closed     bool
}

// OpenMemoryUnit materializes one empty in-memory unit. Opening the same
// unit name twice without closing is a caller bug and fails with
// `unit-already-open` (tracked through the returned release func).
func OpenMemoryUnit(descriptor KvUnitDescriptor, openUnits map[string]struct{}) (*MemoryUnit, func(), error) {
	if openUnits != nil {
		if _, taken := openUnits[descriptor.Name]; taken {
			return nil, nil, NewUnitError(CodeUnitAlreadyOpen, "unit '%s' is already open", descriptor.Name)
		}
		openUnits[descriptor.Name] = struct{}{}
	}
	unit := &MemoryUnit{
		descriptor: descriptor,
		records:    map[string]map[string]json.RawMessage{},
	backups:    map[string]map[string]json.RawMessage{},
	}
	for _, table := range descriptor.Tables {
		unit.records[table] = map[string]json.RawMessage{}
	}
	release := func() {
		if openUnits != nil {
			delete(openUnits, descriptor.Name)
		}
	}
	return unit, release, nil
}

// assertOpen fails every call after Close with the `closed` code.
func (u *MemoryUnit) assertOpen() error {
	if u.closed {
		return NewUnitError(CodeClosed, "unit '%s' is closed", u.descriptor.Name)
	}
	return nil
}

// LoadAll reads the full current snapshot.
func (u *MemoryUnit) LoadAll() (map[string]map[string]json.RawMessage, json.RawMessage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return nil, nil, err
	}
	tables := make(map[string]map[string]json.RawMessage, len(u.records))
	for table, records := range u.records {
		copied := make(map[string]json.RawMessage, len(records))
		for key, value := range records {
			copied[key] = append(json.RawMessage(nil), value...)
		}
		tables[table] = copied
	}
	var global json.RawMessage
	if u.global != nil {
		global = append(json.RawMessage(nil), u.global...)
	}
	return tables, global, nil
}

// PutRecord upserts one record durably.
func (u *MemoryUnit) PutRecord(table string, key string, value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	if _, declared := u.records[table]; !declared {
		return NewUnitError(CodeMalformedMedium, "unit '%s' has no declared table '%s'", u.descriptor.Name, table)
	}
	u.records[table][key] = append(json.RawMessage(nil), value...)
	return nil
}

// DeleteRecord deletes one record durably; a missing key is a no-op.
func (u *MemoryUnit) DeleteRecord(table string, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	if _, declared := u.records[table]; !declared {
		return NewUnitError(CodeMalformedMedium, "unit '%s' has no declared table '%s'", u.descriptor.Name, table)
	}
	delete(u.records[table], key)
	return nil
}

// BackupRecord moves one record into the backup slot, mirroring the json
// backend's `<key>.json.bak.<stamp>` move so the record reads as absent.
func (u *MemoryUnit) BackupRecord(table string, key string) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return "", err
	}
	tableRecords, declared := u.records[table]
	if !declared {
		return "", NewUnitError(CodeMalformedMedium, "unit '%s' has no declared table '%s'", u.descriptor.Name, table)
	}
	value, present := tableRecords[key]
	if !present {
		return "", nil
	}
	slot := u.backups[table]
	if slot == nil {
		slot = map[string]json.RawMessage{}
		u.backups[table] = slot
	}
	slot[key] = value
	delete(tableRecords, key)
	return fmt.Sprintf("memory://%s/%s.bak", table, key), nil
}

// SetGlobal writes the global singleton durably.
func (u *MemoryUnit) SetGlobal(value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	if !u.descriptor.HasGlobal {
		return NewUnitError(CodeMalformedMedium, "unit '%s' declares no global slot", u.descriptor.Name)
	}
	u.global = append(json.RawMessage(nil), value...)
	return nil
}

// Close drains and releases the unit. Idempotent.
func (u *MemoryUnit) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closed = true
	return nil
}
