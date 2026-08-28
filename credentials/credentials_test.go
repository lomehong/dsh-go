package credentials

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCredentialRefValidatesPosixIdentifiers(t *testing.T) {
	for _, valid := range []string{"DEEPSEEK_API_KEY", "_X", "a", "A_b_1"} {
		if _, err := CredentialRef(valid); err != nil {
			t.Fatalf("%q must be a valid ref: %v", valid, err)
		}
	}
	for _, invalid := range []string{"1ABC", "HAS-DASH", "", "a.b"} {
		if _, err := CredentialRef(invalid); err == nil {
			t.Fatalf("%q must be rejected", invalid)
		}
	}
	if !IsCredentialRefName("OK_NAME") || IsCredentialRefName("1BAD") {
		t.Fatal("IsCredentialRefName must mirror CredentialRef's grammar")
	}
}

func TestCredentialKeyGrammarAndSplitting(t *testing.T) {
	key, err := CredentialKey("llm-pi-ai", "route-1")
	if err != nil || key != "llm-pi-ai/route-1" {
		t.Fatalf("valid key must brand, got %q %v", key, err)
	}
	if _, err := CredentialKey("LLM", "route"); err == nil {
		t.Fatal("uppercase scope must be rejected")
	}
	if _, err := CredentialKey("scope", "route_x"); err == nil {
		t.Fatal("underscore id must be rejected")
	}
	parsed, err := ParseCredentialKey("llm-pi-ai/route-1")
	if err != nil || parsed != key {
		t.Fatalf("parse must round-trip, got %q %v", parsed, err)
	}
	for _, invalid := range []string{"no-slash", "a/b/c", "a/", "/b"} {
		if _, err := ParseCredentialKey(invalid); err == nil {
			t.Fatalf("%q must be rejected", invalid)
		}
	}
	if scope, id := CredentialKeyScope(key), CredentialKeyID(key); scope != "llm-pi-ai" || id != "route-1" {
		t.Fatalf("split wrong: %q %q", scope, id)
	}
	if !IsCredentialKeySegment("ok-1") || IsCredentialKeySegment("Not_Ok") {
		t.Fatal("IsCredentialKeySegment must mirror the segment grammar")
	}
}

func TestMemoryProviderEmptyValueIsAbsentEverywhere(t *testing.T) {
	provider := NewMemoryProvider(map[string]string{"EMPTY": ""})
	resolved, err := provider.Resolve("EMPTY")
	if err != nil || resolved != nil {
		t.Fatalf("an empty stored value must resolve as absent, got %#v %v", resolved, err)
	}
	info, err := provider.Describe("EMPTY")
	if err != nil || info.Configured {
		t.Fatalf("an empty stored value must describe as unconfigured, got %#v %v", info, err)
	}
}

func TestMemoryProviderReferenceLifecycle(t *testing.T) {
	provider := NewMemoryProvider(nil)
	events := 0
	provider.Notifier().On(func(subject string) error {
		if subject == "DEEPSEEK_API_KEY" {
			events++
		}
		return nil
	})

	if err := provider.Set("DEEPSEEK_API_KEY", "sk-test"); err != nil {
		t.Fatalf("set failed: %v", err)
	}
	if events != 1 {
		t.Fatalf("set must notify after commit, got %d events", events)
	}
	resolved, err := provider.Resolve("DEEPSEEK_API_KEY")
	if err != nil || resolved == nil || resolved.Value != "sk-test" || resolved.Source != "memory" {
		t.Fatalf("resolve wrong: %#v %v", resolved, err)
	}

	if err := provider.Set("DEEPSEEK_API_KEY", ""); err == nil || !strings.Contains(err.Error(), "empty value") {
		t.Fatalf("empty set must be refused, got %v", err)
	}
	if events != 1 {
		t.Fatal("a refused set must not notify")
	}

	_ = provider.Set("OTHER", "x")
	_ = provider.Unset("ABSENT") // no-op, no event
	if events != 1 {
		t.Fatal("unsetting an absent ref must not notify")
	}
	if err := provider.Unset("OTHER"); err != nil {
		t.Fatalf("unset failed: %v", err)
	}
	if resolved, _ := provider.Resolve("OTHER"); resolved != nil {
		t.Fatal("unset must remove the value")
	}
}

