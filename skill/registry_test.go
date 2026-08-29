package skill

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dshgo/cordis"
	"dshgo/scope"
)

// warnLogger records warnings.
type warnLogger struct {
	mu       sync.Mutex
	warnings []string
}

func (l *warnLogger) Warn(args ...any) {
	l.mu.Lock()
	l.warnings = append(l.warnings, fmt.Sprint(args...))
	l.mu.Unlock()
}
func (l *warnLogger) Error(args ...any) {}
func (l *warnLogger) Info(args ...any)  {}

func (l *warnLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.warnings...)
}

// stubProvider serves a fixed candidate list with an optional failure.
type stubProvider struct {
	name        string
	candidates  []Candidate
	definitions map[string]*Definition
	listErr     error
	complete    bool
	mu          sync.Mutex
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) List(options LookupOptions) (ProviderObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return ProviderObservation{}, p.listErr
	}
	return ProviderObservation{Candidates: p.candidates, Complete: p.complete}, nil
}

func (p *stubProvider) Get(candidate Candidate, options LookupOptions) (*Definition, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.definitions == nil {
		return nil, nil
	}
	return p.definitions[candidate.Name], nil
}

func candidate(name, provider string) Candidate {
	return Candidate{
		Summary: Summary{
			Name:        name,
			Description: "the " + name + " skill",
			Invocation:  InvocationPolicy{ModelInvocable: true, UserInvocable: true},
			Source:      "test",
			Provider:    provider,
		},
		Rank: 500,
	}
}

