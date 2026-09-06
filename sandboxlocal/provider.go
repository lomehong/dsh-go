// Package sandboxlocal is the Windows slot for the sandbox provider seam.
//
// Status: FAIL-CLOSED. The icacls preamble experiment (deny-write on the
// workspace parent) was reverted: a deny ACE on the parent directory is a
// SYSTEM-WIDE effect (every process of the user loses write access for the
// command”s duration, not just the confined child), and a killed runner
// between deny and cleanup leaves the deny ACE permanently stuck. Both are
// disqualifying for a security component.
//
// The correct per-process path is CreateRestrictedToken (Win32, not
// exposed by x/sys/windows — requires a manual syscall bridge) or a Job
// Object runner. That is a dedicated security round (ROADMAP: sandbox
// family). Until it lands, Confine fails closed with SANDBOX_UNAVAILABLE —
// the official semantics for "missing confinement refuses rather than
// runs unconfined".
package sandboxlocal

import (
	"dshgo/sandbox"
)

// Config is accepted for seam parity with the future real provider; the
// fail-closed provider ignores it.
type Config struct {
	WorkspaceRoot string
}

// provider fails closed for every confining mode.
type provider struct {
	workspaceRoot string
}

// NewProvider returns the fail-closed Windows provider.
func NewProvider(config Config) *provider {
	return &provider{workspaceRoot: config.WorkspaceRoot}
}

// Confine always refuses with SANDBOX_UNAVAILABLE for confining modes: no
// per-process enforcement backend exists on Windows in this build. A
// danger-full-access policy is not confinement and passes through
// unmodified (full enforcement of "no confinement").
func (p *provider) Confine(argv []string, policy sandbox.Policy) (sandbox.ConfinedArgv, error) {
	if policy.Mode == sandbox.ModeDangerFullAccess {
		return sandbox.ConfinedArgv{
			Argv:        argv,
			Enforcement: sandbox.EnforcementFull,
		}, nil
	}
	return sandbox.ConfinedArgv{}, &sandbox.UnavailableError{
		Mode: policy.Mode,
		Detail: "windows per-process enforcement (CreateRestrictedToken / Job Object runner) " +
			"is not implemented in this build; the icacls preamble experiment was reverted " +
			"(system-wide deny side effect + ACE residue on kill)",
	}
}

// Ensure provider implements the sandbox.Provider interface.
var _ sandbox.Provider = (*provider)(nil)
