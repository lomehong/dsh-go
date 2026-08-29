package preset

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fixtures --------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// compositionRow is one minimal plugin row a shape check accepts.
func compositionRow(id string) string {
	return "- name: cordis:tools\n  id: " + id + "\n"
}

// --- metadata --------------------------------------------------------------

func TestReadPresetMetadata(t *testing.T) {
	dir := t.TempDir()
	if got := ReadPresetMetadata(dir); got.Name != nil || got.Description != nil || got.Order != nil {
		t.Fatalf("absent metadata = %+v", got)
	}
	writeFile(t, filepath.Join(dir, MetadataFile), "not: [valid\n  yaml")
	if got := ReadPresetMetadata(dir); got.Name != nil {
		t.Fatalf("unparsable metadata = %+v", got)
	}
	writeFile(t, filepath.Join(dir, MetadataFile), "- one\n- two\n")
	if got := ReadPresetMetadata(dir); got.Name != nil {
		t.Fatalf("array metadata = %+v", got)
	}
	writeFile(t, filepath.Join(dir, MetadataFile), "name: \"  Standard  \"\ndescription: \"   \"\norder: 2.5\n")
	got := ReadPresetMetadata(dir)
	if got.Name == nil || *got.Name != "Standard" {
		t.Fatalf("name = %+v", got.Name)
	}
	if got.Description != nil {
		t.Fatalf("blank description = %+v", *got.Description)
	}
	if got.Order == nil || *got.Order != 2.5 {
		t.Fatalf("order = %+v", got.Order)
	}
	writeFile(t, filepath.Join(dir, MetadataFile), "order: nah\n")
	if got := ReadPresetMetadata(dir); got.Order != nil {
		t.Fatalf("string order = %+v", *got.Order)
	}
}

func TestRenderPresetMetadata(t *testing.T) {
	if _, ok := RenderPresetMetadata(PresetMetadata{}); ok {
		t.Fatal("empty metadata rendered")
	}
	if _, ok := RenderPresetMetadata(PresetMetadata{Name: strP("   ")}); ok {
		t.Fatal("blank name rendered")
	}
	rendered, ok := RenderPresetMetadata(PresetMetadata{Name: strP("Standard"), Description: strP("The default set."), Order: floatP(3)})
	if !ok {
		t.Fatal("full metadata not rendered")
	}
	// Round-trip: the rendered document reads back to the same fields.
	if err := os.WriteFile(filepath.Join(t.TempDir(), MetadataFile), []byte(rendered), 0o644); err != nil {
		t.Fatalf("write rendered: %v", err)
	}
}

func strP(v string) *string { return &v }

func floatP(v float64) *float64 { return &v }

// --- composition health ----------------------------------------------------

func TestCompositionShapeProblems(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CompositionFile)
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"top-level map", "name: cordis:tools\n", "the composition must be a top-level list of plugin rows"},
		{"scalar row", "- cordis:tools\n", `row 1 is not a plugin row (expected a map with a "name")`},
		{"missing name", "- id: x\n", `row 1 names no plugin (a "name" string is required)`},
		{"empty name", "- name: \"\"\n", `row 1 names no plugin (a "name" string is required)`},
		{"group not list", "- name: host\n  group: true\n  config: nope\n", "group row 1 must hold a list of plugin rows"},
		{"nested scalar row", "- name: host\n  group: true\n  config:\n    - nope\n", `row 1 row 1 is not a plugin row (expected a map with a "name")`},
		{"nested id label", "- name: host\n  group: true\n  id: wrapper\n  config:\n    - nope\n", `row 1 row 1 is not a plugin row (expected a map with a "name")`},
	}
	for _, testCase := range cases {
		writeFile(t, path, testCase.yaml)
		problem := compositionProblem(path, dir)
		if problem == nil || *problem != testCase.want {
			t.Fatalf("%s: problem = %v, want %q", testCase.name, problem, testCase.want)
		}
	}
}

