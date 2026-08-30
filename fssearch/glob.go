package fssearch

import (
	"path/filepath"
	"strings"
)

// GlobMaxResultsDefault is the default cap on paths retained inline by one
// glob call, matching Claude Code's default GlobTool result limit.
const GlobMaxResultsDefault = 100

// GlobVcsExcludes lists the directory names ripgrep must never descend into
// for a discovery listing: VCS metadata stores. `--no-ignore --hidden`
// would otherwise surface them in every broad search. Each name is excluded
// with TWO negated `--glob`s (see BuildGlobCommand): an any-depth directory
// glob that matches — and prunes — the directory during traversal, and a
// contents glob that still excludes the internals when the search root
// itself is at or inside the directory (an explicit `path` of `.git` or
// `sub/.git`), where the prune glob alone never matches.
var GlobVcsExcludes = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// GlobInput is the validated glob arguments.
type GlobInput struct {
	Pattern string
	Path    string
}

// ParseGlobArgs validates value constraints the schema DSL can't express: a
// non-blank pattern, and a non-blank path when given.
func ParseGlobArgs(args map[string]any) (GlobInput, error) {
	pattern, _ := args["pattern"].(string)
	if strings.TrimSpace(pattern) == "" {
		return GlobInput{}, errArgs("pattern must be a non-empty string")
	}
	input := GlobInput{Pattern: pattern}
	if raw, has := args["path"]; has {
		path, _ := raw.(string)
		if strings.TrimSpace(path) == "" {
			return GlobInput{}, errArgs("path must be a non-empty string when given")
		}
		input.Path = path
	}
	return input, nil
}

// BuildGlobCommand builds the fixed `rg --files` argv for one glob call.
// Every model-controlled value (pattern, path) is a plain argv element — no
// shell layer exists, so no quoting applies; the search root rides behind
// `--` so a leading-dash path can never be parsed as a flag. `--sort=modified`
// orders by modification time, `--no-ignore --hidden` searches ignored and
// hidden files, and GlobVcsExcludes keeps VCS metadata out.
func BuildGlobCommand(input GlobInput) []string {
	parts := []string{
		"--files",
		"--glob=" + input.Pattern,
		"--sort=modified",
		"--no-ignore",
		"--hidden",
		// Two negated globs per VCS name: the bare form prunes the
		// directory during traversal; the /** form still excludes the
		// contents when the search root is AT or INSIDE the directory
		// (where the bare form, matched against root-prefixed paths, never
		// fires).
	}
	for _, name := range GlobVcsExcludes {
		parts = append(parts, "--glob=!**/"+name, "--glob=!**/"+name+"/**")
	}
	if input.Path != "" {
		parts = append(parts, "--", input.Path)
	}
	return parts
}

// GlobSample is the inline page of a capped glob result, plus how much of
// the complete result's top level it reaches.
type GlobSample struct {
	// Items are the paths to show inline: grouped by top-level entry,
	// modification-time ordered within each group.
	Items []string
	// Shown counts distinct top-level entries the shown paths reach.
	Shown int
	// Total counts distinct top-level entries across the complete result.
	Total int
}

// relativeToSearchRoot removes the displayed search-root prefix before
// choosing a top-level group.
func relativeToSearchRoot(path, root string) string {
	sepStr := sep()
	if root == "." {
		return strings.TrimPrefix(path, "."+sepStr)
	}
	rootEnd := len(root)
	for rootEnd > 0 && root[rootEnd-1] == filepath.Separator {
		rootEnd--
	}
	trimmedRoot := root[:rootEnd]
	if trimmedRoot == "" {
		return stripLeadingSeparators(path)
	}
	if path == trimmedRoot {
		return ""
	}
	if strings.HasPrefix(path, trimmedRoot+sepStr) {
		return path[len(trimmedRoot)+len(sepStr):]
	}
	return path
}

// stripLeadingSeparators strips only separators recognized by the
// execution platform.
func stripLeadingSeparators(path string) string {
	return strings.TrimLeft(path, sep())
}

// topLevelSegment is the leading path segment of one display path — the
// top-level entry, relative to the search root, that the path sits under.
// A path with no separator is its own top-level entry. Leading separators
// are stripped first so an absolute path (one outside the workdir, which
// toWorkdirRelative leaves untouched) groups by its first real name instead
// of collapsing every such path into one empty group.
func topLevelSegment(path string) string {
	trimmed := stripLeadingSeparators(path)
	if cut := strings.Index(trimmed, sep()); cut != -1 {
		return trimmed[:cut]
	}
	return trimmed
}

