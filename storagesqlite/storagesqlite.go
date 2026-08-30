// Package storagesqlite ports @deepseek-ai/dsh-storage-sqlite: the SQLite
// storage backend for the storage hub. One database file hosts every routed
// unit, document-per-row (`key TEXT` / `value TEXT` JSON) over the
// pure-Go modernc.org/sqlite driver; registers as backend `sqlite`.
package storagesqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"dshgo/storagedomain"
)

// SchemaVersion is the on-disk physical layout version, stored in
// `PRAGMA user_version`. Orthogonal to each unit's own version (stamped per
// unit in the `units` row). Any other stamped version rejects — this format
// has no migrations.
const SchemaVersion = 1

// JournalMode is a durable SQLite journal mode accepted by the backend.
// `memory`/`off` are excluded: dropping journal durability silently
// contradicts the durability clause of the KV backend contract.
type JournalMode string

// Journal modes, in the official vocabulary.
const (
	JournalWAL      JournalMode = "wal"
	JournalDelete   JournalMode = "delete"
	JournalTruncate JournalMode = "truncate"
	JournalPersist  JournalMode = "persist"
)

// Config carries the backend configuration.
type Config struct {
	// Path is the SQLite database file, or `:memory:` for an in-process
	// database (tests).
	Path string
	// JournalMode selects the journal pragma; empty selects wal.
	JournalMode JournalMode
}

// recordTableName is the physical table name for one unit table. Both
// segments are validated against UnitNamePattern before reaching this, so
// the result is safe to interpolate into DDL.
func recordTableName(unit string, table string) string {
	return fmt.Sprintf("u_%s_%s", unit, table)
}

// SqliteBackend owns one database connection and the open-unit table.
type SqliteBackend struct {
	mu     sync.Mutex
	db     *sql.DB
	open   map[string]*SqliteKvUnit
	closed bool
}

