package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/storagedomain"
)

// SessionPersistence is the stored-history listing seam (the source's
// mandatory `sessionPersistence` dependency): an unavailable peer must never
// be mistaken for an empty history and commit the initialized marker.
type SessionPersistence interface {
	// List returns every stored session's header.
	List(ctx context.Context) ([]session.SessionHeader, error)
}

// LiveSessions is the optional live-session table seam (`ctx.get('sessions')`
// in the source): headers of live sessions outrank the persisted index.
type LiveSessions interface {
	// Header returns one live session's header, and whether it is live.
	Header(id session.SessionID) (session.SessionHeader, bool)
	// List returns every live session's header.
	List() []session.SessionHeader
}

// RegistryHost is the composition the registry runs against.
type RegistryHost struct {
	// Persistence is the mandatory stored-history listing.
	Persistence SessionPersistence
	// Sessions is the optional live-session table.
	Sessions LiveSessions
	// Logger receives filtered-candidate and marker-cleanup diagnostics.
	Logger cordis.Logger
}

// domainSpec projects the shipped workspace spec onto the domain layer: one
// `workspaces` table keyed by WorkspaceID plus the bootstrap/order singleton,
// validated at the durable read boundary by the shipped schemas.
func domainSpec() storagedomain.DomainSpec {
	return storagedomain.DomainSpec{
		Name:              WorkspaceDomainSpec.Name,
		Version:           WorkspaceDomainSpec.Version,
		Tables:            []string{"workspaces"},
		HasGlobal:         true,
		InitialGlobalJSON: json.RawMessage(WorkspaceDomainSpec.InitialJSON),
		ValidateRecord: func(table string, key string, raw json.RawMessage) error {
			if _, err := ValidateWorkspaceRecord(raw); err != nil {
				return err
			}
			return nil
		},
		ValidateGlobal: func(raw json.RawMessage) error {
			if _, err := ValidateDomainState(raw); err != nil {
				return err
			}
			return nil
		},
	}
}

// headerEntry is one indexed session header with its canonical-path verdict.
type headerEntry struct {
	header session.SessionHeader
	path   string
	reason string
}

// Registry is the durable workspace registry: stable records, stable display
// order, and header-validated session membership over the domain data form.
// Every mutation serializes through one operation chain (the source's
// `enqueueOperation` promise tail; Go's synchronous writes collapse it to a
// mutex), which re-runs pending-marker recovery before each create/delete.
type Registry struct {
	host   RegistryHost
	table  storagedomain.Table
	global storagedomain.Global

	mu               sync.Mutex
	state            DomainState
	entities         map[WorkspaceID]*Entity
	headers          map[session.SessionID]headerEntry
	ownedEntityTable TableStore
}

// NewRegistry opens the workspace domain over the facility, finishes the
// one-time history bootstrap when required, and rebuilds the ordered cache.
// The persistence dependency is mandatory so an unavailable peer can never
// be mistaken for an empty history.
func NewRegistry(ctx context.Context, host RegistryHost, facility *storagedomain.Facility) (*Registry, func(), error) {
	domain, err := facility.Open(domainSpec())
	if err != nil {
		return nil, nil, err
	}
	registry := &Registry{
		host:     host,
		table:    domain.Table("workspaces"),
		global:   domain.Global(),
		entities: map[WorkspaceID]*Entity{},
		headers:  map[session.SessionID]headerEntry{},
	}
	registry.ownedEntityTable = domainTableStore{table: registry.table}
	if err := registry.start(ctx); err != nil {
		_ = domain.Close()
		return nil, nil, err
	}
	return registry, func() { _ = domain.Close() }, nil
}

// start runs the startup sequence: recovery, validation, bootstrap or index
// refresh, live-session indexing, and entity rebuild.
func (r *Registry) start(ctx context.Context) error {
	state, err := r.loadState()
	if err != nil {
		return err
	}
	r.state = state
	if err := r.recoverPendingMutationLocked(); err != nil {
		return err
	}
	if err := r.validateStoredState(r.state); err != nil {
		return err
	}
	headers, err := r.host.Persistence.List(ctx)
	if err != nil {
		return err
	}
	if !r.state.Initialized {
		r.replaceHeaderIndex(headers)
		if err := r.bootstrap(headers); err != nil {
			return err
		}
	} else if r.table.Size() > 0 {
		r.replaceHeaderIndex(headers)
	}
	r.indexLiveSessions()
	if err := r.validateStoredState(r.state); err != nil {
		return err
	}
	r.rebuildEntities()
	r.reportFilteredCandidates()
	return nil
}

// loadState reads the stored global through the shipped validator.
func (r *Registry) loadState() (DomainState, error) {
	raw := r.global.InitialOrValue()
	state, err := ValidateDomainState(raw)
	if err != nil {
		return DomainState{}, err
	}
	return state, nil
}

