// Package sessionquerysqlite ports the durable half of
// @deepseek-ai/dsh-session-query-sqlite: a disposable SQLite FTS5 read model
// over session event search documents (schema guards, per-session document
// replacement, phrase-inert full-text queries, metadata predicates, and
// snippets deferred). The live-corpus feed and rebuild orchestration of the
// official index.ts remain engine-composition concerns; this package is the
// store they drive.
package sessionquerysqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"dshgo/sessionquery"
)

// SchemaVersion is the current derived-index schema version. Incompatible
// versions reset in place.
const SchemaVersion = 8

// ApplicationID is the SQLite application id protecting unrelated databases
// from derived resets.
const ApplicationID = 0x44534851

// Supported SQLite journal modes.
const (
	JournalWAL      = "wal"
	JournalDelete   = "delete"
	JournalTruncate = "truncate"
	JournalPersist  = "persist"
)

// Highlight markers reserved by the snippet pipeline; colliding text is
// sanitized before it enters the index or a MATCH query.
const (
	highlightStart = "\u2060"
	highlightEnd   = "\u2063"
)

// derivedUserTables are the user tables a recognized derived index may
// contain (FTS5 shadow tables included).
var derivedUserTables = map[string]bool{
	"search_state":           true,
	"persisted_sessions":     true,
	"persisted_docs":         true,
	"persisted_docs_data":    true,
	"persisted_docs_idx":     true,
	"persisted_docs_content": true,
	"persisted_docs_docsize": true,
	"persisted_docs_config":  true,
	"live_sessions":          true,
	"live_docs":              true,
	"live_docs_data":         true,
	"live_docs_idx":          true,
	"live_docs_content":      true,
	"live_docs_docsize":      true,
	"live_docs_config":       true,
}

// Config is the store's deployment policy.
type Config struct {
	// Path is a dedicated derived-index path or ":memory:".
	Path string
	// JournalMode is one of the supported journal modes.
	JournalMode string
}

// Store owns the derived-index database.
type Store struct {
	db   *sql.DB
	path string
}

// LazyStore defers Open to first use — the official base profile mounts the
// search service with `openAt: never`, so assembly must not touch the
// filesystem until a consumer drives the store.
type LazyStore struct {
	config Config
	once   sync.Once
	store  *Store
	err    error
}

// NewLazyStore records the deferred open policy.
func NewLazyStore(config Config) *LazyStore {
	return &LazyStore{config: config}
}

// Get opens the store on first call and returns the opened handle forever
// after (including on error — the failure is sticky, matching fail-loud
// composition).
func (l *LazyStore) Get() (*Store, error) {
	l.once.Do(func() {
		l.store, l.err = Open(l.config)
	})
	return l.store, l.err
}

// Close closes the underlying store if it was opened; a never-opened lazy
// store closes as a no-op.
func (l *LazyStore) Close() error {
	if l.store != nil {
		return l.store.Close()
	}
	return nil
}

