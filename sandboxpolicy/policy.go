// Package sandboxpolicy is the sandbox POLICY home (official
// @deepseek-ai/dsh-sandbox-policy): one owner of the deployment's fallback
// mode and workspace root plus the per-call resolution the enforcing
// filesystem and shell backends read. Session overrides ride the
// `sandbox/mode` knob events (permissionpresets owns the vocabulary and the
// fold); this service applies the precedence.
package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"

	"dshgo/fs"
	"dshgo/sandbox"
)

// Config is the deployment's sandbox default. All optional: an empty mode is
// the fail-safe read-only default; an empty workspace root falls back to the
// process cwd at construction, always stored absolute.
type Config struct {
	// Mode is the file-sandbox mode a session starts from.
	Mode string
	// WorkspaceRoot is the fallback root for agentless calls and sessions
	// without a cwd.
	WorkspaceRoot string
}

// Service owns the deployment default mode and fallback workspace root.
// Executors and providers stay session-free: resolution takes explicit
// per-call inputs.
type Service struct {
	defaultMode   string
	workspaceRoot string
}

// NewService validates the config and resolves the fallback root absolute.
func NewService(config Config, fallbackCwd string) (*Service, error) {
	mode := config.Mode
	if mode == "" {
		mode = permissionReadOnly
	}
	if mode != permissionReadOnly && mode != permissionWorkspaceWrite && mode != permissionDangerFullAccess {
		return nil, fmt.Errorf("sandboxpolicy: unknown sandbox mode %q", mode)
	}
	root := config.WorkspaceRoot
	if root == "" {
		root = fallbackCwd
		if root == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, fmt.Errorf("sandboxpolicy: resolve fallback workspace root: %w", err)
			}
			root = cwd
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("sandboxpolicy: resolve workspace root: %w", err)
	}
	return &Service{defaultMode: mode, workspaceRoot: sandbox.CanonicalPath(absolute)}, nil
}

// The mode words (the same vocabulary the permissionpresets knob events
// carry; mirrored here because the policy service is the resolution home).
const (
	permissionReadOnly         = "read-only"
	permissionWorkspaceWrite   = "workspace-write"
	permissionDangerFullAccess = "danger-full-access"
)

// DefaultMode exposes the deployment default — the capability fact the tool
// layer reads to advertise escalation.
func (s *Service) DefaultMode() string { return s.defaultMode }

// WorkspaceRoot exposes the absolute fallback root.
func (s *Service) WorkspaceRoot() string { return s.workspaceRoot }

// Resolve resolves the complete policy for one capability call. An approved
// explicit mode outranks the session's last `sandbox/mode` override, which
// outranks the deployment default. A session cwd is its workspace-write
// boundary; the configured root is the fallback for agentless calls and
// sessions without a cwd. Empty arguments mean absent.
func (s *Service) Resolve(sessionCwd string, sessionOverride string, approvedMode string) fs.SandboxExecutionPolicy {
	mode := s.defaultMode
	if sessionOverride != "" {
		mode = sessionOverride
	}
	if approvedMode != "" {
		mode = approvedMode
	}
	root := s.workspaceRoot
	if sessionCwd != "" {
		if absolute, err := filepath.Abs(sessionCwd); err == nil {
			root = sandbox.CanonicalPath(absolute)
		}
	}
	return fs.SandboxExecutionPolicy{Mode: mode, WorkspaceRoot: root}
}
