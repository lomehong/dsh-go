package boot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis/loader"
)

// writeBundle materializes one bundle package under a node_modules tree:
// its manifest (name + dsh.bundle.patch) and a patch file with one override
// row.
func writeBundle(t *testing.T, root, packageName, patchFile string, patchRows string) string {
	t.Helper()
	dir := filepath.Join(root, "node_modules", filepath.FromSlash(packageName))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	manifest := map[string]any{
		"name": packageName,
		"dsh":  map[string]any{"bundle": map[string]any{"patch": patchFile}},
	}
	encoded, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, patchFile), []byte(patchRows), 0o644); err != nil {
		t.Fatalf("bundle patch: %v", err)
	}
	return dir
}

func writeProfile(t *testing.T, home, name, bundles string) string {
	t.Helper()
	dir := filepath.Join(home, ProfilesDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir profile: %v", err)
	}
	manifest := map[string]any{
		"name":    "dsh-profile-" + name,
		"private": true,
		"custom":  "keep-me",
		"dsh":     map[string]any{"profile": map[string]any{"bundles": json.RawMessage(bundles), "patchReload": "startup"}},
	}
	encoded, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), encoded, 0o644); err != nil {
		t.Fatalf("profile manifest: %v", err)
	}
	return dir
}

func TestResolveProfileDirValidatesNames(t *testing.T) {
	home := t.TempDir()
	for _, bad := range []string{"", ".", "..", "node_modules", "a/b", `a\b`} {
		if _, err := ResolveProfileDir(bad, home); err == nil {
			t.Fatalf("name %q was accepted", bad)
		}
	}
	dir, err := ResolveProfileDir("web", home)
	if err != nil || dir != filepath.Join(home, ProfilesDir, "web") {
		t.Fatalf("dir = %q %v", dir, err)
	}
}

func TestInitProfileIsIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "headless")
	if err := InitProfile(dir, []string{"one"}, PatchReloadStartup); err != nil {
		t.Fatalf("init: %v", err)
	}
	manifestPath := filepath.Join(dir, "package.json")
	before, _ := os.ReadFile(manifestPath)
	// Re-running with different arguments must not touch existing files.
	if err := InitProfile(dir, []string{"other"}, PatchReloadLive); err != nil {
		t.Fatalf("reinit: %v", err)
	}
	after, _ := os.ReadFile(manifestPath)
	if string(before) != string(after) {
		t.Fatal("reinit rewrote the manifest")
	}
	var decoded map[string]any
	if err := json.Unmarshal(after, &decoded); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if decoded["name"] != "dsh-profile-headless" || decoded["private"] != true {
		t.Fatalf("manifest = %v", decoded)
	}
	patch, err := os.ReadFile(filepath.Join(dir, ProfilePatchFilename))
	if err != nil || !strings.Contains(string(patch), "patch layer") {
		t.Fatalf("patch template = %q %v", patch, err)
	}
}

