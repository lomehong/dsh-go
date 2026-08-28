package workspace

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dshgo/session"
	"dshgo/storagedomain"
)

// timeRFC3339Millis is the record timestamp format (JS toISOString).
const timeRFC3339Millis = "2006-01-02T15:04:05.000Z07:00"

// NewWorkspaceID generates one record id: a random RFC-4122 v4 UUID, never
// derived from the path — path normalization rewrites paths, and a reference
// anchor must stay stable.
func NewWorkspaceID() WorkspaceID {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("workspace: cannot generate an id: %v", err))
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// baseName is the display-title fallback: the path's final element.
func baseName(path string) string {
	return filepath.Base(strings.TrimSuffix(path, string(filepath.Separator)))
}

// domainTableStore adapts the domain-backed table to the entity-layer
// TableStore seam: the entity's synchronous read-modify-write lands through
// the domain's serialized write chain, so entity mutations hit the medium.
type domainTableStore struct {
	table storagedomain.Table
}

// Get decodes one stored record; an absent key reads as not-found.
func (s domainTableStore) Get(id WorkspaceID) (WorkspaceRecord, bool) {
	raw := s.table.Get(string(id))
	if raw == nil {
		return WorkspaceRecord{}, false
	}
	record, err := ValidateWorkspaceRecord(raw)
	if err != nil {
		return WorkspaceRecord{}, false
	}
	return record, true
}

// Update applies fn to the record current at its chain slot; fn returning an
// error (including the entity's unchanged sentinel) aborts the slot with no
// write.
func (s domainTableStore) Update(id WorkspaceID, fn func(current WorkspaceRecord) (WorkspaceRecord, error)) (WorkspaceRecord, error) {
	raw := s.table.Get(string(id))
	if raw == nil {
		return WorkspaceRecord{}, fmt.Errorf("workspace '%s' is not in the open table", id)
	}
	current, err := ValidateWorkspaceRecord(raw)
	if err != nil {
		return WorkspaceRecord{}, err
	}
	next, err := fn(current)
	if err != nil {
		return WorkspaceRecord{}, err
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return WorkspaceRecord{}, err
	}
	if _, err := s.table.Update(string(id), func(json.RawMessage) json.RawMessage { return encoded }); err != nil {
		return WorkspaceRecord{}, err
	}
	return next, nil
}

// entityHost wires the registry machinery the entity writes through.
func (r *Registry) entityHost() EntityHost {
	return registryHost{registry: r}
}

type registryHost struct{ registry *Registry }

func (h registryHost) Table() TableStore { return h.registry.ownedEntityTable }

func (h registryHost) ReadSessionPath(id session.SessionID) string {
	h.registry.mu.Lock()
	defer h.registry.mu.Unlock()
	entry, ok := h.registry.headers[id]
	if !ok {
		return ""
	}
	return entry.path
}

func (h registryHost) ReadSessionHeader(id session.SessionID) (session.SessionHeader, error) {
	return h.registry.readSessionHeader(id)
}

func (h registryHost) RememberSessionPath(id session.SessionID, path string) {
	h.registry.mu.Lock()
	defer h.registry.mu.Unlock()
	entry, ok := h.registry.headers[id]
	if !ok {
		entry = headerEntry{}
	}
	entry.path = path
	entry.reason = ""
	h.registry.headers[id] = entry
}

// readSessionHeader resolves one stored header: live first, then the index,
// then a fresh persistence listing.
func (r *Registry) readSessionHeader(id session.SessionID) (session.SessionHeader, error) {
	if r.host.Sessions != nil {
		if header, live := r.host.Sessions.Header(id); live {
			entry := r.headers[id]
			r.headers[id] = headerEntry{header: header, path: entry.path}
			return header, nil
		}
	}
	if entry, ok := r.headers[id]; ok {
		return entry.header, nil
	}
	headers, err := r.host.Persistence.List(context.Background())
	if err != nil {
		return session.SessionHeader{}, err
	}
	r.indexHeaders(headers)
	if entry, ok := r.headers[id]; ok {
		return entry.header, nil
	}
	return session.SessionHeader{}, fmt.Errorf("cannot validate session '%s': session persistence holds no such session", id)
}

