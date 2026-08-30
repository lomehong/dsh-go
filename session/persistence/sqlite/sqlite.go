// Package sqlite re-implements the SQLite durable session persistence of
// @deepseek-ai/dsh-session-persistence-sqlite (official tag dsh-v0.1.2-alpha.1)
// as a persistence.Backend over the modernc.org/sqlite pure-Go driver: the
// schema-ownership checks (application id, schema user version), journal-mode
// selection, per-session monotonic revisions composed with a store identity
// and a per-session incarnation UUID, torn-tail detection and repair, and the
// coordinator's physical backend hooks.
//
// Honest degradations against the official backend: rows written by this
// build are never packed (is_packed = 0) — the physical chunk-packing codec
// and row compression are deferred; packed rows written by another build are
// refused loud at read. Connection-security pragmas cover foreign keys,
// journal mode, busy timeout, trusted_schema, and mmap_size; Node-specific
// file-mode ownership checks are POSIX-only, matching the official guard.
package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	_ "modernc.org/sqlite"
)

// SCHEMA_VERSION is the schema generation this build reads and writes; any
// other on-disk user_version is incompatible (no migration).
const SCHEMA_VERSION = 19

// SessionPersistenceSqliteApplicationID stamps the database as a dsh session
// store (the bytes "DSHP").
const SessionPersistenceSqliteApplicationID = 0x44534850

// JournalMode is a durable SQLite journal mode accepted by the backend.
type JournalMode string

// Journal modes, in the official vocabulary.
const (
	JournalWAL      JournalMode = "wal"
	JournalDelete   JournalMode = "delete"
	JournalTruncate JournalMode = "truncate"
	JournalPersist  JournalMode = "persist"
)

// DefaultBusyTimeoutMs is the default wait for another SQLite connection's
// write reservation.
const DefaultBusyTimeoutMs = 5000

// schemaSQL is the owned physical layout: one persistence-state singleton,
// one row per materialized session, and physical event rows addressed by
// (session, seq). STRICT tables keep column types honest where the driver's
// SQLite build supports them.
const schemaSQL = `
CREATE TABLE persistence_state (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  store_id  TEXT NOT NULL
);

CREATE TABLE sessions (
  id               INTEGER PRIMARY KEY,
  session_key      TEXT NOT NULL UNIQUE,
  version          INTEGER NOT NULL,
  created_at       INTEGER NOT NULL,
  cwd              TEXT,
  parent_session   TEXT,
  seed_length      INTEGER,
  origin           TEXT,
  delegation_depth INTEGER,
  agent_preset     TEXT,
  incarnation      TEXT NOT NULL,
  revision         INTEGER NOT NULL
);

CREATE TABLE events (
  session_id        INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq               INTEGER NOT NULL,
  type              TEXT NOT NULL,
  time              INTEGER NOT NULL,
  data              ANY NOT NULL,
  source_event_seqs ANY,
  surface_op        TEXT,
  is_packed         INTEGER NOT NULL CHECK (is_packed IN (0, 1)),
  PRIMARY KEY (session_id, seq)
);
`

// requiredObjects are the schema objects every mutation expects to exist —
// validated before each write so external tampering fails loud at the next
// write instead of corrupting the store.
var requiredObjects = []string{"persistence_state", "sessions", "events"}

// Config carries the store's resolved options. Defaults are applied by
// NormalizeConfig, not by hidden fallbacks inside operations.
type Config struct {
	// Path is the SQLite database path, or ":memory:" for an in-process
	// database.
	Path string
	// JournalMode is the durable journal pragma; empty selects wal.
	JournalMode JournalMode
	// BusyTimeoutMs is the maximum wait for a competing SQLite lock.
	BusyTimeoutMs int64
}

// NormalizeConfig applies the documented defaults and validates the enum.
func NormalizeConfig(config Config) (Config, error) {
	if config.Path == "" {
		return Config{}, errors.New("sqlite: path must not be empty")
	}
	if config.JournalMode == "" {
		config.JournalMode = JournalWAL
	}
	switch config.JournalMode {
	case JournalWAL, JournalDelete, JournalTruncate, JournalPersist:
	default:
		return Config{}, fmt.Errorf("sqlite: unknown journal mode %q", config.JournalMode)
	}
	if config.BusyTimeoutMs <= 0 {
		config.BusyTimeoutMs = DefaultBusyTimeoutMs
	}
	return config, nil
}

