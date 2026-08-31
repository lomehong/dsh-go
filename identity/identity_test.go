package identity

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// resetMemo clears the process-lifetime memo between tests (distinct file
// paths never share ids, but the same temp path could recur).
func resetMemo() {
	memo.Range(func(key, _ any) bool {
		memo.Delete(key)
		return true
	})
}

func TestGetOrCreatePersistsAndSticks(t *testing.T) {
	resetMemo()
	home := t.TempDir()
	getenv := func(key string) string {
		if key == "DSH_HOME" {
			return home
		}
		return ""
	}
	first := GetOrCreateAnonymousUserID(Options{Getenv: getenv})
	if !uuidPattern.MatchString(first) {
		t.Fatalf("id = %q, want a UUID", first)
	}
	raw, err := os.ReadFile(filepath.Join(home, AnonymousUserIDFileName))
	if err != nil {
		t.Fatalf("read persisted: %v", err)
	}
	if string(raw) != first+"\n" {
		t.Fatalf("file = %q, want %q", raw, first+"\n")
	}
	// Memoized: the same path answers from memory.
	if second := GetOrCreateAnonymousUserID(Options{Getenv: getenv}); second != first {
		t.Fatal("the second call minted a new id")
	}
}

func TestGetOrCreateAdoptsPersistedID(t *testing.T) {
	resetMemo()
	home := t.TempDir()
	persisted := "01234567-89ab-cdef-0123-456789abcdef"
	if err := os.WriteFile(filepath.Join(home, AnonymousUserIDFileName), []byte(persisted+"\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := GetOrCreateAnonymousUserID(Options{Getenv: func(key string) string {
		if key == "DSH_HOME" {
			return home
		}
		return ""
	}})
	if id != persisted {
		t.Fatalf("id = %q, want the persisted %q", id, persisted)
	}
}

func TestGetOrCreateRejectsCorruptAndOverwrites(t *testing.T) {
	resetMemo()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, AnonymousUserIDFileName), []byte("not-a-uuid\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	generated := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	id := GetOrCreateAnonymousUserID(Options{
		Getenv: func(key string) string {
			if key == "DSH_HOME" {
				return home
			}
			return ""
		},
		RandomUUID: func() string { return generated },
	})
	if id != generated {
		t.Fatalf("id = %q, want the corrupt file to be replaced by %q", id, generated)
	}
}

func TestGetOrCreateSurvivesUnwritableHome(t *testing.T) {
	resetMemo()
	// A file occupying the home path makes every write fail while resolution
	// still succeeds.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	generated := "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	first := GetOrCreateAnonymousUserID(Options{
		Getenv: func(key string) string {
			if key == "DSH_HOME" {
				return blocker
			}
			return ""
		},
		RandomUUID: func() string { return generated },
	})
	if first != generated {
		t.Fatalf("id = %q, want the in-memory %q despite the unwritable home", first, generated)
	}
}

func TestGetOrCreateConcurrentFirstLaunchConverges(t *testing.T) {
	resetMemo()
	home := t.TempDir()
	getenv := func(key string) string {
		if key == "DSH_HOME" {
			return home
		}
		return ""
	}
	// A pre-persisted id plays the concurrent winner: the exclusive create
	// refuses and the reread adopts it.
	persisted := "99999999-9999-9999-9999-999999999999"
	var once sync.Once
	var mu sync.Mutex
	var wg sync.WaitGroup
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			once.Do(func() {
				_ = os.WriteFile(filepath.Join(home, AnonymousUserIDFileName), []byte(persisted+"\n"), 0o644)
			})
			id := GetOrCreateAnonymousUserID(Options{Getenv: getenv})
			mu.Lock()
			ids = append(ids, id)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != persisted {
			t.Fatalf("id = %q, want every racer to converge on %q", id, persisted)
		}
	}
}

func TestIDsDifferAcrossHomes(t *testing.T) {
	resetMemo()
	first := GetOrCreateAnonymousUserID(Options{Getenv: func(string) string { return t.TempDir() }})
	second := GetOrCreateAnonymousUserID(Options{Getenv: func(string) string { return t.TempDir() }})
	if first == second {
		t.Fatal("distinct harness homes shared one id")
	}
}

func TestRandomUUIDShape(t *testing.T) {
	id := RandomUUID()
	if !strings.Contains(id, "-4") && !uuidPattern.MatchString(id) {
		t.Fatalf("uuid = %q", id)
	}
	if !uuidPattern.MatchString(id) {
		t.Fatalf("uuid = %q, want v4 shape", id)
	}
	// Version and variant nibbles.
	if id[14] != '4' || id[19] != '8' && id[19] != '9' && id[19] != 'a' && id[19] != 'b' {
		t.Fatalf("uuid = %q, want v4 version/variant", id)
	}
}
