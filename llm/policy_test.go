package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckApiKey(t *testing.T) {
	value, _, ok := CheckApiKey("  sk-abc_123~-=  ")
	if !ok || value != "sk-abc_123~-=" {
		t.Fatalf("trimmed = %q, ok = %v", value, ok)
	}
	if _, reason, ok := CheckApiKey("   "); ok || reason != ApiKeyEmpty {
		t.Fatalf("blank reason = %q, ok = %v", reason, ok)
	}
	if _, reason, ok := CheckApiKey(""); ok || reason != ApiKeyEmpty {
		t.Fatalf("empty reason = %q, ok = %v", reason, ok)
	}
	// Space inside is not a header-safe character.
	if _, reason, ok := CheckApiKey("sk abc"); ok || reason != ApiKeyIllegalCharacter {
		t.Fatalf("space reason = %q, ok = %v", reason, ok)
	}
	// Non-ASCII rejected.
	if _, reason, ok := CheckApiKey("sk-é"); ok || reason != ApiKeyIllegalCharacter {
		t.Fatalf("non-ascii reason = %q, ok = %v", reason, ok)
	}
}

func TestAssertUsableApiKeyNeverEchoes(t *testing.T) {
	secret := "sk-SUPERSECRET-VALUE"
	_, err := AssertUsableApiKey("", "llm-deepseek", "settings.models.deepseek.apiKey")
	if err == nil || !strings.Contains(err.Error(), "llm-deepseek") || !strings.Contains(err.Error(), "settings.models.deepseek.apiKey") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SUPERSECRET") {
		t.Fatal("error message echoes key material")
	}
	_, err = AssertUsableApiKey("bad key with spaces", "llm-deepseek", "ref")
	if err == nil || strings.Contains(err.Error(), "SUPERSECRET") && strings.Contains(err.Error(), "bad key") {
		t.Fatalf("err = %v", err)
	}
	value, err := AssertUsableApiKey(secret, "llm-deepseek", "ref")
	if err != nil || value != secret {
		t.Fatalf("usable = %q, %v", value, err)
	}
}

func TestResolveRetryPolicyDefaults(t *testing.T) {
	policy, err := ResolveRetryPolicy(nil, "llm: deepseek")
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if policy.Mode != RetryModeNormal || policy.MaxRetries != 5 ||
		policy.InitialDelayMs != 500 || policy.MaxDelayMs != 10_000 || policy.JitterRatio != 0.1 {
		t.Fatalf("policy = %+v", policy)
	}
	if len(policy.RetryableCodes) != 5 {
		t.Fatalf("codes = %v", policy.RetryableCodes)
	}
	// Detached: mutating the result must not touch the defaults.
	policy.RetryableCodes[0] = "MUTATED"
	if DefaultRetryableCodes[0] == "MUTATED" {
		t.Fatal("resolved policy aliases DefaultRetryableCodes")
	}
	// Always mode.
	policy, err = ResolveRetryPolicy(&RetryPolicyConfig{Mode: "always"}, "p")
	if err != nil || policy.Mode != RetryModeAlways || policy.MaxRetries != 0 {
		t.Fatalf("always = %+v, %v", policy, err)
	}
	// Explicit overrides.
	negative := -1
	zero := int64(0)
	policy, err = ResolveRetryPolicy(&RetryPolicyConfig{
		Mode: "normal", MaxRetries: &zero,
		Backoff: &RetryBackoffConfig{InitialDelayMs: int64Ptr2(250), MaxDelayMs: int64Ptr2(2000), JitterRatio: float64Ptr2(0)},
	}, "p")
	if err != nil || policy.MaxRetries != 0 || policy.InitialDelayMs != 250 || policy.MaxDelayMs != 2000 || policy.JitterRatio != 0 {
		t.Fatalf("overrides = %+v, %v", policy, err)
	}
	_ = negative
}

func int64Ptr2(v int64) *int64       { return &v }
func float64Ptr2(v float64) *float64 { return &v }

