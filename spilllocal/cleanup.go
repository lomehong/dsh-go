package spilllocal

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
)

// defaultRootRe matches a backend-generated default root name EXACTLY —
// `dsh-spill-` plus the 6-character suffix this port's temp-root creator
// appends — so an unrelated `dsh-spill-test-*` fixture or a foreign tool's
// differently-shaped `dsh-spill-…` directory is never mistaken for a backend
// root to sweep.
var defaultRootRe = regexp.MustCompile(`^` + DefaultRootPrefix + `[A-Za-z0-9]{6}$`)

// sessionDirRe matches a backend-generated session directory name EXACTLY:
// `session-` plus the 12 lowercase hex characters sessionDir derives from
// sha256(sessionID). The sweep only descends into entries of this shape, so
// an unrelated `session-backup` directory under a shared configured root is
// never swept.
var sessionDirRe = regexp.MustCompile(`^session-[0-9a-f]{12}$`)

// resolvedRoot is an existing root resolved to one stable filesystem
// identity.
type resolvedRoot struct {
	// Path is the canonical absolute path used for the sweep.
	Path string
	// Identity de-duplicates filesystem aliases.
	Identity string
}

// WarnFn is a one-argument warning sink — the sweep's only side effect on
// failure (never throws).
type WarnFn func(message string)

// warnSafely reports a best-effort sweep failure without allowing the
// warning sink to reject cleanup.
func warnSafely(warn WarnFn, message string) {
	if warn == nil {
		return
	}
	defer func() { recover() }()
	warn(message)
}

// lstatInfo wraps os.Lstat with the entry's existence reported distinctly
// from other failures.
func lstatInfo(path string) (os.FileInfo, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, os.IsNotExist(err), err
	}
	return info, false, nil
}

// isTrustedDirectory reports whether another local OS user cannot replace
// children of this directory. POSIX ownership and mode bits have no Windows
// equivalent.
func isTrustedDirectory(info os.FileInfo) bool {
	if !info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return rootUID(info) == os.Geteuid() && info.Mode().Perm()&0o022 == 0
}

// rootIdentity is the stable identity for de-duplicating aliases of one
// root. Windows file indexes are not portable inode identities, so the
// lowercased canonical path stands in; POSIX uses device:inode.
func rootIdentity(path string, info os.FileInfo) string {
	if runtime.GOOS == "windows" {
		return idFromPath(path)
	}
	return idFromFileInfo(path, info)
}

// hasProtectedAncestors checks that no ancestor permits another local OS
// user to replace the selected child. A sticky writable ancestor is safe
// because the child is owned by the current user; this admits normal
// per-process roots below /tmp. POSIX ancestry checks have no Windows ACL
// equivalent.
func hasProtectedAncestors(path string) (bool, error) {
	if runtime.GOOS == "windows" {
		return true, nil
	}
	currentUID := os.Geteuid()
	child := path
	childInfo, _, err := lstatInfo(child)
	if err != nil {
		return false, err
	}
	for {
		parent := filepath.Dir(child)
		if parent == child {
			return true, nil
		}
		info, _, err := lstatInfo(parent)
		if err != nil {
			return false, err
		}
		if !info.IsDir() {
			return false, nil
		}
		writableByOthers := info.Mode().Perm()&0o022 != 0
		sticky := info.Mode().Perm()&0o1000 != 0
		if writableByOthers && !sticky {
			return false, nil
		}
		// Requires an ancestor owned by another OS account inside a
		// writable sticky parent; ordinary test fixtures cannot change uid.
		if writableByOthers && rootUID(childInfo) != currentUID {
			return false, nil
		}
		child = parent
		childInfo = info
	}
}

// resolveRoot resolves one existing root without admitting a directory
// another local user can replace during the path-based sweep. A configured
// root may be a symlink; discovery passes false so a symlink cannot
// impersonate a default root.
func resolveRoot(path string, allowSymlink bool, warn WarnFn) *resolvedRoot {
	initial, absent, err := lstatInfo(path)
	if err != nil {
		if !absent {
			warnSafely(warn, "spill-local: failed to inspect root "+path+": "+err.Error())
		}
		return nil
	}
	if initial.Mode()&os.ModeSymlink != 0 {
		if !allowSymlink {
			return nil
		}
	} else if !isTrustedDirectory(initial) {
		warnSafely(warn, "spill-local: skipped unsafe root "+path+": expected a directory owned by the current user and not writable by group or others")
		return nil
	}

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			warnSafely(warn, "spill-local: failed to resolve root "+path+": "+err.Error())
		}
		return nil
	}
	stats, absent, err := lstatInfo(canonical)
	if err != nil {
		if !absent {
			warnSafely(warn, "spill-local: failed to resolve root "+path+": "+err.Error())
		}
		return nil
	}
	protected, err := hasProtectedAncestors(canonical)
	if err != nil {
		if !os.IsNotExist(err) {
			warnSafely(warn, "spill-local: failed to inspect ancestors of root "+canonical+": "+err.Error())
		}
		return nil
	}
	if !isTrustedDirectory(stats) || !protected {
		warnSafely(warn, "spill-local: skipped unsafe root "+canonical+": expected a current-user-owned directory with protected write and ancestor permissions")
		return nil
	}
	return &resolvedRoot{Path: canonical, Identity: rootIdentity(canonical, stats)}
}