// Store opens, owns, and drives the physical session database.
type Store struct {
	config    Config
	db        *sql.DB
	storeID   string
	pathReady error
	opened    bool
}

// Open validates filesystem ownership without opening the database; the
// database itself opens lazily on first persistence use (the official
// open-on-first-use split). Call ValidatePath first when composition wants
// the path check early.
func Open(config Config) (*Store, error) {
	normalized, err := NormalizeConfig(config)
	if err != nil {
		return nil, err
	}
	store := &Store{config: normalized}
	if normalized.Path != ":memory:" {
		store.pathReady = preparePath(normalized.Path)
	}
	return store, nil
}

// ValidatePath settles the store's one path-validation operation.
func (s *Store) ValidatePath() error { return s.pathReady }

// openDB lazily opens and validates the database.
func (s *Store) openDB() error {
	if s.opened {
		return nil
	}
	if s.pathReady != nil {
		return s.pathReady
	}
	dsn := s.config.Path
	if dsn != ":memory:" {
		dsn = "file:" + filepath.ToSlash(dsn)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("sqlite: open %q: %w", s.config.Path, err)
	}
	// One connection: the official store drives one DatabaseSync handle, and
	// single-connection use keeps BEGIN IMMEDIATE bookkeeping exact.
	db.SetMaxOpenConns(1)
	s.db = db
	if err := s.configureAndValidate(); err != nil {
		_ = db.Close()
		s.db = nil
		return err
	}
	s.opened = true
	return nil
}

// configureAndValidate applies connection pragmas and schema-ownership
// validation: journal mode, busy timeout, foreign keys, trusted schema, and
// the application-id / user-version identity checks (initializing an empty
// database).
func (s *Store) configureAndValidate() error {
	if _, err := s.exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("sqlite: foreign_keys: %w", err)
	}
	if err := s.selectJournalMode(); err != nil {
		return err
	}
	if _, err := s.exec(fmt.Sprintf("PRAGMA busy_timeout = %d", s.config.BusyTimeoutMs)); err != nil {
		return fmt.Errorf("sqlite: busy_timeout: %w", err)
	}
	// Connection-security settings, checked the way the official open does:
	// the value must survive as configured, or the build refuses the store.
	if _, err := s.exec("PRAGMA trusted_schema = OFF"); err != nil {
		return fmt.Errorf("sqlite: trusted_schema: %w", err)
	}
	trusted, err := s.queryInt("PRAGMA trusted_schema")
	if err != nil {
		return fmt.Errorf("sqlite: trusted_schema read: %w", err)
	}
	if trusted != 0 {
		return fmt.Errorf("session database at %q retained trusted_schema=%d, expected 0", s.config.Path, trusted)
	}
	if _, err := s.exec("PRAGMA mmap_size = 0"); err != nil {
		return fmt.Errorf("sqlite: mmap_size: %w", err)
	}
	if err := s.configureDurability(); err != nil {
		return err
	}

	beginErr := s.immediate(func() error {
		userVersion, err := s.queryInt("PRAGMA user_version")
		if err != nil {
			return fmt.Errorf("sqlite: user_version read: %w", err)
		}
		applicationID, err := s.queryInt("PRAGMA application_id")
		if err != nil {
			return fmt.Errorf("sqlite: application_id read: %w", err)
		}
		userObjects, err := s.userObjectCount()
		if err != nil {
			return err
		}
		if userVersion == 0 && (applicationID != 0 || userObjects > 0) {
			return fmt.Errorf("session database at %q has an unversioned schema or application identity", s.config.Path)
		}
		if userVersion != 0 && userVersion != SCHEMA_VERSION {
			return fmt.Errorf("session database at %q has schema version %d, incompatible with this build (%d)", s.config.Path, userVersion, SCHEMA_VERSION)
		}
		if userVersion != 0 && applicationID != SessionPersistenceSqliteApplicationID {
			return fmt.Errorf("session database at %q has application id %d, expected %d", s.config.Path, applicationID, SessionPersistenceSqliteApplicationID)
		}
		if userVersion == 0 {
			if err := s.initialize(); err != nil {
				return err
			}
		}
		if err := s.validateRequiredSchema(); err != nil {
			return err
		}
		storeID, err := s.readString("SELECT store_id FROM persistence_state WHERE singleton = 1")
		if err != nil {
			return fmt.Errorf("sqlite: store identity read: %w", err)
		}
		if storeID == "" {
			return fmt.Errorf("session database at %q has an empty store identity", s.config.Path)
		}
		s.storeID = storeID
		return nil
	})
	return beginErr
}

