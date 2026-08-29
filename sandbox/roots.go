// Package sandbox holds the sandbox policy vocabulary shared by every
// enforcement dialect (official @deepseek-ai/dsh-sandbox): the mode words
// live with the permissionpresets knob vocabulary, and this package owns the
// writable-root derivation both the fs fence and (later) the process sandbox
// read, so "the write tool cannot write /tmp but bash can" asymmetries cannot
// arise between them.
package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"dshgo/fs"
)

// CanonicalPath resolves a granted root to the path the enforcement layer
// actually compares: canonical (symlinks resolved), because containment
// checks match resolved paths. Resolution failure returns the spelling
// as-is — a missing root matches nothing until it exists (the conservative
// outcome; inventing a fallback would grant a path the caller never named).
func CanonicalPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return resolved
}

// WritableRoots is the roots one confined execution may WRITE under — the
// mode's meaning as a canonical, deduplicated allow-list. read-only allows
// nothing; workspace-write allows the policy's workspace root, the host /tmp,
// and the per-user platform temp dir (the real temp area for mkstemp-family
// tools; omitting it would deny what the mode promises).
func WritableRoots(policy fs.SandboxExecutionPolicy) []string {
	if policy.Mode != "workspace-write" {
		return nil
	}
	candidates := []string{policy.WorkspaceRoot, "/tmp", os.TempDir()}
	roots := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		canonical := CanonicalPath(candidate)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		roots = append(roots, canonical)
	}
	return roots
}

// LexicallyUnder reports whether path equals root or lies beneath it, in the
// host's case convention. Both spellings must already be canonical.
func LexicallyUnder(path string, root string) bool {
	comparableTarget := comparablePath(path)
	comparableRoot := comparablePath(root)
	if comparableTarget == comparableRoot {
		return true
	}
	separator := "/"
	if runtime.GOOS == "windows" {
		separator = "\\"
	}
	prefix := comparableRoot
	if !strings.HasSuffix(prefix, separator) {
		prefix += separator
	}
	return strings.HasPrefix(comparableTarget, prefix)
}

// comparablePath lowercases on Windows (the only case-insensitive supported
// platform's convention).
func comparablePath(path string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
