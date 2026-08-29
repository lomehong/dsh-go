package subagent

import (
	"context"
	"errors"
	"testing"

	"dshgo/session"
)

// listing fakes ----------------------------------------------------------------

type fakeProjections struct {
	values map[session.SessionID]*SubagentIdentityProjection
	fail   map[session.SessionID]bool
}

func (f *fakeProjections) Snapshot(sess *session.Session, units []string) (SubagentProjectionValues, error) {
	if f.fail[sess.Header().ID] {
		return SubagentProjectionValues{}, errors.New("fold blew up")
	}
	return SubagentProjectionValues{Subagent: f.values[sess.Header().ID]}, nil
}

type fakeSessionStore struct {
	sessions map[session.SessionID]*session.Session
}

func (f *fakeSessionStore) Get(id session.SessionID) *session.Session { return f.sessions[id] }

type fakeQuery struct {
	headers []session.SessionHeader
	observe map[session.SessionID]*SubagentObservedSession
	errs    map[session.SessionID]error
}

func (f *fakeQuery) ListSessions() ([]session.SessionHeader, error) { return f.headers, nil }

func (f *fakeQuery) ObserveSession(id session.SessionID) (*SubagentObservedSession, error) {
	if err, ok := f.errs[id]; ok {
		return nil, err
	}
	return f.observe[id], nil
}

type fakeCache struct {
	rows map[session.SessionID]*SubagentIdentityProjection
}

func (f *fakeCache) CachedSnapshot(header session.SessionHeader, units []string) (*SubagentIdentityProjection, error) {
	return f.rows[header.ID], nil
}

// helpers ----------------------------------------------------------------------

func listingHeader(id string, parent string, createdAt int64, origin string, seedLength *int64) session.SessionHeader {
	return session.SessionHeader{
		// This build stores format version 0 only (fail-closed vocabulary).
		Version: 0, ID: session.SessionID(id), CreatedAt: createdAt,
		CWD: "D:\\work", ParentSession: session.SessionID(parent), Origin: origin,
		SeedLength: seedLength,
	}
}

func listingIdentity(mode string, label string, seq int64) *SubagentIdentityProjection {
	identity := &SubagentIdentityProjection{Mode: mode, Seq: seq}
	if label != "" {
		identity.Label = &label
	}
	return identity
}

func int64Ptr(v int64) *int64 { return &v }

// listingServices assembles one consistent fake world. The cache is
// conditionally assigned: a nil *fakeCache converted to the interface would
// be a non-nil interface carrying a nil pointer.
func listingServices(query *fakeQuery, store *fakeSessionStore, projections *fakeProjections, cache *fakeCache) ListChildrenServices {
	services := ListChildrenServices{Projections: projections, Sessions: store, Query: query}
	if cache != nil {
		services.Cache = cache
	}
	return services
}

