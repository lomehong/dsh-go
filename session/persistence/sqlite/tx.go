package sqlite

import (
	"database/sql"
	"fmt"
)

// exec runs one statement on the single connection.
func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	return s.db.Exec(query, args...)
}

// queryInt reads a single integer scalar.
func (s *Store) queryInt(query string, args ...any) (int64, error) {
	var value int64
	if err := s.db.QueryRow(query, args...).Scan(&value); err != nil {
		return 0, err
	}
	return value, nil
}

// readString reads a single text scalar.
func (s *Store) readString(query string) (string, error) {
	var value string
	if err := s.db.QueryRow(query).Scan(&value); err != nil {
		return "", err
	}
	return value, nil
}

// userObjectCount counts non-internal schema objects — the unversioned-
// database probe (a foreign database with objects but no user_version is
// refused, never initialized over).
func (s *Store) userObjectCount() (int64, error) {
	count, err := s.queryInt("SELECT COUNT(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'")
	if err != nil {
		return 0, fmt.Errorf("sqlite: schema object count: %w", err)
	}
	return count, nil
}

// begin runs read work inside a deferred transaction on the single
// connection. A panic in the work rolls the transaction back before it
// propagates: the store forces one connection, so a dangling open
// transaction would silently swallow every later statement.
func (s *Store) begin(read func() error) error {
	if _, err := s.exec("BEGIN"); err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			s.rollback()
		}
	}()
	if err := read(); err != nil {
		return err
	}
	if _, err := s.exec("COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	committed = true
	return nil
}

// immediate runs mutation work inside a BEGIN IMMEDIATE transaction,
// rolling back on any failure — including a panic — so the store keeps its
// pre-operation state.
func (s *Store) immediate(mutate func() error) error {
	if _, err := s.exec("BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("sqlite: begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			s.rollback()
		}
	}()
	if err := mutate(); err != nil {
		return err
	}
	if _, err := s.exec("COMMIT"); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	committed = true
	return nil
}

// rollback discards an in-flight transaction; the original error is the one
// reported.
func (s *Store) rollback() {
	_, _ = s.exec("ROLLBACK")
}

// validateSchemaForMutation re-probes the required objects before a write so
// external tampering fails loud at the next mutation.
func (s *Store) validateSchemaForMutation() error {
	return s.validateRequiredSchema()
}
