package subagent

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"dshgo/session"
)

// Read-only enumeration of durable subagent children and descendant trees
// (official list-children.ts). Candidates come from one live-preferred
// corpus; each child's mode/label is the `subagent` projection unit's value,
// resolved down a three-rung ladder: the registry's watermark snapshot for a
// live child, a durable projection-cache row when it serves an own-suffix
// identity, and one shared session observation otherwise, validated against
// the enumerated lifecycle. The projection fold is the single classification
// authority — this module parses no descriptor itself. Absent persistence,
// enumeration is live-only.
//
// Go adaptations: the cordis `ctx.get` service web is an explicit
// ListChildrenServices value; AbortSignal becomes context.Context;
// `localeCompare` becomes byte-order string comparison (documented);
// `localeCompare`'s collation is never load-bearing in tests or callers.

// Cold read concurrency per explicit catalog listing. Current session
// persistence providers are local; a networked provider must promote this to
// a validated deployment setting.
const coldReadConcurrency = 4

// Subagent list entry kinds.
const (
	ListKindChild      = "child"
	ListKindDiagnostic = "diagnostic"
)

// SubagentListEntry is one row of a direct-child catalog: a served `subagent`
// projection value produces a child; a settled candidate whose fold served no
// identity produces a diagnostic; a running candidate without one is omitted
// — its descriptor may not be appended yet (the creation window).
type SubagentListEntry struct {
	// Kind discriminates child rows from diagnostics.
	Kind string `json:"kind"`
	// ID is the durable child session id, stable across Activations.
	ID session.SessionID `json:"id"`
	// Activity is whether the child was live when its reader sampled it:
	// running means the logical record is resident, inactive that it exists
	// only in persistence. Neither encodes a durable outcome.
	Activity string `json:"activity,omitempty"`
	// Mode is the descriptor lifecycle (child rows only).
	Mode string `json:"mode,omitempty"`
	// Label is the durable creation label; optional for one-shot children.
	Label *string `json:"label,omitempty"`
	// HasChildren reports a direct descendant with durable origin subagent.
	HasChildren bool `json:"hasChildren,omitempty"`
	// Reason is the diagnostic verdict (diagnostic rows only): corrupt for
	// deterministic data damage, unavailable for a retryable read failure.
	// unsupported is never produced; it remains in the vocabulary for
	// consumers that route on it.
	Reason string `json:"reason,omitempty"`
}

// SubagentDescendantListEntry adds one candidate's position in the complete
// session tree to its interpreted facts.
type SubagentDescendantListEntry struct {
	SubagentListEntry
	// ParentID is the durable direct parent in the enumerated tree.
	ParentID session.SessionID `json:"parentId"`
	// Depth counts edges from the requested root; direct children are 1.
	Depth int `json:"depth"`
}

// SubagentQueryError carries a session-query stable code across the seam so
// the per-child isolation can distinguish stable corruption from retryable
// absence or backend failure.
type SubagentQueryError struct {
	Code string
	Err  error
}

