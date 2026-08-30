// Filesystem discovery of agent presets. A preset is a directory holding
// CompositionFile, optionally beside MetadataFile carrying its display
// text; the directory name is the preset id. Discovery re-reads the roots
// on every call so a preset authored while the process is running is
// visible without a restart.
//
// Discovery also owns preset HEALTH: a directory whose composition is
// missing or unloadable is reported as a broken roster row rather than
// skipped. A skipped directory would still occupy its id on disk — the
// copy path refuses the name while no surface shows anything to delete —
// and a malformed composition would otherwise read as an ordinary preset
// until the first session fails to mount it.
//
// Health is what every consumer reads before offering a preset — the
// pickers drop a broken row rather than defer the discovery to a failed
// session start — so it covers the way an authored preset actually rots: a
// row naming a package that was renamed or uninstalled. Resolving those
// names is a separate pass from the shape check and stops short of
// importing anything, so a composition is judged without running a line of
// plugin code.
package preset

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dshgo/homepaths"
	"gopkg.in/yaml.v3"
)

// CompositionFile is the composition file that makes a directory a preset.
const CompositionFile = "agent.cordis.yml"

// UserPresetDir is the harness-home directory holding locally authored
// presets. This package owns the writable root: where a person's own
// presets go is the same place in every deployment that does not say
// otherwise, so a launcher that forgets to configure one still finds them.
const UserPresetDir = ".agent-presets"

// entryListProblem is why `rows` cannot be an entry list, or nil when it
// can.
//
// A shallow shape check, deliberately short of the loader's work: it does
// not resolve plugin names or apply configs. What it catches is the
// hand-edit that produces a file the loader cannot even begin with — and it
// must accept everything the loader accepts, which is why rows are only
// required to be maps carrying a plugin `name` (groups recurse into their
// own lists).
func entryListProblem(rows any, at string) *string {
	list, ok := rows.([]any)
	if !ok {
		message := "the composition must be a top-level list of plugin rows"
		if at != "" {
			message = fmt.Sprintf("group %s must hold a list of plugin rows", at)
		}
		return &message
	}
	for index, entry := range list {
		label := fmt.Sprintf("row %d", index+1)
		if at != "" {
			label = fmt.Sprintf("%s row %d", at, index+1)
		}
		row, ok := entry.(map[string]any)
		if !ok {
			message := fmt.Sprintf("%s is not a plugin row (expected a map with a \"name\")", label)
			return &message
		}
		name, hasName := rowValue(row)["name"]
		nameText, isText := name.(string)
		if !hasName || !isText || nameText == "" {
			message := fmt.Sprintf("%s names no plugin (a \"name\" string is required)", label)
			return &message
		}
		if group, _ := rowValue(row)["group"].(bool); group {
			if nested := entryListProblem(rowValue(row)["config"], label); nested != nil {
				return nested
			}
		}
	}
	return nil
}

// rowValue guards the nil-map case that a plain type assertion leaves nil
// (a nil `map[string]any` reads every key as absent).
func rowValue(row map[string]any) map[string]any {
	if row == nil {
		return map[string]any{}
	}
	return row
}