// setState persists the registry state, then swaps the cache (publish at the
// commit point).
func (r *Registry) setState(state DomainState) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := r.global.Set(encoded); err != nil {
		return err
	}
	r.state = state
	return nil
}

// Create makes or reuses a workspace for an existing directory. The path is
// canonicalized; a nonexistent path fails with the original error and a
// non-directory fails. Repeated calls for the same canonical path return the
// existing entity without changing its title. A newly created workspace is
// prepended to the durable registry order.
func (r *Registry) Create(ctx context.Context, path string, title string) (*Entity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPendingMutationLocked(); err != nil {
		return nil, err
	}
	canonical, err := RealpathNormalize(path)
	if err != nil {
		return nil, err
	}
	if !dirExists(canonical) {
		return nil, fmt.Errorf("cannot create a workspace at '%s': path is not a directory", canonical)
	}
	for _, entity := range r.entities {
		if entity.Path() == canonical {
			return entity, nil
		}
	}
	displayTitle := title
	if displayTitle == "" {
		displayTitle = baseName(canonical)
	}
	id := NewWorkspaceID()
	now := time.Now().UTC().Format(timeRFC3339Millis)
	record := WorkspaceRecord{
		Path: canonical, Title: displayTitle, SessionIDs: []session.SessionID{},
		CreatedAt: now, UpdatedAt: now,
	}
	entity := NewEntity(r.entityHost(), id, record)
	r.entities[id] = entity
	pending := r.state
	pending.PendingMutation = &pendingMutation{Operation: "create", WorkspaceID: id}
	if err := r.setState(pending); err != nil {
		delete(r.entities, id)
		return nil, err
	}
	if err := r.putRecord(id, record); err != nil {
		delete(r.entities, id)
		if rollbackErr := r.setState(r.stateWithoutPending()); rollbackErr != nil {
			return nil, fmt.Errorf("workspace '%s' record write and pending-marker rollback both failed: %v; %v", id, err, rollbackErr)
		}
		return nil, err
	}
	// The final order write replaces the whole state — clearing the pending
	// marker exactly like the source's object literal (no spread of the
	// marker field).
	next := r.stateWithoutPending()
	next.Initialized = true
	next.WorkspaceIDs = append([]WorkspaceID{id}, next.WorkspaceIDs...)
	if err := r.setState(next); err != nil {
		delete(r.entities, id)
		if rollbackErr := r.deleteRecord(id); rollbackErr != nil {
			return nil, fmt.Errorf("workspace '%s' order write and record rollback both failed; the pending marker remains recoverable: %v; %v", id, err, rollbackErr)
		}
		if rollbackErr := r.setState(r.stateWithoutPending()); rollbackErr != nil {
			return nil, fmt.Errorf("workspace '%s' order write and pending-marker rollback both failed: %v; %v", id, err, rollbackErr)
		}
		return nil, err
	}
	return entity, nil
}

// Get looks up a workspace by id; unknown ids return nil.
func (r *Registry) Get(id WorkspaceID) *Entity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entities[id]
}

// List returns the synchronous workspace projection in durable registry
// order. Entity SessionIDs getters filter through the startup/live
// canonical-cwd header index; no persistence reads happen here.
func (r *Registry) List() []*Entity {
	r.mu.Lock()
	defer r.mu.Unlock()
	entities := make([]*Entity, 0, len(r.state.WorkspaceIDs))
	for _, id := range r.state.WorkspaceIDs {
		entity, ok := r.entities[id]
		if !ok {
			panic(fmt.Sprintf("workspace registry order references missing workspace '%s'", id))
		}
		entities = append(entities, entity)
	}
	return entities
}

// Delete removes one workspace registration while retaining its directory
// and every session log. The durable order is updated before the table
// deletion; a failed table write restores the prior order and keeps the
// entity published. Unknown ids are an idempotent no-op. It reports whether
// a record was deleted.
func (r *Registry) Delete(ctx context.Context, id WorkspaceID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPendingMutationLocked(); err != nil {
		return false, err
	}
	entity, ok := r.entities[id]
	if !ok {
		return false, nil
	}
	previous := r.state
	next := r.state
	next.Initialized = true
	next.WorkspaceIDs = withoutID(next.WorkspaceIDs, id)
	marked := next
	marked.PendingMutation = &pendingMutation{Operation: "delete", WorkspaceID: id}
	if err := r.setState(marked); err != nil {
		return false, err
	}
	delete(r.entities, id)
	_ = entity
	if err := r.deleteRecord(id); err != nil {
		r.entities[id] = entity
		if rollbackErr := r.setState(previous); rollbackErr != nil {
			// The durable marker still says to finish deletion, so the cache
			// must agree with that recoverable direction rather than
			// republish a row absent from the persisted order.
			delete(r.entities, id)
			return false, fmt.Errorf("workspace '%s' record deletion and registry-order rollback both failed: %v; %v", id, err, rollbackErr)
		}
		return false, err
	}
	if err := r.setState(next); err != nil {
		// The deletion committed at the table write and was already published
		// to Host streams. Keep the durable marker for startup recovery
		// rather than reporting failure after the requested state became true.
		if r.host.Logger != nil {
			r.host.Logger.Warn(fmt.Sprintf("workspace '%s' was deleted but its pending marker could not be cleared: %v", id, err))
		}
	}
	return true, nil
}

