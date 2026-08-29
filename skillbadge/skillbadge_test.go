package skillbadge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/skill"
)

func TestBundledSkillServesBodyFromAssets(t *testing.T) {
	registry, err := skill.NewRegistry(cordis.Discard{}, skill.Config{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	assetDir := filepath.Join(t.TempDir(), "badge-assets")
	detach, err := RegisterIn(registry, nil, assetDir)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()

	observation, err := provider(t, registry).List(skill.LookupOptions{Context: context.Background()})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(observation.Candidates) != 1 || observation.Candidates[0].Name != SkillName {
		t.Fatalf("candidates = %+v", observation.Candidates)
	}
	candidate := observation.Candidates[0]
	if candidate.Rank != skill.BundledSkillRank || candidate.Source != "bundled" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if !skill.IsModelInvocable(candidate.Summary) || !skill.IsUserInvocable(candidate.Summary) {
		t.Fatalf("invocation = %+v", candidate.Invocation)
	}
	definition, err := provider(t, registry).Get(candidate, skill.LookupOptions{Context: context.Background()})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(definition.Content, "# dsh Badge") ||
		!strings.Contains(definition.Content, "img.shields.io/badge/powered_by-dsh-4D6BFE") {
		t.Fatalf("body = %.200s", definition.Content)
	}
	if definition.ResourceBase.Kind != "directory" || !strings.HasSuffix(definition.ResourceBase.Path, "dsh-badge") && !strings.HasSuffix(definition.ResourceBase.Path, string(os.PathSeparator)) {
		t.Fatalf("resourceBase = %+v", definition.ResourceBase)
	}
	if !filepath.IsAbs(definition.ResourceBase.Path) {
		t.Fatalf("resourceBase not absolute: %q", definition.ResourceBase.Path)
	}
	// The extracted body resolves beside the resource base.
	extracted, err := os.ReadFile(filepath.Join(assetDir, "dsh-badge.md"))
	if err != nil || string(extracted) != definition.Content {
		t.Fatalf("extracted body mismatch: %v", err)
	}
}

func TestRegistrySeesBundledSkill(t *testing.T) {
	registry, err := skill.NewRegistry(cordis.Discard{}, skill.Config{})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	assetDir := filepath.Join(t.TempDir(), "badge-assets")
	detach, err := RegisterIn(registry, nil, assetDir)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()
	definition, err := registry.Get(SkillName, skill.ViewOptions{LookupOptions: skill.LookupOptions{Context: context.Background()}})
	if err != nil || definition == nil {
		t.Fatalf("get: %v", definition)
	}
	if definition.Provider != ProviderName || definition.Source != "bundled" {
		t.Fatalf("definition = %+v", definition.Summary)
	}
	detach()
	definition, err = registry.Get(SkillName, skill.ViewOptions{LookupOptions: skill.LookupOptions{Context: context.Background()}})
	if err != nil || definition != nil {
		t.Fatalf("skill survived disposal: %+v", definition)
	}
}

func provider(t *testing.T, _ *skill.Registry) skill.Provider {
	t.Helper()
	p, err := New(filepath.Join(t.TempDir(), "probe-assets"))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return p
}
