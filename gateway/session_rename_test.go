package gateway

import (
	"context"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/sessiontitle"
)

// renameFixture wires the create-plane fake factory (live agents) plus a
// session-title service over the same store, matching the composed wiring.
type renameFixture struct {
	*createFakeFactory
	store    *session.Store
	titles   *sessiontitle.Service
	controller *SessionController
}

func newRenameFixture(t *testing.T) *renameFixture {
	t.Helper()
	controller, factory, store := newCreateController(t)
	titles, err := sessiontitle.NewService(store, sessiontitle.Config{
		FallbackMaxWords: 6, FallbackMaxBytes: 40, MaxTitleBytes: 40,
	}, cordis.Discard{})
	if err != nil {
		t.Fatalf("session title service: %v", err)
	}
	t.Cleanup(titles.Dispose)
	controller.EnableCreate(SessionCreateDeps{
		Agents:   func() any { return factory.registry },
		Sessions: func() any { return store },
		Titles:   func() any { return titles },
	})
	return &renameFixture{createFakeFactory: factory, store: store, titles: titles, controller: controller}
}

// createLive creates one live agent-backed session and returns its id.
func (f *renameFixture) createLive(t *testing.T) string {
	t.Helper()
	value, err := f.controller.Create(context.Background(), map[string]any{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return value.(map[string]any)["sessionId"].(string)
}

func TestRenamePinsAnExplicitUserTitle(t *testing.T) {
	f := newRenameFixture(t)
	sessionID := f.createLive(t)
	value, err := f.controller.Rename(context.Background(), map[string]any{
		"sessionId": sessionID,
		"title":     "  My Project  ",
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	row := value.(map[string]any)
	if row["title"] != "My Project" {
		t.Fatalf("title = %v, want the normalized text", row["title"])
	}
	if seq, ok := row["seq"].(int64); !ok || seq < 0 {
		t.Fatalf("seq = %v (%T), want a non-negative event seq", row["seq"], row["seq"])
	}
}

func TestRenameRefusesEmptyNormalizedTitles(t *testing.T) {
	f := newRenameFixture(t)
	sessionID := f.createLive(t)
	_, err := f.controller.Rename(context.Background(), map[string]any{
		"sessionId": sessionID,
		"title":     "   ",
	})
	if gerr := asGatewayError(t, err); gerr == nil || gerr.Code != "session/title-invalid" {
		t.Fatalf("want the session/title-invalid code, got %v", err)
	}
}

func TestRenameRefusesUnknownSessions(t *testing.T) {
	f := newRenameFixture(t)
	_, err := f.controller.Rename(context.Background(), map[string]any{
		"sessionId": "session-missing",
		"title":     "hello",
	})
	if gerr := asGatewayError(t, err); gerr == nil || gerr.Code != "session/not-found" {
		t.Fatalf("want the session/not-found code, got %v", err)
	}
}

// asGatewayError unwraps err into the gateway error, or fails the test when
// err is not one.
func asGatewayError(t *testing.T, err error) *GatewayError {
	t.Helper()
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if gerr, ok := err.(*GatewayError); ok {
		return gerr
	}
	t.Fatalf("want a gateway error, got %T: %v", err, err)
	return nil
}

func TestRenameAnswersNotComposedWithoutTitles(t *testing.T) {
	controller, factory, store := newCreateController(t)
	controller.EnableCreate(SessionCreateDeps{
		Agents:   func() any { return factory.registry },
		Sessions: func() any { return store },
	})
	value, err := controller.Create(context.Background(), map[string]any{"cwd": t.TempDir()})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sessionID := value.(map[string]any)["sessionId"].(string)
	_, err = controller.Rename(context.Background(), map[string]any{
		"sessionId": sessionID,
		"title":     "hello",
	})
	if err == nil || !strings.Contains(err.Error(), "no session-title service") {
		t.Fatalf("want the no-title-service refusal, got %v", err)
	}
}
