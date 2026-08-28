package workspace

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"dshgo/session"
)

// WorkspaceMoveInvalidError marks an InsertSessionBefore request that named
// a session or anchor not on the account (storage failures stay plain
// errors). Detect it with errors.As into *WorkspaceMoveInvalidError.
type WorkspaceMoveInvalidError struct{ Message string }

func (e *WorkspaceMoveInvalidError) Error() string { return e.Message }

// TableStore is the open workspaces table seam the entity mutates through.
// Update applies fn to the value current at the caller's chain slot and
// stores the result. The entity-layer Table is the in-memory seam (tests and
// pre-bootstrap use); the registry provides the domain-backed store.
type TableStore interface {
	Get(id WorkspaceID) (WorkspaceRecord, bool)
	Update(id WorkspaceID, fn func(current WorkspaceRecord) (WorkspaceRecord, error)) (WorkspaceRecord, error)
}

// EntityHost is the registry-owned machinery an entity mutates through.
// Entities never see the registry itself — only the open table, the
// canonical session-path index backing the SessionIDs projection, and
// attach-time header reads.
type EntityHost interface {
	// Table resolves the open `workspaces` table. The source keeps this on
	// the host rather than the entity so construction can precede registry
	// start; Go adaptation resolves it once at entity construction instead,
	// which preserves the entity's ignorance of the registry.
	Table() TableStore
	// ReadSessionPath reads a session's canonical directory from the
	// registry's header index: the canonical directory, or "" when the
	// header is missing or its cwd cannot identify an existing directory.
	ReadSessionPath(id session.SessionID) string
	// ReadSessionHeader reads one stored session header for attach
	// validation. It fails when session persistence is absent or holds no
	// session with this id.
	ReadSessionHeader(id session.SessionID) (session.SessionHeader, error)
	// RememberSessionPath publishes a successfully validated canonical cwd
	// to the projection index.
	RememberSessionPath(id session.SessionID, path string)
}

// Table is the open workspaces table seam: Update applies fn to the value
// current at the caller's chain slot and stores the result. Go adaptation:
// the source's async KvTable collapses to a synchronous keyed update; the
// entity only relies on read-modify-write serialization, which the mutex
// provides.
type Table struct {
	mu      sync.Mutex
	entries map[WorkspaceID]WorkspaceRecord
}

// NewTable builds an empty table.
func NewTable() *Table { return &Table{entries: map[WorkspaceID]WorkspaceRecord{}} }

// Get reads one record.
func (t *Table) Get(id WorkspaceID) (WorkspaceRecord, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record, ok := t.entries[id]
	return record, ok
}

// Update applies fn to the record current at its chain slot; fn returning
// an error aborts the slot with no write.
func (t *Table) Update(id WorkspaceID, fn func(current WorkspaceRecord) (WorkspaceRecord, error)) (WorkspaceRecord, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	current, ok := t.entries[id]
	if !ok {
		return WorkspaceRecord{}, fmt.Errorf("workspace '%s' is not in the open table", id)
	}
	next, err := fn(current)
	if err != nil {
		return WorkspaceRecord{}, err
	}
	t.entries[id] = next
	return next, nil
}

// Put seeds one record directly (registry create transaction; not part of
// the entity write path).
func (t *Table) Put(id WorkspaceID, record WorkspaceRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.entries[id] = record
}

// errUnchanged is the chain-slot abort sentinel: the update fn found the
// record needs no change; only the mutate write path observes it.
var errUnchanged = errors.New("workspace record unchanged (internal sentinel)")

// Entity is the single Workspace implementation; constructed only by the
// registry. It holds a record snapshot that is swapped after each durable
// mutation; every write funnels through mutate so updatedAt stamping and
// invalid-account pruning happen exactly once.
type Entity struct {
	host EntityHost
	id   WorkspaceID

	mu     sync.Mutex
	record WorkspaceRecord
}

// NewEntity builds the entity over a validated record snapshot loaded or
// just written.
func NewEntity(host EntityHost, id WorkspaceID, record WorkspaceRecord) *Entity {
	return &Entity{host: host, id: id, record: record}
}

// ID returns the record's stable id.
func (e *Entity) ID() WorkspaceID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.id
}

// Path returns the canonical directory path stamped at create — never
// rewritten afterwards, even when the directory disappears.
func (e *Entity) Path() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.record.Path
}

