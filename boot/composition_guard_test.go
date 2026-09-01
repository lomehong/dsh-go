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
// in a bundle cordis.patch.yml. Rows nested under `- insert:` and override
// rows are both captured by id; the effective disabled flag is the row's own.
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

// unresolvedOfficialRow is one enabled official row with no direct Go catalog
// key, carrying the disposition category that exempts it from failing the
// guard until its batch lands.
type unresolvedOfficialRow struct {
	ID       string
	Name     string
	Category string
}

// guardDispositions is the guard's view of the shared disposition table
// (boot.Dispositions) — the single source of truth both the guard and the
// assembly mount loop read. Categories are frontend-domain (browser dist
// owns the row), T2-disposition (recorded no-port decision), and
// T3-planned (Go port scheduled; note the package status). A row absent
// here is genuine drift and fails the guard.
var guardDispositions = Dispositions

// TestOfficialBundleCompositionGuard is the drift guard (v2): every enabled
// official row must either resolve through the Go catalog (direct key or
// official-name alias) or carry a disposition category. Unlisted rows are
// genuine drift and fail. The disposition table shrinks as batches land;
// when it empties the guard asserts full resolution.
func TestOfficialBundleCompositionGuard(t *testing.T) {
	catalog := catalogKeys(t)
	base := parseOfficialPatch(t, filepath.Join("testdata", "official-base.cordis.patch.yml"))
	webapp := parseOfficialPatch(t, filepath.Join("testdata", "official-webapp.cordis.patch.yml"))
	effective := map[string]officialBundleEntry{}
	for _, entry := range base {
		effective[entry.ID] = entry
	}
	for _, entry := range webapp {
		effective[entry.ID] = entry
	}
	var unresolved []unresolvedOfficialRow
	for _, entry := range effective {
		if entry.Disabled || entry.Name == "" {
			continue
		}
		if _, ok := catalog[entry.Name]; ok {
			continue
		}
		unresolved = append(unresolved, unresolvedOfficialRow{
			ID:       entry.ID,
			Name:     entry.Name,
			Category: guardDispositions[entry.Name],
		})
	}
	var drift []unresolvedOfficialRow
	for _, row := range unresolved {
		if row.Category == "" {
			drift = append(drift, row)
		}
	}
	if len(drift) > 0 {
		lines := make([]string, 0, len(drift))
		for _, row := range drift {
			lines = append(lines, row.ID+" -> "+row.Name)
		}
		t.Fatalf("%d unregistered official rows (drift):\n%s", len(drift), strings.Join(lines, "\n"))
	}
	// Disposition shrinkage is asserted as a count in the log; the guard
	// stays green as long as every unresolved row is classified.
	t.Logf("guard: %d unresolved official rows remain (%d frontend-domain, %d T2, %d T3-planned)",
		len(unresolved),
		countByCategory(unresolved, "frontend-domain"),
		countByCategory(unresolved, "T2-disposition"),
		countByCategory(unresolved, "T3-planned"))
}

func countByCategory(rows []unresolvedOfficialRow, prefix string) int {
	count := 0
	for _, row := range rows {
		if strings.HasPrefix(row.Category, prefix) {
			count++
		}
	}
	return count
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
