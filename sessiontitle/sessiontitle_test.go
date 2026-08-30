package sessiontitle

import (
	"errors"
	"strings"
	"testing"

	"dshgo/session"
	"dshgo/sessionquery"
)

func shippedConfig() Config {
	return Config{FallbackMaxWords: 5, FallbackMaxBytes: 40, MaxTitleBytes: 80}
}

func newTitleService(t *testing.T) (*Service, *session.Store) {
	t.Helper()
	store := session.NewStore(nil)
	service, err := NewService(store, shippedConfig())
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return service, store
}

func createSession(t *testing.T, store *session.Store, id string) *session.Session {
	t.Helper()
	sess, err := store.Create(session.SessionID(id), session.CreateOptions{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestRenamePinsTheUserTitle(t *testing.T) {
	service, store := newTitleService(t)
	sess := createSession(t, store, "rename-1")
	accepted, err := service.Rename(sess, "  Hand\tpicked   name  ")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if accepted.Title != "Hand picked name" {
		t.Fatalf("title = %q", accepted.Title)
	}
	if accepted.Source.Kind != sessionquery.TitleSourceUser {
		t.Fatalf("source = %+v", accepted.Source)
	}
	if len(accepted.MessageSeqs) != 0 {
		t.Fatalf("messageSeqs = %v", accepted.MessageSeqs)
	}
	// The durable event carries the exact accepted record; the fold
	// round-trips the user source kind.
	events := sess.Events()
	last := events[len(events)-1]
	if last.Type != EventTitle {
		t.Fatalf("last event = %q", last.Type)
	}
	if folded := service.Get(sess); folded == nil || folded.Title != "Hand picked name" {
		t.Fatalf("fold = %+v", folded)
	}
}

func TestRenameRejectsEmptyTitlesAndDeadSessions(t *testing.T) {
	service, store := newTitleService(t)
	sess := createSession(t, store, "rename-reject")
	_, err := service.Rename(sess, "  \t ")
	var invalid *SessionTitleInvalidError
	if !errors.As(err, &invalid) || invalid.Message != "session title must contain visible characters" {
		t.Fatalf("empty refusal = %v", err)
	}
	// A session the store never entered is dead to the service.
	detached, err := session.NewDetached(session.SessionID("detached"), nil, &session.SessionHeader{ID: session.SessionID("detached")})
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	_, err = service.Rename(detached, "name")
	if err == nil || !strings.Contains(err.Error(), "not live in this store") {
		t.Fatalf("dead refusal = %v", err)
	}
}

func TestRenameEnforcesTheByteBudget(t *testing.T) {
	store := session.NewStore(nil)
	service, err := NewService(store, Config{FallbackMaxWords: 5, FallbackMaxBytes: 3, MaxTitleBytes: 4})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	sess := createSession(t, store, "rename-budget")
	// Each rune costs 3 bytes at 4-byte budget: one fits, the second
	// would split the budget and is dropped.
	accepted, err := service.Rename(sess, "标题题")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if accepted.Title != "标" {
		t.Fatalf("title = %q", accepted.Title)
	}
}

func TestRenameLatestWins(t *testing.T) {
	service, store := newTitleService(t)
	sess := createSession(t, store, "rename-latest")
	if _, err := service.Rename(sess, "First pin"); err != nil {
		t.Fatalf("first rename: %v", err)
	}
	if _, err := service.Rename(sess, "Second pin"); err != nil {
		t.Fatalf("second rename: %v", err)
	}
	if folded := service.Get(sess); folded == nil || folded.Title != "Second pin" {
		t.Fatalf("fold = %+v", folded)
	}
}

func TestNewServiceValidatesTheLimits(t *testing.T) {
	store := session.NewStore(nil)
	cases := []struct {
		name   string
		config Config
		frag   string
	}{
		{"zero words", Config{FallbackMaxWords: 0, FallbackMaxBytes: 40, MaxTitleBytes: 80}, "fallbackMaxWords"},
		{"zero fallback bytes", Config{FallbackMaxWords: 5, FallbackMaxBytes: 0, MaxTitleBytes: 80}, "fallbackMaxBytes"},
		{"zero title bytes", Config{FallbackMaxWords: 5, FallbackMaxBytes: 40, MaxTitleBytes: 0}, "maxTitleBytes"},
		{"fallback over title", Config{FallbackMaxWords: 5, FallbackMaxBytes: 81, MaxTitleBytes: 80}, "must not exceed"},
	}
	for _, testCase := range cases {
		if _, err := NewService(store, testCase.config); err == nil || !strings.Contains(err.Error(), testCase.frag) {
			t.Fatalf("%s: err = %v", testCase.name, err)
		}
	}
	if _, err := NewService(nil, shippedConfig()); err == nil {
		t.Fatal("nil store accepted")
	}
}