// Title returns the display title.
func (e *Entity) Title() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.record.Title
}

// CreatedAt returns the ISO-8601 creation instant.
func (e *Entity) CreatedAt() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.record.CreatedAt
}

// UpdatedAt returns the ISO-8601 instant of the last durable mutation.
func (e *Entity) UpdatedAt() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.record.UpdatedAt
}

// SessionIDs returns header-validated sessions in manually owned order.
// The durable candidate account is filtered synchronously: missing headers,
// invalid cwd values, and canonical cwd mismatches are never returned. A
// subsequent workspace mutation prunes those filtered candidates durably.
func (e *Entity) SessionIDs() []session.SessionID {
	e.mu.Lock()
	record := e.record
	e.mu.Unlock()
	kept := make([]session.SessionID, 0, len(record.SessionIDs))
	for _, id := range record.SessionIDs {
		if e.host.ReadSessionPath(id) == record.Path {
			kept = append(kept, id)
		}
	}
	return kept
}

// SetTitle replaces the display title durably. Any string is legal;
// duplicates across workspaces are allowed.
func (e *Entity) SetTitle(title string) error {
	return e.mutate(func(record WorkspaceRecord) (WorkspaceRecord, error) {
		record.Title = title
		return record, nil
	})
}

// AttachSession prepends a session to the candidate account. An already
// accounted id resolves without writing (aside from the durable
// filtered-candidate prune every accepted mutation performs). A new id's
// stored header cwd must resolve to an existing directory equal to Path;
// unknown ids, missing or invalid cwd values, and mismatches fail without
// writing. Validation is skipped when the settled snapshot already accounts
// the id: the cwd fact was checked when it first attached and both inputs
// (stored header cwd, workspace path) are immutable. Membership itself is
// decided on the write chain inside mutate, never on the snapshot.
func (e *Entity) AttachSession(sessionID session.SessionID) error {
	e.mu.Lock()
	accounted := contains(e.record.SessionIDs, sessionID)
	path := e.record.Path
	e.mu.Unlock()
	if !accounted {
		header, err := e.host.ReadSessionHeader(sessionID)
		if err != nil {
			return err
		}
		if header.CWD == "" {
			return fmt.Errorf(
				"cannot attach session '%s' to workspace '%s': its stored header carries no cwd to validate against",
				sessionID, path)
		}
		cwd, err := RealpathNormalize(header.CWD)
		if err != nil {
			return fmt.Errorf(
				"cannot attach session '%s' to workspace '%s': its cwd '%s' does not resolve, so it cannot be validated",
				sessionID, path, header.CWD)
		}
		if !dirExists(cwd) {
			return fmt.Errorf(
				"cannot attach session '%s' to workspace '%s': its cwd '%s' is not a directory",
				sessionID, path, header.CWD)
		}
		if cwd != path {
			return fmt.Errorf(
				"cannot attach session '%s' to workspace '%s': its cwd resolves to '%s'",
				sessionID, path, cwd)
		}
		e.host.RememberSessionPath(sessionID, cwd)
	}
	return e.mutate(func(record WorkspaceRecord) (WorkspaceRecord, error) {
		if contains(record.SessionIDs, sessionID) {
			return record, nil
		}
		record.SessionIDs = append([]session.SessionID{sessionID}, record.SessionIDs...)
		return record, nil
	})
}

// InsertSessionBefore moves an accounted session within the manual order,
// DOM-insertBefore-like: with an anchor the session lands before it,
// without one it appends to the end. Only the moved id changes position. A
// session or anchor absent from the account fails without writing; a move
// to the current position resolves without writing, aside from the durable
// filtered-candidate prune every accepted mutation performs.
func (e *Entity) InsertSessionBefore(sessionID session.SessionID, beforeSessionID session.SessionID) error {
	return e.mutate(func(record WorkspaceRecord) (WorkspaceRecord, error) {
		if !contains(record.SessionIDs, sessionID) {
			return WorkspaceRecord{}, &WorkspaceMoveInvalidError{
				Message: fmt.Sprintf(
					"cannot move session '%s' in workspace '%s': the session is not accounted",
					sessionID, record.Path)}
		}
		if beforeSessionID != "" && !contains(record.SessionIDs, beforeSessionID) {
			return WorkspaceRecord{}, &WorkspaceMoveInvalidError{
				Message: fmt.Sprintf(
					"cannot move session '%s' before '%s' in workspace '%s': the anchor session is not accounted",
					sessionID, beforeSessionID, record.Path)}
		}
		if beforeSessionID == sessionID {
			return record, nil
		}
		without := make([]session.SessionID, 0, len(record.SessionIDs))
		for _, id := range record.SessionIDs {
			if id != sessionID {
				without = append(without, id)
			}
		}
		at := len(without)
		if beforeSessionID != "" {
			at = indexOf(without, beforeSessionID)
		}
		sessionIDs := make([]session.SessionID, 0, len(without)+1)
		sessionIDs = append(sessionIDs, without[:at]...)
		sessionIDs = append(sessionIDs, sessionID)
		sessionIDs = append(sessionIDs, without[at:]...)
		if equalSlices(sessionIDs, record.SessionIDs) {
			return record, nil
		}
		record.SessionIDs = sessionIDs
		return record, nil
	})
}

