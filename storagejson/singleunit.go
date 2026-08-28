package storagejson

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"dshgo/storagedomain"
)

// SingleJsonUnit is one opened JSON unit in `single` layout: the whole unit
// is one document at `<root>/<name>.json`. The in-memory state is
// authoritative; every write primitive mutates it and republishes the whole
// file atomically, rolling the mutation back on a failed publish so a
// rejected write does not survive in memory (or ride along with the next
// publish). Go adaptation: the unit serializes its own calls with a mutex —
// write ordering still belongs to the caller (the domain layer), the mutex
// only keeps the Go memory model honest for stray concurrent calls.
type SingleJsonUnit struct {
	mu         sync.Mutex
	closed     bool
	descriptor storagedomain.KvUnitDescriptor
	path       string
	state      UnitState
	onClose    func()
}

// OpenSingleUnit opens (loads or lazily creates) one single-layout unit
// under root: the unit file is `<root>/<name>.json`. A missing file is an
// empty unit; materialization defers to the first write.
func OpenSingleUnit(descriptor storagedomain.KvUnitDescriptor, root string, onClose func()) (*SingleJsonUnit, error) {
	path := filepath.Join(root, descriptor.Name+".json")
	text, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// Missing file = empty unit.
		text = nil
	}
	state := UnitState{
		Version: descriptor.Version,
		Tables:  map[string]map[string]json.RawMessage{},
	}
	for _, table := range descriptor.Tables {
		state.Tables[table] = map[string]json.RawMessage{}
	}
	if text != nil {
		parsed, err := Parse(text, descriptor)
		if err != nil {
			return nil, err
		}
		state = parsed
	}
	return &SingleJsonUnit{descriptor: descriptor, path: path, state: state, onClose: onClose}, nil
}

// LoadAll reads the full current snapshot.
func (u *SingleJsonUnit) LoadAll() (map[string]map[string]json.RawMessage, json.RawMessage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return nil, nil, err
	}
	tables := make(map[string]map[string]json.RawMessage, len(u.state.Tables))
	for table, records := range u.state.Tables {
		copied := make(map[string]json.RawMessage, len(records))
		for key, value := range records {
			copied[key] = value
		}
		tables[table] = copied
	}
	return tables, u.state.Global, nil
}

// PutRecord upserts one record durably.
func (u *SingleJsonUnit) PutRecord(table string, key string, value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	records, err := u.records(table)
	if err != nil {
		return err
	}
	previous, hadKey := records[key]
	records[key] = value
	if err := u.publish(); err != nil {
		if hadKey {
			records[key] = previous
		} else {
			delete(records, key)
		}
		return err
	}
	return nil
}

// DeleteRecord deletes one record durably; a missing key is a no-op.
func (u *SingleJsonUnit) DeleteRecord(table string, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	records, err := u.records(table)
	if err != nil {
		return err
	}
	previous, hadKey := records[key]
	if !hadKey {
		return nil
	}
	delete(records, key)
	if err := u.publish(); err != nil {
		records[key] = previous
		return err
	}
	return nil
}

// SetGlobal durably replaces the global singleton; only valid when declared.
func (u *SingleJsonUnit) SetGlobal(value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.assertOpen(); err != nil {
		return err
	}
	if !u.descriptor.HasGlobal {
		return fmt.Errorf("unit '%s' does not declare a global slot", u.descriptor.Name)
	}
	previous := u.state.Global
	u.state.Global = value
	if err := u.publish(); err != nil {
		u.state.Global = previous
		return err
	}
	return nil
}

// Close drains and releases the unit. Idempotent; the open-slot release runs
// exactly once.
func (u *SingleJsonUnit) Close() error {
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

func (u *SingleJsonUnit) assertOpen() error {
	if u.closed {
		return storagedomain.NewUnitError(storagedomain.CodeClosed, "unit '%s' is closed", u.descriptor.Name)
	}
	return nil
}

// records resolves a declared table's map; an undeclared table is a caller
// bug and throws.
func (u *SingleJsonUnit) records(table string) (map[string]json.RawMessage, error) {
	records, declared := u.state.Tables[table]
	if !declared {
		return nil, fmt.Errorf("unit '%s' does not declare table '%s'", u.descriptor.Name, table)
	}
	return records, nil
}

// publish republishes the whole file atomically from the authoritative
// state.
func (u *SingleJsonUnit) publish() error {
	content, err := Serialize(u.descriptor.Name, u.state)
	if err != nil {
		return err
	}
	return WriteAtomic(u.path, content)
}
