// Package fslocal is the host-filesystem implementation of the fs seam
// (official @deepseek-ai/dsh-fs-local). Realpath-derived target identity
// makes aliases share stale guards, and writes through a symlink update its
// target without replacing the link. Reads resolve relative paths from the
// configured cwd (a resolution default, NOT a containment boundary — enforce
// containment with a stricter backend or an execute permission plugin).
package fslocal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"dshgo/fs"
)

// Config is the local backend configuration.
type Config struct {
	// Cwd is the base directory for relative paths; empty defaults to the
	// process working directory.
	Cwd string
	// DiffBasisMaxBytes is the exclusive UTF-8 byte limit on each
	// overwrite-diff side. Zero applies the 10 MiB default.
	DiffBasisMaxBytes int64
}

const defaultDiffBasisMaxBytes = int64(10 * 1024 * 1024)

// Local is the host-filesystem backend.
type Local struct {
	cwd             string
	diffBasisBytes  int64
	locks           map[string]*sync.Mutex
	lockMu          sync.Mutex
	nowVersionExtra func(path string) string
}

// New builds one local backend and validates the configuration fail loud.
func New(config Config) (*Local, error) {
	cwd := config.Cwd
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("fslocal: resolve working directory: %w", err)
		}
		cwd = wd
	}
	basis := config.DiffBasisMaxBytes
	if basis == 0 {
		basis = defaultDiffBasisMaxBytes
	}
	if basis <= 0 {
		return nil, fmt.Errorf("fslocal: diffBasisMaxBytes must be a positive integer")
	}
	return &Local{
		cwd:            cwd,
		diffBasisBytes: basis,
		locks:          map[string]*sync.Mutex{},
	}, nil
}

// withLock runs op with exclusive access to one target key (serialized per
// key) so the read→guard→write window cannot interleave: concurrent writes
// and edits are deterministically ordered — one wins, the rest see the new
// version and reject as stale.
func (l *Local) withLock(key string, op func() error) error {
	l.lockMu.Lock()
	mu, ok := l.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		l.locks[key] = mu
	}
	l.lockMu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return op()
}

// versionOf derives the opaque version token from high-resolution identity
// and freshness metadata. The portable os.FileInfo surface exposes no dev/ino
// and no creation time, so this build's token degrades to size+mtime; the
// token stays opaque, so the degradation is invisible to consumers as long as
// every backend in one process derives it the same way.
func versionOf(info os.FileInfo) fs.Version {
	return fs.Version(fmt.Sprintf("0:0:%d:%d:%d", info.Size(), info.ModTime().UnixNano(), 0))
}

// resolveLocal resolves a model-supplied path: the display path is the cwd-
// joined absolute path; the target key prefers the file's own realpath so a
// symlinked file resolves to its target. An absent file realpaths the nearest
// existing ancestor and re-appends the missing suffix, so the key is stable
// across creation of those dirs.
func resolveLocal(cwd string, path string) (fs.Target, error) {
	if strings.TrimSpace(path) == "" {
		return fs.Target{}, fs.NewError(fs.CodeNotFound, "file_path must be a non-empty string", nil)
	}
	display := path
	if !filepath.IsAbs(display) {
		display = filepath.Join(cwd, path)
	}
	display = filepath.Clean(display)
	if resolved, err := filepath.EvalSymlinks(display); err == nil {
		return fs.Target{Key: fs.TargetKey(resolved), DisplayPath: display}, nil
	} else if !os.IsNotExist(err) {
		// A parent path segment is not a directory (or another hard
		// fault): the target can neither exist nor be created — surface
		// the structured taxonomy instead of a raw errno.
		if _, statErr := os.Lstat(display); statErr != nil && !os.IsNotExist(statErr) {
			return fs.Target{}, fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot resolve %q: a parent path segment is not a directory", display), err)
		}
	}
	// File absent: walk up to the nearest existing ancestor.
	missing := []string{}
	ancestor := display
	for {
		if resolved, err := filepath.EvalSymlinks(ancestor); err == nil {
			key := resolved
			for i := len(missing) - 1; i >= 0; i-- {
				key = filepath.Join(key, missing[i])
			}
			return fs.Target{Key: fs.TargetKey(key), DisplayPath: display}, nil
		}
		missing = append(missing, filepath.Base(ancestor))
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fs.Target{}, fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot resolve %q: no existing ancestor", display), nil)
		}
		ancestor = parent
	}
}