// QuoteFtsData quotes caller text as one FTS5 phrase so query syntax remains
// inert data.
func QuoteFtsData(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

// SanitizeFtsText removes reserved marker collisions before text enters FTS5
// or MATCH (NUL and the highlight markers map to replacement characters).
func SanitizeFtsText(text string) string {
	text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	text = strings.ReplaceAll(text, highlightStart, "\uFFFD")
	return strings.ReplaceAll(text, highlightEnd, "\uFFFD")
}

// Open validates and initializes the derived-index database.
func Open(config Config) (*Store, error) {
	path := config.Path
	if path == "" {
		return nil, errors.New("session-query-sqlite: path is required")
	}
	switch config.JournalMode {
	case JournalWAL, JournalDelete, JournalTruncate, JournalPersist:
	default:
		return nil, fmt.Errorf("session-query-sqlite: unsupported journal mode %q", config.JournalMode)
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("session-query-sqlite: create index directory: %w", err)
		}
		handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			handle.Close()
		} else if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("session-query-sqlite: create index file: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("session-query-sqlite: open: %w", err)
	}
	store := &Store{db: db, path: path}
	if err := store.initialize(config.JournalMode); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initialize(journalMode string) error {
	var applicationID int64
	var version int64
	if err := s.db.QueryRow("PRAGMA application_id").Scan(&applicationID); err != nil {
		return fmt.Errorf("session-query-sqlite: application id: %w", err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("session-query-sqlite: user version: %w", err)
	}
	userTables, err := s.listUserTables()
	if err != nil {
		return err
	}
	if applicationID != 0 && applicationID != ApplicationID {
		return fmt.Errorf("session-query-sqlite: database at %q belongs to another application", s.path)
	}
	if applicationID == 0 && len(userTables) > 0 {
		return fmt.Errorf("session-query-sqlite: database at %q is not an empty or recognized derived index", s.path)
	}
	if applicationID == ApplicationID {
		for _, name := range userTables {
			if !derivedUserTables[name] {
				return fmt.Errorf("session-query-sqlite: database at %q has unrecognized user tables: %s", s.path, name)
			}
		}
		if version != SchemaVersion {
			if err := s.resetDerivedSchema(userTables); err != nil {
				return err
			}
		}
	}
	// journalMode is a validated closed union, not caller-controlled SQL.
	if _, err := s.db.Exec("PRAGMA journal_mode = " + strings.ToUpper(journalMode)); err != nil {
		return fmt.Errorf("session-query-sqlite: journal mode: %w", err)
	}
	return s.ensurePersistentSchema()
}

func (s *Store) listUserTables() ([]string, error) {
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT GLOB 'sqlite_*' ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("session-query-sqlite: list tables: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("session-query-sqlite: list tables: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *Store) resetDerivedSchema(userTables []string) error {
	for _, name := range userTables {
		if _, err := s.db.Exec("DROP TABLE IF EXISTS " + name); err != nil {
			return fmt.Errorf("session-query-sqlite: reset table %s: %w", name, err)
		}
	}
	if _, err := s.db.Exec("PRAGMA user_version = 0"); err != nil {
		return fmt.Errorf("session-query-sqlite: reset version: %w", err)
	}
	return nil
}

func (s *Store) ensurePersistentSchema() error {
	statements := []string{
		fmt.Sprintf("PRAGMA application_id = %d", ApplicationID),
		`CREATE TABLE IF NOT EXISTS search_state (
			singleton         INTEGER PRIMARY KEY CHECK (singleton = 1),
			global_generation INTEGER NOT NULL
		)`,
		`INSERT OR IGNORE INTO search_state (singleton, global_generation) VALUES (1, 0)`,
		`CREATE TABLE IF NOT EXISTS persisted_sessions (
			id             TEXT PRIMARY KEY,
			version        INTEGER NOT NULL,
			created_at     INTEGER NOT NULL,
			cwd            TEXT,
			parent_session TEXT,
			seed_length    INTEGER,
			delegation_depth INTEGER,
			agent_preset  TEXT,
			revision       TEXT NOT NULL,
			generation     INTEGER NOT NULL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS persisted_docs USING fts5(
			text,
			session_id UNINDEXED,
			seq UNINDEXED,
			type UNINDEXED,
			time UNINDEXED,
			surface UNINDEXED,
			codepoint_length UNINDEXED,
			tokenize = 'unicode61'
		)`,
		fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion),
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("session-query-sqlite: schema: %w", err)
		}
	}
	return nil
}

// Close releases the database handle. Idempotent.
func (s *Store) Close() error {
	return s.db.Close()
}

// ReplaceSessionDocuments atomically replaces one session's indexed
// documents and session row at the supplied persistence revision and
// generation. Empty documents clear the session from the index.
func (s *Store) ReplaceSessionDocuments(
	id string,
	version int64,
	createdAt int64,
	parentSession *string,
	seedLength *int64,
	delegationDepth *int64,
	agentPreset *string,
	revision string,
	generation int64,
	documents []sessionquery.SessionEventSearchDocument,
) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("session-query-sqlite: begin replace: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM persisted_docs WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("session-query-sqlite: clear documents: %w", err)
	}
	for _, document := range documents {
		text := SanitizeFtsText(document.Text)
		if _, err := tx.Exec(
			`INSERT INTO persisted_docs (text, session_id, seq, type, time, surface, codepoint_length)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			text, document.SessionID, document.Seq, document.Type, document.Time, document.Surface,
			len([]rune(text)),
		); err != nil {
			return fmt.Errorf("session-query-sqlite: insert document: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO persisted_sessions
			(id, version, created_at, cwd, parent_session, seed_length, delegation_depth, agent_preset, revision, generation)
		 VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			version = excluded.version,
			created_at = excluded.created_at,
			parent_session = excluded.parent_session,
			seed_length = excluded.seed_length,
			delegation_depth = excluded.delegation_depth,
			agent_preset = excluded.agent_preset,
			revision = excluded.revision,
			generation = excluded.generation`,
		id, version, createdAt, parentSession, seedLength, delegationDepth, agentPreset, revision, generation,
	); err != nil {
		return fmt.Errorf("session-query-sqlite: upsert session: %w", err)
	}
	if _, err := tx.Exec("UPDATE search_state SET global_generation = global_generation + 1 WHERE singleton = 1"); err != nil {
		return fmt.Errorf("session-query-sqlite: bump generation: %w", err)
	}
	return tx.Commit()
}

// DeleteSession removes one session from the derived index entirely.
func (s *Store) DeleteSession(id string) error {
	if _, err := s.db.Exec("DELETE FROM persisted_docs WHERE session_id = ?", id); err != nil {
		return fmt.Errorf("session-query-sqlite: delete documents: %w", err)
	}
	if _, err := s.db.Exec("DELETE FROM persisted_sessions WHERE id = ?", id); err != nil {
		return fmt.Errorf("session-query-sqlite: delete session: %w", err)
	}
	return nil
}

// EventHit is one full-text match with its indexed metadata.
type EventHit struct {
	SessionID string
	Seq       int64
	Type      string
	Time      int64
	Surface   string
	Text      string
}

// SearchEvents returns the indexed documents matching one phrase-inert full
// text query plus ANDed metadata filters. An empty query matches every
// indexed document.
func (s *Store) SearchEvents(query string, filters []sessionquery.SessionEventMetadataFilter, limit int) ([]EventHit, error) {
	if limit <= 0 {
		limit = 50
	}
	where := []string{"persisted_docs MATCH ?"}
	params := []any{QuoteFtsData(SanitizeFtsText(query))}
	clauses, clauseParams, err := buildEventWhere(filters)
	if err != nil {
		return nil, err
	}
	where = append(where, clauses...)
	params = append(params, clauseParams...)
	rows, err := s.db.Query(
		`SELECT session_id, seq, type, time, surface, text
		 FROM persisted_docs
		 WHERE `+strings.Join(where, " AND ")+
			" ORDER BY rank LIMIT ?",
		append(params, limit)...,
	)
	if err != nil {
		return nil, fmt.Errorf("session-query-sqlite: search: %w", err)
	}
	defer rows.Close()
	var hits []EventHit
	for rows.Next() {
		var hit EventHit
		if err := rows.Scan(&hit.SessionID, &hit.Seq, &hit.Type, &hit.Time, &hit.Surface, &hit.Text); err != nil {
			return nil, fmt.Errorf("session-query-sqlite: search scan: %w", err)
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

// buildEventWhere compiles metadata filters into FTS5-outer predicates. An
// FTS5 query plan tolerates at most a bounded number of outer predicates
// (the official cap is asserted at 8); exceeding it fails loud.
func buildEventWhere(filters []sessionquery.SessionEventMetadataFilter) ([]string, []any, error) {
	clauses := []string{}
	params := []any{}
	for _, filter := range filters {
		switch filter.Kind {
		case "seq", "time":
			column := "seq"
			if filter.Kind == "time" {
				column = "time"
			}
			bounds := []string{}
			if filter.From != nil {
				bounds = append(bounds, fmt.Sprintf("%s >= ?", column))
				params = append(params, int64(*filter.From))
			}
			if filter.To != nil {
				bounds = append(bounds, fmt.Sprintf("%s <= ?", column))
				params = append(params, int64(*filter.To))
			}
			clauses = append(clauses, bounds...)
		case "type", "surface":
			column := "type"
			if filter.Kind == "surface" {
				column = "surface"
			}
			if len(filter.Values) == 0 {
				return nil, nil, fmt.Errorf("session-query-sqlite: empty %s values match nothing", filter.Kind)
			}
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(filter.Values)), ",")
			clauses = append(clauses, column+" IN ("+placeholders+")")
			for _, value := range filter.Values {
				params = append(params, value)
			}
		default:
			return nil, nil, fmt.Errorf("session-query-sqlite: unknown event filter kind %q", filter.Kind)
		}
	}
	if len(clauses) > 8 {
		return nil, nil, fmt.Errorf("session-query-sqlite: too many outer predicates (%d)", len(clauses))
	}
	return clauses, params, nil
}