func TestMemoryProviderRecordLifecycle(t *testing.T) {
	provider := NewMemoryProvider(nil)
	var subjects []string
	provider.Notifier().On(func(subject string) error {
		subjects = append(subjects, subject)
		return nil
	})

	grant := Record{Kind: KindGrant, Payload: map[string]any{"refresh": "token-1"}}
	written, err := provider.ModifyRecord("llm-pi-ai/route", func(*Record) *Record { return &grant })
	if err != nil || written == nil || written.Kind != KindGrant {
		t.Fatalf("modifyRecord write failed: %#v %v", written, err)
	}
	if len(subjects) != 1 || subjects[0] != "llm-pi-ai/route" {
		t.Fatalf("a record write must notify, got %v", subjects)
	}

	read, ok, err := provider.ReadRecord("llm-pi-ai/route")
	if err != nil || !ok || read.Payload.(map[string]any)["refresh"] != "token-1" {
		t.Fatalf("readRecord must return the payload verbatim: %#v %v", read, err)
	}

	info, err := provider.DescribeRecord("llm-pi-ai/route")
	if err != nil || !info.Configured || info.Kind != KindGrant || !info.Writable {
		t.Fatalf("describeRecord wrong: %#v %v", info, err)
	}

	// Declining mutate leaves the entry untouched and reports the current.
	current, err := provider.ModifyRecord("llm-pi-ai/route", func(cur *Record) *Record { return nil })
	if err != nil || current == nil || current.Payload.(map[string]any)["refresh"] != "token-1" {
		t.Fatalf("declined mutate must report the current record: %#v %v", current, err)
	}
	if len(subjects) != 1 {
		t.Fatal("a declined mutate must not notify")
	}

	if err := provider.DeleteRecord("absent/x"); err != nil {
		t.Fatalf("absent delete failed: %v", err)
	}
	if len(subjects) != 1 {
		t.Fatal("absent delete must not notify")
	}
	if err := provider.DeleteRecord("llm-pi-ai/route"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	if len(subjects) != 2 {
		t.Fatal("record delete must notify")
	}
	if _, ok, _ := provider.ReadRecord("llm-pi-ai/route"); ok {
		t.Fatal("deleted record must be gone")
	}
}

func TestListRecordsExcludesValues(t *testing.T) {
	provider := NewMemoryProvider(nil)
	_, _ = provider.ModifyRecord("a/one", func(*Record) *Record {
		return &Record{Kind: KindAPIKey, Key: "sk-secret"}
	})
	_, _ = provider.ModifyRecord("b/two", func(*Record) *Record {
		return &Record{Kind: KindGrant, Payload: true}
	})
	entries, err := provider.ListRecords()
	if err != nil || len(entries) != 2 {
		t.Fatalf("list failed: %#v %v", entries, err)
	}
	if entries[0].Key != "a/one" || entries[0].Kind != KindAPIKey {
		t.Fatalf("entries must carry address and tag only: %#v", entries)
	}
	for _, entry := range entries {
		if strings.Contains(string(entry.Key), "sk-secret") {
			t.Fatal("a value must never ride in an entry")
		}
	}
}

func TestModifyRecordIsSerialized(t *testing.T) {
	provider := NewMemoryProvider(nil)
	const key = Key("scope/id")
	inside := false
	races := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = provider.ModifyRecord(key, func(*Record) *Record {
				mu.Lock()
				if inside {
					races++
				}
				inside = true
				mu.Unlock()
				mu.Lock()
				inside = false
				mu.Unlock()
				return &Record{Kind: KindGrant, Payload: n}
			})
		}(i)
	}
	wg.Wait()
	if races != 0 {
		t.Fatalf("modifyRecord must serialize, saw %d overlaps", races)
	}
}

func TestNotifierContainsListenerFailuresButRethrowsInvariants(t *testing.T) {
	provider := NewMemoryProvider(nil)
	notifier := provider.Notifier()
	invariant := NewCodedError(InvariantCode, errors.New("recorded state drifted"))
	secondRan := false
	notifier.On(func(string) error { return errors.New("ordinary listener broke") })
	notifier.On(func(string) error { return invariant })
	notifier.On(func(string) error { secondRan = true; return nil })

	// Committed reference writes surface the rethrow only from the
	// notifier's dispatch, after every listener ran.
	if err := provider.Set("K", "v"); err == nil {
		t.Fatal("an INVARIANT-coded failure must rethrow")
	}
	if !secondRan {
		t.Fatal("every listener must run before the invariant rethrow")
	}

	// Ordinary failures are contained: the write still succeeded.
	if resolved, _ := provider.Resolve("K"); resolved == nil {
		t.Fatal("a contained listener failure must not make a committed write look failed")
	}
}
