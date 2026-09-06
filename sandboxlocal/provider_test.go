package sandboxlocal

import (
	"errors"
	"testing"

	"dshgo/sandbox"
)

// Confine fails closed for every confining mode: the provider must never
// return unconfined argv under a confinement policy.
func TestConfineFailsClosed(t *testing.T) {
	p := NewProvider(Config{WorkspaceRoot: `C:\work`})
	for _, mode := range []sandbox.ConfinedMode{sandbox.ModeWorkspaceWrite, sandbox.ModeReadOnly} {
		result, err := p.Confine([]string{"pwsh", "-Command", "echo hello"}, sandbox.Policy{
			Mode: mode,
			ExecutionPolicy: sandbox.ExecutionPolicy{
				Mode:          mode,
				WorkspaceRoot: `C:\work`,
			},
		})
		if err == nil {
			t.Fatalf("mode %q: fail-closed provider returned no error", mode)
		}
		var unavailable *sandbox.UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("mode %q: error is %T, want *sandbox.UnavailableError", mode, err)
		}
		if unavailable.Code() != sandbox.UnavailableCode {
			t.Fatalf("mode %q: code = %q", mode, unavailable.Code())
		}
		if len(result.Argv) != 0 {
			t.Fatalf("mode %q: refuse must not carry argv", mode)
		}
	}
}

// Danger-full-access is not confinement: it passes through unmodified.
func TestDangerFullAccessPassthrough(t *testing.T) {
	p := NewProvider(Config{WorkspaceRoot: `C:\work`})
	argv := []string{"pwsh", "-Command", "echo hi"}
	result, err := p.Confine(argv, sandbox.Policy{
		Mode: sandbox.ModeDangerFullAccess,
		ExecutionPolicy: sandbox.ExecutionPolicy{
			Mode:          sandbox.ModeDangerFullAccess,
			WorkspaceRoot: `C:\work`,
		},
	})
	if err != nil {
		t.Fatalf("confine: %v", err)
	}
	if result.Enforcement != sandbox.EnforcementFull {
		t.Fatalf("enforcement = %q, want full for danger-full-access", result.Enforcement)
	}
	if len(result.Argv) != len(argv) {
		t.Fatalf("argv changed for danger-full-access: %v", result.Argv)
	}
}

// Empty argv still refuses under a confining mode (fail-closed covers it).
func TestConfineEmptyArgvFails(t *testing.T) {
	p := NewProvider(Config{})
	if _, err := p.Confine([]string{}, sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}); err == nil {
		t.Fatal("empty argv must fail")
	}
}