// DetachSession removes a session from the account. Idempotent: an id not
// on the account resolves without writing, aside from the durable
// filtered-candidate prune every accepted mutation performs. Never touches
// the session's own stored log.
func (e *Entity) DetachSession(sessionID session.SessionID) error {
	return e.mutate(func(record WorkspaceRecord) (WorkspaceRecord, error) {
		if !contains(record.SessionIDs, sessionID) {
			return record, nil
		}
		kept := make([]session.SessionID, 0, len(record.SessionIDs)-1)
		for _, id := range record.SessionIDs {
			if id != sessionID {
				kept = append(kept, id)
			}
		}
		record.SessionIDs = kept
		return record, nil
	})
}

// Status is the live directory check, uncached: whether Path currently
// exists and is a directory. A missing directory never mutates the record —
// the directory may only be temporarily moved.
func (e *Entity) Status() string {
	e.mu.Lock()
	path := e.record.Path
	e.mu.Unlock()
	if dirExists(path) {
		return "ok"
	}
	return "missing-dir"
}

// mutate is the single write path: run fn on the domain write chain via
// Table.Update, stamping updatedAt and pruning candidates that no longer
// pass the id-plus-canonical-cwd membership check, then swap the snapshot.
//
// fn sees the value current at its chain slot, so membership decisions
// (attach/detach idempotence) are race-free against queued writes; a fn
// signalling no change by returning current verbatim aborts the slot
// through the sentinel when pruning also finds nothing, so a no-op neither
// rewrites the medium nor emits a change event.
func (e *Entity) mutate(fn func(record WorkspaceRecord) (WorkspaceRecord, error)) error {
	next, err := e.table().Update(e.id, func(current WorkspaceRecord) (WorkspaceRecord, error) {
		changed, err := fn(current)
		if err != nil {
			return WorkspaceRecord{}, err
		}
		sessionIDs := make([]session.SessionID, 0, len(changed.SessionIDs))
		for _, id := range changed.SessionIDs {
			if e.host.ReadSessionPath(id) == changed.Path {
				sessionIDs = append(sessionIDs, id)
			}
		}
		unchanged := sameRecordIdentity(changed, current) && len(sessionIDs) == len(current.SessionIDs)
		if unchanged {
			return WorkspaceRecord{}, errUnchanged
		}
		changed.SessionIDs = sessionIDs
		changed.UpdatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
		return changed, nil
	})
	if err != nil {
		if errors.Is(err, errUnchanged) {
			return nil
		}
		return err
	}
	e.mu.Lock()
	e.record = next
	e.mu.Unlock()
	return nil
}

// table resolves the open table through the host seam.
func (e *Entity) table() TableStore { return e.host.Table() }

// sameRecordIdentity reports whether the fn's change touched nothing but
// the fields mutate itself stamps.
func sameRecordIdentity(a, b WorkspaceRecord) bool {
	return a.Path == b.Path && a.Title == b.Title && a.CreatedAt == b.CreatedAt &&
		equalSlices(a.SessionIDs, b.SessionIDs)
}

func contains(ids []session.SessionID, needle session.SessionID) bool {
	return indexOf(ids, needle) >= 0
}

func indexOf(ids []session.SessionID, needle session.SessionID) int {
	for i, id := range ids {
		if id == needle {
			return i
		}
	}
	return -1
}

func equalSlices(a, b []session.SessionID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
