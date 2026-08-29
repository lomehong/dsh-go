// Package boot ports packages/boot (app-boot/profile): profile discovery,
// initialization, and patch-layer composition for the dsh --profile launcher
// family. A profile is a directory under `<home>/profiles/<name>` holding a
// package.json manifest (with its ordered `dsh.profile.bundles` list) and a
// `cordis.patch.yml` user patch layer applied after every bundle layer.
//
// Go adaptation: module resolution walks `<anchor>/node_modules/<name>`
// parent chains instead of Node's require.paths, and the pnpm workspace
// bootstrap is absent — the Go runtime mounts plugins from directories, so
// out-of-tree dependency healing is a plain directory lookup.
package boot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"dshgo/cordis/loader"
	"dshgo/homepaths"
)

// ProfilesDir is the directory under the harness home holding every profile.
const ProfilesDir = "profiles"

// ProfilePatchFilename is the user patch layer inside a profile directory.
const ProfilePatchFilename = "cordis.patch.yml"

// ProfilePatchReload is the user patch-file lifecycle selected by a profile.
type ProfilePatchReload string

// PatchReloadLive reloads user patch files while the profile stays active.
const PatchReloadLive ProfilePatchReload = "live"

// PatchReloadStartup reads user patch files once at startup.
const PatchReloadStartup ProfilePatchReload = "startup"

// ProfileTemplate is the installation-owned default used when a shipped
// profile is first opened.
type ProfileTemplate struct {
	// Bundles is the ordered bundle layer list.
	Bundles []string
	// PatchReload is the user patch-file lifecycle for the generated
	// profile.
	PatchReload ProfilePatchReload
}