// SweepRoot is one root to sweep, plus whether the root itself may be pruned
// once empty. PruneWhenEmpty is set for DISCOVERED prior-default roots (one
// per past process — otherwise they accumulate empty forever), never for the
// active root the live process is still writing into. Every root prunes
// empty session directories; writes retry if that races their removal.
type SweepRoot struct {
	// Path is the absolute spill root to sweep.
	Path string
	// PruneWhenEmpty removes the root after its empty session-<hex> children
	// are pruned.
	PruneWhenEmpty bool
}

// SweepOptions parameterize sweepSpillRoots: the roots to scan, the age
// cutoff, and a failure sink.
type SweepOptions struct {
	// Roots to sweep (configured/active root and/or discovered prior-default
	// roots).
	Roots []SweepRoot
	// CutoffUnixMilli: a regular file is deleted when its mtime is strictly
	// older than this. The caller derives it from now - CleanupPeriodDays,
	// so a file written exactly at the boundary is kept (only
	// strictly-older expires).
	CutoffUnixMilli int64
	// Warn is where a contained filesystem failure is reported; the sweep
	// itself never fails.
	Warn WarnFn
}

// sweepSpillRoots is the best-effort one-shot cleanup: across each root,
// delete expired regular files under its session-<hex> directories and prune
// every empty session directory. Only a discovered prior-default root is
// itself removed. Writes recreate a session directory when pruning races a
// local write. Every filesystem and warning-sink failure is contained, so a
// caller can await this during activation/disposal without it ever failing.
func sweepSpillRoots(options SweepOptions) {
	cutoff := options.CutoffUnixMilli
	roots := map[string]SweepRoot{}
	for _, candidate := range options.Roots {
		resolved := resolveRoot(candidate.Path, false, options.Warn)
		if resolved == nil {
			continue
		}
		existing, seen := roots[resolved.Identity]
		prune := candidate.PruneWhenEmpty
		if seen {
			prune = existing.PruneWhenEmpty && prune
		}
		roots[resolved.Identity] = SweepRoot{Path: resolved.Path, PruneWhenEmpty: prune}
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			// A root that does not exist yet (no spill ever written) is the
			// common case, not an error: absence is silent, anything else is
			// reported.
			if !os.IsNotExist(err) {
				warnSafely(options.Warn, "spill-local: failed to read root "+root.Path+": "+err.Error())
			}
			continue
		}
		// Track whether the root holds ANY entry the sweep did not fully
		// reclaim, so a discovered prior-default root can be pruned only
		// when nothing remains.
		rootEmptiable := true
		for _, entry := range entries {
			// Only the backend's own session-<12 hex> directories are
			// swept; an unrelated sibling (session-backup, a stray file) is
			// left untouched and blocks pruning the root.
			if !sessionDirRe.MatchString(entry.Name()) {
				rootEmptiable = false
				continue
			}
			dir := filepath.Join(root.Path, entry.Name())
			stats, absent, err := lstatInfo(dir)
			if err != nil {
				if !absent {
					warnSafely(options.Warn, "spill-local: failed to stat "+dir+": "+err.Error())
				}
				continue
			}
			// A session-* SYMLINK must never be followed (deletion through
			// it would touch a foreign target). Only a real directory is
			// swept.
			if !isTrustedDirectory(stats) {
				warnSafely(options.Warn, "spill-local: skipped unsafe session directory "+dir)
				rootEmptiable = false
				continue
			}
			empty := sweepSessionDir(dir, cutoff, options.Warn)
			if !empty {
				rootEmptiable = false
				continue
			}
			if err := os.Remove(dir); err != nil {
				// Prune runs only on a dir observed empty; a failure here
				// means a concurrent writer added a file (ENOTEMPTY) or a
				// permission/IO fault struck — both are races outside
				// deterministic in-process testing.
				rootEmptiable = false
				if !os.IsNotExist(err) && !os.IsExist(err) {
					warnSafely(options.Warn, "spill-local: failed to prune "+dir+": "+err.Error())
				}
			}
		}
		// A discovered prior-default root (one per past process) is removed
		// once its last session dir is gone — otherwise empty roots
		// accumulate forever and every future startup rescans them. The
		// active root itself is never pruned.
		if root.PruneWhenEmpty && rootEmptiable {
			if err := os.Remove(root.Path); err != nil && !os.IsNotExist(err) && !os.IsExist(err) {
				warnSafely(options.Warn, "spill-local: failed to prune root "+root.Path+": "+err.Error())
			}
		}
	}
}

