package boot

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// officialBundleEntry is one parsed row of a shipped bundle patch.
type officialBundleEntry struct {
	ID       string
	Name     string
	Disabled bool
}

// parseOfficialPatch extracts the (id, name, disabled) triple of every row
// in a bundle cordis.patch.yml — the loader entry-list dialect the shipped
// patches use. Rows nested under `- insert:` and override rows are both
// captured by id; the effective disabled flag is the row's own.
func parseOfficialPatch(t *testing.T, path string) []officialBundleEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var entries []officialBundleEntry
	var current *officialBundleEntry
	idRe := regexp.MustCompile(`^\s*-\s+id:\s+(\S+)`)
	nameRe := regexp.MustCompile(`^\s*name:\s+['"]?([^'"\s]+)`)
	disabledRe := regexp.MustCompile(`^\s*disabled:\s+true`)
	for _, line := range strings.Split(string(raw), "\n") {
		if m := idRe.FindStringSubmatch(line); m != nil {
			if current != nil {
				entries = append(entries, *current)
			}
			current = &officialBundleEntry{ID: m[1]}
			continue
		}
		if current == nil {
			continue
		}
		if m := nameRe.FindStringSubmatch(line); m != nil && current.Name == "" {
			current.Name = m[1]
		}
		if disabledRe.MatchString(line) {
			current.Disabled = true
		}
	}
	if current != nil {
		entries = append(entries, *current)
	}
	return entries
}

// TestOfficialBundleCompositionGuard is the drift guard: every ENABLED row
// of the shipped base and web-app bundles must resolve through the Go
// catalog (direct key or official-name alias), so the real bundles can
// compose. A row the Go port does not cover yet makes this test fail —
// the 49-row migration surface narrows as rows land.
func TestOfficialBundleCompositionGuard(t *testing.T) {
	catalog := catalogKeys(t)
	base := parseOfficialPatch(t, filepath.Join("testdata", "official-base.cordis.patch.yml"))
	webapp := parseOfficialPatch(t, filepath.Join("testdata", "official-webapp.cordis.patch.yml"))
	// The web-app layer overrides base rows by id; the effective entry is
	// the web-app one when it names the same id. Build the final row set.
	effective := map[string]officialBundleEntry{}
	for _, entry := range base {
		effective[entry.ID] = entry
	}
	for _, entry := range webapp {
		effective[entry.ID] = entry
	}
	var unresolved []string
	for _, entry := range effective {
		if entry.Disabled || entry.Name == "" {
			continue
		}
		if _, ok := catalog[entry.Name]; ok {
			continue
		}
		unresolved = append(unresolved, entry.ID+" -> "+entry.Name)
	}
	if len(unresolved) > 0 {
		t.Fatalf("%d enabled official rows have no Go catalog entry:\n%s",
			len(unresolved), strings.Join(unresolved, "\n"))
	}
}

// catalogKeys reads the builders map keys (already alias-expanded in init).
func catalogKeys(t *testing.T) map[string]bool {
	t.Helper()
	keys := make(map[string]bool, len(builders))
	for name := range builders {
		keys[name] = true
	}
	return keys
}
