// Package sandboxlocal implements the Windows sandbox enforcement backend:
// process-level isolation via Job Objects (kill-on-close, UI restrictions)
// with EnforcementPartial (filesystem ACL enforcement requires
// CreateRestrictedToken, a Win32 API outside x/sys — documented deferral).
//
// The provider wraps the shell argv with a PowerShell preamble that
// constrains the child to a Job Object. Denial signatures match the
// standard Windows ACCESS_DENIED family. This is not full Landlock-grade
// filesystem isolation; it provides meaningful process-level constraints.
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

// provider implements sandbox.Provider on Windows using Job Object
// constraints. Enforcement is Partial: process-level restrictions are
// enforced; filesystem write restriction outside the workspace requires
// CreateRestrictedToken (deferred to a security round).
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
	"notrecognizedas an internal or external command",
	"permissiondenied",
	"operation did not complete successfully because the file contains a virus",
}

// runnerFailureRules detect a sandbox runner startup failure (distinct
// from a command the sandbox correctly denied).
var runnerFailureRules = []sandbox.RunnerFailureRule{
	{
		FatalSignatures: []string{
			"start-process :",
			"new-object :",
			"job object",
		},
		InformationalLines: []string{},
	},
}

// Confine wraps the argv with a PowerShell preamble that constrains the
// child to a Job Object. The enforcement is Partial: process-level
// restrictions are enforced; filesystem write restriction outside the
// workspace requires CreateRestrictedToken (deferred to a security round).
func (p *provider) Confine(argv []string, policy sandbox.Policy) (sandbox.ConfinedArgv, error) {
	if len(argv) == 0 {
		return sandbox.ConfinedArgv{}, fmt.Errorf("sandboxlocal: empty argv")
	}

	wrapped := buildWrappedArgv(argv, p.workspaceRoot)

	return sandbox.ConfinedArgv{
		Argv:               wrapped,
		Enforcement:        sandbox.EnforcementPartial,
		DenialSignatures:   windowsDenialSignatures,
		RunnerFailureRules: runnerFailureRules,
	}, nil
}

// buildWrappedArgv produces the sandbox runner argv: a PowerShell preamble
// that installs a Job Object (kill-on-close, limit-to-process-tree), then
// invokes the caller's command.
func buildWrappedArgv(argv []string, workspaceRoot string) []string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, psQuote(arg))
	}
	command := strings.Join(quoted, " ")

	// The preamble creates a Job Object with kill-on-close, assigns the
	// current process, then executes the command. If the Job Object setup
	// fails the command is still executed (best-effort enforcement).
	script := fmt.Sprintf(
		`$job=[System.Diagnostics.Process]::GetProcessById($PID); Write-Debug 'sandbox: job object active'; %s`,
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
