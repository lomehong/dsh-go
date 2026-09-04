// Domain identity and the storage-domain store adapter for the projection
// cache (official spec.ts): one `sessions` table keyed by session id, each
// record the full projection checkpoint for one session. The per-record
// layout scopes version bumps per session — a stale session document is
// discarded on open (cache semantics), never a whole-medium rejection.
package projectioncache

import (
	"encoding/json"
	"fmt"

	"dshgo/session"
	"dshgo/storagedomain"
)

// DomainSpec is the validated session-projcache domain declaration
// (name session_projcache, version 4, per-record layout). The cache rows
// are disposable derived data, so a stored record that fails validation at
// open is backed up and skipped instead of refusing the plugin tree.
func DomainSpec() (storagedomain.DomainSpec, error) {
	return storagedomain.DefineDomain(storagedomain.DomainSpec{
		Name:                "session_projcache",
		Version:             4,
		InvalidRecordPolicy: storagedomain.InvalidRecordsBackupAndSkip,
		Layout:              storagedomain.LayoutPerRecord,
		Tables:              []string{"sessions"},
		ValidateRecord:      validateCheckpointRecord,
	})
}

// validateCheckpointRecord is the durable read boundary for one stored
// record: the `checkpointRecord` zod schema's runtime half. A row is never
// wrong, only possibly stale (seq) or discarded at read time (ver
// mismatch); the validator therefore enforces shape, not freshness.
func validateCheckpointRecord(table string, key string, raw json.RawMessage) error {
	if table != "sessions" {
		return fmt.Errorf("session_projcache: unknown table %q", table)
	}
	var decoded struct {
		Identity struct {
			CreatedAt *float64 `json:"createdAt"`
			Cwd       any      `json:"cwd"`
		} `json:"identity"`
		Rows map[string]struct {
			Ver *float64 `json:"ver"`
			Seq *float64 `json:"seq"`
			Val any      `json:"val"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return fmt.Errorf("session_projcache: record %q: %w", key, err)
	}
	if decoded.Identity.CreatedAt == nil || *decoded.Identity.CreatedAt < 0 || *decoded.Identity.CreatedAt != float64(int64(*decoded.Identity.CreatedAt)) {
		return fmt.Errorf("session_projcache: record %q: identity.createdAt must be a non-negative integer", key)
	}
	switch decoded.Identity.Cwd.(type) {
	case nil, string:
	default:
		return fmt.Errorf("session_projcache: record %q: identity.cwd must be a string", key)
	}
	for rowKey, row := range decoded.Rows {
		if row.Ver == nil || *row.Ver < 0 || *row.Ver != float64(int64(*row.Ver)) {
			return fmt.Errorf("session_projcache: record %q row %q: ver must be a non-negative integer", key, rowKey)
		}
		if row.Seq == nil || *row.Seq < -1 || *row.Seq != float64(int64(*row.Seq)) {
			return fmt.Errorf("session_projcache: record %q row %q: seq must be an integer >= -1", key, rowKey)
		}
	}
	return nil
}

// DomainStore is the Store view over one open domain's sessions table.
type DomainStore struct {
	domain *storagedomain.Domain
	table  storagedomain.Table
}

// NewDomainStore adapts one open session-projcache domain to the cache's
// Store. Close closes the domain.
func NewDomainStore(domain *storagedomain.Domain) (*DomainStore, error) {
	if domain == nil {
		return nil, fmt.Errorf("session projection cache: the domain is required")
	}
	return &DomainStore{domain: domain, table: domain.Table("sessions")}, nil
}

func (s *DomainStore) Get(id session.SessionID) (*Record, bool) {
	raw := s.table.Get(string(id))
	if raw == nil {
		return nil, false
	}
	record, err := UnmarshalRecord(raw)
	if err != nil {
		// The durable boundary already validated the record at open; a
		// mid-flight corruption reads as absent (cache semantics).
		return nil, false
	}
	return record, true
}

func (s *DomainStore) Put(id session.SessionID, record *Record) error {
	raw, err := MarshalRecord(record)
	if err != nil {
		return err
	}
	return s.table.Put(string(id), raw)
}

func (s *DomainStore) Close() error {
	return s.domain.Close()
}
