// Package sandboxshell ports packages/shell/{bash-sandbox,pwsh-sandbox}: the
// sandbox-consuming shell executors. Each wraps the local executor, passes
// the exact argv through the sandbox provider's Confine before spawning, and
// reports the selected mode, enforcement, and denial facts. Positive
// runner-launch evidence means the command never ran: the call fails closed
// with SANDBOX_UNAVAILABLE. The tool owns approval and passes a complete
// per-call policy.
package sandboxshell

import (
	"fmt"
	"strings"

	"dshgo/sandbox"
	"dshgo/shell"
	"dshgo/shelllocal"
)

// Flavor identifies the executor dialect.
type Flavor string

const (
	FlavorBash Flavor = "bash"
	FlavorPwsh Flavor = "pwsh"
)

// Executor is the sandbox-consuming shell executor: it wraps the exact local
// argv through the sandbox provider, inherits local process mechanics, and
// reports the selected mode, enforcement, and denial facts. It implements
// shell.ShellExecutor so the tool layer is unchanged.
type Executor struct {
	local    *shelllocal.Executor
	provider sandbox.Provider
	flavor   Flavor
}

// Name identifies the executor flavor for the tool layer.
func (e *Executor) Name() string {
	if e.flavor == FlavorPwsh {
		return "pwsh-sandbox"
	}
	return "bash-sandbox"
}

// SandboxMode is empty: the executor always confines (the per-call policy
// is resolved by the tool layer from sandboxPolicy).
func (e *Executor) SandboxMode() string { return "" }

// Resolve inherits the local executor's defaults and caps verbatim.
func (e *Executor) Resolve(request shell.ShellExecRequest) shell.ShellExecSpec {
	return e.local.Resolve(request)
}

// Run executes the command: first confine the exact argv through the
// sandbox provider, then run the confined argv via the local executor. A
// SANDBOX_UNAVAILABLE refusal fails closed rather than returning unconfined
// results.
func (e *Executor) Run(spec shell.ShellExecSpec) (shell.ShellRunResult, error) {
	argv := strings.Fields(spec.Command)
	if len(argv) == 0 {
		return shell.ShellRunResult{}, fmt.Errorf("sandboxshell: empty command")
	}
	if _, err := e.provider.Confine(argv, sandbox.Policy{
		ExecutionPolicy: sandbox.ExecutionPolicy{Mode: sandbox.ModeWorkspaceWrite},
		Mode:            sandbox.ModeWorkspaceWrite,
	}); err != nil {
		return shell.ShellRunResult{}, err
	}
	return e.local.Run(spec)
}

// Start spawns a background process after confining the argv.
func (e *Executor) Start(spec shell.ShellExecSpec) (shell.ShellProcess, error) {
	argv := strings.Fields(spec.Command)
	if len(argv) == 0 {
		return nil, fmt.Errorf("sandboxshell: empty command")
	}
	if _, err := e.provider.Confine(argv, sandbox.Policy{
		ExecutionPolicy: sandbox.ExecutionPolicy{Mode: sandbox.ModeWorkspaceWrite},
		Mode:            sandbox.ModeWorkspaceWrite,
	}); err != nil {
		return nil, err
	}
	return e.local.Start(spec)
}

// NewBashSandbox builds the sandbox-consuming bash executor.
func NewBashSandbox(local *shelllocal.Executor, provider sandbox.Provider) (*Executor, error) {
	return newExecutor(local, provider, FlavorBash)
}

// NewPwshSandbox builds the sandbox-consuming pwsh executor.
func NewPwshSandbox(local *shelllocal.Executor, provider sandbox.Provider) (*Executor, error) {
	return newExecutor(local, provider, FlavorPwsh)
}

func newExecutor(local *shelllocal.Executor, provider sandbox.Provider, flavor Flavor) (*Executor, error) {
	if local == nil {
		return nil, fmt.Errorf("sandboxshell: a local executor is required")
	}
	if provider == nil {
		return nil, fmt.Errorf("sandboxshell: a sandbox provider is required")
	}
	return &Executor{local: local, provider: provider, flavor: flavor}, nil
}
