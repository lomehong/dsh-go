package spilllocal

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/spill"
)

func TestEncodeSegmentIsInjectiveAndSafe(t *testing.T) {
	cases := map[string]string{
		"":          "~",
		".":         "~002E",
		"..":        "~002E~002E",
		"~":         "~007E",
		"a.txt":     "a.txt",
		"my-file_1": "my-file_1",
		"../evil":   "..~002Fevil",
		"C:\\x":     "C~003A~005Cx",
		"a/b":       "a~002Fb",
		"a\x00b":    "a~0000b",
		"a b":       "a~0020b",
	}
	for raw, want := range cases {
		if got := encodeSegment(raw); got != want {
			t.Fatalf("encodeSegment(%q) = %q, want %q", raw, got, want)
		}
	}
	// Reversibility implies injectivity for these shapes: distinct inputs
	// never collide and no segment escapes the session directory.
	for raw := range cases {
		encoded := encodeSegment(raw)
		if encoded == "." || encoded == ".." || strings.ContainsAny(encoded, "/\\") {
			t.Fatalf("unsafe segment %q from %q", encoded, raw)
		}
	}
	if encodeSegment("é") == encodeSegment("e") {
		t.Fatal("distinct runes collided")
	}
}

func TestSessionDirIsStableAndHashShaped(t *testing.T) {
	first := SessionDir("root", "session-a")
	second := SessionDir("root", "session-a")
	if first != second {
		t.Fatal("session dir unstable")
	}
	if !regexp.MustCompile(`^root[/\\]session-[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("shape = %q", first)
	}
	if SessionDir("root", "session-b") == first {
		t.Fatal("distinct sessions collided")
	}
}

func TestSaveTextFileWritesFreshPrivateFile(t *testing.T) {
	root := t.TempDir()
	first, err := SaveTextFile(SaveTextOptions{Root: root, SessionID: "s", SuggestedName: "web_fetch.txt", Content: "hello 世界"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	second, err := SaveTextFile(SaveTextOptions{Root: root, SessionID: "s", SuggestedName: "web_fetch.txt", Content: "hello 世界"})
	if err != nil {
		t.Fatalf("save 2: %v", err)
	}
	if first.Path == second.Path {
		t.Fatal("suggested name reused verbatim; collision")
	}
	if first.Bytes != len("hello 世界") || second.Bytes != first.Bytes {
		t.Fatalf("bytes = %d/%d", first.Bytes, second.Bytes)
	}
	data, err := os.ReadFile(first.Path)
	if err != nil || string(data) != "hello 世界" {
		t.Fatalf("content = %q err %v", data, err)
	}
	// Both files live inside the same session directory below the root.
	if filepath.Dir(first.Path) != filepath.Dir(second.Path) || !strings.HasPrefix(filepath.Dir(first.Path), root) {
		t.Fatalf("paths = %q %q", first.Path, second.Path)
	}
	// A traversal suggested name stays inside the session directory.
	escaped, err := SaveTextFile(SaveTextOptions{Root: root, SessionID: "s", SuggestedName: "../../evil.txt", Content: "x"})
	if err != nil {
		t.Fatalf("save traversal: %v", err)
	}
	if !strings.HasPrefix(escaped.Path, filepath.Dir(first.Path)) {
		t.Fatalf("escaped session dir: %q", escaped.Path)
	}
}

func TestPrivateRootMatchesBackendShape(t *testing.T) {
	root, err := PrivateRoot()
	if err != nil {
		t.Fatalf("private root: %v", err)
	}
	again, err := PrivateRoot()
	if err != nil || again != root {
		t.Fatalf("private root unstable: %q vs %q", root, again)
	}
	if !regexp.MustCompile(`^` + regexp.QuoteMeta(os.TempDir()) + `[/\\]dsh-spill-[A-Za-z0-9]{6}$`).MatchString(root) {
		t.Fatalf("shape = %q", root)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("root missing: %v", err)
	}
}

// newSweepFixture builds one root with a backend-shaped session directory
// holding one old and one fresh file, plus one foreign sibling.
func newSweepFixture(t *testing.T) (root string, oldFile string, freshFile string) {
	t.Helper()
	root = t.TempDir()
	sessionDir := filepath.Join(root, "session-000000000000")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	oldFile = filepath.Join(sessionDir, "old.txt")
	freshFile = filepath.Join(sessionDir, "fresh.txt")
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.WriteFile(oldFile, []byte("old"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(freshFile, []byte("fresh"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chtimes(oldFile, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return root, oldFile, freshFile
}

func TestSweepDeletesExpiredKeepsFreshAndForeign(t *testing.T) {
	root, oldFile, freshFile := newSweepFixture(t)
	foreign := filepath.Join(root, "session-backup")
	if err := os.Mkdir(foreign, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	stray := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(stray, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var warnings []string
	cutoff := time.Now().UnixMilli() - 30*24*60*60*1000
	sweepSpillRoots(SweepOptions{Roots: []SweepRoot{{Path: root, PruneWhenEmpty: false}}, CutoffUnixMilli: cutoff, Warn: func(m string) { warnings = append(warnings, m) }})
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("expired file survived")
	}
	if _, err := os.Stat(freshFile); err != nil {
		t.Fatal("fresh file deleted")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("foreign directory swept")
	}
	// The active root is never pruned; the session dir still holds a fresh
	// file anyway.
	if _, err := os.Stat(root); err != nil {
		t.Fatal("active root pruned")
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
}

func TestSweepPrunesEmptySessionDirsAndDiscoveredRoots(t *testing.T) {
	root, oldFile, _ := newSweepFixture(t)
	// A second, stale default-shaped root from a "prior process".
	staleRoot := filepath.Join(t.TempDir(), "dsh-spill-abc123")
	staleSession := filepath.Join(staleRoot, "session-000000000000")
	if err := os.MkdirAll(staleSession, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	staleFile := filepath.Join(staleSession, "gone.txt")
	if err := os.WriteFile(staleFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	past := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(staleFile, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	base := filepath.Dir(staleRoot)
	roots := GatherSweepRoots(root, nil, base)
	if len(roots) != 2 {
		t.Fatalf("roots = %+v", roots)
	}
	cutoff := time.Now().UnixMilli() - 30*24*60*60*1000
	sweepSpillRoots(SweepOptions{Roots: roots, CutoffUnixMilli: cutoff, Warn: nil})

	// The discovered root pruned once empty; the active root stays.
	if _, err := os.Stat(staleRoot); !os.IsNotExist(err) {
		t.Fatal("discovered empty root survived")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("active root pruned")
	}
	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Fatal("expired file survived")
	}
}

func TestDiscoveryMatchesExactShapeOnly(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	real := filepath.Join(base, "dsh-spill-Zz9AbC")
	fixture := filepath.Join(base, "dsh-spill-test-fixture")
	file := filepath.Join(base, "dsh-spill-plainname")
	for _, path := range []string{real, fixture} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(file, []byte("not a dir shape"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	discovered := DiscoverDefaultRoots(nil, base)
	if len(discovered) != 1 || discovered[0] != real {
		t.Fatalf("discovered = %v", discovered)
	}
}

func TestLocalStoreSavesAndSweeps(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocalSpillStore(Config{Root: root, CleanupPeriodDays: 0, CleanupPeriodDaysSet: true}, cordis.Discard{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	ref, err := store.SaveText(context.Background(), spill.SaveTextSpill{
		Owner:         spill.SpillOwner{SessionID: "session-7"},
		Source:        spill.SpillSource{ToolName: "web_fetch", CallID: "c1", Label: "result"},
		SuggestedName: "web_fetch.txt",
		Content:       "the full oversized text",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if ref.Bytes != len("the full oversized text") || ref.RetrievalHint != "Use read with offset/limit, or grep this path to search within it." {
		t.Fatalf("ref = %+v", ref)
	}
	data, err := os.ReadFile(ref.Locator)
	if err != nil || string(data) != "the full oversized text" {
		t.Fatalf("content = %q err %v", data, err)
	}
	if !strings.HasPrefix(filepath.Dir(ref.Locator), root) {
		t.Fatalf("locator = %q outside root %q", ref.Locator, root)
	}
	// Cleanup disabled resolves to a no-op sweep and an immediate close.
	store.Close()
	if ResolveConfig(Config{}).CleanupPeriodDays != 30 {
		t.Fatal("default cleanup period lost")
	}
}

func TestSweepQuiescesBeforeClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership trust checks drive the gather seam on this host")
	}
	root := t.TempDir()
	// Cleanup disabled at construction: the test seam owns the only sweep.
	store, err := NewLocalSpillStore(Config{Root: root, CleanupPeriodDays: 0, CleanupPeriodDaysSet: true}, cordis.Discard{})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	release := make(chan struct{})
	sweepStarted := make(chan struct{})
	store.SetGatherRoots(func(warn WarnFn) []SweepRoot {
		close(sweepStarted)
		<-release
		return nil
	})
	// Kick a fresh sweep through the test seam.
	done := store.LaunchSweepForTest(nil)
	<-sweepStarted
	closed := make(chan struct{})
	go func() {
		store.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while the sweep was still gathering")
	default:
	}
	close(release)
	done()
	<-closed
}

func TestNegativeCleanupPeriodFailsLoud(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("negative cleanupPeriodDays accepted")
		}
	}()
	ResolveConfig(Config{CleanupPeriodDays: -1, CleanupPeriodDaysSet: true})
}
