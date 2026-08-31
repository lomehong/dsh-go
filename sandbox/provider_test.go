package sandbox

import (
	"strings"
	"testing"
)

func TestUnavailableErrorFailsClosedWithGuidance(t *testing.T) {
	err := &UnavailableError{Mode: ModeReadOnly}
	if err.Code() != UnavailableCode {
		t.Fatalf("code = %q", err.Code())
	}
	message := err.Error()
	for _, want := range []string{
		`sandbox mode "read-only" is requested but no sandbox backend is usable`,
		"refusing to run the command unconfined",
		"danger-full-access",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q missing %q", message, want)
		}
	}
	withDetail := &UnavailableError{Mode: ModeWorkspaceWrite, Detail: "bwrap refused the profile"}
	if !strings.Contains(withDetail.Error(), "Runner failure: bwrap refused the profile") {
		t.Fatalf("detail not carried: %q", withDetail.Error())
	}
}

func TestConfinedArgvCarriesDenialDialectAndRules(t *testing.T) {
	confined := ConfinedArgv{
		Argv:             []string{"bwrap", "--ro-bind", "/", "/", "--", "true"},
		Enforcement:      EnforcementFull,
		DenialSignatures: []string{"EROFS"},
		RunnerFailureRules: []RunnerFailureRule{
			{FatalSignatures: []string{"unable to start sandbox"}},
		},
	}
	if len(confined.Argv) != 6 || confined.Enforcement != EnforcementFull {
		t.Fatalf("confined = %+v", confined)
	}
	if len(confined.DenialSignatures) != 1 || len(confined.RunnerFailureRules) != 1 {
		t.Fatalf("dialect/rules = %+v", confined)
	}
}

func TestPolicyCarriesWorkspaceAndSession(t *testing.T) {
	sessionID := "s-1"
	policy := Policy{
		ExecutionPolicy: ExecutionPolicy{Mode: ModeWorkspaceWrite, WorkspaceRoot: "/ws", SessionID: &sessionID},
		Mode:            ModeWorkspaceWrite,
	}
	if policy.WorkspaceRoot != "/ws" || policy.SessionID == nil || *policy.SessionID != "s-1" {
		t.Fatalf("policy = %+v", policy)
	}
}