// Error implements error.
func (e *SubagentQueryError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

// Unwrap reaches the cause.
func (e *SubagentQueryError) Unwrap() error { return e.Err }

// Session query codes the listing classifies.
const (
	QueryCodeCorruptSession = "SESSION_QUERY_CORRUPT_SESSION"
	QueryCodeSourceConflict = "SESSION_QUERY_SOURCE_CONFLICT"
)

// ListChildrenServices are the listing seams (official
// ctx.get('sessionProjections' | 'sessions' | 'sessionQuery' |
// 'sessionProjectionCache')). Projections, Sessions, and Query are required;
// Cache is optional acceleration.
type ListChildrenServices struct {
	Projections SubagentProjectionRegistry
	Sessions    SubagentSessionStore
	Query       SubagentQueryEngine
	Cache       SubagentProjectionCache
}

// SubagentProjectionRegistry folds live sessions into projection values
// (official `sessionProjections`). A fold error is deterministic data damage
// in that one session.
type SubagentProjectionRegistry interface {
	Snapshot(sess *session.Session, units []string) (SubagentProjectionValues, error)
}

// SubagentSessionStore resolves live sessions by id (official `sessions`).
type SubagentSessionStore interface {
	Get(id session.SessionID) *session.Session
}

// SubagentObservedSession is one cold observation with its folded projection
// cut.
type SubagentObservedSession struct {
	Header      session.SessionHeader
	Projections *SubagentProjectionValues
}

// SubagentQueryEngine is the cold read seam (official `sessionQuery`).
type SubagentQueryEngine interface {
	ListSessions() ([]session.SessionHeader, error)
	ObserveSession(id session.SessionID) (*SubagentObservedSession, error)
}

// SubagentProjectionCache serves durable projection-cache rows (official
// `sessionProjectionCache`). It is derived data: a read error renders no
// verdict and falls through to the authoritative re-fold.
type SubagentProjectionCache interface {
	CachedSnapshot(header session.SessionHeader, units []string) (*SubagentIdentityProjection, error)
}

// corpusRecord pairs one enumerated header with its live session, if any.
type corpusRecord struct {
	header session.SessionHeader
	live   *session.Session
}

// positionedCandidate is a candidate with its tree position.
type positionedCandidate struct {
	record   corpusRecord
	parentID session.SessionID
	depth    int
}

// listingRuntime resolves the services once and carries one live-preferred
// session corpus.
type listingRuntime struct {
	services        ListChildrenServices
	corpus          map[session.SessionID]corpusRecord
	subagentParents map[session.SessionID]struct{}
}

// ListChildren enumerates one parent's origin-classified direct children from
// the live-preferred merge of the session store and session persistence,
// serving each identity from the `subagent` projection unit. Entries are
// ordered by createdAt, then id.
func ListChildren(ctx context.Context, services ListChildrenServices, parentSessionID session.SessionID) ([]SubagentListEntry, error) {
	listing, err := prepareListing(ctx, services)
	if err != nil {
		return nil, err
	}
	candidates := make([]corpusRecord, 0, len(listing.corpus))
	for _, record := range listing.corpus {
		if record.header.ParentSession == parentSessionID && record.header.Origin == SubagentOrigin {
			candidates = append(candidates, record)
		}
	}
	sortCorpusRecords(candidates)
	rows, err := resolveCandidateRows(ctx, candidates, listing)
	if err != nil {
		return nil, err
	}
	entries := make([]SubagentListEntry, 0, len(rows))
	for _, row := range rows {
		if row != nil {
			entries = append(entries, *row)
		}
	}
	return entries, nil
}

// ListDescendants enumerates every session-backed subagent below one root in
// stable pre-order. Ordinary sessions and one-shot children remain traversal
// nodes, so a continuable child below either is still discovered. No Agent is
// loaded or resumed.
func ListDescendants(ctx context.Context, services ListChildrenServices, rootSessionID session.SessionID) ([]SubagentDescendantListEntry, error) {
	listing, err := prepareListing(ctx, services)
	if err != nil {
		return nil, err
	}
	positioned := descendantCandidates(listing.corpus, rootSessionID)
	records := make([]corpusRecord, len(positioned))
	for i, candidate := range positioned {
		records[i] = candidate.record
	}
	rows, err := resolveCandidateRows(ctx, records, listing)
	if err != nil {
		return nil, err
	}
	entries := make([]SubagentDescendantListEntry, 0, len(rows))
	for i, row := range rows {
		if row != nil {
			entries = append(entries, SubagentDescendantListEntry{
				SubagentListEntry: *row,
				ParentID:          positioned[i].parentID,
				Depth:             positioned[i].depth,
			})
		}
	}
	return entries, nil
}

// prepareListing resolves the listing services once and builds one
// live-preferred session corpus. A missing fold capability is a
// deterministic deployment configuration error, never an empty success, and
// is checked before any read — even with zero candidates.
func prepareListing(ctx context.Context, services ListChildrenServices) (*listingRuntime, error) {
	if services.Projections == nil {
		return nil, newSubagentError(
			"listing subagents requires the sessionProjections registry (load @deepseek-ai/dsh-session-projection)",
			CodeControlProjectionsUnavailable, nil)
	}
	if services.Sessions == nil {
		return nil, newSubagentError(
			"listing subagents requires the session store (load @deepseek-ai/dsh-session)",
			CodeControlSessionStoreUnavailable, nil)
	}
	if err := assertListingNotCancelled(ctx); err != nil {
		return nil, err
	}
	if services.Query == nil {
		return nil, newSubagentError(
			"listing subagents requires the sessionQuery service (load @deepseek-ai/dsh-session-query)",
			CodeControlQueryUnavailable, nil)
	}
	headers, err := services.Query.ListSessions()
	if err != nil {
		if cancelErr := assertListingNotCancelled(ctx); cancelErr != nil {
			return nil, cancelErr
		}
		return nil, err
	}
	if err := assertListingNotCancelled(ctx); err != nil {
		return nil, err
	}
	// Live-preferred merge without header reconciliation: a live record wins
	// its id wholesale.
	corpus := make(map[session.SessionID]corpusRecord, len(headers))
	for _, header := range headers {
		live := services.Sessions.Get(header.ID)
		if live != nil {
			header = live.Header()
		}
		corpus[header.ID] = corpusRecord{header: header, live: live}
	}
	subagentParents := map[session.SessionID]struct{}{}
	for _, record := range corpus {
		if record.header.Origin == SubagentOrigin && record.header.ParentSession != "" {
			subagentParents[record.header.ParentSession] = struct{}{}
		}
	}
	return &listingRuntime{services: services, corpus: corpus, subagentParents: subagentParents}, nil
}

// resolveCandidateRows resolves projection-backed rows for aligned
// candidates with bounded cold reads. A nil row omits a running candidate
// whose identity has not landed yet.
func resolveCandidateRows(ctx context.Context, candidates []corpusRecord, listing *listingRuntime) ([]*SubagentListEntry, error) {
	rows := make([]*SubagentListEntry, len(candidates))
	type coldRead struct {
		index  int
		header session.SessionHeader
	}
	var coldReads []coldRead
	for index, candidate := range candidates {
		childID := candidate.header.ID
		if candidate.live == nil {
			coldReads = append(coldReads, coldRead{index: index, header: candidate.header})
			continue
		}
		// Read only the identity unit. A live child without an identity yet
		// is the creation window before the establishing provider appends
		// its descriptor.
		values, err := listing.services.Projections.Snapshot(candidate.live, []string{"subagent"})
		if err != nil {
			// A rejecting identity fold is deterministic data damage in this
			// child; contain it as one diagnostic instead of failing the
			// whole listing.
			rows[index] = diagnosticRow(childID, SubagentDiagnosticCorrupt)
			continue
		}
		if values.Subagent == nil || values.Subagent.Seq < seedLengthOf(candidate.header) {
			continue
		}
		rows[index] = childRow(childID, values.Subagent, SubagentActivityLive, listing.hasChildren(childID))
	}
	if len(coldReads) > 0 {
		jobs := make(chan coldRead)
		var workers sync.WaitGroup
		workerCount := coldReadConcurrency
		if len(coldReads) < workerCount {
			workerCount = len(coldReads)
		}
		for range workerCount {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for job := range jobs {
					rows[job.index] = resolveColdIdentity(ctx, listing, job.header)
				}
			}()
		}
		for _, job := range coldReads {
			jobs <- job
		}
		close(jobs)
		workers.Wait()
	}
	return rows, assertListingNotCancelled(ctx)
}

