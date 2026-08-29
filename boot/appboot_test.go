package boot

import (
	"os"
	"path/filepath"
	"testing"

	"dshgo/cordis"
)

// fixtureBundle writes one bundle package (manifest declaring its patch file
// plus the patch itself inserting the two cheap runtime rows) under an
// anchor's node_modules, the way an installed bundle package looks.
func fixtureBundle(t *testing.T, anchor string) {
	t.Helper()
	packageDir := filepath.Join(anchor, "node_modules", "@test-fixture", "base-bundle")
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	manifest := `{"name":"@test-fixture/base-bundle","private":true,` +
		`"dsh":{"bundle":{"patch":"cordis.patch.yml"}}}`
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	patch := `- insert:
    - id: tools
      name: '@deepseek-ai/dsh-tools'
    - id: commands
      name: '@deepseek-ai/dsh-commands'
`
	if err := os.WriteFile(filepath.Join(packageDir, "cordis.patch.yml"), []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
}

func TestAssembleProfileComposesAndMounts(t *testing.T) {
	home := t.TempDir()
	anchor := t.TempDir()
	fixtureBundle(t, anchor)

	profileDir, err := ResolveProfileDir("testprofile", home)
	if err != nil {
		t.Fatalf("resolve profile dir: %v", err)
	}
	if err := InitProfile(profileDir, []string{"@test-fixture/base-bundle"}, DefaultProfilePatchReload); err != nil {
		t.Fatalf("init profile: %v", err)
	}

	app, warnings, err := AssembleProfile("dsh-test", "testprofile", filepath.Join(anchor, "package.json"), home, CatalogDeps{
		Logger: cordis.Discard{},
		Home:   home,
	})
	if err != nil {
		t.Fatalf("assemble profile: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	root := app.Root()
	if root.Get(ServiceTools) == nil {
		t.Fatal("tools service missing")
	}
	if root.Get(ServiceCommands) == nil {
		t.Fatal("commands service missing")
	}
	if err := app.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestAssembleProfileFailsLoudOnUnknownBundle(t *testing.T) {
	home := t.TempDir()
	profileDir, err := ResolveProfileDir("broken", home)
	if err != nil {
		t.Fatalf("resolve profile dir: %v", err)
	}
	if err := InitProfile(profileDir, []string{"@test-fixture/absent-bundle"}, DefaultProfilePatchReload); err != nil {
		t.Fatalf("init profile: %v", err)
	}

	_, _, err = AssembleProfile("dsh-test", "broken", t.TempDir(), home, CatalogDeps{
		Logger: cordis.Discard{},
		Home:   home,
	})
	if err == nil {
		t.Fatal("absent bundle must fail loud")
	}
}
