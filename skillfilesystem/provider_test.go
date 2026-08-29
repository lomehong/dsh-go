package skillfilesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/skill"
)

func boolPtr(value bool) *bool { return &value }

func contextOptions() skill.LookupOptions {
	return skill.LookupOptions{Context: context.Background()}
}

// testProvider builds a provider over isolated project/user/custom roots.
type fixture struct {
	root      string
	project   string
	dshHome   string
	agents    string
	custom    string
	bundled   string
	provider  *Provider
	invalidCh chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	f := &fixture{
		root:      root,
		project:   filepath.Join(root, "project"),
		dshHome:   filepath.Join(root, "dsh-home"),
		agents:    filepath.Join(root, "agents-home"),
		custom:    filepath.Join(root, "custom"),
		bundled:   filepath.Join(root, "bundled"),
		invalidCh: make(chan struct{}, 16),
	}
	for _, dir := range []string{f.project, f.dshHome, f.agents, f.custom, f.bundled} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	resolved, err := ResolveConfig(Config{
		DSHHome:             f.dshHome,
		AgentsHome:          f.agents,
		CustomSkillDirs:     []string{f.custom},
		BundledSkillDir:     f.bundled,
		WatchPollIntervalMs: 10,
		Getenv:              func(string) string { return "" },
	})
	if err != nil {
		t.Fatalf("resolve config: %v", err)
	}
	f.provider = New(resolved, func() {
		select {
		case f.invalidCh <- struct{}{}:
		default:
		}
	}, cordis.Discard{})
	t.Cleanup(f.provider.Dispose)
	return f
}

func writeSkill(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const sampleBundle = `---
name: deploy
description: Deploy the service.
whenToUse: When a deployment is requested.
invocation:
  model: true
  user: false
metadata:
  owner: platform
---

Run the deploy script.
`

func namesOf(observation skill.ProviderObservation) []string {
	names := make([]string, 0, len(observation.Candidates))
	for _, candidate := range observation.Candidates {
		names = append(names, candidate.Name)
	}
	return names
}

func TestDiscoveryShapesRanksAndSources(t *testing.T) {
	f := newFixture(t)
	// The nearest enclosing .git marks the project root for nested cwds.
	if err := os.MkdirAll(filepath.Join(f.project, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "deploy", "SKILL.md"), sampleBundle)
	writeSkill(t, filepath.Join(f.project, ".agents", "skills", "agents-flat.md"), "---\nname: agents-flat\ndescription: Flat agent skill.\n---\n\nBody B.")
	writeSkill(t, filepath.Join(f.custom, "custom-skill", "SKILL.md"), "---\nname: custom-skill\ndescription: Custom.\n---\n\nC.")
	writeSkill(t, filepath.Join(f.dshHome, "skills", "user-skill", "SKILL.md"), "---\nname: user-skill\ndescription: User.\n---\n\nU.")
	writeSkill(t, filepath.Join(f.dshHome, "skills", ".system", "internal", "SKILL.md"), "---\nname: internal\ndescription: Hidden.\n---\n\nH.")
	writeSkill(t, filepath.Join(f.agents, "skills", "shared-user.md"), "---\nname: shared-user\ndescription: Shared user.\n---\n\nS.")
	writeSkill(t, filepath.Join(f.bundled, "bundled-skill", "SKILL.md"), "---\nname: bundled-skill\ndescription: Bundled.\n---\n\nB.")

	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: filepath.Join(f.project, "nested")})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !observation.Complete {
		t.Fatal("observation incomplete")
	}
	got := strings.Join(namesOf(observation), ",")
	want := "deploy,agents-flat,custom-skill,user-skill,shared-user,bundled-skill"
	if got != want {
		t.Fatalf("names = %q, want %q", got, want)
	}
	ranks := map[string]float64{}
	sources := map[string]string{}
	for _, candidate := range observation.Candidates {
		ranks[candidate.Name] = candidate.Rank
		sources[candidate.Name] = candidate.Source
	}
	if ranks["deploy"] != ProjectDshRank || ranks["agents-flat"] != ProjectAgentsRank || ranks["custom-skill"] != CustomRank || ranks["user-skill"] != UserDshRank || ranks["shared-user"] != UserAgentsRank || ranks["bundled-skill"] != skill.BundledSkillRank {
		t.Fatalf("ranks = %v", ranks)
	}
	if sources["deploy"] != "project-dsh" || sources["bundled-skill"] != "bundled" {
		t.Fatalf("sources = %v", sources)
	}
	// The bundled skill parses despite host trust: content survived.
	definition, err := f.provider.Get(observation.Candidates[len(observation.Candidates)-1], contextOptions())
	if err != nil || definition == nil || definition.Content != "B." {
		t.Fatalf("bundled get = %+v %v", definition, err)
	}
}

