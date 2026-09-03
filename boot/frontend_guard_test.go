package boot

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// frontendUpstreamPinRe extracts the pinned upstream commit from
// frontend/UPSTREAM.md (written by scripts/sync-frontend.ps1).
var frontendUpstreamPinRe = regexp.MustCompile(`- Upstream commit:\s+([0-9a-f]{7,40})`)

// upstreamCloneCandidates are the likely homes of the official
// deepseek-harness clone the frontend fork syncs against, in order. An env
// override wins (DSH_UPSTREAM_CLONE), matching the other repo-local tooling.
func upstreamCloneCandidates() []string {
	if override := os.Getenv("DSH_UPSTREAM_CLONE"); override != "" {
		return []string{override}
	}
	return []string{`E:\code\nodejs\deepseek-harness`}
}

// upstreamHead returns the clone's current HEAD commit, or "" when no clone
// is present at any candidate (the guard then skips with a record).
func upstreamHead(t *testing.T) string {
	t.Helper()
	for _, candidate := range upstreamCloneCandidates() {
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err != nil {
			continue
		}
		out, err := exec.Command("git", "-C", candidate, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse in %s: %v", candidate, err)
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// TestFrontendUpstreamPinMatchesClone is the v5 frontend-drift guard: the
// webassets the Go host serves are built from a source fork pinned in
// frontend/UPSTREAM.md; when the official clone they were synced from has
// moved on, this test fails so the drift surfaces in the gate instead of as
// another stale-artifact outage (the r97-era 13-day webassets drift).
func TestFrontendUpstreamPinMatchesClone(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "frontend", "UPSTREAM.md"))
	if err != nil {
		t.Fatalf("frontend/UPSTREAM.md missing: %v (run scripts/sync-frontend.ps1)", err)
	}
	m := frontendUpstreamPinRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("frontend/UPSTREAM.md has no pinned upstream commit")
	}
	pinned := string(m[1])

	head := upstreamHead(t)
	if head == "" {
		t.Logf("guard v5: no upstream clone present; pin %s not checked (set DSH_UPSTREAM_CLONE to enable)", pinned)
		return
	}
	if !strings.HasPrefix(head, pinned) && !strings.HasPrefix(pinned, head) {
		t.Fatalf("frontend fork drifted: UPSTREAM.md pins %s but the official clone is at %s\nrun scripts/sync-frontend.ps1 and commit the refreshed fork", pinned, head)
	}
	t.Logf("guard v5: frontend pin %s matches clone HEAD %s", pinned, head)
}