// InsertBefore moves one workspace within the durable display order,
// DOM-insertBefore-like: with an anchor it lands before that workspace,
// without one it appends. It returns the complete committed order.
func (r *Registry) InsertBefore(ctx context.Context, id WorkspaceID, beforeID WorkspaceID) ([]WorkspaceID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPendingMutationLocked(); err != nil {
		return nil, err
	}
	if !containsID(r.state.WorkspaceIDs, id) {
		return nil, &WorkspaceOrderInvalidError{WorkspaceID: id}
	}
	if beforeID != "" && !containsID(r.state.WorkspaceIDs, beforeID) {
		return nil, &WorkspaceOrderInvalidError{WorkspaceID: beforeID}
	}
	if beforeID == id {
		return append([]WorkspaceID{}, r.state.WorkspaceIDs...), nil
	}
	without := withoutID(r.state.WorkspaceIDs, id)
	at := len(without)
	if beforeID != "" {
		at = indexOfID(without, beforeID)
	}
	workspaceIDs := make([]WorkspaceID, 0, len(without)+1)
	workspaceIDs = append(workspaceIDs, without[:at]...)
	workspaceIDs = append(workspaceIDs, id)
	workspaceIDs = append(workspaceIDs, without[at:]...)
	if sameIDs(workspaceIDs, r.state.WorkspaceIDs) {
		return append([]WorkspaceID{}, r.state.WorkspaceIDs...), nil
	}
	next := r.state
	next.WorkspaceIDs = workspaceIDs
	if err := r.setState(next); err != nil {
		return nil, err
	}
	return append([]WorkspaceID{}, workspaceIDs...), nil
}

// ArchivedSessionIDs returns the registry-global archive set: sessions
// hidden from every grouping surface, in archive order. Archiving never
// touches workspace accounting — an archived session keeps its sessionIds
// slot so unarchiving restores its position.
func (r *Registry) ArchivedSessionIDs() []session.SessionID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]session.SessionID{}, r.state.ArchivedSessionIDs...)
}

// ArchiveSession archives one session durably. The session must exist (live
// or in session persistence); its workspace accounting is irrelevant. An
// already archived id resolves without writing.
func (r *Registry) ArchiveSession(ctx context.Context, id session.SessionID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverPendingMutationLocked(); err != nil {
		return err
	}
	if containsSessionID(r.state.ArchivedSessionIDs, id) {
		return nil
	}
	known, err := r.sessionKnown(ctx, id)
	if err != nil {
		return err
	}
	if !known {
		return &WorkspaceUnknownSessionError{SessionID: string(id)}
	}
	next := r.state
	next.ArchivedSessionIDs = append(append([]session.SessionID{}, next.ArchivedSessionIDs...), id)
	return r.setState(next)
}

// ResolveByPath resolves by canonical directory path without creating or
// mutating a workspace. A missing path fails during realpath; an existing
// unowned directory returns nil.
func (r *Registry) ResolveByPath(ctx context.Context, path string) (*Entity, error) {
	canonical, err := RealpathNormalize(path)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.state.WorkspaceIDs {
		if entity := r.entities[id]; entity != nil && entity.Path() == canonical {
			return entity, nil
		}
	}
	return nil, nil
}

// WorkspaceOrderInvalidError marks a reorder that named a source or anchor
// absent from the durable registry order.
type WorkspaceOrderInvalidError struct{ WorkspaceID WorkspaceID }

func (e *WorkspaceOrderInvalidError) Error() string {
	return fmt.Sprintf("cannot reorder unknown workspace '%s'", e.WorkspaceID)
}

// WorkspaceUnknownSessionError marks an archiveSession request that named a
// session neither live nor in session persistence — a definite miss only;
// storage faults propagate as themselves.
type WorkspaceUnknownSessionError struct{ SessionID string }

func (e *WorkspaceUnknownSessionError) Error() string {
	return fmt.Sprintf("cannot archive session '%s': live sessions and session persistence hold no such session", e.SessionID)
}