func TestResolveRetryPolicyValidation(t *testing.T) {
	cases := []struct {
		name   string
		config *RetryPolicyConfig
		fails  string
	}{
		{"unknown mode", &RetryPolicyConfig{Mode: "sometimes"}, "mode"},
		{"negative retries", &RetryPolicyConfig{Mode: "normal", MaxRetries: int64Ptr2(-1)}, "maxRetries"},
		{"empty codes", &RetryPolicyConfig{Mode: "normal", RetryableCodes: []string{}}, "empty"},
		{"empty code string", &RetryPolicyConfig{Mode: "normal", RetryableCodes: []string{"X", ""}}, "non-empty"},
		{"dup codes", &RetryPolicyConfig{Mode: "normal", RetryableCodes: []string{"X", "X"}}, "duplicates"},
		{"initial zero", &RetryPolicyConfig{Mode: "normal", Backoff: &RetryBackoffConfig{InitialDelayMs: int64Ptr2(0)}}, "initialDelayMs"},
		{"max overflow", &RetryPolicyConfig{Mode: "normal", Backoff: &RetryBackoffConfig{MaxDelayMs: int64Ptr2(2_147_483_648)}}, "maxDelayMs"},
		{"initial above max", &RetryPolicyConfig{Mode: "normal", Backoff: &RetryBackoffConfig{InitialDelayMs: int64Ptr2(2000), MaxDelayMs: int64Ptr2(1000)}}, "less than or equal"},
		{"jitter below zero", &RetryPolicyConfig{Mode: "normal", Backoff: &RetryBackoffConfig{JitterRatio: float64Ptr2(-0.1)}}, "jitterRatio"},
		{"jitter above one", &RetryPolicyConfig{Mode: "normal", Backoff: &RetryBackoffConfig{JitterRatio: float64Ptr2(1.5)}}, "jitterRatio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveRetryPolicy(tc.config, "llm: test")
			if err == nil || !strings.Contains(err.Error(), tc.fails) {
				t.Fatalf("err = %v, want containing %q", err, tc.fails)
			}
		})
	}
}

func TestNormalizeLlmFailure(t *testing.T) {
	// Own facts trusted when codes agree.
	own := NewLlmError("quota gone", QuotaExceededCode, LlmFailure{
		Status: 429, ProviderRetryAfterMs: 1500, RequestID: "req-1",
	})
	failure := NormalizeLlmFailure(own)
	if failure.Code != QuotaExceededCode || failure.Status != 429 ||
		failure.ProviderRetryAfterMs != 1500 || failure.RequestID != "req-1" || failure.Message != "quota gone" {
		t.Fatalf("own facts = %+v", failure)
	}
	// Plain harness error → code from taxonomy, facts zeroed.
	harness := NewError(EmptyResponseCode, "empty", nil)
	failure = NormalizeLlmFailure(harness)
	if failure.Code != EmptyResponseCode || failure.Status != 0 || failure.RequestID != "" {
		t.Fatalf("harness facts = %+v", failure)
	}
	// Plain error → UNKNOWN.
	failure = NormalizeLlmFailure(errors.New("boom"))
	if failure.Code != "UNKNOWN" || failure.Message != "boom" {
		t.Fatalf("plain facts = %+v", failure)
	}
	// Nil → UNKNOWN placeholder.
	failure = NormalizeLlmFailure(nil)
	if failure.Code != "UNKNOWN" {
		t.Fatalf("nil facts = %+v", failure)
	}
	// Wrapped *LlmError found through the chain.
	wrapped := NewLlmError("transport down", "TRANSPORT", LlmFailure{Status: 502})
	failure = NormalizeLlmFailure(wrapped)
	if failure.Code != "TRANSPORT" || failure.Status != 502 {
		t.Fatalf("wrapped facts = %+v", failure)
	}
}

func TestAdapterFailureChunk(t *testing.T) {
	chunk := adapterFailureChunk(NewLlmError("rate limited", "RATE_LIMIT", LlmFailure{
		Status: 429, ProviderRetryAfterMs: 800, RequestID: "req-9",
	}), false)
	if chunk.Type != ChunkFinish || chunk.Reason == nil || chunk.Reason.Kind != FinishError {
		t.Fatalf("chunk = %+v", chunk)
	}
	failure := chunk.Reason.Failure
	if failure == nil || failure.Code != "RATE_LIMIT" || failure.Status != 429 ||
		failure.ProviderRetryAfterMs != 800 || failure.RequestID != "req-9" {
		t.Fatalf("failure = %+v", failure)
	}
	// Aborted classification.
	chunk = adapterFailureChunk(errors.New("x"), true)
	if chunk.Reason.Kind != FinishAborted {
		t.Fatalf("aborted kind = %v", chunk.Reason.Kind)
	}
	// Code ABORTED classifies even without a canceled context.
	chunk = adapterFailureChunk(NewLlmError("stopped", "ABORTED", LlmFailure{}), false)
	if chunk.Reason.Kind != FinishAborted {
		t.Fatalf("code-aborted kind = %v", chunk.Reason.Kind)
	}
	// Context cancellation without a code is aborted (AbortError parity).
	chunk = adapterFailureChunk(context.Canceled, false)
	if chunk.Reason.Kind != FinishAborted {
		t.Fatalf("ctx chunk = %+v", chunk)
	}
}