func newTestRegistry(t *testing.T) (*Registry, *warnLogger) {
	t.Helper()
	logger := &warnLogger{}
	registry, err := NewRegistry(logger, Config{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return registry, logger
}

func ctx() LookupOptions { return LookupOptions{Context: context.Background()} }

func TestIsSkillNameGrammar(t *testing.T) {
	valid := []string{"a", "skill-name", "a-1-b", "skill0"}
	invalid := []string{"", "-lead", "trail-", "Skill", "double--dash", "under_score", "sp ace"}
	for _, name := range valid {
		if !IsSkillName(name) {
			t.Fatalf("valid name %q rejected", name)
		}
	}
	for _, name := range invalid {
		if IsSkillName(name) {
			t.Fatalf("invalid name %q accepted", name)
		}
	}
}

func TestRegisterProviderAndRuntimeBasics(t *testing.T) {
	registry, _ := newTestRegistry(t)
	provider := &stubProvider{name: "local", candidates: []Candidate{candidate("deploy", "local")}, definitions: map[string]*Definition{}}
	definition := Definition{Summary: candidate("deploy", "local").Summary, Content: "how to deploy"}
	provider.definitions["deploy"] = &definition
	detach, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) {
		return provider, nil
	})
	if err != nil {
		t.Fatalf("register provider: %v", err)
	}
	defer detach()

	summaries, err := registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Name != "deploy" {
		t.Fatalf("summaries = %+v", summaries)
	}
	loaded, err := registry.Get("deploy", ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded == nil || loaded.Content != "how to deploy" {
		t.Fatalf("loaded = %+v", loaded)
	}
	// Unknown and invalid names are absent, not errors.
	if missing, err := registry.Get("nope", ViewOptions{LookupOptions: ctx()}); err != nil || missing != nil {
		t.Fatalf("missing = %+v %v", missing, err)
	}
	if missing, err := registry.Get("Bad_Name", ViewOptions{LookupOptions: ctx()}); err != nil || missing != nil {
		t.Fatalf("invalid = %+v %v", missing, err)
	}

	// A runtime registration appears alongside provider skills.
	runtimeDetach, err := registry.RegisterIn(nil, Registration{Name: "alpha", Description: "d", Content: "c"})
	if err != nil {
		t.Fatalf("register runtime: %v", err)
	}
	defer runtimeDetach()
	summaries, err = registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(summaries) != 2 || summaries[0].Name != "alpha" || summaries[1].Name != "deploy" {
		t.Fatalf("summaries = %+v", summaries)
	}
	if summaries[0].Provider != RuntimeProvider {
		t.Fatalf("runtime provider = %q", summaries[0].Provider)
	}
	if summaries[0].Invocation.ModelInvocable != true || summaries[0].Invocation.UserInvocable != true {
		t.Fatalf("default invocation = %+v", summaries[0].Invocation)
	}
}

func TestRegisterProviderReservedAndDuplicateNames(t *testing.T) {
	registry, _ := newTestRegistry(t)
	if _, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) {
		return &stubProvider{name: RuntimeProvider}, nil
	}); err == nil || !strings.Contains(err.Error(), "reserved for runtime skill registrations") {
		t.Fatalf("reserved name accepted: %v", err)
	}
	detach, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) {
		return &stubProvider{name: "local"}, nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	if _, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) {
		return &stubProvider{name: "local"}, nil
	}); err == nil || !strings.Contains(err.Error(), `a skill provider named "local" is already registered`) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestRuntimeDuplicateIsFirstWinsWithNoopDisposer(t *testing.T) {
	registry, logger := newTestRegistry(t)
	first, err := registry.RegisterIn(nil, Registration{Name: "alpha", Description: "first", Content: "one"})
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	second, err := registry.RegisterIn(nil, Registration{Name: "alpha", Description: "second", Content: "two"})
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	second()
	loaded, err := registry.Get("alpha", ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if loaded == nil || loaded.Description != "first" {
		t.Fatalf("winner = %+v", loaded)
	}
	// Disposing the loser's no-op must not remove the winner.
	first()
	if warnings := logger.all(); len(warnings) == 0 || !strings.Contains(warnings[0], `runtime skill "alpha" ignored because it is already registered`) {
		t.Fatalf("warnings = %v", logger.all())
	}
	remaining, err := registry.Get("alpha", ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("get after winner dispose: %v", err)
	}
	_ = remaining
}

func TestProviderFailureIsContainedAndIncompletesObservation(t *testing.T) {
	registry, logger := newTestRegistry(t)
	failing := &stubProvider{name: "broken", listErr: errors.New("auth down")}
	detach, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return failing, nil })
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	good := &stubProvider{name: "local", candidates: []Candidate{candidate("deploy", "local")}}
	detachGood, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return good, nil })
	if err != nil {
		t.Fatalf("register good: %v", err)
	}
	defer detachGood()

	snapshot, err := registry.Snapshot(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Complete {
		t.Fatal("observation cached despite provider failure")
	}
	if len(snapshot.Skills) != 1 || snapshot.Skills[0].Name != "deploy" {
		t.Fatalf("skills = %+v", snapshot.Skills)
	}
	warnings := logger.all()
	if len(warnings) == 0 || !strings.Contains(warnings[0], `skill provider "broken" skipped: auth down`) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestScopeChainNearestLayerWinsAndRankOrdersWithinLayer(t *testing.T) {
	registry, _ := newTestRegistry(t)
	preset := scope.NewScopeKey(nil)
	child := scope.NewScopeKey(preset)

	// Global layer runtime skill + a scoped runtime skill with the same
	// name: the exact scope's entry wins for views through that scope.
	if _, err := registry.RegisterIn(nil, Registration{Name: "deploy", Description: "global", Content: "g"}); err != nil {
		t.Fatalf("register global: %v", err)
	}
	if _, err := registry.RegisterIn(preset, Registration{Name: "deploy", Description: "preset", Content: "p"}); err != nil {
		t.Fatalf("register preset: %v", err)
	}
	globalView, err := registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("global list: %v", err)
	}
	if len(globalView) != 1 || globalView[0].Description != "global" {
		t.Fatalf("global view = %+v", globalView)
	}
	scopedView, err := registry.List(ViewOptions{LookupOptions: ctx(), Scope: child})
	if err != nil {
		t.Fatalf("scoped list: %v", err)
	}
	if len(scopedView) != 1 || scopedView[0].Description != "preset" {
		t.Fatalf("scoped view = %+v", scopedView)
	}

	// Rank orders duplicates within one layer: lower wins.
	lowProvider := &stubProvider{name: "low", candidates: func() []Candidate {
		entry := candidate("shared", "low")
		entry.Rank = 100
		return []Candidate{entry}
	}()}
	detachLow, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return lowProvider, nil })
	if err != nil {
		t.Fatalf("register low: %v", err)
	}
	defer detachLow()
	highProvider := &stubProvider{name: "high", candidates: func() []Candidate {
		entry := candidate("shared", "high")
		entry.Rank = 900
		return []Candidate{entry}
	}()}
	detachHigh, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return highProvider, nil })
	if err != nil {
		t.Fatalf("register high: %v", err)
	}
	defer detachHigh()
	globalView, err = registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("rank list: %v", err)
	}
	var shared *Summary
	for index := range globalView {
		if globalView[index].Name == "shared" {
			shared = &globalView[index]
		}
	}
	if shared == nil || shared.Provider != "low" {
		t.Fatalf("rank winner = %+v", globalView)
	}
	// Duplicate candidates in one layer warn behind the winner.
	registry2, logger := newTestRegistry(t)
	dupProvider := &stubProvider{name: "dup", candidates: []Candidate{candidate("dupe", "dup"), candidate("dupe", "dup")}}
	detachDup, err := registry2.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return dupProvider, nil })
	if err != nil {
		t.Fatalf("register dup: %v", err)
	}
	defer detachDup()
	dupView, err := registry2.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("dup list: %v", err)
	}
	dupes := 0
	for _, entry := range dupView {
		if entry.Name == "dupe" {
			dupes++
		}
	}
	if dupes != 1 {
		t.Fatalf("duplicate candidates survived: %d", dupes)
	}
	if warnings := logger.all(); len(warnings) == 0 || !strings.Contains(warnings[0], `skill "dupe" from test ignored because a higher-priority skill already exists`) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestRegistrationInvalidatesCacheAndNotifiesObservers(t *testing.T) {
	registry, _ := newTestRegistry(t)
	provider := &stubProvider{name: "local", candidates: []Candidate{candidate("deploy", "local")}}
	changes := make(chan int, 4)
	detachObserve := registry.OnChange(func() { changes <- 1 })
	detachProvider, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return provider, nil })
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := registry.List(ViewOptions{LookupOptions: ctx()}); err != nil {
		t.Fatalf("list: %v", err)
	}
	// A provider-driven invalidation must be visible to observers.
	provider.mu.Lock()
	provider.candidates = append(provider.candidates, candidate("rollback", "local"))
	provider.mu.Unlock()
	// Provider-driven invalidation only flows through control.Invalidate;
	// the registry's own mutation hooks fire on registration and disposal.
	detachProvider()
	select {
	case <-changes:
	default:
		t.Fatal("no change notification for registration/disposal")
	}
	detachObserve()

	// Re-registering restores the skill and disposal removes it.
	detachProvider2, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return provider, nil })
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	summaries, err := registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %+v", summaries)
	}
	detachProvider2()
	summaries, err = registry.List(ViewOptions{LookupOptions: ctx()})
	if err != nil {
		t.Fatalf("list 3: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("skills survived disposal: %+v", summaries)
	}
}

