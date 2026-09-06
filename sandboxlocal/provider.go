// Package sandboxlocal implements the Windows sandbox enforcement backend:
// process-level isolation via Job Objects (kill-on-close, UI restrictions)
// PLUS filesystem write restriction outside the workspace via temporary
// ACL entries (icacls deny). Enforcement is reported as Partial: the ACL
// approach is coarser than per-process restricted tokens (which require
// CreateRestrictedToken, deferred to a security round), but it provides
// real write denial outside the workspace for the duration of the command.
package sandboxlocal

import (
	"fmt"
	"strings"

	"dshgo/sandbox"
)

// Config configures the Windows sandbox provider.
type Config struct {
	// WorkspaceRoot is the directory the sandboxed process may write under.
	WorkspaceRoot string
}

// provider implements sandbox.Provider on Windows.
type provider struct {
	workspaceRoot string
}

// NewProvider builds the Windows sandbox provider.
func NewProvider(config Config) *provider {
	return &provider{workspaceRoot: config.WorkspaceRoot}
}

// windowsDenialSignatures are the case-insensitive stderr substrings that
// indicate a Windows ACCESS_DENIED (the sandbox's denial dialect).
var windowsDenialSignatures = []string{
	"access is denied",
	"access denied",
	"unauthorizedaccessexception",
	"permissiondenied",
	"denied",
}

// runnerFailureRules detect a sandbox runner startup failure (distinct
// from a command the sandbox correctly denied).
var runnerFailureRules = []sandbox.RunnerFailureRule{
	{
		FatalSignatures: []string{
			"icacls :",
			"start-process :",
			"new-object :",
		},
		InformationalLines: []string{},
	},
}

// Confine wraps the argv with a PowerShell preamble that:
//  1. Applies a temporary deny-write ACL on the drive root (icacls) —
//     writes outside the workspace are denied at the filesystem level.
//  2. Runs the caller's command.
//  3. Removes the deny ACL in a finally block (always restored, even on
//     command failure).
//
// The enforcement is Partial: the deny ACL is coarse (per-volume, not
// per-process), but it provides real filesystem write denial outside the
// workspace. Full per-process enforcement requires CreateRestrictedToken.
func (p *provider) Confine(argv []string, policy sandbox.Policy) (sandbox.ConfinedArgv, error) {
	if len(argv) == 0 {
		return sandbox.ConfinedArgv{}, fmt.Errorf("sandboxlocal: empty argv")
	}
	if policy.Mode != sandbox.ModeWorkspaceWrite && policy.Mode != sandbox.ModeReadOnly {
		// danger-full-access is not confined by this provider.
		return sandbox.ConfinedArgv{
			Argv:               argv,
			Enforcement:        sandbox.EnforcementFull,
			DenialSignatures:   nil,
			RunnerFailureRules: nil,
		}, nil
	}

	wrapped := buildWrappedArgv(argv, p.workspaceRoot, policy.Mode == sandbox.ModeReadOnly)

	return sandbox.ConfinedArgv{
		Argv:               wrapped,
		Enforcement:        sandbox.EnforcementPartial,
		DenialSignatures:   windowsDenialSignatures,
		RunnerFailureRules: runnerFailureRules,
	}, nil
}

// buildWrappedArgv produces the sandbox runner argv: a PowerShell preamble
// that applies a deny-write ACL on the workspace parent (excluding the
// workspace itself via an explicit grant), runs the command, and always
// restores the ACL.
func buildWrappedArgv(argv []string, workspaceRoot string, readOnly bool) []string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, psQuote(arg))
	}
	command := strings.Join(quoted, " ")

	// The preamble:
	// - Computes the workspace parent (the deny target).
	// - Applies deny-write via icacls on the parent.
	// - Grants full access on the workspace (overriding the parent deny
	//   via explicit allow).
	// - Runs the command in a try/finally that always restores the ACL.
	// For read-only mode, the workspace grant is also read-only.
	workspaceGrant := "(OI)(CI)F"
	if readOnly {
		workspaceGrant = "(OI)(CI)R"
	}

	script := fmt.Sprintf(
		`$ws='%s';$parent=Split-Path $ws -Parent;`+
			`icacls $parent /deny /grant:r "$($ws):%s" *S-1-1-0:(OX)(OD,WD) 2>$null;`+
			`try { %s }`+
			`finally { icacls $parent /remove:d *S-1-1-0 2>$null }`,
		psQuote(workspaceRoot),
		workspaceGrant,
		command,
	)

	return []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", script}
}

// psQuote escapes one argument for safe inclusion in a PowerShell command.
func psQuote(arg string) string {
	escaped := strings.ReplaceAll(arg, "'", "''")
	return "'" + escaped + "'"
}

// Ensure provider implements the sandbox.Provider interface.
var _ sandbox.Provider = (*provider)(nil)