func TestParsedFieldsAndLocators(t *testing.T) {
	f := newFixture(t)
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "deploy", "SKILL.md"), sampleBundle)
	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(observation.Candidates) != 1 {
		t.Fatalf("candidates = %+v", observation.Candidates)
	}
	candidate := observation.Candidates[0]
	if candidate.WhenToUse != "When a deployment is requested." {
		t.Fatalf("whenToUse = %q", candidate.WhenToUse)
	}
	if candidate.Invocation.ModelInvocable != true || candidate.Invocation.UserInvocable != false {
		t.Fatalf("invocation = %+v", candidate.Invocation)
	}
	if candidate.Metadata["owner"] != "platform" {
		t.Fatalf("metadata = %+v", candidate.Metadata)
	}
	definition, err := f.provider.Get(candidate, contextOptions())
	if err != nil || definition == nil {
		t.Fatalf("get: %v", err)
	}
	if definition.Content != "Run the deploy script." {
		t.Fatalf("content = %q", definition.Content)
	}
	if definition.ResourceBase == nil || definition.ResourceBase.Path != filepath.Join(f.project, ".dsh", "skills", "deploy") {
		t.Fatalf("resourceBase = %+v", definition.ResourceBase)
	}
	if definition.Path != filepath.Join(f.project, ".dsh", "skills", "deploy", "SKILL.md") {
		t.Fatalf("path = %q", definition.Path)
	}
	if definition.Source != "project-dsh" || definition.Provider != "filesystem" {
		t.Fatalf("identity = %q/%q", definition.Source, definition.Provider)
	}
}

func TestMalformedFilesAreSkippedWithWarnings(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.project, ".dsh", "skills")
	writeSkill(t, filepath.Join(root, "no-front", "SKILL.md"), "just prose")
	writeSkill(t, filepath.Join(root, "no-desc", "SKILL.md"), "---\nname: no-desc\n---\n\nBody.")
	writeSkill(t, filepath.Join(root, "bad-name", "SKILL.md"), "---\nname: Bad Name\ndescription: d\n---\n\nBody.")
	writeSkill(t, filepath.Join(root, "legacy", "SKILL.md"), "---\nname: legacy\ndescription: d\ndisable-model-invocation: true\n---\n\nBody.")
	writeSkill(t, filepath.Join(root, "bad-yaml", "SKILL.md"), "---\nname: [unclosed\n---\n\nBody.")
	writeSkill(t, filepath.Join(root, "ok.md"), "---\nname: ok\ndescription: d\n---\n\nBody.")

	warnings := &stringLogger{}
	f.provider.logger = warnings
	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := strings.Join(namesOf(observation), ","); got != "ok" {
		t.Fatalf("names = %q", got)
	}
	joined := warnings.join()
	for _, fragment := range []string{
		"ignored: missing YAML frontmatter",
		"ignored: frontmatter requires name and description",
		`invalid skill name "Bad Name"`,
		`unsupported; use "invocation"`,
		"invalid YAML frontmatter",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("warnings missing %q: %v", fragment, joined)
		}
	}
}

// stringLogger collects warnings for assertions.
type stringLogger struct {
	lines []string
}

func (l *stringLogger) Warn(args ...any)  { l.lines = append(l.lines, "\n"+fmtSprint(args...)) }
func (l *stringLogger) Error(args ...any) {}
func (l *stringLogger) Info(args ...any)  {}

func (l *stringLogger) join() string { return strings.Join(l.lines, "") }

func fmtSprint(args ...any) string { return fmt.Sprint(args...) }

