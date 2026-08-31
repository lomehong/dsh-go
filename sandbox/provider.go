package sandbox

import (
	"fmt"
)

// Mode is the file-effect policy for confined processes (official
// SandboxMode): read-only permits only required sinks such as /dev/null;
// workspace-write also permits the workspace and a backend-defined temp
// area; danger-full-access bypasses confinement. Network and process
// visibility are outside this vocabulary.
type Mode string

const (
	ModeReadOnly         Mode = "read-only"
	ModeWorkspaceWrite   Mode = "workspace-write"
	ModeDangerFullAccess Mode = "danger-full-access"
)

// ConfinedMode is a confining (non-danger-full-access) mode — the modes a
// Policy can carry.
type ConfinedMode = Mode

// ExecutionPolicy is the complete file-effect policy resolved for one
// capability call. The root is carried even under modes that do not consume
// it so callers can resolve policy once before choosing the enforcement
// path.
type ExecutionPolicy struct {
	// Mode is the file-effect mode this execution runs under.
	Mode Mode
	// WorkspaceRoot is the absolute root directory workspace-write may write
	// under.
	WorkspaceRoot string
	// SessionID is the opaque identity of the calling session; absent for
	// agentless calls, which fall back to per-call backend state.
	SessionID *string
}

// Policy is what one confined execution is allowed to touch, carried per
// call — two consumers may confine under different policies at the same
// instant, and an approved escalated retry is a new call with a wider
// policy. The provider treats the policy as fully specified.
type Policy struct {
	ExecutionPolicy
	// Mode is a confining mode only.
	Mode ConfinedMode
}

// Enforcement is how completely the selected backend enforces the policy's
// file effects. Partial means an active backend or older kernel ABI cannot
// govern every promised file effect; callers requiring an absolute boundary
// must not treat it as full.
type Enforcement string

const (
	EnforcementFull    Enforcement = "full"
	EnforcementPartial Enforcement = "partial"
)

// RunnerFailureRule is evidence that identifies a sandbox runner failing
// before it executes the wrapped command. A consumer first applies
// AllowedExitCodes when present, removes InformationalLines by
// case-insensitive exact line equality, then matches FatalSignatures
// case-insensitively within each remaining stderr line. Exit status alone
// never proves runner failure.
type RunnerFailureRule struct {
	// AllowedExitCodes is the nonzero process exit codes on which this rule
	// may match; nil permits any nonzero exit.
	AllowedExitCodes []int
	// FatalSignatures is the non-empty substrings identifying a fatal
	// runner diagnostic on one stderr line.
	FatalSignatures []string
	// InformationalLines is the benign stderr lines excluded by exact
	// full-line equality before fatal matching.
	InformationalLines []string
}

// ConfinedArgv is a SandboxProvider.Confine result: the argv to spawn in
// place of the caller's own, plus the enforcement completeness the selected
// backend achieves for it.
type ConfinedArgv struct {
	// Argv is the wrapped argv (runner, profile, separator, then the
	// caller's argv).
	Argv []string
	// Enforcement is how completely the selected backend enforces the
	// policy's file effects.
	Enforcement Enforcement
	// DenialSignatures is the selected backend's denial DIALECT: the
	// case-insensitive stderr substrings a file effect denied by THIS
	// backend produces. A consumer that infers denials from a failed run's
	// stderr matches against exactly these rather than a cross-backend
	// union.
	DenialSignatures []string
	// RunnerFailureRules is the structured runner-failure evidence rules.
	RunnerFailureRules []RunnerFailureRule
}

// UnavailableCode is the error code for a requested confined mode when no
// backend is usable; the provider fails closed (official
// SANDBOX_UNAVAILABLE).
const UnavailableCode = "SANDBOX_UNAVAILABLE"

// UnavailableError is thrown when a provider cannot enforce the requested
// mode; it carries UnavailableCode through the structured error channel.
type UnavailableError struct {
	Mode   ConfinedMode
	Detail string
}

// Error renders the fail-closed refusal with the platform guidance.
func (e *UnavailableError) Error() string {
	message := fmt.Sprintf(
		`sandbox mode %q is requested but no sandbox backend is usable on this host; refusing to run the command unconfined. `+
			`Install bubblewrap or run a Landlock-enforcing kernel (Linux), ensure sandbox-exec is usable (macOS), or ensure the ACL `+
			`restricted-token runner can start (Windows) — otherwise switch the consumer to danger-full-access.`,
		e.Mode,
	)
	if e.Detail != "" {
		message += fmt.Sprintf(" Runner failure: %s", e.Detail)
	}
	return message
}

// Code reports the stable failure code.
func (e *UnavailableError) Code() string { return UnavailableCode }

// Provider is the abstract process-sandbox service: Confine must return
// enforcing argv or fail closed at wrap time; silent unconfined passthrough
// is forbidden. Functional probes arbitrate multi-runner chains and may be
// skipped for a sole candidate, whose own refusal remains the fail-closed
// end.
type Provider interface {
	// Confine wraps argv so it executes confined under policy on this host;
	// the caller spawns the returned argv in place of its own.
	Confine(argv []string, policy Policy) (ConfinedArgv, error)
}

// FailClosedProvider is the honest default when no native enforcement runner
// is composed: Confine always refuses with SANDBOX_UNAVAILABLE, never
// returning unconfined argv. This is the official semantics for a host
// without a usable backend — "missing or unusable confinement fails closed".
type FailClosedProvider struct{}

// Confine always fails closed for any confining mode.
func (FailClosedProvider) Confine(argv []string, policy Policy) (ConfinedArgv, error) {
	return ConfinedArgv{}, &UnavailableError{Mode: policy.Mode}
}