// Resolve implements fs.FileSystem.
func (l *Local) Resolve(ctx context.Context, path string, cwd string) (fs.Target, error) {
	if err := ctx.Err(); err != nil {
		return fs.Target{}, fs.NewError(fs.CodeAborted, "resolve aborted", err)
	}
	base := cwd
	if base == "" {
		base = l.cwd
	}
	return resolveLocal(base, path)
}

// ProcessPath implements fs.FileSystem.
func (l *Local) ProcessPath(target fs.Target) string { return string(target.Key) }

// ProcessPathFromHostPath implements fs.FileSystem: a host-backed backend.
func (l *Local) ProcessPathFromHostPath(hostPath string) string {
	if filepath.IsAbs(hostPath) {
		return filepath.Clean(hostPath)
	}
	return ""
}

// FileURL implements fs.FileSystem.
func (l *Local) FileURL(target fs.Target) string {
	p := l.ProcessPath(target)
	return "file://" + filepath.ToSlash(p)
}

// Contains implements fs.FileSystem.
func (l *Local) Contains(parent fs.Target, child fs.Target) bool {
	rel, err := filepath.Rel(string(parent.Key), string(child.Key))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// Stat implements fs.FileSystem.
func (l *Local) Stat(ctx context.Context, target fs.Target) (*fs.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, fs.NewError(fs.CodeAborted, "stat aborted", err)
	}
	info, err := os.Stat(string(target.Key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot stat %q", target.DisplayPath), err)
	}
	return probeToInfo(string(target.Key), info), nil
}

// Lstat implements fs.FileSystem.
func (l *Local) Lstat(ctx context.Context, path string, cwd string) (*fs.PathInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fs.NewError(fs.CodeAborted, "lstat aborted", err)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fs.NewError(fs.CodeNotFound, "file_path must be a non-empty string", nil)
	}
	base := cwd
	if base == "" {
		base = l.cwd
	}
	full := path
	if !filepath.IsAbs(full) {
		full = filepath.Join(base, path)
	}
	info, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot lstat %q", full), err)
	}
	probe := probeToInfo(full, info)
	return &fs.PathInfo{
		Version: probe.Version,
		Type:    lstatType(info),
		Size:    probe.Size,
	}, nil
}

// ReadText implements fs.FileSystem: full decode with binary rejection.
func (l *Local) ReadText(ctx context.Context, target fs.Target) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fs.NewError(fs.CodeAborted, "read aborted", err)
	}
	raw, err := os.ReadFile(string(target.Key))
	if err != nil {
		return "", readError(target.DisplayPath, err)
	}
	if bytes.ContainsRune(raw, 0) {
		return "", fs.NewError(fs.CodeNotText, fmt.Sprintf("cannot read %q: binary file", target.DisplayPath), nil)
	}
	if !utf8.Valid(raw) {
		return "", fs.NewError(fs.CodeNotText, fmt.Sprintf("cannot read %q: invalid UTF-8", target.DisplayPath), nil)
	}
	return string(raw), nil
}

// StreamText implements fs.FileSystem: decoded 64 KiB chunks with the same
// binary rejection as ReadText, owned by the backend.
func (l *Local) StreamText(ctx context.Context, target fs.Target) (func() (string, bool), error) {
	file, err := os.Open(string(target.Key))
	if err != nil {
		return nil, readError(target.DisplayPath, err)
	}
	buf := make([]byte, 64*1024)
	var carry []byte
	var done bool
	return func() (string, bool) {
		if done {
			return "", false
		}
		for {
			if err := ctx.Err(); err != nil {
				done = true
				file.Close()
				return "", false
			}
			n, readErr := file.Read(buf)
			if n > 0 {
				chunk := append(carry, buf[:n]...)
				carry = nil
				if bytes.ContainsRune(chunk, 0) {
					done = true
					file.Close()
					return "", false
				}
				if !utf8.Valid(chunk) {
					// Hold back a possibly-split trailing rune.
					keep := trailingPartialBytes(chunk)
					carry = append([]byte{}, chunk[len(chunk)-keep:]...)
					chunk = chunk[:len(chunk)-keep]
				}
				return string(chunk), true
			}
			if readErr != nil {
				done = true
				file.Close()
				return "", false
			}
		}
	}, nil
}