// New opens the database and applies its schema and pragmas. Missing
// directories and database files are created owner-only (`:memory:` skips
// filesystem setup). A zero `user_version` is stamped with SchemaVersion;
// every other non-current version rejects rather than migrating in place.
func New(config Config) (*SqliteBackend, error) {
	mode := JournalWAL
	if config.JournalMode != "" {
		switch config.JournalMode {
		case JournalWAL, JournalDelete, JournalTruncate, JournalPersist:
			mode = config.JournalMode
		default:
			return nil, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
				"invalid journal mode %q", string(config.JournalMode))
		}
	}
	path := config.Path
	if path != ":memory:" {
		path = filepath.Clean(path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		// Exclusively create a missing database file owner-only; existing
		// files retain their modes and errors other than EEXIST propagate.
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			handle.Close()
		} else if !os.IsExist(err) {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	backend := &SqliteBackend{db: db, open: map[string]*SqliteKvUnit{}}
	if err := backend.configure(string(mode)); err != nil {
		db.Close()
		return nil, err
	}
	return backend, nil
}

// configure applies pragmas, rejects foreign schema versions, and ensures
// the unit metadata tables.
func (b *SqliteBackend) configure(journalMode string) error {
	if _, err := b.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	// The validated union is safe to interpolate into a non-bindable PRAGMA.
	if _, err := b.db.Exec(fmt.Sprintf(`PRAGMA journal_mode = %s`, strings.ToUpper(journalMode))); err != nil {
		return err
	}
	var onDisk int
	if err := b.db.QueryRow(`PRAGMA user_version`).Scan(&onDisk); err != nil {
		return err
	}
	if onDisk != 0 && onDisk != SchemaVersion {
		return storagedomain.NewUnitError(storagedomain.CodeVersionMismatch,
			"storage database has schema version %d, incompatible with this build (%d)", onDisk, SchemaVersion)
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS units (
			name    TEXT PRIMARY KEY,
			version INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS unit_globals (
			unit  TEXT PRIMARY KEY REFERENCES units(name),
			value TEXT NOT NULL
		)`,
	} {
		if _, err := b.db.Exec(ddl); err != nil {
			return err
		}
	}
	if onDisk == 0 {
		// Stamp fresh databases LAST: the stamp asserts the layout is
		// complete, so a failure above leaves the medium unstamped and a
		// re-open after the obstruction retries materialization.
		if _, err := b.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
			return err
		}
	}
	return nil
}

// Open opens one unit, creating it when the medium holds no trace of it yet.
// Double-open is a caller bug, not a medium condition.
func (b *SqliteBackend) Open(descriptor storagedomain.KvUnitDescriptor) (storagedomain.KvUnit, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, storagedomain.NewUnitError(storagedomain.CodeClosed, "sqlite storage backend is closed")
	}
	if err := validateDescriptor(descriptor); err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if _, live := b.open[descriptor.Name]; live {
		b.mu.Unlock()
		return nil, fmt.Errorf("kv unit '%s' is already open (double-open is a caller bug)", descriptor.Name)
	}
	b.mu.Unlock()

	// The units row stamps the unit's format version at first
	// materialization; a foreign stamp rejects.
	var stamped int
	err := b.db.QueryRow(`SELECT version FROM units WHERE name = ?`, descriptor.Name).Scan(&stamped)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if err == sql.ErrNoRows {
		if _, err := b.db.Exec(`INSERT INTO units (name, version) VALUES (?, ?)`, descriptor.Name, descriptor.Version); err != nil {
			return nil, err
		}
	} else if stamped != descriptor.Version {
		return nil, storagedomain.NewUnitError(storagedomain.CodeVersionMismatch,
			"kv unit '%s' is stamped version %d on the medium, incompatible with descriptor version %d",
			descriptor.Name, stamped, descriptor.Version)
	}
	for _, table := range descriptor.Tables {
		ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS "%s" (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`, recordTableName(descriptor.Name, table))
		if _, err := b.db.Exec(ddl); err != nil {
			return nil, err
		}
	}
	unit, err := newKvUnit(b.db, descriptor)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.open[descriptor.Name] = unit
	b.mu.Unlock()
	// The unit's own Close releases the backend's open-name slot (the
	// official onClose handoff), so a closed handle can be reopened.
	unit.mu.Lock()
	unit.onClose = func() {
		b.mu.Lock()
		delete(b.open, descriptor.Name)
		b.mu.Unlock()
	}
	unit.mu.Unlock()
	return unit, nil
}

// Close closes every open unit and releases the database. Idempotent.
func (b *SqliteBackend) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	units := make([]*SqliteKvUnit, 0, len(b.open))
	for _, unit := range b.open {
		units = append(units, unit)
	}
	b.open = map[string]*SqliteKvUnit{}
	b.mu.Unlock()
	for _, unit := range units {
		unit.close()
	}
	return b.db.Close()
}

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

// SqliteKvUnit is one opened unit: per-table statements over the
// `u_<unit>_<table>` record tables plus this unit's row in the shared
// `unit_globals` table. Values are stored as JSON text.
type SqliteKvUnit struct {
	mu           sync.Mutex
	db           *sql.DB
	descriptor   storagedomain.KvUnitDescriptor
	closed       bool
	onClose      func()
	globalUpsert *sql.Stmt
	globalSelect *sql.Stmt
}

func newKvUnit(db *sql.DB, descriptor storagedomain.KvUnitDescriptor) (*SqliteKvUnit, error) {
	unit := &SqliteKvUnit{db: db, descriptor: descriptor}
	if descriptor.HasGlobal {
		upsert, err := db.Prepare(`INSERT INTO unit_globals (unit, value) VALUES (?, ?) ON CONFLICT(unit) DO UPDATE SET value = excluded.value`)
		if err != nil {
			return nil, err
		}
		selectStmt, err := db.Prepare(`SELECT value FROM unit_globals WHERE unit = ?`)
		if err != nil {
			upsert.Close()
			return nil, err
		}
		unit.globalUpsert = upsert
		unit.globalSelect = selectStmt
	}
	return unit, nil
}

