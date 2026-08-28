package storagejson

import (
	"fmt"
	"os"
	"sync"

	"dshgo/storagedomain"
)

// JsonStorageBackend is the JSON backend: it owns the file-tree root and
// serves the kv facet. Registers as backend `json` on the storage hub.
type JsonStorageBackend struct {
	mu     sync.Mutex
	root   string
	closed bool
	open   map[string]storagedomain.KvUnit
}

// NewJsonStorageBackend builds the backend over root. The root has NO
// default on purpose: a process-CWD fallback would scatter unit files
// wherever the process happens to start; assemblies state the location
// explicitly.
func NewJsonStorageBackend(root string) *JsonStorageBackend {
	return &JsonStorageBackend{root: root, open: map[string]storagedomain.KvUnit{}}
}

// Open opens one unit, creating it when the medium holds no trace of it yet.
// Double-open is a caller bug, not a medium condition.
func (b *JsonStorageBackend) Open(descriptor storagedomain.KvUnitDescriptor) (storagedomain.KvUnit, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, storagedomain.NewUnitError(storagedomain.CodeClosed, "json backend is closed")
	}
	if err := validateDescriptor(descriptor); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if _, live := b.open[descriptor.Name]; live {
		b.mu.Unlock()
		return nil, fmt.Errorf("unit '%s' is already open; a unit has exactly one live handle", descriptor.Name)
	}
	b.mu.Unlock()

	if err := os.MkdirAll(b.root, 0o700); err != nil {
		return nil, err
	}
	// The two layouts differ in medium shape only; each opener owns its own
	// path convention under the shared root.
	onClose := func() {
		b.mu.Lock()
		delete(b.open, descriptor.Name)
		b.mu.Unlock()
	}
	var unit storagedomain.KvUnit
	var err error
	if descriptor.Layout == storagedomain.LayoutPerRecord {
		unit, err = OpenPerRecordUnit(descriptor, b.root, onClose)
	} else {
		unit, err = OpenSingleUnit(descriptor, b.root, onClose)
	}
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.closed {
		// The backend closed while this open was in flight: do not hand out
		// a live unit past Close.
		b.mu.Unlock()
		_ = unit.Close()
		return nil, storagedomain.NewUnitError(storagedomain.CodeClosed, "json backend is closed")
	}
	b.open[descriptor.Name] = unit
	b.mu.Unlock()
	return unit, nil
}

// Close drains open units and releases the backend. Idempotent.
func (b *JsonStorageBackend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	units := make([]storagedomain.KvUnit, 0, len(b.open))
	for _, unit := range b.open {
		units = append(units, unit)
	}
	b.mu.Unlock()
	for _, unit := range units {
		_ = unit.Close()
	}
	return nil
}

// validateDescriptor rejects names outside the unit-name format: unsafe as
// file names and as identifier segments.
func validateDescriptor(descriptor storagedomain.KvUnitDescriptor) error {
	if !storagedomain.UnitNamePattern.MatchString(descriptor.Name) {
		return storagedomain.NewUnitError(storagedomain.CodeMalformedMedium, "invalid unit name '%s'", descriptor.Name)
	}
	for _, table := range descriptor.Tables {
		if !storagedomain.UnitNamePattern.MatchString(table) {
			return storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
				"invalid table name '%s' in unit '%s'", table, descriptor.Name)
		}
	}
	return nil
}