// sampleAcrossTopLevel chooses the inline page of an over-cap result by
// round-robin across the complete result's top-level entries, instead of
// taking its head.
//
// Every top-level entry receives a slot before any receives a second;
// exhausted groups drop out. Group order and order within each group follow
// paths, so a flat result reproduces the modification-time head.
func sampleAcrossTopLevel(paths []string, maxItems int, root string) GlobSample {
	type activeGroup struct {
		key     string
		items   []string
		index   int
		current string
	}
	groups := map[string][]string{}
	var order []string
	var active []activeGroup
	for _, path := range paths {
		key := topLevelSegment(relativeToSearchRoot(path, root))
		if _, has := groups[key]; !has {
			groups[key] = []string{path}
			order = append(order, key)
			active = append(active, activeGroup{key: key, items: groups[key], index: 0, current: path})
		} else {
			groups[key] = append(groups[key], path)
			for i := range active {
				if active[i].key == key {
					active[i].items = groups[key]
					break
				}
			}
		}
	}
	taken := map[string][]string{}
	var takenOrder []string
	count := 0
	for len(active) > 0 && count < maxItems {
		var nextActive []activeGroup
		for _, group := range active {
			if count >= maxItems {
				break
			}
			count++
			if _, has := taken[group.key]; !has {
				taken[group.key] = []string{group.current}
				takenOrder = append(takenOrder, group.key)
			} else {
				taken[group.key] = append(taken[group.key], group.current)
			}
			nextIndex := group.index + 1
			if nextIndex < len(group.items) {
				nextActive = append(nextActive, activeGroup{key: group.key, items: group.items, index: nextIndex, current: group.items[nextIndex]})
			}
		}
		active = nextActive
	}
	items := make([]string, 0, count)
	for _, key := range takenOrder {
		items = append(items, taken[key]...)
	}
	return GlobSample{Items: items, Shown: len(takenOrder), Total: len(order)}
}

// FormatGlobOutput formats a capped sampled page and its complete-result
// recovery path. A flat result keeps the plain footer because its sample is
// the modification-time head.
func FormatGlobOutput(sample GlobSample, seen int, spillRef *SpillRef) string {
	basis := "."
	if sample.Total != seen {
		basis = ", sampled across " + itoa(sample.Shown) + " of the " + itoa(sample.Total) + " top-level entries this pattern matched instead of taken in modification-time order."
		if sample.Shown < sample.Total {
			basis += " Narrow path to inspect a specific subtree."
		}
	}
	return formatGlobPage(sample.Items, seen, spillRef, basis)
}

// formatGlobPage formats one bounded page and the recovery path for its
// complete sorted result.
func formatGlobPage(items []string, seen int, spillRef *SpillRef, basis string) string {
	body := strings.Join(items, "\n")
	recovery := "The complete result could not be saved; narrow pattern or path to see more."
	if spillRef != nil {
		recovery = "Full sorted result stored at: " + spillRef.Locator + ". " + spillRef.RetrievalHint
	}
	return body + "\n\n(Showing " + itoa(len(items)) + " of " + itoa(seen) + " paths" + basis + " " + recovery + ")"
}

// RenderGlobPaths bounds and formats one canonical path list relative to
// its search root. A result that fits is shown whole, untouched:
// modification-time order is the tool's contract, and over a complete
// result it is what answers age questions.
func RenderGlobPaths(paths []string, caps SearchCaps, root string, spillRef *SpillRef) string {
	if len(paths) == 0 {
		return "No files found"
	}
	if len(paths) <= caps.GlobMaxResults {
		return strings.Join(paths, "\n")
	}
	if !caps.SampleOverCapGlobResults {
		return formatGlobPage(paths[:caps.GlobMaxResults], len(paths), spillRef, ".")
	}
	return FormatGlobOutput(sampleAcrossTopLevel(paths, caps.GlobMaxResults, root), len(paths), spillRef)
}

// globCardPage is the inline page of paths a completed glob card shows,
// computed the SAME way RenderGlobPaths computes its model-facing page so
// the card and the text agree on which paths survived the cap.
func globCardPage(paths []string, caps SearchCaps, root string) ([]string, bool) {
	if len(paths) <= caps.GlobMaxResults {
		return paths, false
	}
	if !caps.SampleOverCapGlobResults {
		return paths[:caps.GlobMaxResults], true
	}
	return sampleAcrossTopLevel(paths, caps.GlobMaxResults, root).Items, true
}

// parseGlobLines converts complete raw `rg --files` stdout to display paths.
func parseGlobLines(stdout, workdir string) []string {
	var all []string
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		all = append(all, toWorkdirRelative(line, workdir))
	}
	return all
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