// LoadAll reads the full current snapshot: every table's records keyed by
// table name, plus the global singleton (nil when never written).
func (u *SqliteKvUnit) LoadAll() (map[string]map[string]json.RawMessage, json.RawMessage, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.ensureOpen(); err != nil {
		return nil, nil, err
	}
	tables := map[string]map[string]json.RawMessage{}
	for _, table := range u.descriptor.Tables {
		records := map[string]json.RawMessage{}
		rows, err := u.db.Query(fmt.Sprintf(`SELECT key, value FROM "%s"`, recordTableName(u.descriptor.Name, table)))
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				rows.Close()
				return nil, nil, err
			}
			parsed, err := u.parseValue(value, fmt.Sprintf("table '%s' key '%s'", table, key))
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			records[key] = parsed
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		rows.Close()
		tables[table] = records
	}
	var global json.RawMessage
	if u.globalSelect != nil {
		var value string
		err := u.globalSelect.QueryRow(u.descriptor.Name).Scan(&value)
		if err != nil && err != sql.ErrNoRows {
			return nil, nil, err
		}
		if err == nil {
			parsed, err := u.parseValue(value, "global slot")
			if err != nil {
				return nil, nil, err
			}
			global = parsed
		}
	}
	return tables, global, nil
}

// parseValue maps bad JSON in the value column to `malformed-medium`.
func (u *SqliteKvUnit) parseValue(text string, slot string) (json.RawMessage, error) {
	if !json.Valid([]byte(text)) {
		return nil, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"kv unit '%s' holds unparsable JSON at %s", u.descriptor.Name, slot)
	}
	return json.RawMessage(text), nil
}

// PutRecord upserts one record durably; an existing key is replaced.
func (u *SqliteKvUnit) PutRecord(table string, key string, value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.ensureOpen(); err != nil {
		return err
	}
	_, err := u.db.Exec(
		fmt.Sprintf(`INSERT INTO "%s" (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			recordTableName(u.descriptor.Name, table)),
		key, string(value))
	return err
}

// DeleteRecord deletes one record durably. Idempotent: a missing key is a
// no-op.
func (u *SqliteKvUnit) DeleteRecord(table string, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.ensureOpen(); err != nil {
		return err
	}
	_, err := u.db.Exec(fmt.Sprintf(`DELETE FROM "%s" WHERE key = ?`, recordTableName(u.descriptor.Name, table)), key)
	return err
}

// SetGlobal writes the global singleton durably; only valid when the
// descriptor declared HasGlobal.
func (u *SqliteKvUnit) SetGlobal(value json.RawMessage) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.ensureOpen(); err != nil {
		return err
	}
	if u.globalUpsert == nil {
		return fmt.Errorf("kv unit '%s' declared no global slot", u.descriptor.Name)
	}
	_, err := u.globalUpsert.Exec(u.descriptor.Name, string(value))
	return err
}

// Close releases the unit. Idempotent.
func (u *SqliteKvUnit) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	u.close()
	return nil
}

// close performs the idempotent teardown under the held lock; the backend
// path calls it without needing the error.
func (u *SqliteKvUnit) close() {
	u.closed = true
	if u.globalUpsert != nil {
		u.globalUpsert.Close()
		u.globalUpsert = nil
	}
	if u.globalSelect != nil {
		u.globalSelect.Close()
		u.globalSelect = nil
	}
	if u.onClose != nil {
		u.onClose()
	}
}

func (u *SqliteKvUnit) ensureOpen() error {
	if u.closed {
		return storagedomain.NewUnitError(storagedomain.CodeClosed, "kv unit '%s' is closed", u.descriptor.Name)
	}
	return nil
}
