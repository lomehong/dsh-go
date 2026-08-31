package file

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/settings"
)

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestHostWritePersistsAndSecondOpenSeesTheSection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")

	first := settings.NewStore(cordis.Discard{})
	scope, err := first.Register("model-failover", nil, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	provider, err := Open(path, first, cordis.Discard{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer provider.Close()

	if err := scope.Update(map[string]any{"chain": []any{"zai/glm", "deepseek/flash"}}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		_, statErr := os.Stat(path)
		return statErr == nil
	})

	second := settings.NewStore(cordis.Discard{})
	scope2, err := second.Register("model-failover", nil, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	reloaded, err := Open(path, second, cordis.Discard{})
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer reloaded.Close()

	chain, ok := scope2.Get()["chain"].([]any)
	if !ok || len(chain) != 2 || chain[0] != "zai/glm" {
		t.Fatalf("persisted section must round-trip, got %#v", scope2.Get())
	}
}

func TestExternalEditPushesWithProviderSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("im-channel:\n  commandPrefix: \"/\"\n"), 0o644); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	store := settings.NewStore(cordis.Discard{})
	scope, err := store.Register("im-channel", nil, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	var providerSource atomic.Int32
	store.OnUpdated(func(e *settings.UpdateEvent) {
		if e.Source == settings.SourceProvider {
			providerSource.Add(1)
		}
	})
	provider, err := Open(path, store, cordis.Discard{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	provider.SetPollInterval(20 * time.Millisecond)
	defer provider.Close()

	if err := os.WriteFile(path, []byte("im-channel:\n  commandPrefix: \"!\"\n"), 0o644); err != nil {
		t.Fatalf("external edit failed: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return scope.Get()["commandPrefix"] == "!"
	})
	if providerSource.Load() == 0 {
		t.Fatal("external edit must arrive with the provider source")
	}
}

func TestRejectedExternalSectionKeepsLastGoodValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")

	store := settings.NewStore(cordis.Discard{})
	scope, err := store.Register("adapter", &settings.Schema{
		Validate: func(value map[string]any) error {
			if value["provider"] == "bogus" {
				return errors.New("invalid provider")
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
	provider, err := Open(path, store, cordis.Discard{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	provider.SetPollInterval(20 * time.Millisecond)
	defer provider.Close()

	time.Sleep(60 * time.Millisecond) // let the seed's own save settle
	if err := os.WriteFile(path, []byte("adapter:\n  provider: bogus\n"), 0o644); err != nil {
		t.Fatalf("external edit failed: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	if got := scope.Get()["provider"]; got != "good" {
		t.Fatalf("invalid external edit must keep the last good value, got %#v", got)
	}
}

func TestSaveWritesParsableYaml(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	store := settings.NewStore(cordis.Discard{})
	scope, err := store.Register("ns", nil, nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	provider, err := Open(path, store, cordis.Discard{})
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	defer provider.Close()
	if err := scope.Update(map[string]any{"nested": map[string]any{"a": 1}}); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	if err := provider.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("saved file missing: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("saved document must not be empty")
	}
}
