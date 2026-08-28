package settings

import (
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
)

func newTestStore(t *testing.T) (*Store, *Scope) {
	t.Helper()
	st := NewStore(cordis.Discard{})
	scope, err := st.Register("test-ns", &Schema{
		Defaults: func() map[string]any {
			return map[string]any{"retries": 3, "nested": map[string]any{"enabled": true}}
		},
	}, map[string]any{"retries": 5})
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	return st, scope
}

func TestResolutionLayersDefaultsBaseUser(t *testing.T) {
	_, scope := newTestStore(t)
	got := scope.Get()
	if got["retries"] != 5 {
		t.Fatalf("base must override defaults, got %#v", got["retries"])
	}
	nested := got["nested"].(map[string]any)
	if nested["enabled"] != true {
		t.Fatalf("defaults must reach untouched keys, got %#v", got)
	}
	if err := scope.Update(map[string]any{"retries": 9}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if got := scope.Get(); got["retries"] != 9 {
		t.Fatalf("user layer must win, got %#v", got["retries"])
	}
}

func TestUpdateTouchesUserLayerOnly(t *testing.T) {
	st, scope := newTestStore(t)
	if err := scope.Update(map[string]any{"retries": 9}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	user := st.Section("test-ns")
	if user["retries"] != 9 {
		t.Fatalf("patch must land in the user layer, got %#v", user)
	}
	if _, has := user["nested"]; has {
		t.Fatal("sparse patch must not copy unrelated keys into the user layer")
	}
	if st.register["test-ns"].base["retries"] != 5 {
		t.Fatal("update must never write into the composition base")
	}
}

func TestReplaceReinheritsAbsentKeys(t *testing.T) {
	_, scope := newTestStore(t)
	if err := scope.Update(map[string]any{"retries": 9, "only": "mine"}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if err := scope.Replace(map[string]any{"only": "mine"}); err != nil {
		t.Fatalf("replace failed: %v", err)
	}
	got := scope.Get()
	if got["retries"] != 5 {
		t.Fatalf("keys absent from the replacement must re-inherit base, got %#v", got["retries"])
	}
	if got["only"] != "mine" {
		t.Fatalf("replacement keys must survive, got %#v", got)
	}
}

func TestMutateStructuredOpsAndRevisionConflict(t *testing.T) {
	_, scope := newTestStore(t)
	if err := scope.Update(map[string]any{"retries": 7}); err != nil {
		t.Fatalf("seed write failed: %v", err)
	}
	stale := scope.ExpectedRevision() - 1

	err := scope.Mutate([]PathOp{{Op: "set", Path: []string{"nested", "enabled"}, Value: false}}, &stale)
	if err == nil || !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("stale revision must be refused, got %v", err)
	}

	rev := scope.ExpectedRevision()
	if err := scope.Mutate([]PathOp{
		{Op: "set", Path: []string{"nested", "enabled"}, Value: false},
		{Op: "set", Path: []string{"retries"}, Value: 1},
	}, &rev); err != nil {
		t.Fatalf("mutate failed: %v", err)
	}
	got := scope.Get()
	if got["retries"] != 1 {
		t.Fatalf("set must land at the path, got %#v", got["retries"])
	}
	if got["nested"].(map[string]any)["enabled"] != false {
		t.Fatalf("deep set must land, got %#v", got["nested"])
	}

	rev = scope.ExpectedRevision()
	if err := scope.Mutate([]PathOp{{Op: "unset", Path: []string{"retries"}}}, &rev); err != nil {
		t.Fatalf("unset failed: %v", err)
	}
	if got := scope.Get(); got["retries"] != 5 {
		t.Fatalf("unset must re-inherit through base, got %#v", got["retries"])
	}
}

func TestValidateRefusesTheWholeWrite(t *testing.T) {
	st := NewStore(cordis.Discard{})
	rejected := errors.New("no route would serve this profile")
	scope, err := st.Register("adapter", &Schema{
		Validate: func(value map[string]any) error {
			if value["provider"] == "bogus" {
				return rejected
			}
			return nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}

	rev := scope.ExpectedRevision()
	if err := scope.Update(map[string]any{"provider": "bogus"}); !errors.Is(err, rejected) {
		t.Fatalf("validate must refuse the write, got %v", err)
	}
	if scope.ExpectedRevision() != rev {
		t.Fatal("a refused write must not bump the revision")
	}
	if st.Section("adapter") != nil {
		t.Fatal("a refused write must not land in the user layer")
	}
}

func TestDeepEqualWritesAreNotEmitted(t *testing.T) {
	st, scope := newTestStore(t)
	events := 0
	dispose := st.OnUpdated(func(*UpdateEvent) { events++ })
	defer dispose()

	if err := scope.Update(map[string]any{"retries": 5}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if events != 0 {
		t.Fatalf("a deep-equal resolved value must not emit, got %d events", events)
	}
	if err := scope.Update(map[string]any{"retries": 7}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if events != 1 {
		t.Fatalf("a real change must emit, got %d events", events)
	}
}

func TestUpdateEventCarriesNextPrevSourceAndWatcherIsDisposable(t *testing.T) {
	st, scope := newTestStore(t)
	var seen []*UpdateEvent
	dispose := st.OnUpdated(func(e *UpdateEvent) { seen = append(seen, e) })

	if err := scope.Update(map[string]any{"retries": 9}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("want one event, got %d", len(seen))
	}
	event := seen[0]
	if event.Namespace != "test-ns" || event.Source != SourceUpdate {
		t.Fatalf("event metadata wrong: %+v", event)
	}
	if event.Prev["retries"] != 5 || event.Next["retries"] != 9 {
		t.Fatalf("event must carry resolved prev/next, got %+v", event)
	}

	dispose()
	_ = scope.Update(map[string]any{"retries": 10})
	if len(seen) != 1 {
		t.Fatal("disposed watcher must stop receiving events")
	}
}

func TestPanickingWatcherIsContained(t *testing.T) {
	st, scope := newTestStore(t)
	good := 0
	st.OnUpdated(func(*UpdateEvent) { panic("watcher exploded") })
	st.OnUpdated(func(*UpdateEvent) { good++ })
	if err := scope.Update(map[string]any{"retries": 9}); err != nil {
		t.Fatalf("a panicking watcher must never fail the write, got %v", err)
	}
	if good != 1 {
		t.Fatalf("remaining watchers must still run, got %d", good)
	}
}

func TestNamespaceProbeAndDuplicateRegistration(t *testing.T) {
	st, _ := newTestStore(t)
	if !st.HasNamespace("test-ns") || st.HasNamespace("other") {
		t.Fatal("HasNamespace must mirror the registration probe")
	}
	if _, err := st.Register("test-ns", nil, nil); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate registration must fail loudly, got %v", err)
	}
}

func TestProviderPushRejectsBadExternalSectionsAndKeepsLastGoodValue(t *testing.T) {
	st := NewStore(cordis.Discard{})
	scope, err := st.Register("adapter", &Schema{
		Validate: func(value map[string]any) error {
			if value["provider"] == "bogus" {
				return errors.New("invalid")
			}
			return nil
		},
	}, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if err := scope.Update(map[string]any{"provider": "good"}); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	if err := st.ProviderPush("adapter", map[string]any{"provider": "bogus"}); err == nil {
		t.Fatal("an invalid external section must be refused")
	}
	if got := scope.Get()["provider"]; got != "good" {
		t.Fatalf("a rejected external section must keep the last good value, got %#v", got)
	}

	events := 0
	st.OnUpdated(func(e *UpdateEvent) {
		if e.Source == SourceProvider {
			events++
		}
	})
	if err := st.ProviderPush("adapter", map[string]any{"provider": "external"}); err != nil {
		t.Fatalf("valid external section must land, got %v", err)
	}
	if events != 1 || scope.Get()["provider"] != "external" {
		t.Fatalf("valid external edits must push through as provider source, events=%d", events)
	}
}
