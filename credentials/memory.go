package credentials

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// MemoryProvider is the in-memory provider for interface and consumer tests:
// one always-writable `memory` source, seeded like the official
// MemoryCredentials. Records serialize through one mutex, so ModifyRecord's
// read-decide-replace is exclusive within the process.
type MemoryProvider struct {
	notifier *Notifier

	mu      sync.Mutex
	store   map[string]string
	records map[Key]Record
}

// NewMemoryProvider builds a provider seeded with reference values.
func NewMemoryProvider(seed map[string]string) *MemoryProvider {
	store := make(map[string]string, len(seed))
	for ref, value := range seed {
		store[ref] = value
	}
	return &MemoryProvider{
		notifier: NewNotifier(nil),
		store:    store,
		records:  map[Key]Record{},
	}
}

// Notifier exposes the provider's listener registry for tests and surfaces.
func (m *MemoryProvider) Notifier() *Notifier { return m.notifier }

// Resolve returns the stored value from the single memory layer. An empty
// stored value is absent, per the seam-wide rule.
func (m *MemoryProvider) Resolve(ref Ref) (*Resolved, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.store[string(ref)]
	if !ok || len(value) == 0 {
		return nil, nil
	}
	return &Resolved{Value: value, Source: "memory"}, nil
}

// Describe reports presence without the value; the memory source is always
// writable.
func (m *MemoryProvider) Describe(ref Ref) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.store[string(ref)]
	configured := ok && len(value) > 0
	info := Info{Configured: configured, Writable: true}
	if configured {
		info.Source = "memory"
	}
	return info, nil
}

// Set stores one non-empty value and notifies only after the write commits.
func (m *MemoryProvider) Set(ref Ref, value string) error {
	if len(value) == 0 {
		return errors.New("memory credentials: an empty value cannot be stored; use unset")
	}
	m.mu.Lock()
	m.store[string(ref)] = value
	m.mu.Unlock()
	return m.notifier.FanOut("credentials/reference-updated", string(ref))
}

// Unset removes one reference; removing an absent reference emits nothing.
func (m *MemoryProvider) Unset(ref Ref) error {
	m.mu.Lock()
	_, existed := m.store[string(ref)]
	delete(m.store, string(ref))
	m.mu.Unlock()
	if !existed {
		return nil
	}
	return m.notifier.FanOut("credentials/reference-updated", string(ref))
}

// ReadRecord returns the stored record as its owner wrote it; a grant
// payload is not interpreted on the way out.
func (m *MemoryProvider) ReadRecord(key Key) (Record, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[key]
	return record, ok, nil
}

// DescribeRecord reports presence, discriminant, and writability.
func (m *MemoryProvider) DescribeRecord(key Key) (RecordInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.records[key]
	if !ok {
		return RecordInfo{Configured: false, Writable: true}, nil
	}
	return RecordInfo{Configured: true, Kind: stored.Kind, Writable: true}, nil
}

// ListRecords enumerates every stored record's address and tag, values
// excluded, in stable key order.
func (m *MemoryProvider) ListRecords() ([]RecordEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := make([]RecordEntry, 0, len(m.records))
	for key, record := range m.records {
		entries = append(entries, RecordEntry{Key: key, Kind: record.Kind})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

// ModifyRecord runs the read-decide-replace under the provider mutex: mutate
// sees the record as it stands when the write is exclusive; nil leaves the
// entry untouched and reports the current one.
func (m *MemoryProvider) ModifyRecord(key Key, mutate func(current *Record) *Record) (*Record, error) {
	if mutate == nil {
		return nil, fmt.Errorf("memory credentials: record %q mutate must not be nil", string(key))
	}
	m.mu.Lock()
	current, ok := m.records[key]
	var currentPtr *Record
	if ok {
		currentPtr = &current
	}
	next := mutate(currentPtr)
	if next == nil {
		m.mu.Unlock()
		if !ok {
			return nil, nil
		}
		return &current, nil
	}
	m.records[key] = *next
	m.mu.Unlock()
	if err := m.notifier.FanOut("credentials/record-updated", string(key)); err != nil {
		return nil, err
	}
	return next, nil
}

// DeleteRecord removes one record; removing an absent record emits nothing.
func (m *MemoryProvider) DeleteRecord(key Key) error {
	m.mu.Lock()
	_, existed := m.records[key]
	delete(m.records, key)
	m.mu.Unlock()
	if !existed {
		return nil
	}
	return m.notifier.FanOut("credentials/record-updated", string(key))
}