// recoverPendingMutationLocked completes the one mutation explicitly named
// by durable state. Unexplained order/table divergence still reaches
// validateStoredState and fails loud; this path never guesses which
// operation created a row from its shape alone.
func (r *Registry) recoverPendingMutationLocked() error {
	pending := r.state.PendingMutation
	if pending == nil {
		return nil
	}
	if containsID(r.state.WorkspaceIDs, pending.WorkspaceID) {
		return fmt.Errorf(
			"workspace domain is inconsistent: pending %s workspace '%s' is still present in registry order",
			pending.Operation, pending.WorkspaceID)
	}
	if _, err := r.table.Delete(string(pending.WorkspaceID)); err != nil && !isMissingKey(err) {
		return err
	}
	cleared := r.state
	cleared.PendingMutation = nil
	return r.setState(cleared)
}

// isMissingKey reports whether the table deletion missed an absent record —
// recovery tolerates the record already being gone.
func isMissingKey(err error) bool {
	var unitErr *storagedomain.UnitError
	if errorsAs(err, &unitErr) {
		return unitErr.Code == "missing-key"
	}
	return false
}

func errorsAs(err error, target **storagedomain.UnitError) bool {
	for err != nil {
		if unitErr, ok := err.(*storagedomain.UnitError); ok {
			*target = unitErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

// bootstrap rebuilds the whole registry from the header index: group
// sessions by canonical cwd, create one record per group missing one, merge
// historical sessions into existing records, and rank workspaces by newest
// session (ties by path, then prior durable rank, then id) — exactly once,
// guarded by the initialized marker.
func (r *Registry) bootstrap(headers []session.SessionHeader) error {
	type group struct {
		path     string
		headers  []session.SessionHeader
		newestAt int64
	}
	groupsByPath := map[string][]session.SessionHeader{}
	var order []string
	for _, header := range headers {
		entry, ok := r.headers[header.ID]
		if !ok || entry.path == "" {
			continue
		}
		if _, seen := groupsByPath[entry.path]; !seen {
			order = append(order, entry.path)
		}
		groupsByPath[entry.path] = append(groupsByPath[entry.path], header)
	}
	groups := make([]group, 0, len(groupsByPath))
	for _, path := range order {
		members := groupsByPath[path]
		sort.SliceStable(members, func(i, j int) bool {
			left, right := members[i], members[j]
			if left.CreatedAt != right.CreatedAt {
				return right.CreatedAt < left.CreatedAt
			}
			return left.ID < right.ID
		})
		groups = append(groups, group{path: path, headers: members, newestAt: members[0].CreatedAt})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		left, right := groups[i], groups[j]
		if left.newestAt != right.newestAt {
			return left.newestAt > right.newestAt
		}
		return left.path < right.path
	})

	byPath := map[string]WorkspaceID{}
	accounted := map[session.SessionID]WorkspaceID{}
	for _, id := range r.tableKeys() {
		record := r.record(id)
		byPath[record.Path] = id
		for _, sessionID := range record.SessionIDs {
			accounted[sessionID] = id
		}
	}

	for _, grp := range groups {
		id, known := byPath[grp.path]
		if !known {
			sessionIDs := make([]session.SessionID, 0, len(grp.headers))
			for _, header := range grp.headers {
				if _, seen := accounted[header.ID]; !seen {
					sessionIDs = append(sessionIDs, header.ID)
				}
			}
			if len(sessionIDs) == 0 {
				continue
			}
			id = NewWorkspaceID()
			createdAt := time.UnixMilli(grp.newestAt).UTC().Format(timeRFC3339Millis)
			record := WorkspaceRecord{
				Path: grp.path, Title: baseName(grp.path), SessionIDs: sessionIDs,
				CreatedAt: createdAt, UpdatedAt: createdAt,
			}
			if err := r.putRecord(id, record); err != nil {
				return err
			}
			byPath[grp.path] = id
			for _, sessionID := range sessionIDs {
				accounted[sessionID] = id
			}
			continue
		}
		current := r.record(id)
		historical := make([]session.SessionID, 0, len(grp.headers))
		for _, header := range grp.headers {
			holder, seen := accounted[header.ID]
			if !seen || holder == id {
				historical = append(historical, header.ID)
			}
		}
		historicalSet := map[session.SessionID]bool{}
		for _, sessionID := range historical {
			historicalSet[sessionID] = true
		}
		sessionIDs := append([]session.SessionID{}, historical...)
		for _, sessionID := range current.SessionIDs {
			if !historicalSet[sessionID] {
				sessionIDs = append(sessionIDs, sessionID)
			}
		}
		if sameSessionIDs(current.SessionIDs, sessionIDs) {
			continue
		}
		updated := current
		updated.SessionIDs = sessionIDs
		updated.UpdatedAt = time.Now().UTC().Format(timeRFC3339Millis)
		if err := r.putRecord(id, updated); err != nil {
			return err
		}
		for _, sessionID := range historical {
			accounted[sessionID] = id
		}
	}

	groupRank := map[string]int64{}
	for _, grp := range groups {
		groupRank[grp.path] = grp.newestAt
	}
	priorRank := map[WorkspaceID]int{}
	for index, id := range r.state.WorkspaceIDs {
		priorRank[id] = index
	}
	ids := append([]WorkspaceID{}, r.tableKeys()...)
	sort.SliceStable(ids, func(i, j int) bool {
		left, right := r.record(ids[i]), r.record(ids[j])
		leftTime, ok := groupRank[left.Path]
		if !ok {
			leftTime = recordCreatedAtMillis(left)
		}
		rightTime, ok := groupRank[right.Path]
		if !ok {
			rightTime = recordCreatedAtMillis(right)
		}
		if leftTime != rightTime {
			return leftTime > rightTime
		}
		leftRank, lok := priorRank[ids[i]]
		if !lok {
			leftRank = math.MaxInt64
		}
		rightRank, rok := priorRank[ids[j]]
		if !rok {
			rightRank = math.MaxInt64
		}
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return ids[i] < ids[j]
	})

	if !sameIDs(r.state.WorkspaceIDs, ids) {
		marking := r.state
		marking.Initialized = false
		marking.WorkspaceIDs = ids
		if err := r.setState(marking); err != nil {
			return err
		}
	}
	final := r.state
	final.Initialized = true
	final.WorkspaceIDs = ids
	return r.setState(final)
}

// recordCreatedAtMillis reads one record's creation stamp as epoch millis;
// an unparseable stamp ranks as zero.
func recordCreatedAtMillis(record WorkspaceRecord) int64 {
	parsed, err := time.Parse(timeRFC3339Millis, record.CreatedAt)
	if err != nil {
		return 0
	}
	return parsed.UnixMilli()
}

// record decodes one stored record; a missing row decodes as zero.
func (r *Registry) record(id WorkspaceID) WorkspaceRecord {
	raw := r.table.Get(string(id))
	if raw == nil {
		return WorkspaceRecord{}
	}
	record, err := ValidateWorkspaceRecord(raw)
	if err != nil {
		return WorkspaceRecord{}
	}
	return record
}

// tableKeys returns the stored record ids in sorted order.
func (r *Registry) tableKeys() []WorkspaceID {
	entries := r.table.Entries()
	ids := make([]WorkspaceID, 0, len(entries))
	for key := range entries {
		ids = append(ids, WorkspaceID(key))
	}
	sort.Strings(ids)
	return ids
}

// putRecord writes one record through the domain table.
func (r *Registry) putRecord(id WorkspaceID, record WorkspaceRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return r.table.Put(string(id), encoded)
}

// deleteRecord removes one record; an absent record deletes idempotently
// (recovery and rollback paths tolerate the row already being gone).
func (r *Registry) deleteRecord(id WorkspaceID) error {
	_, err := r.table.Delete(string(id))
	if err != nil && !isMissingKey(err) {
		return err
	}
	return nil
}

// validateStoredState asserts the owned order/table relationships.
func (r *Registry) validateStoredState(state DomainState) error {
	seen := map[WorkspaceID]bool{}
	for _, id := range state.WorkspaceIDs {
		if seen[id] {
			return fmt.Errorf("workspace domain is inconsistent: registry order repeats workspace '%s'", id)
		}
		if r.table.Get(string(id)) == nil {
			return fmt.Errorf("workspace domain is inconsistent: registry order references missing workspace '%s'", id)
		}
		seen[id] = true
	}
	if state.Initialized && len(seen) != r.table.Size() {
		for _, id := range r.tableKeys() {
			if !seen[id] {
				return fmt.Errorf("workspace domain is inconsistent: workspace '%s' is absent from registry order", id)
			}
		}
	}
	paths := map[string]WorkspaceID{}
	accounted := map[session.SessionID]WorkspaceID{}
	for _, id := range r.tableKeys() {
		record := r.record(id)
		if holder, clash := paths[record.Path]; clash {
			return fmt.Errorf(
				"workspace domain is inconsistent: path '%s' is claimed by both workspace '%s' and workspace '%s'",
				record.Path, holder, id)
		}
		paths[record.Path] = id
		for _, sessionID := range record.SessionIDs {
			if holder, clash := accounted[sessionID]; clash {
				return fmt.Errorf(
					"workspace domain is inconsistent: session '%s' is accounted by both workspace '%s' and workspace '%s'",
					sessionID, holder, id)
			}
			accounted[sessionID] = id
		}
	}
	return nil
}

// rebuildEntities rebuilds the ordered entity cache from the durable state.
func (r *Registry) rebuildEntities() {
	r.entities = map[WorkspaceID]*Entity{}
	for _, id := range r.state.WorkspaceIDs {
		r.entities[id] = NewEntity(r.entityHost(), id, r.record(id))
	}
}

// replaceHeaderIndex drops and rebuilds the derived header index.
func (r *Registry) replaceHeaderIndex(headers []session.SessionHeader) {
	r.headers = map[session.SessionID]headerEntry{}
	r.indexHeaders(headers)
}

// indexHeaders validates each header's cwd against the real filesystem.
func (r *Registry) indexHeaders(headers []session.SessionHeader) {
	for _, header := range headers {
		r.indexHeader(header)
	}
}

// indexHeader records one header and its canonical-path verdict: resolved
// directories index with their path; missing cwd, non-directories, and
// unresolvable paths index with a filter reason.
func (r *Registry) indexHeader(header session.SessionHeader) {
	entry := headerEntry{header: header}
	if header.CWD == "" {
		entry.reason = "header has no cwd"
		r.headers[header.ID] = entry
		return
	}
	path, err := RealpathNormalize(header.CWD)
	if err != nil {
		entry.reason = fmt.Sprintf("cwd '%s' does not resolve", header.CWD)
		r.headers[header.ID] = entry
		return
	}
	if !dirExists(path) {
		entry.reason = fmt.Sprintf("cwd '%s' is not a directory", header.CWD)
		r.headers[header.ID] = entry
		return
	}
	entry.path = path
	r.headers[header.ID] = entry
}

// indexLiveSessions overlays live session headers on the persisted index.
func (r *Registry) indexLiveSessions() {
	if r.host.Sessions == nil {
		return
	}
	r.indexHeaders(r.host.Sessions.List())
}

// reportFilteredCandidates logs every durable member the header index no
// longer validates, with the specific reason.
func (r *Registry) reportFilteredCandidates() {
	if r.host.Logger == nil {
		return
	}
	for _, id := range r.state.WorkspaceIDs {
		record := r.record(id)
		for _, sessionID := range record.SessionIDs {
			entry, indexed := r.headers[sessionID]
			if indexed && entry.path == record.Path {
				continue
			}
			reason := entry.reason
			if reason == "" {
				if indexed {
					reason = fmt.Sprintf("canonical cwd '%s' differs from workspace path '%s'", entry.path, record.Path)
				} else {
					reason = "session header is missing"
				}
			}
			r.host.Logger.Warn(fmt.Sprintf(
				"workspace '%s' filtered session '%s' from membership: %s", id, sessionID, reason))
		}
	}
}

// sessionKnown reports whether a session is live, header-indexed, or present
// in a fresh persistence listing. Only a definite miss returns false — a
// failing listing propagates so storage faults never masquerade as an
// unknown session.
func (r *Registry) sessionKnown(ctx context.Context, id session.SessionID) (bool, error) {
	if r.host.Sessions != nil {
		if _, live := r.host.Sessions.Header(id); live {
			return true, nil
		}
	}
	if _, ok := r.headers[id]; ok {
		return true, nil
	}
	headers, err := r.host.Persistence.List(ctx)
	if err != nil {
		return false, err
	}
	r.indexHeaders(headers)
	_, ok := r.headers[id]
	return ok, nil
}

// stateWithoutPending returns the current state minus its mutation marker.
func (r *Registry) stateWithoutPending() DomainState {
	cleared := r.state
	cleared.PendingMutation = nil
	return cleared
}

func containsID(ids []WorkspaceID, needle WorkspaceID) bool {
	return indexOfID(ids, needle) >= 0
}

func indexOfID(ids []WorkspaceID, needle WorkspaceID) int {
	for index, id := range ids {
		if id == needle {
			return index
		}
	}
	return -1
}

func containsSessionID(ids []session.SessionID, needle session.SessionID) bool {
	for _, id := range ids {
		if id == needle {
			return true
		}
	}
	return false
}

func sameIDs(left, right []WorkspaceID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameSessionIDs(left, right []session.SessionID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func withoutID(ids []WorkspaceID, drop WorkspaceID) []WorkspaceID {
	kept := make([]WorkspaceID, 0, len(ids))
	for _, id := range ids {
		if id != drop {
			kept = append(kept, id)
		}
	}
	return kept
}