func TestCompositionHealthResolution(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CompositionFile)

	// A builtin row and a shipped relative row both resolve.
	writeFile(t, path, "- name: cordis:tools\n- name: ./helpers.js\n")
	writeFile(t, filepath.Join(dir, "helpers.js"), "export const x = 1\n")
	if problem := compositionProblem(path, dir); problem != nil {
		t.Fatalf("healthy composition reported: %v", *problem)
	}

	// A relative row whose file vanished is the rot that actually happens.
	if err := os.Remove(filepath.Join(dir, "helpers.js")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	problem := compositionProblem(path, dir)
	if problem == nil || *problem != `row 2 names a plugin that cannot be resolved: ./helpers.js` {
		t.Fatalf("missing file = %v", problem)
	}

	// A renamed or uninstalled package resolves through the same walk the
	// loader would start. The name is quoted: `@` is a reserved YAML
	// indicator and cannot open a plain scalar.
	writeFile(t, path, "- name: \"@deepseek-ai/dsh-todo/removed-tool\"\n")
	problem = compositionProblem(path, dir)
	if problem == nil ||
		*problem != `row 1 names a plugin that cannot be resolved: @deepseek-ai/dsh-todo/removed-tool` {
		t.Fatalf("missing package = %v", problem)
	}
	installed := filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh-todo")
	writeFile(t, filepath.Join(installed, "package.json"), "{}\n")
	if problem := compositionProblem(path, dir); problem != nil {
		t.Fatalf("installed package reported: %v", *problem)
	}

	// Multiple unresolvable rows list every offender with row ids when the
	// rows carry one.
	writeFile(t, path, "- name: cordis:tools\n  id: tools\n- name: gone-a\n  id: a\n- name: gone-b\n")
	problem = compositionProblem(path, dir)
	want := "2 rows name plugins that cannot be resolved:\n- row \"a\": gone-a\n- row 3: gone-b"
	if problem == nil || *problem != want {
		t.Fatalf("multi = %q, want %q", *problem, want)
	}

	// A disabled row is skipped; a falsy disabled value still counts as
	// started (the loader's own Boolean test).
	writeFile(t, path, "- name: gone-c\n  disabled: true\n- name: gone-d\n  disabled: 0\n")
	problem = compositionProblem(path, dir)
	if problem == nil || *problem != `row 2 names a plugin that cannot be resolved: gone-d` {
		t.Fatalf("disabled handling = %v", problem)
	}

	// An unparsable document.
	writeFile(t, path, "not: [valid\n")
	problem = compositionProblem(path, dir)
	if problem == nil || !strings.HasPrefix(*problem, "the composition is not valid YAML: ") {
		t.Fatalf("unparsable = %v", problem)
	}
}

func TestPackageInstalledWalk(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	writeFile(t, filepath.Join(root, "node_modules", "pkg", "package.json"), "{}\n")
	if !packageInstalled("pkg", deep) {
		t.Fatal("package not found through the upward walk")
	}
	if !packageInstalled("pkg/subpath", deep) {
		t.Fatal("subpath export should resolve through the package")
	}
	if packageInstalled("missing", deep) {
		t.Fatal("missing package reported installed")
	}
}

// --- scanRoot and discovery ------------------------------------------------

