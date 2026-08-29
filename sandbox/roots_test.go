package sandbox

import (
	"path/filepath"
	"runtime"
	"testing"

	"dshgo/fs"
)

func TestWritableRootsByMode(t *testing.T) {
	workspace := t.TempDir()
	if roots := WritableRoots(fs.SandboxExecutionPolicy{Mode: "read-only", WorkspaceRoot: workspace}); roots != nil {
		t.Fatalf("read-only must allow nothing: %v", roots)
	}
	if roots := WritableRoots(fs.SandboxExecutionPolicy{Mode: "danger-full-access", WorkspaceRoot: workspace}); roots != nil {
		t.Fatalf("danger-full-access uses no allow-list: %v", roots)
	}
	roots := WritableRoots(fs.SandboxExecutionPolicy{Mode: "workspace-write", WorkspaceRoot: workspace})
	if len(roots) < 2 {
		t.Fatalf("workspace-write needs the root and a temp area: %v", roots)
	}
	seen := make(map[string]bool, len(roots))
	for _, root := range roots {
		if seen[root] {
			t.Fatalf("roots must be deduplicated: %v", roots)
		}
		seen[root] = true
		// The /tmp grant is inert on Windows (nothing canonicalizes into
		// it); every other root must be absolute.
		if runtime.GOOS == "windows" && root == "/tmp" {
			continue
		}
		if !filepath.IsAbs(root) {
			t.Fatalf("roots must be absolute: %q", root)
		}
	}
}

func TestCanonicalPathFallsBackToSpelling(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := CanonicalPath(missing); got != missing {
		t.Fatalf("missing root must come back as spelled: %q", got)
	}
	existing := t.TempDir()
	if got := CanonicalPath(existing); got == "" {
		t.Fatal("existing root must resolve")
	}
}

func TestLexicallyUnderMatchesPrefixAndEquality(t *testing.T) {
	separator := "/"
	if runtime.GOOS == "windows" {
		separator = "\\"
	}
	root := "/w" + separator + "workspace"
	if !LexicallyUnder(root, root) {
		t.Fatal("a root contains itself")
	}
	if !LexicallyUnder(root+separator+"child.txt", root) {
		t.Fatal("a child lies beneath")
	}
	// Prefix-by-directory only: /w2 must NOT match /w.
	if LexicallyUnder(root+"2"+separator+"x", root) {
		t.Fatal("directory-boundary prefix must not match")
	}
	if runtime.GOOS == "windows" {
		if !LexicallyUnder("C:\\W\\Child", "c:\\w") {
			t.Fatal("windows comparison must be case-insensitive")
		}
	} else if LexicallyUnder("/W/Child", "/w") {
		t.Fatal("unix comparison must be case-sensitive")
	}
}