// ProfileTemplates are the shipped profile templates auto-initialized on
// first use, by name.
var ProfileTemplates = map[string]ProfileTemplate{
	"acp":         {Bundles: []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-acp-app"}, PatchReload: PatchReloadStartup},
	"web":         {Bundles: []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"}, PatchReload: PatchReloadLive},
	"headless":    {Bundles: []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-headless"}, PatchReload: PatchReloadStartup},
	"sdk":         {Bundles: []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-sdk-app"}, PatchReload: PatchReloadStartup},
	"sdk-minimal": {Bundles: []string{"@deepseek-ai/dsh-sdk-minimal"}, PatchReload: PatchReloadStartup},
}

// installationOwnedProfileTuples normalizes to the shipped template when an
// exact retired bundle tuple is still on disk.
var installationOwnedProfileTuples = map[string][]string{
	"headless": {"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app", "@deepseek-ai/dsh-headless"},
}

// DefaultProfileBundles is the bundle list a dsh plugin init uses for a name
// with no shipped template.
var DefaultProfileBundles = []string{"@deepseek-ai/dsh-base"}

// DefaultProfilePatchReload: custom profiles retain the historical live
// patch-file behavior.
const DefaultProfilePatchReload = PatchReloadLive

const profilePatchTemplate = `# Your patch layer for this dsh profile, applied after every bundle layer:
# a top-level YAML array of loader patch entries (id-targeted config
# overrides, disables, and insert lists; ` + "`!!js`" + ` expressions allowed).
[]
`

// ProfileLayer is one resolved bundle layer of a profile.
type ProfileLayer struct {
	// PackageName is the bundle's package name from dsh.profile.bundles.
	PackageName string
	// PackageDir is the absolute directory of the resolved bundle package.
	PackageDir string
	// PatchPath is the absolute path of the bundle's patch file.
	PatchPath string
	// Patches is the parsed patch list.
	Patches []loader.Patch
}

// Profile is a loaded profile: resolved bundle layers plus the user's own
// patch layer.
type Profile struct {
	// Name is the profile name (its directory basename).
	Name string
	// Dir is the absolute profile directory.
	Dir string
	// Layers are the bundle layers in dsh.profile.bundles order.
	Layers []ProfileLayer
	// PatchPath is the absolute path of the profile's own patch file.
	PatchPath string
	// Patches is the profile's own patch list; empty when the file is
	// absent or the user layer was skipped.
	Patches []loader.Patch
	// PatchReload selects the launcher's user patch-file lifecycle.
	PatchReload ProfilePatchReload
}

// ResolveProfileDir resolves a profile's directory under the harness home.
// The name must be a single safe path segment.
func ResolveProfileDir(name, home string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || name == "node_modules" {
		return "", fmt.Errorf("dsh: invalid profile name %q", name)
	}
	return filepath.Join(home, ProfilesDir, name), nil
}

// profileHome resolves the harness home: an empty value follows the
// DSH_HOME-aware default resolution.
func profileHome(home string) string {
	if home == "" {
		return homepaths.ResolveDshHome("", nil)
	}
	return home
}

// ResolveProfileDirDefault resolves against the default harness home.
func ResolveProfileDirDefault(name string) (string, error) {
	return ResolveProfileDir(name, homepaths.ResolveDshHome("", nil))
}

// InitProfile initializes a profile directory: manifest and an empty user
// patch layer. Existing files are never touched, so re-running is a no-op on
// an initialized profile.
func InitProfile(dir string, bundles []string, patchReload ProfilePatchReload) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, "package.json")
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		manifest := map[string]any{
			"name":    "dsh-profile-" + filepath.Base(dir),
			"private": true,
			"dsh": map[string]any{
				"profile": map[string]any{
					"bundles":     append([]string{}, bundles...),
					"patchReload": string(patchReload),
				},
			},
		}
		encoded, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
	}
	patchPath := filepath.Join(dir, ProfilePatchFilename)
	if _, err := os.Stat(patchPath); os.IsNotExist(err) {
		if err := os.WriteFile(patchPath, []byte(profilePatchTemplate), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ReadProfileManifest reads a profile's manifest, preserving every field for
// write-back: the manifest is a raw JSON object whose `dsh` section this
// package reads and rewrites without dropping consumer-owned keys.
func ReadProfileManifest(binName, dir string) (map[string]any, error) {
	path := filepath.Join(dir, "package.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: failed to read profile manifest %s: %v", binName, path, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed == nil {
		return nil, fmt.Errorf("%s: profile manifest %s must hold a JSON object", binName, path)
	}
	return parsed, nil
}

// WriteProfileManifest writes a profile's manifest back (2-space JSON,
// trailing newline).
func WriteProfileManifest(dir string, manifest map[string]any) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "package.json"), append(encoded, '\n'), 0o644)
}

// profileSection reads manifest.dsh.profile with its field checks.
func profileSection(manifest map[string]any) (bundles []string, patchReload string, hasPatchReload bool, err error) {
	dsh, _ := manifest["dsh"].(map[string]any)
	if dsh == nil {
		return nil, "", false, nil
	}
	profile, _ := dsh["profile"].(map[string]any)
	if profile == nil {
		return nil, "", false, nil
	}
	if raw, present := profile["bundles"]; present {
		list, ok := raw.([]any)
		if !ok {
			return nil, "", false, fmt.Errorf("dsh.profile.bundles must be an array of package names")
		}
		for _, item := range list {
			name, ok := item.(string)
			if !ok {
				return nil, "", false, fmt.Errorf("dsh.profile.bundles must be an array of package names")
			}
			bundles = append(bundles, name)
		}
	}
	if raw, present := profile["patchReload"]; present {
		text, ok := raw.(string)
		if !ok || (text != string(PatchReloadLive) && text != string(PatchReloadStartup)) {
			return nil, "", false, fmt.Errorf("dsh.profile.patchReload must be %q or %q", PatchReloadLive, PatchReloadStartup)
		}
		patchReload, hasPatchReload = text, true
	}
	return bundles, patchReload, hasPatchReload, nil
}

// sameBundles reports whether two bundle lists have the same values in the
// same order.
func sameBundles(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// normalizeShippedProfile normalizes an exact installation-owned bundle
// tuple to its shipped template, or adds the shipped reload default to an
// exact current tuple. A changed value is written back during profile
// loading while every other manifest field is preserved; any other bundle
// list is user-owned and remains untouched.
func normalizeShippedProfile(name, dir string, manifest map[string]any) (map[string]any, error) {
	template, known := ProfileTemplates[name]
	retired, hasRetired := installationOwnedProfileTuples[name]
	bundles, patchReload, hasPatchReload, err := profileSection(manifest)
	if err != nil || !known || bundles == nil {
		return manifest, err
	}
	isRetiredTuple := hasRetired && sameBundles(bundles, retired)
	isCurrentTuple := sameBundles(bundles, template.Bundles)
	needsReloadDefault := !hasPatchReload && isCurrentTuple
	if !isRetiredTuple && !needsReloadDefault {
		return manifest, nil
	}
	dsh, _ := manifest["dsh"].(map[string]any)
	if dsh == nil {
		dsh = map[string]any{}
	}
	profile, _ := dsh["profile"].(map[string]any)
	if profile == nil {
		profile = map[string]any{}
	}
	effectiveReload := patchReload
	if !hasPatchReload {
		effectiveReload = string(template.PatchReload)
	}
	anyBundles := make([]any, 0, len(template.Bundles))
	for _, bundle := range template.Bundles {
		anyBundles = append(anyBundles, bundle)
	}
	profile["bundles"] = anyBundles
	profile["patchReload"] = effectiveReload
	dsh["profile"] = profile
	manifest["dsh"] = dsh
	if err := WriteProfileManifest(dir, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

// packageDirFromAnchor resolves one package's root directory from one
// anchor: walk the anchor's parent chain probing `<dir>/node_modules/<name>`
// for a directory holding the named manifest — Node's own nearest-wins
// lookup order.
func packageDirFromAnchor(anchor, packageName string) string {
	dir := filepath.Dir(anchor)
	for {
		candidate := filepath.Join(dir, "node_modules", packageName)
		if _, err := os.Stat(filepath.Join(candidate, "package.json")); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ResolveBundleDir resolves one bundle package's directory: installation
// anchor first, then the profile directory. The installation-first order is
// the contract that in-box bundles always come from the running dsh
// installation, never from a profile-local copy.
func ResolveBundleDir(binName, packageName, installAnchor, profileDir string) (string, error) {
	for _, anchor := range []string{installAnchor, filepath.Join(profileDir, "package.json")} {
		if dir := packageDirFromAnchor(anchor, packageName); dir != "" {
			return dir, nil
		}
	}
	return "", fmt.Errorf(
		"%s: cannot resolve profile bundle %q from the dsh installation or %s; install its dependency first",
		binName, packageName, profileDir,
	)
}

// LoadProfile loads a profile: resolve every dsh.profile.bundles entry to
// its patch layer and parse the profile's own patch file. A listed bundle
// without a dsh.bundle manifest fails loud — naming a bundle-less package as
// a layer is a misconfiguration, not "no patches".
//
// userLayer=false skips reading cordis.patch.yml, so a bundles-only consumer
// cannot fail on a broken user layer.
func LoadProfile(binName, name, installAnchor, home string, userLayer bool) (*Profile, error) {
	home = profileHome(home)
	dir, err := ResolveProfileDir(name, home)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); os.IsNotExist(err) {
		template, known := ProfileTemplates[name]
		if !known {
			return nil, fmt.Errorf("%s: profile %q does not exist; create it with 'dsh plugin --profile %s add <package>'", binName, name, name)
		}
		if err := InitProfile(dir, template.Bundles, template.PatchReload); err != nil {
			return nil, err
		}
	}
	manifest, err := ReadProfileManifest(binName, dir)
	if err != nil {
		return nil, err
	}
	manifest, err = normalizeShippedProfile(name, dir, manifest)
	if err != nil {
		return nil, err
	}
	// A hand-written profile manifest may omit the dsh section entirely.
	bundles, patchReload, hasPatchReload, err := profileSection(manifest)
	if err != nil {
		return nil, fmt.Errorf("%s: profile manifest %s: %v", binName, filepath.Join(dir, "package.json"), err)
	}
	if !hasPatchReload {
		patchReload = string(DefaultProfilePatchReload)
	}
	layers := make([]ProfileLayer, 0, len(bundles))
	for _, packageName := range bundles {
		packageDir, err := ResolveBundleDir(binName, packageName, installAnchor, dir)
		if err != nil {
			return nil, err
		}
		bundleManifest, err := ReadProfileManifest(binName, packageDir)
		if err != nil {
			return nil, err
		}
		dsh, _ := bundleManifest["dsh"].(map[string]any)
		var declared string
		if dsh != nil {
			if bundle, ok := dsh["bundle"].(map[string]any); ok {
				declared, _ = bundle["patch"].(string)
			}
		}
		if declared == "" {
			return nil, fmt.Errorf("%s: profile bundle %q declares no dsh.bundle in its package.json", binName, packageName)
		}
		patchPath := filepath.Join(packageDir, declared)
		patches, err := loader.ParsePatchFile(mustRead(patchPath), filepath.Dir(patchPath))
		if err != nil {
			return nil, err
		}
		layers = append(layers, ProfileLayer{PackageName: packageName, PackageDir: packageDir, PatchPath: patchPath, Patches: patches})
	}
	patchPath := filepath.Join(dir, ProfilePatchFilename)
	var userPatches []loader.Patch
	if userLayer {
		if raw, err := os.ReadFile(patchPath); err == nil {
			userPatches, err = loader.ParsePatchFile(raw, dir)
			if err != nil {
				return nil, err
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return &Profile{
		Name:        name,
		Dir:         dir,
		Layers:      layers,
		PatchPath:   patchPath,
		Patches:     userPatches,
		PatchReload: ProfilePatchReload(patchReload),
	}, nil
}

func mustRead(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}

// ComposeEntries composes patch layers into the effective entry list over an
// empty root — the same single ApplyEntryPatches call the boot include
// makes, so flag derivation and config dumps see exactly what mounts. The
// returned warnings list carries every skipped-patch diagnostic.
func ComposeEntries(layers ...[]loader.Patch) ([]loader.Entry, []string) {
	flat := make([]loader.Patch, 0)
	for _, layer := range layers {
		flat = append(flat, layer...)
	}
	return loader.ApplyEntryPatches(nil, flat)
}