// packageInstalled reports whether a package name is installed anywhere
// above base: the same upward `node_modules` walk Node's own resolver
// starts with, stopping at the package directory. A scoped name spends two
// segments on the package; anything after either form is a subpath export,
// which lives inside the package directory.
func packageInstalled(name, base string) bool {
	segments := strings.Split(name, "/")
	take := 1
	if strings.HasPrefix(name, "@") {
		take = 2
	}
	if len(segments) < take {
		return false
	}
	pkg := strings.Join(segments[:take], "/")
	dir := base
	for {
		if _, err := os.Stat(filepath.Join(dir, "node_modules", pkg, "package.json")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// rowResolves reports whether one classified row names a module that
// exists, importing nothing.
//
// Each kind is checked by what actually answers it. A package name is
// looked up on disk — the same upward walk Node's own resolver starts
// with — and a relative or absolute specifier is statted, because both name
// one file. Nothing is evaluated either way, so a row is judged without its
// plugin observing that discovery looked.
//
// Go deviations from the official TypeScript, both recorded: there is no
// builtin-module universe to consult, so only the disk walk answers a
// package name; and a `file:` URL is decoded to a path before the stat.
func rowResolves(row RowSpecifier, presetDir, harnessBase string) bool {
	switch row.Kind {
	case SpecifierBuiltin:
		return true
	case SpecifierPackage:
		return packageInstalled(row.Specifier, harnessBase)
	case SpecifierPreset:
		return isFile(filepath.Join(presetDir, filepath.FromSlash(row.Specifier)))
	case SpecifierFile:
		return isFile(fileURLToPath(row.Specifier))
	default:
		return false
	}
}

// fileURLToPath decodes a `file:` URL to a filesystem path; a value that
// names no parseable URL resolves to nothing.
func fileURLToPath(value string) string {
	rest := strings.TrimPrefix(value, "file://")
	if rest == value {
		// A bare `file:` with no authority: treat the remainder as a path.
		return strings.TrimPrefix(value, "file:")
	}
	return filepath.FromSlash(strings.TrimPrefix(rest, "/"))
}

// UnresolvableRow is one row that names a module no resolver can find.
type UnresolvableRow struct {
	// Label is `row "id"`, or the row's position when it declares none.
	Label string
	// Name is the specifier exactly as the row wrote it.
	Name string
}

// jsTruthy is the loader's own row-start test: it starts a row when
// `Boolean(options.disabled)` is false, so a present value names a row that
// does NOT start and must not be checked. A `!!js` expression decodes as a
// non-empty opaque value here — an object in the official runtime — which
// skips exactly the rows whose value only the loader context can decide.
func jsTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case uint64:
		return typed != 0
	case float64:
		return typed != 0 && !math.IsNaN(typed)
	default:
		return true
	}
}

// unresolvableRows returns the rows whose module cannot be resolved, in
// composition order.
//
// Only rows that will certainly be started are checked. Shape is the
// caller's precondition: entryListProblem has already proven every row is a
// map carrying a `name` string, and groups recurse the same way it does.
func unresolvableRows(rows []any, presetDir, harnessBase, at string) []UnresolvableRow {
	found := []UnresolvableRow{}
	for index, entry := range rows {
		row := rowValue(mustMap(entry))
		if jsTruthy(row["disabled"]) {
			continue
		}
		positional := fmt.Sprintf("row %d", index+1)
		if at != "" {
			positional = fmt.Sprintf("%s row %d", at, index+1)
		}
		if group, _ := row["group"].(bool); group {
			found = append(found, unresolvableRows(toList(row["config"]), presetDir, harnessBase, positional)...)
			continue
		}
		name, _ := row["name"].(string)
		if rowResolves(ClassifyRowSpecifier(name), presetDir, harnessBase) {
			continue
		}
		label := positional
		if id, ok := row["id"].(string); ok && id != "" {
			label = fmt.Sprintf("row %q", id)
		}
		found = append(found, UnresolvableRow{Label: label, Name: name})
	}
	return found
}

func mustMap(value any) map[string]any {
	row, _ := value.(map[string]any)
	return row
}

func toList(value any) []any {
	list, _ := value.([]any)
	return list
}

// compositionProblem returns why the composition at path cannot mount, or
// nil when it looks loadable.
//
// Parsed with a tolerant YAML pass: the official loader's dialect carries
// the `!!js` tag under plugin `config` and entry `disabled`, so those tags
// are rewritten to opaque strings before parsing rather than rejected —
// health can never call a composition broken that the loader would accept.
// A value only the loader context can decide stays truthy, matching the
// official skip rule.
func compositionProblem(path, harnessBase string) *string {
	raw, err := os.ReadFile(path)
	if err != nil {
		// The caller statted this file moments ago; any read failure now —
		// deleted in between, permissions — is the same answer as
		// unparsable.
		message := fmt.Sprintf("the composition file %s cannot be read", CompositionFile)
		return &message
	}
	rows, err := parseComposition(raw)
	if err != nil {
		message := fmt.Sprintf("the composition is not valid YAML: %s", firstLine(err.Error()))
		return &message
	}
	if shape := entryListProblem(rows, ""); shape != nil {
		return shape
	}
	// The composition's own directory, exactly as the mount derives it, so
	// a row naming a file the preset ships resolves the way the mount will.
	presetDir := filepath.Dir(path)
	unresolvable := unresolvableRows(toList(rows), presetDir, harnessBase, "")
	if len(unresolvable) == 0 {
		return nil
	}
	if len(unresolvable) == 1 {
		message := fmt.Sprintf("%s names a plugin that cannot be resolved: %s", unresolvable[0].Label, unresolvable[0].Name)
		return &message
	}
	lines := make([]string, 0, len(unresolvable))
	for _, row := range unresolvable {
		lines = append(lines, fmt.Sprintf("- %s: %s", row.Label, row.Name))
	}
	message := fmt.Sprintf("%d rows name plugins that cannot be resolved:\n%s", len(unresolvable), strings.Join(lines, "\n"))
	return &message
}

// parseComposition parses a composition document, tolerating the loader's
// `!!js` tag by reading its value as an opaque string.
func parseComposition(raw []byte) (any, error) {
	text := strings.ReplaceAll(string(raw), "!!js", "!!str")
	var document any
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		return nil, err
	}
	return document, nil
}