// hasChildren reports whether a child id owns a durable subagent descendant.
func (l *listingRuntime) hasChildren(childID session.SessionID) bool {
	_, owned := l.subagentParents[childID]
	return owned
}

// descendantCandidates builds origin-classified candidates from the complete
// tree without recursion.
func descendantCandidates(corpus map[session.SessionID]corpusRecord, rootSessionID session.SessionID) []positionedCandidate {
	children := map[session.SessionID][]corpusRecord{}
	for _, record := range corpus {
		if record.header.ParentSession == "" {
			continue
		}
		children[record.header.ParentSession] = append(children[record.header.ParentSession], record)
	}
	for _, siblings := range children {
		sortCorpusRecords(siblings)
	}
	positioned := make([]positionedCandidate, 0)
	// Pre-order with a stack: reversing at push keeps each sibling group in
	// sorted order as frames pop.
	stack := make([]positionedCandidate, 0)
	direct := children[rootSessionID]
	for i := len(direct) - 1; i >= 0; i-- {
		stack = append(stack, positionedCandidate{record: direct[i], parentID: rootSessionID, depth: 1})
	}
	visited := map[session.SessionID]struct{}{rootSessionID: {}}
	for len(stack) > 0 {
		position := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		id := position.record.header.ID
		if _, seen := visited[id]; seen {
			continue
		}
		visited[id] = struct{}{}
		if position.record.header.Origin == SubagentOrigin {
			positioned = append(positioned, position)
		}
		descendants := children[id]
		for i := len(descendants) - 1; i >= 0; i-- {
			stack = append(stack, positionedCandidate{
				record:   descendants[i],
				parentID: id,
				depth:    position.depth + 1,
			})
		}
	}
	return positioned
}

// sortCorpusRecords orders siblings by durable creation time, then id.
func sortCorpusRecords(records []corpusRecord) {
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].header.CreatedAt != records[j].header.CreatedAt {
			return records[i].header.CreatedAt < records[j].header.CreatedAt
		}
		return strings.Compare(string(records[i].header.ID), string(records[j].header.ID)) < 0
	})
}