// ReadBytes implements fs.FileSystem with the inclusive byte cap enforced on
// the opened descriptor: a file discovered above maxBytes fails with
// CodeTooLarge instead of truncating.
func (l *Local) ReadBytes(ctx context.Context, target fs.Target, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fs.NewError(fs.CodeAborted, "read aborted", err)
	}
	file, err := os.Open(string(target.Key))
	if err != nil {
		return nil, readError(target.DisplayPath, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot stat %q", target.DisplayPath), err)
	}
	if info.Size() > maxBytes {
		return nil, fs.NewError(fs.CodeTooLarge, fmt.Sprintf("cannot read %q: file exceeds the %d byte cap", target.DisplayPath, maxBytes), nil)
	}
	raw := make([]byte, info.Size())
	if _, err := io.ReadFull(file, raw); err != nil && info.Size() > 0 {
		return nil, fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot read %q", target.DisplayPath), err)
	}
	return raw, nil
}

// ListDir implements fs.FileSystem: direct children in stable name order,
// metadata and resolved targets only.
func (l *Local) ListDir(ctx context.Context, target fs.Target) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, fs.NewError(fs.CodeAborted, "list aborted", err)
	}
	entries, err := os.ReadDir(string(target.Key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fs.NewError(fs.CodeNotFound, fmt.Sprintf("cannot list %q: directory not found", target.DisplayPath), err)
		}
		return nil, fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot list %q", target.DisplayPath), err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		childPath := filepath.Join(string(target.Key), entry.Name())
		childType := fs.TypeOther
		if entry.IsDir() {
			childType = fs.TypeDirectory
		} else if entry.Type().IsRegular() {
			childType = fs.TypeFile
		}
		child := fs.Target{Key: fs.TargetKey(childPath), DisplayPath: childPath}
		if resolved, err := filepath.EvalSymlinks(childPath); err == nil {
			child.Key = fs.TargetKey(resolved)
		}
		row := fs.DirEntry{Name: entry.Name(), Type: childType, Target: child}
		if info, err := entry.Info(); err == nil {
			probe := probeToInfo(childPath, info)
			row.Version = probe.Version
			if childType == fs.TypeFile {
				size := info.Size()
				row.Size = &size
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// WriteText implements fs.FileSystem: guards, optional contextual-diff basis,
// then an atomic publication inside the per-target lock.
func (l *Local) WriteText(ctx context.Context, target fs.Target, content string, expected *fs.WriteIntent, sandboxPolicy *fs.SandboxExecutionPolicy) (fs.WriteOutcome, error) {
	var outcome fs.WriteOutcome
	err := l.withLock(string(target.Key), func() error {
		if err := ctx.Err(); err != nil {
			return fs.NewError(fs.CodeAborted, "write aborted", err)
		}
		info, statErr := os.Stat(string(target.Key))
		existing := statErr == nil
		if existing && !info.Mode().IsRegular() {
			return fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot write %q: not a regular file", target.DisplayPath), nil)
		}
		if expected != nil && expected.Kind == fs.IntentReplaceIfVersion {
			if !existing {
				return fs.NewError(fs.CodeStaleVersion, fmt.Sprintf("cannot write %q: file no longer exists", target.DisplayPath), nil)
			}
			if versionOf(info) != expected.Version {
				return fs.NewError(fs.CodeStaleVersion, fmt.Sprintf("cannot write %q: file changed since it was read", target.DisplayPath), nil)
			}
		} else if expected != nil && expected.Kind == fs.IntentCreateIfAbsent && existing {
			// createIfAbsent onto an existing file: a blind overwrite —
			// require a read first.
			return fs.NewError(fs.CodeNotObserved, fmt.Sprintf("cannot overwrite existing %q without reading it first", target.DisplayPath), nil)
		}
		// No expectation means an unconditional but still atomic write.

		// Capture an optional contextual-diff basis before the write.
		// Binary, invalid UTF-8, either side at/above the limit, or a
		// file deleted after the preflight yields Before: nil; consumers
		// retain their whole-file fallback.
		var before *string
		if existing && int64(len(content)) < l.diffBasisBytes {
			if basis, basisErr := readDiffBasis(string(target.Key), l.diffBasisBytes); basisErr == nil {
				normalized := normalizeLineEndings(string(basis))
				before = &normalized
			}
		}
		mode := os.FileMode(0644)
		if existing {
			mode = info.Mode().Perm()
		}
		if err := writeFileAtomic(string(target.Key), content, mode); err != nil {
			return err
		}
		after, err := os.Stat(string(target.Key))
		if err != nil {
			return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot verify %q after write", target.DisplayPath), err)
		}
		operation := "create"
		if existing {
			operation = "update"
		}
		outcome = fs.WriteOutcome{
			Operation: operation,
			Version:   versionOf(after),
			Before:    before,
			// LF-normalized to share the diff basis with Before (also
			// LF): a CRLF overwrite must not read as every line changed.
			After: normalizeLineEndings(content),
		}
		return nil
	})
	return outcome, err
}

// EditText implements fs.FileSystem: version guard BEFORE literal matching
// (a stale edit reports CodeStaleVersion, not edit-not-found against newer
// content), then one critical section read→match→write.
func (l *Local) EditText(ctx context.Context, target fs.Target, edit fs.EditRequest, expected *fs.Version, sandboxPolicy *fs.SandboxExecutionPolicy) (fs.EditOutcome, error) {
	var outcome fs.EditOutcome
	err := l.withLock(string(target.Key), func() error {
		if err := ctx.Err(); err != nil {
			return fs.NewError(fs.CodeAborted, "edit aborted", err)
		}
		info, statErr := os.Stat(string(target.Key))
		if statErr != nil {
			// Missing targets use the same stale code on guarded and
			// unconditional edit paths.
			return fs.NewError(fs.CodeStaleVersion, fmt.Sprintf("cannot edit %q: file changed since it was read", target.DisplayPath), statErr)
		}
		if !info.Mode().IsRegular() {
			return fs.NewError(fs.CodeNotRegularFile, fmt.Sprintf("cannot edit %q: not a regular file", target.DisplayPath), nil)
		}
		if expected != nil && versionOf(info) != *expected {
			return fs.NewError(fs.CodeStaleVersion, fmt.Sprintf("cannot edit %q: file changed since it was read", target.DisplayPath), nil)
		}
		raw, err := os.ReadFile(string(target.Key))
		if err != nil {
			return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot read %q for edit", target.DisplayPath), err)
		}
		if bytes.ContainsRune(raw, 0) {
			return fs.NewError(fs.CodeNotText, fmt.Sprintf("cannot edit %q: binary file", target.DisplayPath), nil)
		}
		if !utf8.Valid(raw) {
			return fs.NewError(fs.CodeNotText, fmt.Sprintf("cannot edit %q: invalid UTF-8", target.DisplayPath), nil)
		}
		content := normalizeLineEndings(string(raw))
		edited, replacements, editErr := applyLiteralEdit(content, edit, target.DisplayPath)
		if editErr != nil {
			return editErr
		}
		if err := writeFileAtomic(string(target.Key), edited, info.Mode().Perm()); err != nil {
			return err
		}
		after, err := os.Stat(string(target.Key))
		if err != nil {
			return fs.NewError(fs.CodeIOError, fmt.Sprintf("cannot verify %q after edit", target.DisplayPath), err)
		}
		outcome = fs.EditOutcome{
			Version: versionOf(after),
			Before:  content,
			After:   edited,
		}
		_ = replacements
		return nil
	})
	return outcome, err
}

// SandboxMode implements fs.FileSystem: the bare local backend never
// confines by default.
func (l *Local) SandboxMode() string { return "" }

// Cwd exposes the backend's resolution base (tests and diagnostics).
func (l *Local) Cwd() string { return l.cwd }