func TestProviderControlLifecycle(t *testing.T) {
	registry, _ := newTestRegistry(t)
	var control ProviderControl
	detach, err := registry.RegisterProviderIn(nil, func(received ProviderControl) (Provider, error) {
		control = received
		return &stubProvider{name: "local"}, nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if control.Context == nil || control.Context.Err() != nil {
		t.Fatalf("lifecycle context not live at registration")
	}
	detach()
	if control.Context.Err() == nil {
		t.Fatal("lifecycle context not cancelled at disposal")
	}
	// Registration failure also aborts the lifecycle.
	if _, err := registry.RegisterProviderIn(nil, func(received ProviderControl) (Provider, error) {
		return nil, errors.New("cannot start")
	}); err == nil {
		t.Fatal("failed registration accepted")
	}
}

func TestStaleDefinitionReloadInvalidates(t *testing.T) {
	registry, _ := newTestRegistry(t)
	deploySummary := candidate("deploy", "local").Summary
	definition := Definition{Summary: deploySummary, Content: "v1"}
	provider := &stubProvider{
		name:        "local",
		candidates:  []Candidate{candidate("deploy", "local")},
		definitions: map[string]*Definition{"deploy": &definition},
	}
	detach, err := registry.RegisterProviderIn(nil, func(control ProviderControl) (Provider, error) { return provider, nil })
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	loaded, err := registry.Get("deploy", ViewOptions{LookupOptions: ctx()})
	if err != nil || loaded == nil || loaded.Content != "v1" {
		t.Fatalf("loaded = %+v %v", loaded, err)
	}
	// The provider now hands out the skill under a different name: a stale
	// definition invalidates the entry and the load returns absent.
	provider.mu.Lock()
	renamed := Definition{Summary: Summary{Name: "moved", Description: "the moved skill", Provider: "local", Source: "test", Invocation: InvocationPolicy{ModelInvocable: true, UserInvocable: true}}, Content: "v2"}
	provider.definitions["deploy"] = &renamed
	provider.mu.Unlock()
	loaded, err = registry.Get("deploy", ViewOptions{LookupOptions: ctx()})
	if err != nil || loaded != nil {
		t.Fatalf("stale load = %+v %v", loaded, err)
	}
}

func TestCollectCacheMaxEntriesValidation(t *testing.T) {
	if _, err := NewRegistry(cordis.Discard{}, Config{CollectCacheMaxEntries: -3}); err == nil ||
		!strings.Contains(err.Error(), "skill: collectCacheMaxEntries must be an integer greater than or equal to 1") {
		t.Fatalf("negative cache size accepted: %v", err)
	}
	if _, err := NewRegistry(cordis.Discard{}, Config{CollectCacheMaxEntries: 1}); err != nil {
		t.Fatalf("one-entry cache rejected: %v", err)
	}
}

func TestRenderSkillContentCanonicalShape(t *testing.T) {
	rendered := RenderSkillContent(Definition{
		Summary: Summary{
			Name:     "deploy",
			Provider: "local",
		},
		Content: "Run the deploy script.",
	})
	want := strings.Join([]string{
		`<skill_content name="deploy">`,
		"<skill_resources>",
		`Resources for this skill are managed by provider "local".`,
		"Load referenced resources only as needed.",
		"</skill_resources>",
		"",
		"<skill_instructions>",
		"Run the deploy script.",
		"</skill_instructions>",
		"</skill_content>",
	}, "\n")
	if rendered != want {
		t.Fatalf("rendered =\n%s\nwant\n%s", rendered, want)
	}
	// Directory resource base renders the base-directory hint.
	withBase := RenderSkillContent(Definition{
		Summary: Summary{
			Name:         "deploy",
			Provider:     "local",
			ResourceBase: &ResourceBase{Kind: "directory", Path: "/skills/deploy"},
		},
		Content: "body",
	})
	if !strings.Contains(withBase, "Base directory for this skill: /skills/deploy") {
		t.Fatalf("directory hint missing: %s", withBase)
	}
	// Prose is escaped so it cannot open framing tags.
	escaped := RenderSkillContent(Definition{
		Summary: Summary{
			Name:     "deploy",
			Provider: "a<b&c",
		},
		Content: "body",
	})
	if !strings.Contains(escaped, `provider "a&lt;b&amp;c"`) {
		t.Fatalf("provider not escaped: %s", escaped)
	}
}

func TestCancellationPropagates(t *testing.T) {
	registry, _ := newTestRegistry(t)
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.List(ViewOptions{LookupOptions: ctx()}); err != nil {
		t.Fatalf("un-canceled list failed: %v", err)
	}
	if _, err := registry.List(ViewOptions{LookupOptions: LookupOptions{Context: ctxCanceled}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled list = %v", err)
	}
}
