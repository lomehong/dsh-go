package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestWiderModesLadder(t *testing.T) {
	if got := WiderModes("read-only"); len(got) != 2 || got[0] != "workspace-write" || got[1] != "danger-full-access" {
		t.Fatalf("read-only targets: %v", got)
	}
	if got := WiderModes("workspace-write"); len(got) != 1 || got[0] != "danger-full-access" {
		t.Fatalf("workspace-write targets: %v", got)
	}
	if got := WiderModes("danger-full-access"); got != nil {
		t.Fatalf("the ceiling widens to nothing: %v", got)
	}
	targets := EscalationTargets()
	if len(targets) != 2 || targets[0] != "workspace-write" || targets[1] != "danger-full-access" {
		t.Fatalf("targets: %v", targets)
	}
}

func TestValidateEscalationArgsPairing(t *testing.T) {
	perms, justification := "workspace-write", "because"
	if err := ValidateEscalationArgs(&perms, nil); err == nil || !strings.Contains(err.Error(), "requires a justification") {
		t.Fatalf("perms without justification: %v", err)
	}
	if err := ValidateEscalationArgs(nil, &justification); err == nil || !strings.Contains(err.Error(), "only valid together") {
		t.Fatalf("justification without perms: %v", err)
	}
	blank := "   "
	if err := ValidateEscalationArgs(&perms, &blank); err == nil || !strings.Contains(err.Error(), "non-empty sentence") {
		t.Fatalf("blank justification: %v", err)
	}
	if err := ValidateEscalationArgs(&perms, &justification); err != nil {
		t.Fatalf("valid pair: %v", err)
	}
	if err := ValidateEscalationArgs(nil, nil); err != nil {
		t.Fatalf("absent pair is valid: %v", err)
	}
}

func TestMarkersAreVerbatim(t *testing.T) {
	if got := SandboxDenialMarker("read-only"); got != "[sandbox: file access denied under read-only mode]" {
		t.Fatalf("denial marker: %q", got)
	}
	got := EscalationHintMarker("operation")
	want := "[sandbox: escalation available — retry this exact operation once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]"
	if got != want {
		t.Fatalf("hint marker: %q", got)
	}
}

// stubApprover answers with a scripted outcome.
type stubApprover struct {
	outcome string
	asks    []EscalationAsk
}

func (s *stubApprover) RequestApproval(req EscalationAsk) (string, error) {
	s.asks = append(s.asks, req)
	return s.outcome, nil
}

func TestApproveEscalationOrderedFailClosed(t *testing.T) {
	// Non-widening request never prompts a human.
	if _, err := ApproveEscalation(EscalationRequest{
		RequestedMode: "workspace-write", EffectiveMode: "workspace-write",
		Justification: "why", Subject: "operation",
	}, EscalationApproval{Approver: &stubApprover{outcome: "allowed-once"}}); err == nil || !strings.Contains(err.Error(), "not strictly wider") {
		t.Fatalf("non-widening: %v", err)
	}
	// No approver composed.
	if _, err := ApproveEscalation(EscalationRequest{
		RequestedMode: "danger-full-access", EffectiveMode: "read-only",
		Justification: "why", Subject: "operation",
	}, EscalationApproval{}); err == nil || !strings.Contains(err.Error(), "no approval service is composed") {
		t.Fatalf("approver missing: %v", err)
	}
	// Agent-less execution fails closed even with a channel.
	if _, err := ApproveEscalation(EscalationRequest{
		RequestedMode: "danger-full-access", EffectiveMode: "read-only",
		Justification: "why", Subject: "operation",
	}, EscalationApproval{Approver: &stubApprover{outcome: "allowed-once"}}); err == nil || !strings.Contains(err.Error(), "no agent to route it through") {
		t.Fatalf("agent missing: %v", err)
	}
	// The allowed path returns the granted mode and a self-contained reason.
	approver := &stubApprover{outcome: "allowed-once"}
	granted, err := ApproveEscalation(EscalationRequest{
		RequestedMode: "danger-full-access", EffectiveMode: "workspace-write",
		Justification: "need the repo root", Subject: "operation",
	}, EscalationApproval{Approver: approver, Agent: "agent-1", CallID: "call-9", ToolName: "edit", Signal: context.Background()})
	if err != nil || granted != "danger-full-access" {
		t.Fatalf("granted: %q, %v", granted, err)
	}
	if len(approver.asks) != 1 || approver.asks[0].Reason != "escalate sandbox to danger-full-access: need the repo root" {
		t.Fatalf("ask: %+v", approver.asks)
	}
	// The closed outcome vocabulary maps to distinct verbatim errors.
	for outcome, want := range map[string]string{
		"rejected":    `the user rejected escalating this operation to "danger-full-access"`,
		"cancelled":   `approval for escalating to "danger-full-access" was cancelled`,
		"unavailable": `sandbox escalation to "danger-full-access" requires approval, but no approval channel is available`,
	} {
		if _, err := ApproveEscalation(EscalationRequest{
			RequestedMode: "danger-full-access", EffectiveMode: "workspace-write",
			Justification: "why", Subject: "operation",
		}, EscalationApproval{Approver: &stubApprover{outcome: outcome}, Agent: "a"}); err == nil || err.Error() != want {
			t.Fatalf("outcome %q: %v", outcome, err)
		}
	}
	// A rogue outcome fails closed.
	if _, err := ApproveEscalation(EscalationRequest{
		RequestedMode: "danger-full-access", EffectiveMode: "workspace-write",
		Justification: "why", Subject: "operation",
	}, EscalationApproval{Approver: &stubApprover{outcome: "maybe"}, Agent: "a"}); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("rogue outcome: %v", err)
	}
}
