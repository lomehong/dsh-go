package sandboxlocal

import (
	"strings"
	"testing"

	"dshgo/sandbox"
)

func TestConfineReturnsPartialEnforcement(t *testing.T) {
	p := NewProvider(Config{WorkspaceRoot: `C:\work`})
	result, err := p.Confine([]string{"pwsh", "-Command", "echo hello"}, sandbox.Policy{
		Mode: sandbox.ModeWorkspaceWrite,
		ExecutionPolicy: sandbox.ExecutionPolicy{
			Mode:          sandbox.ModeWorkspaceWrite,
			WorkspaceRoot: `C:\work`,
		},
	})
	if err != nil {
		t.Fatalf("confine: %v", err)
	}
	if result.Enforcement != sandbox.EnforcementPartial {
		t.Fatalf("enforcement = %q, want partial", result.Enforcement)
	}
	if len(result.Argv) == 0 {
		t.Fatal("argv is empty")
	}
	if len(result.DenialSignatures) == 0 {
		t.Fatal("denial signatures missing")
	}
}

func TestConfineEmptyArgvFails(t *testing.T) {
	p := NewProvider(Config{})
	if _, err := p.Confine([]string{}, sandbox.Policy{Mode: sandbox.ModeWorkspaceWrite}); err == nil {
		t.Fatal("empty argv must fail")
	}
}

func TestDenialSignaturesMatchWindowsAccessDenied(t *testing.T) {
	signatures := strings.Join(windowsDenialSignatures, "\n")
	for _, want := range []string{"access is denied", "unauthorizedaccessexception"} {
		if !strings.Contains(strings.ToLower(signatures), want) {
			t.Fatalf("denial signatures missing %q", want)
		}
	}
}