// selectJournalMode sets the journal pragma and verifies the database
// retained it. In-memory databases have no journal; SQLite reports
// `memory` there and the official open expects exactly that.
func (s *Store) selectJournalMode() error {
	if _, err := s.exec(fmt.Sprintf("PRAGMA journal_mode = %s", s.config.JournalMode)); err != nil {
		return fmt.Errorf("sqlite: journal_mode: %w", err)
	}
	mode, err := s.readString("PRAGMA journal_mode")
	if err != nil {
		return fmt.Errorf("sqlite: journal_mode read: %w", err)
	}
	expected := string(s.config.JournalMode)
	if s.config.Path == ":memory:" {
		expected = "memory"
	}
	if !strings.EqualFold(strings.TrimSpace(mode), expected) {
		return fmt.Errorf("session database at %q selected journal mode %s, expected %s", s.config.Path, mode, expected)
	}
	return nil
}

// configureDurability pins synchronous=FULL and verifies it survived — the
// official durability guarantee behind "durable append".
func (s *Store) configureDurability() error {
	if _, err := s.exec("PRAGMA synchronous = FULL"); err != nil {
		return fmt.Errorf("sqlite: synchronous: %w", err)
	}
	mode, err := s.queryInt("PRAGMA synchronous")
	if err != nil {
		return fmt.Errorf("sqlite: synchronous read: %w", err)
	}
	if mode != 2 {
		return fmt.Errorf("session database at %q retained synchronous=%d, expected FULL (2)", s.config.Path, mode)
	}
	return nil
}

// initialize creates the schema and stamps ownership in one transaction.
func (s *Store) initialize() error {
	if _, err := s.exec("PRAGMA page_size = 8192"); err != nil {
		return fmt.Errorf("sqlite: page_size: %w", err)
	}
	if _, err := s.exec(schemaSQL); err != nil {
		return fmt.Errorf("sqlite: schema: %w", err)
	}
	if _, err := s.exec(fmt.Sprintf("PRAGMA application_id = %d", SessionPersistenceSqliteApplicationID)); err != nil {
		return fmt.Errorf("sqlite: application_id stamp: %w", err)
	}
	if _, err := s.exec(fmt.Sprintf("PRAGMA user_version = %d", SCHEMA_VERSION)); err != nil {
		return fmt.Errorf("sqlite: user_version stamp: %w", err)
	}
	if _, err := s.exec("INSERT INTO persistence_state (singleton, store_id) VALUES (1, ?)", newUUID()); err != nil {
		return fmt.Errorf("sqlite: store identity stamp: %w", err)
	}
	return nil
}

// validateRequiredSchema refuses a mutation-facing store missing any owned
// object.
func (s *Store) validateRequiredSchema() error {
	for _, object := range requiredObjects {
		count, err := s.queryInt("SELECT COUNT(*) FROM sqlite_master WHERE type IN ('table','view') AND name = ?", object)
		if err != nil {
			return fmt.Errorf("sqlite: schema probe: %w", err)
		}
		if count == 0 {
			return fmt.Errorf("session database at %q is missing required schema object %q", s.config.Path, object)
		}
	}
	return nil
}

// preparePath validates filesystem ownership before the database opens.
func preparePath(path string) error {
	if filepath.IsAbs(path) {
		path = filepath.Clean(path)
	} else {
		return fmt.Errorf("sqlite: database path %q must be absolute", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("sqlite: database directory: %w", err)
	}
	if err := validateParentDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sqlite: database stat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("sqlite: database %q must be a regular file, not a symbolic link", path)
	}
	// POSIX mode refusals, matching the official guard's platform split:
	// Windows exposes neither meaningful uid nor mode bits.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("sqlite: database %q must be accessible only by its owner", path)
	}
	return nil
}

func validateParentDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("sqlite: database parent stat: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("sqlite: database parent %q must be a real directory", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("sqlite: database parent %q must not be group/world-writable", path)
	}
	return nil
}