func TestListChildrenOrdersAndClassifies(t *testing.T) {
	live := func(id string, createdAt int64, seedLength *int64) *session.Session {
		header := listingHeader(id, "root", createdAt, SubagentOrigin, seedLength)
		sess, err := session.NewDetached(session.SessionID(id), nil, &header)
		if err != nil {
			t.Fatalf("NewDetached %s: %v", id, err)
		}
		return sess
	}
	kidA := live("kid-a", 1, nil)         // live one-shot
	kidB := live("kid-b", 2, int64Ptr(5)) // live continuable, owns a cold child
	kidW := live("kid-w", 3, nil)         // creation window: no identity yet
	store := &fakeSessionStore{sessions: map[session.SessionID]*session.Session{
		"kid-a": kidA, "kid-b": kidB, "kid-w": kidW,
	}}
	projections := &fakeProjections{
		values: map[session.SessionID]*SubagentIdentityProjection{
			"kid-a": listingIdentity(SubagentModeOneShot, "", 1),
			"kid-b": listingIdentity(SubagentModeContinual, "Explore", 5),
			// kid-w: nil → creation window, omitted.
		},
	}
	query := &fakeQuery{
		headers: []session.SessionHeader{
			listingHeader("kid-b", "root", 2, SubagentOrigin, int64Ptr(5)),
			listingHeader("kid-a", "root", 1, SubagentOrigin, nil),
			listingHeader("kid-w", "root", 3, SubagentOrigin, nil),
			// Cold candidates: absent from the store. "gc" only feeds the
			// has-children set (it belongs to kid-b); kid-c is a root child.
			listingHeader("kid-c", "root", 4, SubagentOrigin, nil),
			listingHeader("kid-x", "root", 5, SubagentOrigin, nil),
			// Cached ancestor descriptor must not outrank the re-fold.
			listingHeader("kid-u", "root", 6, SubagentOrigin, int64Ptr(5)),
			listingHeader("gc", "kid-b", 10, SubagentOrigin, nil),
		},
		observe: map[session.SessionID]*SubagentObservedSession{
			// kid-c is served from the cache rung and never observed.
			"kid-u": {Header: listingHeader("kid-u", "root", 6, SubagentOrigin, int64Ptr(5)),
				Projections: &SubagentProjectionValues{Subagent: listingIdentity(SubagentModeContinual, "Refolded", 7)}},
		},
		errs: map[session.SessionID]error{
			"kid-x": &SubagentQueryError{Code: QueryCodeCorruptSession, Err: errors.New("bad log")},
		},
	}
	cache := &fakeCache{rows: map[session.SessionID]*SubagentIdentityProjection{
		// Own-suffix identity: final, no observation needed.
		"kid-c": listingIdentity(SubagentModeContinual, "Cached", 9),
		// Ancestor replay (seq 1 < seedLength 5): must NOT outrank the re-fold.
		"kid-u": listingIdentity(SubagentModeContinual, "Ancestor", 1),
	}}

	entries, err := ListChildren(context.Background(), listingServices(query, store, projections, cache), "root")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// kid-w (creation window) is omitted; kid-c is served from the cache
	// rung without an observation. Order follows createdAt then id.
	if len(entries) != 5 {
		t.Fatalf("entries = %d (%+v), want 5", len(entries), entries)
	}
	want := []struct {
		id         string
		mode       string
		label      string
		activity   string
		hasChilren bool
	}{
		{"kid-a", SubagentModeOneShot, "", SubagentActivityLive, false},
		{"kid-b", SubagentModeContinual, "Explore", SubagentActivityLive, true},
		{"kid-c", SubagentModeContinual, "Cached", SubagentActivityCold, false},
		{"kid-x", "", "", "", false},
		{"kid-u", SubagentModeContinual, "Refolded", SubagentActivityCold, false},
	}
	for i, entry := range entries {
		if string(entry.ID) != want[i].id {
			t.Fatalf("entry %d = %s, want %s", i, entry.ID, want[i].id)
		}
		w := want[i]
		if w.mode == "" {
			if entry.Kind != ListKindDiagnostic || entry.Reason != SubagentDiagnosticCorrupt {
				t.Fatalf("entry %s = %+v, want corrupt diagnostic", w.id, entry)
			}
			continue
		}
		if entry.Mode != w.mode || entry.Activity != w.activity || entry.HasChildren != w.hasChilren {
			t.Fatalf("entry %s = %+v, want mode %s activity %s hasChildren %v", w.id, entry, w.mode, w.activity, w.hasChilren)
		}
		if (entry.Label == nil) != (w.label == "") || (entry.Label != nil && *entry.Label != w.label) {
			t.Fatalf("entry %s label = %v, want %q", w.id, entry.Label, w.label)
		}
	}
	// An unavailable cold read is retryable, not corrupt.
	query.errs["kid-x"] = errors.New("backend down")
	entries, err = ListChildren(context.Background(), listingServices(query, store, projections, cache), "root")
	if err != nil {
		t.Fatalf("retry list: %v", err)
	}
	for _, entry := range entries {
		if string(entry.ID) == "kid-x" && entry.Reason != SubagentDiagnosticUnavailable {
			t.Fatalf("kid-x = %+v, want unavailable", entry)
		}
	}
}