func firstLine(value string) string {
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		return value[:index]
	}
	return value
}

// isFile reports whether path names an existing regular file. Any stat
// failure — absent, unreadable, a dangling link — means this directory does
// not present a composition, which is not an error: the directory simply is
// not a preset.
func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// ScanRoot scans one root for preset directories and returns the root's
// presets ordered by declared position, then id.
//
// An absent root yields no presets rather than an error: the user root does
// not exist until the first locally authored preset, and naming a default
// that no root supplies already fails loud at resolution.
//
// Every directory whose name is a usable preset id is a roster row — broken
// when its composition is missing or unloadable. A directory named outside
// the id rule is skipped instead: no copy could ever claim that name, so it
// blocks nothing, and reporting `.DS_Store`-grade residue as broken presets
// would teach users to ignore the marker.
func ScanRoot(root PresetRoot, harnessBase string) ([]AgentPreset, error) {
	dir, err := filepath.Abs(homepaths.ExpandHomePath(root.Path))
	if err != nil {
		return nil, fmt.Errorf("agent-presets: cannot read preset root %s: %w", dir, err)
	}
	children, err := os.ReadDir(dir)
	if err != nil {
		// An absent root yields no presets rather than an error: the user
		// root does not exist until the first locally authored preset, and
		// naming a default that no root supplies already fails loud at
		// resolution. A root that exists but is not a directory is a
		// misconfiguration, not an empty roster. Go collapses both into one
		// not-exist error on some platforms, so the stat restores the
		// distinction the official ENOENT/ENOTDIR pair carries.
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(dir); statErr != nil {
				return nil, nil
			}
		}
		return nil, fmt.Errorf("agent-presets: cannot read preset root %s: %w", dir, err)
	}
	found := []AgentPreset{}
	for _, child := range children {
		if !child.IsDir() || !ValidPresetID(child.Name()) {
			continue
		}
		directory := filepath.Join(dir, child.Name())
		path := filepath.Join(directory, CompositionFile)
		var broken *string
		if isFile(path) {
			broken = compositionProblem(path, harnessBase)
		} else {
			message := fmt.Sprintf("the composition file %s is missing — the directory still occupies the id; delete it or restore the file", CompositionFile)
			broken = &message
		}
		// Display text only, and never fatal: a preset with unreadable
		// metadata still mounts, it just shows its id.
		metadata := ReadPresetMetadata(directory)
		found = append(found, AgentPreset{
			ID:          child.Name(),
			Trust:       root.Trust,
			Path:        path,
			Name:        metadata.Name,
			Description: metadata.Description,
			Order:       metadata.Order,
			Broken:      broken,
		})
	}
	// Declared order first so the shipped set reads by capability;
	// everything else falls back to the id, which keeps authored presets
	// stable. Byte order stands in for localeCompare (recorded deviation).
	sort.SliceStable(found, func(left, right int) bool {
		leftOrder := orderKey(found[left].Order)
		rightOrder := orderKey(found[right].Order)
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return found[left].ID < found[right].ID
	})
	return found, nil
}

func orderKey(order *float64) float64 {
	if order == nil {
		return math.Inf(1)
	}
	return *order
}

// DiscoverPresets scans every root in precedence order and returns every
// discovered preset, first-root-wins per id.
func DiscoverPresets(roots []PresetRoot, harnessBase string) ([]AgentPreset, error) {
	byID := map[string]AgentPreset{}
	order := []string{}
	for _, root := range roots {
		presets, err := ScanRoot(root, harnessBase)
		if err != nil {
			return nil, err
		}
		for _, preset := range presets {
			if _, seen := byID[preset.ID]; seen {
				continue
			}
			byID[preset.ID] = preset
			order = append(order, preset.ID)
		}
	}
	out := make([]AgentPreset, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out, nil
}
