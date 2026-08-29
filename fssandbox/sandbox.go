// Package fssandbox is the sandbox-enforcing filesystem backend (official
// @deepseek-ai/dsh-fs-sandbox): all text-storage mechanics stay the local
// implementation's verbatim; this package adds only the per-call POLICY fence
// on the two mutations. Reads pass through untouched: every mode permits
// reading.
//
// The fence is a policy check in trusted code over a model-controlled path,
// not a kernel boundary: only the target path is untrusted, so
// canonicalize-then-contain is the complete answer to this surface. This is
// containment, not a security boundary; kernel-grade isolation of untrusted
// code stays the shell sandbox's job. The residual TOCTOU (an ancestor
// symlink swapped between the containment re-check and the syscall) is
// narrowed by re-canonicalizing immediately before delegating and is accepted
// for this threat model.
package fssandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/sandbox"
	"dshgo/sandboxpolicy"
)

// Sandboxed wraps one local backend with the mutation fence. It satisfies
// fs.FileSystem; loading it INSTEAD of the bare local backend, together with
// a sandboxpolicy Service, is the whole swap — model-facing tools are
// untouched.
type Sandboxed struct {
	*fslocal.Local
	policy *sandboxpolicy.Service
}

// New composes the enforcing backend over one local backend and the policy
// service (the official plugin injects only `sandboxPolicy` and constructs
// the local from its own config).
func New(local *fslocal.Local, policy *sandboxpolicy.Service) *Sandboxed {
	return &Sandboxed{Local: local, policy: policy}
}

// SandboxMode exposes the deployment default mode — the capability fact the
// tool layer reads to advertise escalation and to require the policy
// resolver.
func (s *Sandboxed) SandboxMode() string { return s.policy.DefaultMode() }

// WriteText fences the write by the per-call policy, then delegates to the
// inherited atomic write with the EXACT checked target.
func (s *Sandboxed) WriteText(ctx context.Context, target fs.Target, content string, expected *fs.WriteIntent, sandboxExecutionPolicy *fs.SandboxExecutionPolicy) (fs.WriteOutcome, error) {
	checked, err := s.checkedTarget(target, sandboxExecutionPolicy)
	if err != nil {
		return fs.WriteOutcome{}, err
	}
	return s.Local.WriteText(ctx, checked, content, expected, sandboxExecutionPolicy)
}

// EditText fences the edit by the per-call policy, then delegates to the
// inherited atomic edit with the EXACT checked target.
func (s *Sandboxed) EditText(ctx context.Context, target fs.Target, edit fs.EditRequest, expected *fs.Version, sandboxExecutionPolicy *fs.SandboxExecutionPolicy) (fs.EditOutcome, error) {
	checked, err := s.checkedTarget(target, sandboxExecutionPolicy)
	if err != nil {
		return fs.EditOutcome{}, err
	}
	return s.Local.EditText(ctx, checked, edit, expected, sandboxExecutionPolicy)
}

// checkedTarget enforces the per-call policy against target and returns the
// EXACT target the mutation must use, so the checked identity is the mutated
// one (no check-here-write-there TOCTOU). read-only denies; workspace-write
// re-canonicalizes NOW and requires containment under a writable root;
// danger-full-access returns the caller's target unfenced. A nil policy
// resolves the deployment fallback.
func (s *Sandboxed) checkedTarget(target fs.Target, sandboxExecutionPolicy *fs.SandboxExecutionPolicy) (fs.Target, error) {
	policy := sandboxExecutionPolicy
	if policy == nil {
		fallback := s.policy.Resolve("", "", "")
		policy = &fallback
	}
	switch policy.Mode {
	case "danger-full-access":
		return target, nil
	case "read-only":
		return fs.Target{}, fs.NewError(fs.CodeSandboxDenied, fmt.Sprintf("cannot write %q: file access denied under read-only mode", target.DisplayPath), nil)
	}
	// workspace-write: containment on the FRESH canonical path (catches a
	// symlink ancestor swapped since the tool resolved this target), and the
	// mutation delegates with THIS fresh target — never the stale one.
	fresh, err := s.Local.Resolve(context.Background(), target.DisplayPath, "")
	if err != nil {
		return fs.Target{}, err
	}
	for _, root := range sandbox.WritableRoots(*policy) {
		if isPathUnder(string(fresh.Key), root) {
			return fresh, nil
		}
	}
	return fs.Target{}, fs.NewError(fs.CodeSandboxDenied, fmt.Sprintf("cannot write %q: file access denied under workspace-write mode", target.DisplayPath), nil)
}

// isPathUnder determines whether a canonical target is a writable root or
// lies beneath it. The lexical fast path handles normal canonical spellings;
// when spellings differ, walk the target's existing ancestors and compare
// filesystem identity with the root, recognizing Windows 8.3 aliases and
// casing without weakening containment to a textual approximation.
func isPathUnder(path string, root string) bool {
	if sandbox.LexicallyUnder(path, root) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	ancestor := path
	for {
		if info, err := os.Stat(ancestor); err == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return false
		}
		ancestor = parent
	}
}