func TestListDescendantsPreOrderAndDepth(t *testing.T) {
	headerFor := func(id, parent string, createdAt int64, origin string) session.SessionHeader {
		return listingHeader(id, parent, createdAt, origin, nil)
	}
	// root → s1 (subagent) → s2 (subagent); root → o1 (ordinary) → s3.
	query := &fakeQuery{headers: []session.SessionHeader{
		headerFor("s1", "root", 1, SubagentOrigin),
		headerFor("s2", "s1", 2, SubagentOrigin),
		headerFor("o1", "root", 3, ""),
		headerFor("s3", "o1", 4, SubagentOrigin),
		headerFor("root", "", 0, ""),
	}, observe: map[session.SessionID]*SubagentObservedSession{
		"s1": {Header: headerFor("s1", "root", 1, SubagentOrigin),
			Projections: &SubagentProjectionValues{Subagent: listingIdentity(SubagentModeContinual, "one", 1)}},
		"s2": {Header: headerFor("s2", "s1", 2, SubagentOrigin),
			Projections: &SubagentProjectionValues{Subagent: listingIdentity(SubagentModeContinual, "two", 1)}},
		"s3": {Header: headerFor("s3", "o1", 4, SubagentOrigin),
			Projections: &SubagentProjectionValues{Subagent: listingIdentity(SubagentModeContinual, "three", 1)}},
	}}
	store := &fakeSessionStore{sessions: map[session.SessionID]*session.Session{}}
	projections := &fakeProjections{values: map[session.SessionID]*SubagentIdentityProjection{}}

	entries, err := ListDescendants(context.Background(), listingServices(query, store, projections, nil), "root")
	if err != nil {
		t.Fatalf("descendants: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3", len(entries))
	}
	want := []struct {
		id     string
		parent string
		depth  int
	}{
		{"s1", "root", 1},
		{"s2", "s1", 2},
		{"s3", "o1", 2},
	}
	for i, entry := range entries {
		if string(entry.ID) != want[i].id || string(entry.ParentID) != want[i].parent || entry.Depth != want[i].depth {
			t.Fatalf("entry %d = %s parent %s depth %d, want %+v", i, entry.ID, entry.ParentID, entry.Depth, want[i])
		}
	}
}

func TestListingServiceGatesAndCancellation(t *testing.T) {
	query := &fakeQuery{}
	store := &fakeSessionStore{sessions: map[session.SessionID]*session.Session{}}
	projections := &fakeProjections{values: map[session.SessionID]*SubagentIdentityProjection{}}
	// Projections registry missing → deterministic configuration error.
	if _, err := ListChildren(context.Background(), ListChildrenServices{Sessions: store, Query: query}, "root"); err == nil ||
		asCode(err) != CodeControlProjectionsUnavailable {
		t.Fatalf("no projections = %v", err)
	}
	// Session store missing.
	if _, err := ListChildren(context.Background(), ListChildrenServices{Projections: projections, Query: query}, "root"); err == nil ||
		asCode(err) != CodeControlSessionStoreUnavailable {
		t.Fatalf("no sessions = %v", err)
	}
	// Query missing.
	if _, err := ListChildren(context.Background(), ListChildrenServices{Projections: projections, Sessions: store}, "root"); err == nil ||
		asCode(err) != CodeControlQueryUnavailable {
		t.Fatalf("no query = %v", err)
	}
	// Caller cancellation is observed before any read.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ListChildren(ctx, ListChildrenServices{Projections: projections, Sessions: store, Query: query}, "root"); err == nil ||
		asCode(err) != CodeCancelled {
		t.Fatalf("cancelled = %v", err)
	}
	// A lifecycle swap under the same id is corrupt, not a stale serve.
	query2 := &fakeQuery{headers: []session.SessionHeader{
		listingHeader("kid", "root", 7, SubagentOrigin, nil),
	}, observe: map[session.SessionID]*SubagentObservedSession{
		"kid": {Header: listingHeader("kid", "root", 99, SubagentOrigin, nil),
			Projections: &SubagentProjectionValues{Subagent: listingIdentity(SubagentModeContinual, "other-life", 1)}},
	}}
	entries, err := ListChildren(context.Background(), listingServices(query2, store, projections, nil), "root")
	if err != nil {
		t.Fatalf("lifecycle list: %v", err)
	}
	if len(entries) != 1 || entries[0].Reason != SubagentDiagnosticCorrupt {
		t.Fatalf("lifecycle entries = %+v, want corrupt", entries)
	}
}