// sweepSessionDir sweeps one spill session directory: delete expired regular
// files, skip everything else, and report the directory empty afterward so
// the caller can prune it. The dir entry MUST be a real directory — the
// caller lstats it first and skips a symlink, so this never follows a
// session-* symlink into a foreign tree. Inside, a symlink or any
// non-regular entry is left untouched — Lstat never follows a link, so a
// planted symlink can neither be deleted nor redirect the age check. Every
// per-entry failure is contained: one unreadable file does not abort the
// directory. It reports true when the directory holds no entries after the
// sweep (a prune candidate).
func sweepSessionDir(dir string, cutoff int64, warn WarnFn) bool {
	names, err := os.ReadDir(dir)
	if err != nil {
		// The caller lstat'd this entry and confirmed a real directory just
		// before the call, so ReadDir fails only when the dir races away or
		// a permission/IO fault strikes in that window. False keeps it out
		// of the prune step.
		warnSafely(warn, "spill-local: failed to read "+dir+": "+err.Error())
		return false
	}
	remaining := len(names)
	for _, entry := range names {
		path := filepath.Join(dir, entry.Name())
		stats, absent, err := lstatInfo(path)
		if err != nil {
			// An entry ReadDir just returned fails to lstat only by racing
			// away or a permission/IO fault; keep it out of the
			// deterministic test surface.
			if absent {
				remaining--
				continue
			}
			warnSafely(warn, "spill-local: failed to stat "+path+": "+err.Error())
			continue
		}
		// Only regular files expire. Symlinks and other special entries are
		// skipped (never followed) so the sweep cannot be redirected or
		// delete a link.
		if !stats.Mode().IsRegular() {
			continue
		}
		if stats.ModTime().UnixMilli() >= cutoff {
			continue
		}
		unlinkIdempotent(path, warn)
		remaining--
	}
	return remaining == 0
}

// unlinkIdempotent deletes a single path, treating a concurrent-race
// disappearance as success. A parallel process (or another sweep) may unlink
// the same file between our scan and our own unlink — absence then means the
// goal (file gone) already holds, so it is not a failure. Any other error is
// reported and swallowed.
func unlinkIdempotent(path string, warn WarnFn) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		warnSafely(warn, "spill-local: failed to delete "+path+": "+err.Error())
	}
}

// DiscoverDefaultRoots discovers trusted prior default spill roots below
// base: the `dsh-spill-<6 alnum>` directories that earlier default-root runs
// created. Matching is the EXACT shape, not the bare prefix, so an unrelated
// `dsh-spill-test-*` fixture or a foreign differently-shaped directory is
// never swept; symlinks and non-directories are excluded too. base defaults
// to the OS temp directory (a test seam).
func DiscoverDefaultRoots(warn WarnFn, base string) []string {
	var paths []string
	for _, root := range discoverDefaultRootRecords(warn, base) {
		paths = append(paths, root.Path)
	}
	return paths
}

// discoverDefaultRootRecords scans base for backend-shaped default roots.
func discoverDefaultRootRecords(warn WarnFn, base string) []resolvedRoot {
	entries, err := os.ReadDir(base)
	if err != nil {
		warnSafely(warn, "spill-local: failed to scan "+base+" for default roots: "+err.Error())
		return nil
	}
	var roots []resolvedRoot
	for _, entry := range entries {
		if !defaultRootRe.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(base, entry.Name())
		if resolved := resolveRoot(path, false, warn); resolved != nil {
			roots = append(roots, *resolved)
		}
	}
	return roots
}

// GatherSweepRoots gathers and de-duplicates the trusted roots for one
// startup sweep. The active configured path may be a symlink; its resolved
// identity overrides a matching discovered root so the live target is never
// marked prunable. defaultRootsBase defaults to the OS temp directory.
func GatherSweepRoots(activeRoot string, warn WarnFn, defaultRootsBase string) []SweepRoot {
	if defaultRootsBase == "" {
		defaultRootsBase = os.TempDir()
	}
	var roots map[string]SweepRoot
	// The gather point is the sweep's one async seam; the port resolves
	// synchronously, so a mutex only guards the map across the two passes.
	var mu sync.Mutex
	roots = map[string]SweepRoot{}
	for _, root := range discoverDefaultRootRecords(warn, defaultRootsBase) {
		mu.Lock()
		roots[root.Identity] = SweepRoot{Path: root.Path, PruneWhenEmpty: true}
		mu.Unlock()
	}
	if active := resolveRoot(activeRoot, true, warn); active != nil {
		mu.Lock()
		roots[active.Identity] = SweepRoot{Path: active.Path, PruneWhenEmpty: false}
		mu.Unlock()
	}
	out := make([]SweepRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, root)
	}
	return out
}
