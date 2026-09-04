package storagedomain

import (
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/cordis"
)

// Backend is one registered backend: it owns exactly one medium and exposes
// its operation groups over it. Go adaptation: the source's hub (backend
// registry + service keys) is deferred to the settings/boot assembly round —
// the facility takes a routed name→backend table directly.
type Backend interface {
	// Open opens one unit, creating it when the medium holds no trace of it
	// yet. A version already stamped on the medium that differs from
	// descriptor.Version fails with `version-mismatch`; a medium that cannot
	// be parsed as this unit fails with `malformed-medium`.
	Open(descriptor KvUnitDescriptor) (KvUnit, error)
}

// Config routes which backend serves which domain: Backend is the default
// route and Routes overrides it per domain name. A route naming an
// unregistered backend fails loud at open with `backend-not-found`.
type Config struct {
	// Backend is the default backend name for every domain without an
	// explicit route.
	Backend string
	// Routes are the per-domain overrides: domain name → backend name.
	Routes map[string]string
}

// Facility is the mounted domain facility. It opens declared domains over
// routed backends; one facility instance owns the open-domain table and
// enforces single-open per domain name.
type Facility struct {
	mu        sync.Mutex
	reserved  map[string]struct{}
	domains   map[string]*Domain
	backends  map[string]Backend
	config    Config
	logger    cordis.Logger
	openUnits map[string]struct{}
}

// NewFacility builds the facility over the routed backend table.
func NewFacility(config Config, backends map[string]Backend, logger cordis.Logger) *Facility {
	return &Facility{
		reserved:  map[string]struct{}{},
		domains:   map[string]*Domain{},
		backends:  backends,
		config:    config,
		logger:    logger,
		openUnits: map[string]struct{}{},
	}
}

// parseRecordSlot is the invalid-record diagnostic slot text: the global is
// the empty-table slot.
func parseRecordSlot(table string, key string) string {
	if table == "" {
		return "global"
	}
	return fmt.Sprintf("record '%s' in table '%s'", key, table)
}

// validateStored runs one stored value through its spec validator,
// translating failure to `invalid-record` with its location.
func validateStored(domain string, table string, key string, raw json.RawMessage, validate func(json.RawMessage) error) error {
	if err := validate(raw); err != nil {
		return NewUnitError(CodeInvalidRecord,
			"domain '%s': stored %s does not match its schema: %v", domain, parseRecordSlot(table, key), err)
	}
	return nil
}

// Open opens one declared domain. Steps, each failing the whole call:
// reject a name that is already open (`already-open`); resolve the backend
// route (`backend-not-found`); open the unit projected from the spec
// (`version-mismatch`/`malformed-medium` pass through); load and validate
// every stored record against the spec's validators (`invalid-record` with
// the offending table and key); construct the domain.
//
// Lifecycle: the CALLER owns the returned handle and closes it via Close
// (typically as its own effect disposer) — the facility does not tie the
// domain to any consumer. Domains still open when the facility unmounts are
// closed by CloseAll.
func (f *Facility) Open(spec DomainSpec) (*Domain, error) {
	f.mu.Lock()
	if _, taken := f.reserved[spec.Name]; taken {
		f.mu.Unlock()
		return nil, NewUnitError(CodeAlreadyOpen, "domain '%s' is already open", spec.Name)
	}
	f.reserved[spec.Name] = struct{}{}
	f.mu.Unlock()
	failure := func(err error) (*Domain, error) {
		// Any failure means the domain never registered (nothing can emit
		// after it), so releasing the name reservation is unconditional.
		f.mu.Lock()
		delete(f.reserved, spec.Name)
		f.mu.Unlock()
		return nil, err
	}

	backendName := f.config.Routes[spec.Name]
	if backendName == "" {
		backendName = f.config.Backend
	}
	backend, registered := f.backends[backendName]
	if !registered {
		return failure(NewUnitError(CodeBackendNotFound, "backend '%s' is not registered", backendName))
	}
	unit, err := backend.Open(DescriptorOf(spec))
	if err != nil {
		return failure(err)
	}
	snapshot, global, err := unit.LoadAll()
	if err != nil {
		_ = unit.Close()
		return failure(err)
	}
	tables := map[string]map[string]json.RawMessage{}
	for _, table := range spec.Tables {
		records := map[string]json.RawMessage{}
		for key, raw := range snapshot[table] {
			if err := validateStored(spec.Name, table, key, raw, func(value json.RawMessage) error {
				return spec.ValidateRecord(table, key, value)
			}); err != nil {
				// Backup-and-skip policy (disposable derived data): move the
				// failing record's document aside, log the concrete cause,
				// and open without the record. Backends that cannot move a
				// document keep the loud path.
				if spec.InvalidRecordPolicy == InvalidRecordsBackupAndSkip {
					if backuper, ok := unit.(RecordBackuper); ok {
						if moved, backupErr := backuper.BackupRecord(table, key); backupErr == nil {
							if f.logger != nil {
								f.logger.Warn(fmt.Sprintf(
									"domain '%s': stored record '%s' in table '%s' failed schema validation; moved to '%s' and treated as absent: %v",
									spec.Name, key, table, moved, err))
							}
							continue
						}
					}
				}
				_ = unit.Close()
				return failure(err)
			}
			records[key] = raw
		}
		tables[table] = records
	}
	// A null stored global means "never written": serve `initial` without
	// materializing it — the first Set writes.
	var globalValue json.RawMessage
	if spec.HasGlobal && global != nil && string(global) != "null" {
		if err := validateStored(spec.Name, "", "", global, spec.ValidateGlobal); err != nil {
			_ = unit.Close()
			return failure(err)
		}
		globalValue = global
	}
	f.mu.Lock()
	if _, taken := f.reserved[spec.Name]; !taken {
		f.mu.Unlock()
		_ = unit.Close()
		return nil, NewUnitError(CodeAlreadyOpen, "domain '%s' is already open", spec.Name)
	}
	domain := Open(spec, unit, tables, globalValue, f.logger, func() {
		f.mu.Lock()
		delete(f.domains, spec.Name)
		delete(f.reserved, spec.Name)
		delete(f.openUnits, spec.Name)
		f.mu.Unlock()
	})
	f.domains[spec.Name] = domain
	f.openUnits[spec.Name] = struct{}{}
	f.mu.Unlock()
	return domain, nil
}

// Get looks up an open domain by name, untyped. Diagnostic surface; typed
// consumers hold the handle returned by Open.
func (f *Facility) Get(name string) (*Domain, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	domain, ok := f.domains[name]
	return domain, ok
}

// CloseAll closes every domain still open on this facility. The unmount
// path for consumers that never called Domain.Close themselves; closing is
// idempotent, so double-closing an already-closed domain is harmless.
func (f *Facility) CloseAll() {
	f.mu.Lock()
	open := make([]*Domain, 0, len(f.domains))
	for _, domain := range f.domains {
		open = append(open, domain)
	}
	f.mu.Unlock()
	for _, domain := range open {
		_ = domain.Close()
	}
}