func TestWatchInvalidatesOnDriftAndHostMutation(t *testing.T) {
	f := newFixture(t)
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "first", "SKILL.md"), "---\nname: first\ndescription: d\n---\n\nOne.")
	if _, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project}); err != nil {
		t.Fatalf("list: %v", err)
	}
	// Drain the baseline invalidation if any.
	time.Sleep(30 * time.Millisecond)
	for len(f.invalidCh) > 0 {
		<-f.invalidCh
	}
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "second", "SKILL.md"), "---\nname: second\ndescription: d\n---\n\nTwo.")
	select {
	case <-f.invalidCh:
	case <-time.After(2 * time.Second):
		t.Fatal("root drift did not invalidate")
	}

	// A first-party mutation inside a retained root invalidates promptly.
	f.provider.ObserveHostMutation(filepath.Join(f.project, ".dsh", "skills", "first", "SKILL.md"))
	select {
	case <-f.invalidCh:
	case <-time.After(time.Second):
		t.Fatal("host mutation did not invalidate")
	}
	// An unrelated path does not.
	f.provider.ObserveHostMutation(filepath.Join(f.root, "elsewhere", "SKILL.md"))
	select {
	case <-f.invalidCh:
		t.Fatal("unrelated mutation invalidated")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestDisposeStopsWatchers(t *testing.T) {
	f := newFixture(t)
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "first", "SKILL.md"), "---\nname: first\ndescription: d\n---\n\nOne.")
	if _, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project}); err != nil {
		t.Fatalf("list: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	for len(f.invalidCh) > 0 {
		<-f.invalidCh
	}
	f.provider.Dispose()
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "late", "SKILL.md"), "---\nname: late\ndescription: d\n---\n\nL.")
	select {
	case <-f.invalidCh:
		t.Fatal("invalidation after dispose")
	case <-time.After(150 * time.Millisecond):
	}
	// Discovery still works after disposal.
	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project})
	if err != nil {
		t.Fatalf("list after dispose: %v", err)
	}
	if len(observation.Candidates) != 2 {
		t.Fatalf("candidates = %v", namesOf(observation))
	}
}

func TestConfigValidationAndDefaults(t *testing.T) {
	if _, err := ResolveConfig(Config{WatchPollIntervalMs: -1}); err == nil || !strings.Contains(err.Error(), "skill-filesystem: watchPollIntervalMs must be a positive integer") {
		t.Fatalf("poll interval accepted: %v", err)
	}
	if _, err := ResolveConfig(Config{WatchStabilityThresholdMs: -5}); err == nil || !strings.Contains(err.Error(), "skill-filesystem: watchStabilityThresholdMs must be a positive integer") {
		t.Fatalf("stability accepted: %v", err)
	}
	if _, err := ResolveConfig(Config{WatchMaxProjects: 0}); err != nil {
		t.Fatalf("zero max projects rejected: %v", err)
	}
	resolved, err := ResolveConfig(Config{Getenv: func(name string) string {
		if name == "DSH_BUNDLED_SKILL_DIR" {
			return "/bundled/env"
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "filesystem" || !resolved.IncludeDefaultRoots || !resolved.Watch || resolved.WatchMaxProjects != DefaultWatchMaxProjects {
		t.Fatalf("defaults = %+v", resolved)
	}
	if resolved.BundledSkillDir == "" {
		t.Fatal("bundled env root missing")
	}
	// Without default roots the bundled env default is not applied.
	isolated, err := ResolveConfig(Config{IncludeDefaultRoots: boolPtr(false), Getenv: func(name string) string {
		if name == "DSH_BUNDLED_SKILL_DIR" {
			return "/bundled/env"
		}
		return ""
	}})
	if err != nil {
		t.Fatalf("resolve isolated: %v", err)
	}
	if isolated.BundledSkillDir != "" {
		t.Fatalf("isolated provider picked up bundled env: %q", isolated.BundledSkillDir)
	}
}

func TestFlatFileResourceBaseIsRoot(t *testing.T) {
	f := newFixture(t)
	writeSkill(t, filepath.Join(f.custom, "flat.md"), "---\nname: flat\ndescription: d\n---\n\nBody.")
	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background()})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(observation.Candidates) != 1 {
		t.Fatalf("candidates = %+v", observation.Candidates)
	}
	definition, err := f.provider.Get(observation.Candidates[0], contextOptions())
	if err != nil || definition == nil {
		t.Fatalf("get: %v", err)
	}
	if definition.ResourceBase.Path != f.custom {
		t.Fatalf("resourceBase = %+v", definition.ResourceBase)
	}
}

func TestAbsentRootAndMissingBodyAreTolerated(t *testing.T) {
	f := newFixture(t)
	// No roots exist on disk at all: discovery is empty and complete.
	observation, err := f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(observation.Candidates) != 0 || !observation.Complete {
		t.Fatalf("observation = %+v", observation)
	}
	// A directory bundle whose SKILL.md vanished between discovery and load.
	writeSkill(t, filepath.Join(f.project, ".dsh", "skills", "ghost", "SKILL.md"), "---\nname: ghost\ndescription: d\n---\n\nG.")
	observation, err = f.provider.List(skill.LookupOptions{Context: context.Background(), CWD: f.project})
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	candidate := observation.Candidates[0]
	if err := os.Remove(filepath.Join(f.project, ".dsh", "skills", "ghost", "SKILL.md")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	definition, err := f.provider.Get(candidate, contextOptions())
	if err != nil || definition != nil {
		t.Fatalf("ghost get = %+v %v", definition, err)
	}
}