// resolveColdIdentity resolves one cold candidate down the remaining ladder:
// a durable projection-cache row when it serves an own-suffix identity,
// otherwise one shared session observation. An absent or transiently failed
// observation is one `unavailable` row retried on the next listing; an
// observation source naming another lifecycle, and a settled log the fold
// cannot identify — or that makes a registered unit throw — are final, so
// they report `corrupt`.
func resolveColdIdentity(ctx context.Context, listing *listingRuntime, header session.SessionHeader) *SubagentListEntry {
	childID := header.ID
	hasChildren := listing.hasChildren(childID)
	if listing.services.Cache != nil {
		var cached *SubagentIdentityProjection
		// Unlike the live fold, a throwing cache read renders no verdict: the
		// cache is derived data, so its damage (a poisoned stored row of ANY
		// unit) silently falls through to the authoritative re-fold.
		cached, _ = listing.services.Cache.CachedSnapshot(header, []string{"subagent"})
		// A child's OWN descriptor is immutable once appended, so a cached
		// identity is final only when the seq gate proves it was folded from
		// the own suffix: a creation-window checkpoint may instead carry a
		// fork seed's replayed ANCESTOR descriptor (seq below seedLength),
		// which must not outrank the re-fold. A nil value also falls through:
		// its verdict belongs to the authoritative re-fold, not to a derived
		// row.
		if cached != nil && cached.Seq >= seedLengthOf(header) {
			return childRow(childID, cached, SubagentActivityCold, hasChildren)
		}
	}
	if err := assertListingNotCancelled(ctx); err != nil {
		return diagnosticRow(childID, SubagentDiagnosticUnavailable)
	}
	observation, err := listing.services.Query.ObserveSession(childID)
	if err != nil {
		// Per-child isolation: durable corruption is stable; absence and
		// backend failures remain retryable. Either way, the listing itself
		// still succeeds.
		_ = assertListingNotCancelled(ctx)
		if queryCode(err) == QueryCodeCorruptSession || queryCode(err) == QueryCodeSourceConflict {
			return diagnosticRow(childID, SubagentDiagnosticCorrupt)
		}
		return diagnosticRow(childID, SubagentDiagnosticUnavailable)
	}
	if err := assertListingNotCancelled(ctx); err != nil {
		return diagnosticRow(childID, SubagentDiagnosticUnavailable)
	}
	// A session id names a slot, not a lifecycle: a child deleted and
	// re-published under another owner between the enumeration and this read
	// must not leak into the old parent's listing.
	if observation == nil || !sameLifecycle(observation.Header, header) {
		return diagnosticRow(childID, SubagentDiagnosticCorrupt)
	}
	var identity *SubagentIdentityProjection
	if observation.Projections != nil {
		identity = observation.Projections.Subagent
	}
	if identity == nil || identity.Seq < seedLengthOf(header) {
		return diagnosticRow(childID, SubagentDiagnosticCorrupt)
	}
	return childRow(childID, identity, SubagentActivityCold, hasChildren)
}

// childRow materializes one served identity as its child row.
func childRow(id session.SessionID, identity *SubagentIdentityProjection, activity string, hasChildren bool) *SubagentListEntry {
	return &SubagentListEntry{
		Kind:        ListKindChild,
		ID:          id,
		Mode:        identity.Mode,
		Label:       identity.Label,
		Activity:    activity,
		HasChildren: hasChildren,
	}
}

// diagnosticRow materializes one failed verdict.
func diagnosticRow(id session.SessionID, reason string) *SubagentListEntry {
	return &SubagentListEntry{Kind: ListKindDiagnostic, ID: id, Reason: reason}
}

// SubagentOrigin is the durable header origin of a session created as a
// subagent child.
const SubagentOrigin = "subagent"

// seedLengthOf reads a header's inherited seed length; absent means zero.
func seedLengthOf(header session.SessionHeader) int64 {
	if !header.IsSeeded {
		return 0
	}
	return int64(header.InheritedEventCount)
}

// lifecycleWitnessKeys are the immutable header fields that distinguish one
// session lifecycle from another under the same id. Compared field-by-field
// against the official key list.
func sameLifecycle(meta session.SessionHeader, expected session.SessionHeader) bool {
	return meta.Version == expected.Version &&
		meta.ID == expected.ID &&
		meta.CreatedAt == expected.CreatedAt &&
		meta.CWD == expected.CWD &&
		meta.ParentSession == expected.ParentSession &&
		seedLengthOf(meta) == seedLengthOf(expected) &&
		delegationDepthOf(meta) == delegationDepthOf(expected) &&
		meta.Origin == expected.Origin &&
		meta.AgentPreset == expected.AgentPreset
}

// delegationDepthOf reads an optional header delegation depth; absent means
// zero (both sides absent and zero/absent compare equal, matching `===` on
// undefined numbers for the witness purpose).
func delegationDepthOf(header session.SessionHeader) int64 {
	if header.DelegationDepth == nil {
		return 0
	}
	return *header.DelegationDepth
}

// assertListingNotCancelled stops a listing at its next cancellation
// checkpoint.
func assertListingNotCancelled(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return newSubagentError("subagent listing was cancelled", CodeCancelled, nil)
	}
	return nil
}

// queryCode reads a session-query stable code off an error chain.
func queryCode(err error) string {
	var queryErr *SubagentQueryError
	if errors.As(err, &queryErr) {
		return queryErr.Code
	}
	return ""
}