func TestScanRootRowsAndOrdering(t *testing.T) {
	root := t.TempDir()
	// Declared order first (2 before 5); undeclared after, by id.
	writeFile(t, filepath.Join(root, "ptc", CompositionFile), compositionRow("ptc"))
	writeFile(t, filepath.Join(root, "ptc", MetadataFile), "name: PTC\ndescription: Code execution.\norder: 5\n")
	writeFile(t, filepath.Join(root, "standard", CompositionFile), compositionRow("standard"))
	writeFile(t, filepath.Join(root, "standard", MetadataFile), "name: Standard\norder: 2\n")
	writeFile(t, filepath.Join(root, "alpha", CompositionFile), compositionRow("alpha"))
	// A broken row stays on the roster with its reason.
	writeFile(t, filepath.Join(root, "broken", "notes.txt"), "no composition here")
	// Non-id names are skipped, whatever they hold.
	writeFile(t, filepath.Join(root, ".DS_Store", CompositionFile), compositionRow("junk"))
	writeFile(t, filepath.Join(root, "Not_A_Preset", CompositionFile), compositionRow("junk"))

	presets, err := ScanRoot(PresetRoot{Path: root, Trust: TrustUser}, root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var ids []string
	byID := map[string]AgentPreset{}
	for _, preset := range presets {
		ids = append(ids, preset.ID)
		byID[preset.ID] = preset
	}
	if strings.Join(ids, ",") != "standard,ptc,alpha,broken" {
		t.Fatalf("order = %v", ids)
	}
	if byID["ptc"].Name == nil || *byID["ptc"].Name != "PTC" || byID["ptc"].Description == nil {
		t.Fatalf("ptc metadata = %+v", byID["ptc"])
	}
	if byID["broken"].Broken == nil ||
		*byID["broken"].Broken != "the composition file agent.cordis.yml is missing — the directory still occupies the id; delete it or restore the file" {
		t.Fatalf("broken = %v", byID["broken"].Broken)
	}
	if byID["alpha"].Broken != nil {
		t.Fatalf("alpha broken = %v", *byID["alpha"].Broken)
	}
	if byID["alpha"].Trust != TrustUser {
		t.Fatalf("trust = %q", byID["alpha"].Trust)
	}
	if !filepath.IsAbs(byID["alpha"].Path) || filepath.Base(filepath.Dir(byID["alpha"].Path)) != "alpha" {
		t.Fatalf("path = %q", byID["alpha"].Path)
	}
}

func TestScanRootAbsentAndUnreadable(t *testing.T) {
	presets, err := ScanRoot(PresetRoot{Path: filepath.Join(t.TempDir(), "absent"), Trust: TrustSystem}, ".")
	if err != nil {
		t.Fatalf("absent root errored: %v", err)
	}
	if len(presets) != 0 {
		t.Fatalf("absent root = %v", presets)
	}
	// A root that is a file cannot be read: fail loud with the stable
	// prefix.
	fileRoot := filepath.Join(t.TempDir(), "not-a-dir")
	writeFile(t, fileRoot, "x")
	if _, err := ScanRoot(PresetRoot{Path: fileRoot, Trust: TrustSystem}, "."); err == nil {
		t.Fatal("unreadable root accepted")
	} else if !strings.HasPrefix(err.Error(), "agent-presets: cannot read preset root ") {
		t.Fatalf("root error = %v", err)
	}
}

func TestDiscoverPresetsFirstRootWins(t *testing.T) {
	system := t.TempDir()
	user := t.TempDir()
	writeFile(t, filepath.Join(system, "standard", CompositionFile), compositionRow("standard"))
	writeFile(t, filepath.Join(system, "standard", MetadataFile), "name: Shipped Standard\n")
	writeFile(t, filepath.Join(user, "standard", CompositionFile), compositionRow("standard"))
	writeFile(t, filepath.Join(user, "standard", MetadataFile), "name: Local Standard\n")
	writeFile(t, filepath.Join(user, "custom", CompositionFile), compositionRow("custom"))

	presets, err := DiscoverPresets([]PresetRoot{
		{Path: system, Trust: TrustSystem},
		{Path: user, Trust: TrustUser},
	}, user)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(presets) != 2 {
		t.Fatalf("presets = %+v", presets)
	}
	if presets[0].ID != "standard" || presets[0].Trust != TrustSystem || *presets[0].Name != "Shipped Standard" {
		t.Fatalf("first root lost: %+v", presets[0])
	}
	if presets[1].ID != "custom" {
		t.Fatalf("second preset = %+v", presets[1])
	}
}

// --- authoring -------------------------------------------------------------

func TestWritableRoot(t *testing.T) {
	if _, err := WritableRoot([]PresetRoot{{Path: "sys", Trust: TrustSystem}}); err == nil {
		t.Fatal("no user root accepted")
	} else {
		var notWritable *PresetNotWritableError
		if !errors.As(err, &notWritable) {
			t.Fatalf("error = %v", err)
		}
	}
	user := t.TempDir()
	root, err := WritableRoot([]PresetRoot{
		{Path: "sys", Trust: TrustSystem},
		{Path: user, Trust: TrustUser},
	})
	if err != nil {
		t.Fatalf("writable root: %v", err)
	}
	if root != user {
		t.Fatalf("root = %q, want %q", root, user)
	}
}

func TestCopyComposition(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	writeFile(t, filepath.Join(source, CompositionFile), compositionRow("copied"))
	writeFile(t, filepath.Join(source, MetadataFile), "name: Source\ndescription: Keep this.\norder: 2\n")
	writeFile(t, filepath.Join(source, "skills", "demo", "SKILL.md"), "# demo\n")
	roots := []PresetRoot{{Path: root, Trust: TrustUser}}

	if _, err := CopyComposition(roots, AgentPreset{}, "not_valid", nil); err == nil {
		t.Fatal("invalid id accepted")
	} else {
		var invalid *InvalidPresetIDError
		if !errors.As(err, &invalid) {
			t.Fatalf("error = %v", err)
		}
	}

	name := "Renamed"
	target, err := CopyComposition(roots, AgentPreset{ID: "source", Path: filepath.Join(source, CompositionFile), Description: strP("Keep this.")}, "copied", &name)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if target != filepath.Join(root, "copied") {
		t.Fatalf("target = %q", target)
	}
	if _, err := os.Stat(filepath.Join(target, "skills", "demo", "SKILL.md")); err != nil {
		t.Fatalf("tree not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, CompositionFile)); err != nil {
		t.Fatalf("composition not copied: %v", err)
	}
	metadata := ReadPresetMetadata(target)
	if metadata.Name == nil || *metadata.Name != "Renamed" {
		t.Fatalf("copy name = %+v", metadata.Name)
	}
	if metadata.Description == nil || *metadata.Description != "Keep this." {
		t.Fatalf("copy description = %+v", metadata.Description)
	}
	if metadata.Order != nil {
		t.Fatalf("copy kept the source order: %+v", *metadata.Order)
	}

	// The occupied directory check covers undiscovered residue too.
	if err := os.MkdirAll(filepath.Join(root, "occupied"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := CopyComposition(roots, AgentPreset{ID: "source", Path: filepath.Join(source, CompositionFile)}, "occupied", nil); err == nil {
		t.Fatal("occupied id accepted")
	} else {
		var exists *PresetExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("error = %v", err)
		}
	}

	// A failed copy leaves nothing behind.
	if _, err := CopyComposition(roots, AgentPreset{ID: "ghost", Path: filepath.Join(root, "ghost", CompositionFile)}, "wreck", nil); err == nil {
		t.Fatal("copy from a missing source accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "wreck")); !os.IsNotExist(err) {
		t.Fatalf("wreck residue = %v", err)
	}
}

func TestDeleteComposition(t *testing.T) {
	root := t.TempDir()
	roots := []PresetRoot{{Path: root, Trust: TrustUser}}
	presetDir := filepath.Join(root, "authored")
	writeFile(t, filepath.Join(presetDir, CompositionFile), compositionRow("authored"))

	if err := DeleteComposition(roots, AgentPreset{ID: "shipped", Trust: TrustSystem, Path: filepath.Join(root, "shipped", CompositionFile)}); err == nil {
		t.Fatal("system preset deleted")
	} else {
		var notWritable *PresetNotWritableError
		if !errors.As(err, &notWritable) || notWritable.Error() != `agent-presets: preset "shipped" cannot be written: it ships with the deployment` {
			t.Fatalf("error = %v", err)
		}
	}
	outside := t.TempDir()
	if err := DeleteComposition(roots, AgentPreset{ID: "authored", Trust: TrustUser, Path: filepath.Join(outside, "authored", CompositionFile)}); err == nil {
		t.Fatal("preset outside the writable root deleted")
	} else {
		var notWritable *PresetNotWritableError
		if !errors.As(err, &notWritable) {
			t.Fatalf("error = %v", err)
		}
	}
	if err := DeleteComposition(roots, AgentPreset{ID: "authored", Trust: TrustUser, Path: filepath.Join(presetDir, CompositionFile)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(presetDir); !os.IsNotExist(err) {
		t.Fatalf("preset survived: %v", err)
	}
}

// --- roster ----------------------------------------------------------------

func TestRosterResolutionAndAuthoring(t *testing.T) {
	shipped := t.TempDir()
	user := t.TempDir()
	// The user root is the harness home's own preset directory, resolved
	// through DSH_HOME.
	userRoot := filepath.Join(user, UserPresetDir)
	writeFile(t, filepath.Join(shipped, "standard", CompositionFile), compositionRow("standard"))
	writeFile(t, filepath.Join(shipped, "standard", MetadataFile), "name: Shipped Standard\n")
	writeFile(t, filepath.Join(userRoot, "custom", CompositionFile), compositionRow("custom"))
	writeFile(t, filepath.Join(userRoot, "broken", "notes.txt"), "junk")

	override := ""
	overrideSet := false
	cleared := false
	roster := NewRoster(Config{
		Default:            "standard",
		Roots:              []PresetRoot{{Path: shipped, Trust: TrustSystem}},
		IncludeShippedRoot: true,
		IncludeUserRoot:    true,
	}, RosterOptions{
		ShippedRoot: shipped,
		Getenv:      func(string) string { return user },
		DefaultOverride: func() (string, bool) {
			return override, overrideSet
		},
		ClearDefaultOverride: func() { cleared = true },
	})

	if !roster.Authorable() {
		t.Fatal("roster with a user root is not authorable")
	}
	if roster.DefaultID() != "standard" {
		t.Fatalf("default = %q", roster.DefaultID())
	}
	override, overrideSet = "custom", true
	if roster.DefaultID() != "custom" {
		t.Fatalf("override default = %q", roster.DefaultID())
	}
	override, overrideSet = "", false

	presets, err := roster.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(presets) != 3 {
		t.Fatalf("roster = %+v", presets)
	}
	for _, preset := range presets {
		if preset.ID == "standard" && (preset.Trust != TrustSystem || preset.Name == nil || *preset.Name != "Shipped Standard") {
			t.Fatalf("shipped preset = %+v", preset)
		}
	}

	if _, err := roster.Resolve("missing"); err == nil {
		t.Fatal("unknown preset accepted")
	} else if got := err.Error(); got != `agent-presets: preset "missing" not found (available: standard, broken, custom)` {
		t.Fatalf("unknown = %q", got)
	}

	if _, err := roster.ResolveMountable("broken"); err == nil {
		t.Fatal("broken preset accepted")
	} else {
		var mount *PresetMountError
		if !errors.As(err, &mount) || !strings.Contains(mount.Reason, "the composition file "+CompositionFile+" is missing") {
			t.Fatalf("broken error = %v", err)
		}
	}

	document, err := roster.ReadDocument("custom")
	if err != nil {
		t.Fatalf("read document: %v", err)
	}
	if document.AgentPreset != "custom" || document.Trust != TrustUser || document.Content != compositionRow("custom") {
		t.Fatalf("document = %+v", document)
	}

	name := "My Custom"
	if err := roster.Copy("custom", "mine", &name); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userRoot, "mine", CompositionFile)); err != nil {
		t.Fatalf("copy target missing: %v", err)
	}
	if err := roster.Copy("custom", "mine", nil); err == nil {
		t.Fatal("duplicate copy accepted")
	} else {
		var exists *PresetExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("error = %v", err)
		}
	}
	if err := roster.Copy("missing", "other", nil); err == nil {
		t.Fatal("copy from unknown accepted")
	}

	// Removing the shipped preset is refused; removing the stored user
	// default clears the override.
	override, overrideSet = "mine", true
	if err := roster.Remove("standard"); err == nil {
		t.Fatal("system preset removed")
	} else {
		var notWritable *PresetNotWritableError
		if !errors.As(err, &notWritable) {
			t.Fatalf("error = %v", err)
		}
	}
	if cleared {
		t.Fatal("a refused removal cleared the override")
	}
	if _, err := roster.Resolve("standard"); err != nil {
		t.Fatalf("system preset disappeared: %v", err)
	}
	if err := roster.Remove("mine"); err != nil {
		t.Fatalf("remove authored: %v", err)
	}
	if !cleared {
		t.Fatal("removing the stored default did not clear it")
	}
	if _, err := os.Stat(filepath.Join(userRoot, "mine")); !os.IsNotExist(err) {
		t.Fatalf("authored preset survived: %v", err)
	}
}

func TestStampComposition(t *testing.T) {
	path := filepath.Join(t.TempDir(), CompositionFile)
	if StampComposition(path) != nil {
		t.Fatal("absent file stamped")
	}
	writeFile(t, path, compositionRow("x"))
	first := StampComposition(path)
	if first == nil || first.Size == 0 {
		t.Fatalf("stamp = %+v", first)
	}
	if !SameStamp(*first, *first) {
		t.Fatal("identical stamps differ")
	}
	writeFile(t, path, compositionRow("y"))
	// Both rows are the same size and the two writes can land inside one
	// filesystem mtime tick; move the timestamp forward explicitly so the
	// two states cannot collide by clock granularity.
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	second := StampComposition(path)
	if SameStamp(*first, *second) {
		t.Fatal("different states share a stamp")
	}
}
