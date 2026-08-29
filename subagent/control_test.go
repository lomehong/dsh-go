package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"dshgo/agent"
)

func TestCanonicalClientTimeZone(t *testing.T) {
	if zone, ok := canonicalClientTimeZone("UTC"); !ok || zone != "UTC" {
		t.Fatalf("UTC = %q %v", zone, ok)
	}
	if zone, ok := canonicalClientTimeZone("America/New_York"); !ok || zone == "" {
		t.Fatalf("IANA zone = %q %v", zone, ok)
	}
	for _, bad := range []string{"", " UTC", "UTC ", "EST", "Not A Zone", "unknown/zone"} {
		if zone, ok := canonicalClientTimeZone(bad); ok {
			t.Fatalf("%q = %q, want unusable", bad, zone)
		}
	}
}

func TestValidateControlRequest(t *testing.T) {
	raw := func(value string) json.RawMessage { return json.RawMessage(value) }
	if _, failure := validateControlRequest(ControlMethodList, raw(`{"parentSessionId":"p"}`)); failure != nil {
		t.Fatalf("list payload rejected: %v", failure)
	}
	if _, failure := validateControlRequest(ControlMethodPrompt, raw(`{"parentSessionId":"p","childSessionId":"c","mode":"continuable"}`)); failure != nil {
		t.Fatalf("prompt payload rejected: %v", failure)
	}
	// Missing child id, wrong mode, and unknown fields all fail loud.
	if _, failure := validateControlRequest(ControlMethodPrompt, raw(`{"parentSessionId":"p","mode":"continuable"}`)); failure == nil ||
		failure.Code != ControlCodeBadRequest || failure.Message != "invalid payload for subagent.prompt" {
		t.Fatalf("missing child = %+v", failure)
	}
	if _, failure := validateControlRequest(ControlMethodInterrupt, raw(`{"parentSessionId":"p","childSessionId":"c","mode":"one-shot"}`)); failure == nil {
		t.Fatal("one-shot mode must reject")
	}
	if _, failure := validateControlRequest(ControlMethodList, raw(`{"parentSessionId":"p","extra":1}`)); failure == nil {
		t.Fatal("unknown fields must reject")
	}
	if _, failure := validateControlRequest(ControlMethodList, raw(`{"parentSessionId":""}`)); failure == nil {
		t.Fatal("empty parent id must reject")
	}
}

func TestCatalogViewProjectsLiveStatus(t *testing.T) {
	parent, _ := newManagedAgent(t, "cat-parent", "")
	child, _ := newManagedAgent(t, "cat-child", "cat-parent")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(parent, nil); err != nil {
		t.Fatalf("enter parent: %v", err)
	}
	if _, err := registry.Enter(child, nil); err != nil {
		t.Fatalf("enter child: %v", err)
	}
	entries := []SubagentListEntry{
		{Kind: ListKindChild, ID: "cat-child", Mode: ModeContinuable, Activity: SubagentActivityCold},
		{Kind: ListKindDiagnostic, ID: "ghost", Reason: SubagentDiagnosticUnavailable},
	}
	catalog := catalogView(registry, "cat-parent", entries)
	if !catalog.ParentAvailable {
		t.Fatal("live parent must be available")
	}
	// A fresh agent sits idle: the driver is not running, so the row is
	// inactive even though the agent is resident.
	if catalog.Entries[0].Activity != SubagentActivityCold {
		t.Fatalf("idle child activity = %s", catalog.Entries[0].Activity)
	}
	if catalog.Entries[1].Reason != SubagentDiagnosticUnavailable {
		t.Fatal("diagnostic rows pass through untouched")
	}
	// Without an Agent registry every row is inactive and the parent is
	// unavailable.
	bare := catalogView(nil, "cat-parent", entries)
	if bare.ParentAvailable || bare.Entries[0].Activity != SubagentActivityCold {
		t.Fatalf("registry-less catalog = %+v", bare)
	}
}

func TestPromptControlFailureMapping(t *testing.T) {
	ctx := context.Background()
	// Per-code expectations (kept explicit rather than table-driven because
	// the constructor returns a value type).
	notResumable := promptControlFailure(ctx, newSubagentError("nope", CodeNotResumable, nil), "kid")
	if notResumable.Code != ControlCodeNotResumable || notResumable.Message != "subagent cannot be resumed" {
		t.Fatalf("not-resumable = %+v", notResumable)
	}
	unauthorized := promptControlFailure(ctx, newSubagentError("nope", CodeUnauthorized, nil), "kid")
	if unauthorized.Code != ControlCodeUnauthorized || unauthorized.Message != "subagent does not belong to this parent" {
		t.Fatalf("unauthorized = %+v", unauthorized)
	}
	for _, code := range []string{CodeDraining, CodeActivationClosing, CodeContinuationUnavailable, CodePersistenceUnavailable} {
		unavailable := promptControlFailure(ctx, newSubagentError("nope", code, nil), "kid")
		if unavailable.Code != ControlCodeDeliveryUnavailable || unavailable.Message != "subagent follow-up is temporarily unavailable" {
			t.Fatalf("%s = %+v", code, unavailable)
		}
	}
	internal := promptControlFailure(ctx, newSubagentError("nope", CodeDuplicateChild, nil), "kid")
	if internal.Code != ControlCodeInternal || internal.Message != "subagent prompt failed" {
		t.Fatalf("internal = %+v", internal)
	}
	plain := promptControlFailure(ctx, context.DeadlineExceeded, "kid")
	if plain.Code != ControlCodeInternal {
		t.Fatalf("plain = %+v", plain)
	}
	// Cancellation through the context and through the error chain.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := promptControlFailure(cancelledCtx, nil, "kid"); got.Code != ControlCodeCancelled || got.Message != "subagent prompt was cancelled" {
		t.Fatalf("ctx cancelled = %+v", got)
	}
	if got := promptControlFailure(ctx, newSubagentError("stop", CodeCancelled, nil), "kid"); got.Code != ControlCodeCancelled {
		t.Fatalf("error cancelled = %+v", got)
	}
	details := notResumable.Details.(struct {
		ChildSessionID string `json:"childSessionId"`
	})
	if details.ChildSessionID != "kid" {
		t.Fatalf("details = %+v", details)
	}
}

func TestCatalogReadControlFailureMapping(t *testing.T) {
	ctx := context.Background()
	projections := catalogReadControlFailure(ctx, newSubagentError("nope", CodeControlProjectionsUnavailable, nil))
	if projections.Code != ControlCodeProjectionsUnavailable {
		t.Fatalf("projections = %+v", projections)
	}
	if got := catalogReadControlFailure(ctx, newSubagentError("stop", CodeCancelled, nil)); got.Code != ControlCodeCancelled ||
		got.Message != "subagent catalog read was cancelled" {
		t.Fatalf("cancelled = %+v", got)
	}
	if got := catalogReadControlFailure(ctx, errors.New("boom")); got.Code != ControlCodeInternal ||
		got.Message != "subagent catalog read failed" {
		t.Fatalf("internal = %+v", got)
	}
}