func TestLoadProfileAutoInitializesTemplates(t *testing.T) {
	home := t.TempDir()
	install := t.TempDir()
	base := writeBundle(t, install, "@deepseek-ai/dsh-base", "cordis.patch.yml", "- id: loop\n  config:\n    timeout: 5\n")
	app := writeBundle(t, install, "@deepseek-ai/dsh-headless", "cordis.patch.yml", "[]\n")
	_ = app
	profile, err := LoadProfile("dsh", "headless", filepath.Join(install, "package.json"), home, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if profile.Name != "headless" || profile.PatchReload != PatchReloadStartup {
		t.Fatalf("profile = %+v", profile)
	}
	if len(profile.Layers) != 2 || profile.Layers[0].PackageName != "@deepseek-ai/dsh-base" {
		t.Fatalf("layers = %+v", profile.Layers)
	}
	if profile.Layers[0].PackageDir != base {
		t.Fatalf("base dir = %q", profile.Layers[0].PackageDir)
	}
	if len(profile.Layers[0].Patches) != 1 || profile.Layers[0].Patches[0].ID != "loop" {
		t.Fatalf("patches = %+v", profile.Layers[0].Patches)
	}
	// The user layer template exists and parses to no patches.
	if profile.PatchPath != filepath.Join(profile.Dir, ProfilePatchFilename) || len(profile.Patches) != 0 {
		t.Fatalf("user layer = %q %+v", profile.PatchPath, profile.Patches)
	}
}

func TestLoadProfileFailsLoud(t *testing.T) {
	home := t.TempDir()
	install := t.TempDir()
	// Unknown profile without a template.
	if _, err := LoadProfile("dsh", "custom", filepath.Join(install, "package.json"), home, true); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown = %v", err)
	}
	writeProfile(t, home, "broken", `["@deepseek-ai/dsh-missing"]`)
	if _, err := LoadProfile("dsh", "broken", filepath.Join(install, "package.json"), home, true); err == nil ||
		!strings.Contains(err.Error(), "cannot resolve profile bundle") {
		t.Fatalf("missing bundle = %v", err)
	}
	// A listed bundle without dsh.bundle is a misconfiguration, not silence.
	plain := filepath.Join(install, "node_modules", "@deepseek-ai/dsh-plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(plain, "package.json"), []byte(`{"name":"@deepseek-ai/dsh-plain"}`), 0o644); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	writeProfile(t, home, "plain", `["@deepseek-ai/dsh-plain"]`)
	if _, err := LoadProfile("dsh", "plain", filepath.Join(install, "package.json"), home, true); err == nil ||
		!strings.Contains(err.Error(), "declares no dsh.bundle") {
		t.Fatalf("bundle-less = %v", err)
	}
	// An invalid patchReload value fails the manifest check.
	writeProfile(t, home, "reload", `["@deepseek-ai/dsh-plain"]`)
	reloadManifest := filepath.Join(home, ProfilesDir, "reload", "package.json")
	decoded, _ := os.ReadFile(reloadManifest)
	fixed := strings.Replace(string(decoded), `"patchReload":"startup"`, `"patchReload":"sometimes"`, 1)
	if err := os.WriteFile(reloadManifest, []byte(fixed), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadProfile("dsh", "reload", filepath.Join(install, "package.json"), home, true); err == nil ||
		!strings.Contains(err.Error(), "patchReload") {
		t.Fatalf("reload = %v", err)
	}
}

func TestLoadProfileResolvesProfileLocalBundles(t *testing.T) {
	home := t.TempDir()
	install := t.TempDir()
	writeBundle(t, install, "@deepseek-ai/dsh-base", "cordis.patch.yml", "[]\n")
	profileDir := writeProfile(t, home, "local", `["@deepseek-ai/dsh-local"]`)
	// A bundle carried only by the profile resolves from the second anchor.
	writeBundle(t, profileDir, "@deepseek-ai/dsh-local", "cordis.patch.yml", "[]\n")
	profile, err := LoadProfile("dsh", "local", filepath.Join(install, "package.json"), home, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if profile.Layers[0].PackageDir != filepath.Join(profileDir, "node_modules", "@deepseek-ai/dsh-local") {
		t.Fatalf("dir = %q", profile.Layers[0].PackageDir)
	}
}

func TestInstallationOwnedTupleNormalizes(t *testing.T) {
	home := t.TempDir()
	install := t.TempDir()
	writeBundle(t, install, "@deepseek-ai/dsh-base", "cordis.patch.yml", "[]\n")
	writeBundle(t, install, "@deepseek-ai/dsh-web-app", "cordis.patch.yml", "[]\n")
	writeBundle(t, install, "@deepseek-ai/dsh-headless", "cordis.patch.yml", "[]\n")
	// The retired headless tuple normalizes to the shipped template.
	writeProfile(t, home, "headless", `["@deepseek-ai/dsh-base","@deepseek-ai/dsh-web-app","@deepseek-ai/dsh-headless"]`)
	profile, err := LoadProfile("dsh", "headless", filepath.Join(install, "package.json"), home, false)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(profile.Layers) != 2 {
		t.Fatalf("layers = %+v", profile.Layers)
	}
	manifest, err := os.ReadFile(filepath.Join(profile.Dir, "package.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(manifest), "dsh-web-app") {
		t.Fatal("retired tuple survived normalization")
	}
	// Every other manifest field is preserved across write-back.
	var decoded map[string]any
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if decoded["custom"] != "keep-me" {
		t.Fatalf("custom field lost: %v", decoded)
	}
}

func TestUserLayerSkipAndParse(t *testing.T) {
	home := t.TempDir()
	install := t.TempDir()
	writeBundle(t, install, "@deepseek-ai/dsh-base", "cordis.patch.yml", "[]\n")
	profileDir := writeProfile(t, home, "web", `["@deepseek-ai/dsh-base"]`)
	// A broken user layer fails a normal load...
	if err := os.WriteFile(filepath.Join(profileDir, ProfilePatchFilename), []byte("- [unclosed"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadProfile("dsh", "web", filepath.Join(install, "package.json"), home, true); err == nil {
		t.Fatal("broken user layer was accepted")
	}
	// ...but a bundles-only consumer cannot fail on it.
	profile, err := LoadProfile("dsh", "web", filepath.Join(install, "package.json"), home, false)
	if err != nil {
		t.Fatalf("userLayer=false load: %v", err)
	}
	if len(profile.Patches) != 0 {
		t.Fatalf("patches = %+v", profile.Patches)
	}
	// A working user layer parses with relative-insert anchoring.
	if err := os.WriteFile(filepath.Join(profileDir, ProfilePatchFilename), []byte("- id: loop\n  disabled: true\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	profile, err = LoadProfile("dsh", "web", filepath.Join(install, "package.json"), home, true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(profile.Patches) != 1 || profile.Patches[0].ID != "loop" {
		t.Fatalf("patches = %+v", profile.Patches)
	}
}

func TestComposeEntriesAppliesLayersInOrder(t *testing.T) {
	base := parse(t, `
- id: loop
  name: dsh/loop
  config:
    timeout: 30
`)
	userLayer := parsePatches(t, `
- id: loop
  config:
    timeout: 5
- insert:
  - id: webhook
    name: dsh/webhook
`)
	secondLayer := parsePatches(t, `
- id: webhook
  config:
    port: 8080
`)
	entries, warnings := ComposeEntries(base, userLayer, secondLayer)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	loop := entries[0]
	if loop.ID != "loop" || loop.Config.(map[string]any)["timeout"] != int64(5) {
		t.Fatalf("loop = %+v", loop)
	}
	webhook := entries[1]
	if webhook.ID != "webhook" || webhook.Config.(map[string]any)["port"] != int64(8080) {
		t.Fatalf("webhook = %+v", webhook)
	}
	// Composing over an empty root with no patches yields no entries.
	empty, warnings := ComposeEntries()
	if len(empty) != 0 || len(warnings) != 0 {
		t.Fatalf("empty = %+v %v", empty, warnings)
	}
}

func parse(t *testing.T, text string) []loader.Patch {
	t.Helper()
	// Wrap the entry list as an insert-only patch layer: ComposeEntries
	// composes patch lists, and boot's root list is empty.
	return parsePatches(t, "- insert:\n"+indent(text))
}

func parsePatches(t *testing.T, text string) []loader.Patch {
	t.Helper()
	patches, err := loader.ParsePatchFile([]byte(text), ".")
	if err != nil {
		t.Fatalf("parse patches: %v", err)
	}
	return patches
}

func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for index, line := range lines {
		lines[index] = "  " + line
	}
	return strings.Join(lines, "\n") + "\n"
}
