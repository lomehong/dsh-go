package compactionbasic

import (
	"strings"
	"testing"
)

func TestResolveConfigDefaults(t *testing.T) {
	config, err := ResolveConfig(BasicConfig{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if config.thresholdRatio != 0.8 {
		t.Fatalf("threshold default wrong: %g", config.thresholdRatio)
	}
	if config.Retention.RetainRatio == nil || *config.Retention.RetainRatio != 0.16 || config.Retention.RetainTokens != nil {
		t.Fatalf("retention default wrong: %+v", config.Retention)
	}
	if config.summarizationProvider != "" || config.summarizationModel != "" {
		t.Fatalf("summarization defaults wrong: %q/%q", config.summarizationProvider, config.summarizationModel)
	}
	if config.maxTokens != 8192 || config.compactionRetries != 1 || config.maxOverflowRetries != 1 {
		t.Fatalf("count defaults wrong: %d/%d/%d", config.maxTokens, config.compactionRetries, config.maxOverflowRetries)
	}
	if !config.Auto || len(config.ModelPolicies) != 0 {
		t.Fatalf("auto/policies wrong: %v %d", config.Auto, len(config.ModelPolicies))
	}
}

func TestResolveConfigValueValidation(t *testing.T) {
	over := 1.5
	negative := int64(-1)
	cases := []struct {
		name   string
		config BasicConfig
		detail string
	}{
		{"threshold over one", BasicConfig{PolicyConfig: PolicyConfig{ThresholdRatio: &over}}, "thresholdRatio (1.5) must be a number in (0, 1]"},
		{"negative retries", BasicConfig{PolicyConfig: PolicyConfig{CompactionRetries: &negative}}, "compactionRetries (-1) must be a non-negative integer"},
		{"zero max tokens", BasicConfig{PolicyConfig: PolicyConfig{MaxTokens: &negative}}, "maxTokens (-1) must be a positive integer"},
	}
	for _, testCase := range cases {
		_, err := ResolveConfig(testCase.config)
		if err == nil || !strings.Contains(err.Error(), testCase.detail) {
			t.Fatalf("%s: wrong failure: %v", testCase.name, err)
		}
	}
}

func TestResolveConfigRetentionConflict(t *testing.T) {
	ratio := 0.5
	tokens := int64(100)
	_, err := ResolveConfig(BasicConfig{PolicyConfig: PolicyConfig{RetainRatio: &ratio, RetainTokens: &tokens}})
	if err == nil || !strings.Contains(err.Error(), "retainRatio and retainTokens are mutually exclusive") {
		t.Fatalf("retention conflict must fail loud: %v", err)
	}
	// A retainRatio at or above the threshold fails at load.
	high := 0.95
	threshold := 0.9
	_, err = ResolveConfig(BasicConfig{PolicyConfig: PolicyConfig{RetainRatio: &high, ThresholdRatio: &threshold}})
	if err == nil || !strings.Contains(err.Error(), "retainRatio (0.95) must be less than the resolved thresholdRatio (0.9)") {
		t.Fatalf("ratio retention conflict must fail loud: %v", err)
	}
}

func TestResolveConfigModelPolicies(t *testing.T) {
	provider := "deepseek"
	model := "chat"
	threshold := 0.6
	config, err := ResolveConfig(BasicConfig{
		PolicyConfig: PolicyConfig{SummarizationProvider: &provider, SummarizationModel: &model},
		ModelPolicies: []ModelPolicyConfig{
			{PolicyConfig: PolicyConfig{ThresholdRatio: &threshold}, Provider: "deepseek", Model: "chat"},
			{Provider: "deepseek", Model: "reasoner"},
		},
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if len(config.ModelPolicies) != 2 {
		t.Fatalf("policies wrong: %d", len(config.ModelPolicies))
	}

	_, err = ResolveConfig(BasicConfig{ModelPolicies: []ModelPolicyConfig{
		{Provider: "deepseek", Model: "chat"},
		{Provider: "deepseek", Model: "chat"},
	}})
	if err == nil || !strings.Contains(err.Error(), "duplicate model policy for deepseek/chat") {
		t.Fatalf("duplicate policy must fail loud: %v", err)
	}
	_, err = ResolveConfig(BasicConfig{ModelPolicies: []ModelPolicyConfig{{Provider: "", Model: "chat"}}})
	if err == nil || !strings.Contains(err.Error(), "modelPolicies[0].provider must be a non-empty string") {
		t.Fatalf("empty provider must fail loud: %v", err)
	}
	// A policy retainRatio inherited against its own override threshold
	// still fails at load: the policy pins thresholdRatio 0.9 and retainRatio
	// 0.95.
	policyThreshold := 0.9
	policyRatio := 0.95
	_, err = ResolveConfig(BasicConfig{
		ModelPolicies: []ModelPolicyConfig{
			{PolicyConfig: PolicyConfig{ThresholdRatio: &policyThreshold, RetainRatio: &policyRatio}, Provider: "p", Model: "m"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "retainRatio (0.95) must be less than the resolved thresholdRatio (0.9)") {
		t.Fatalf("policy ratio conflict must fail loud: %v", err)
	}
}

func TestResolveTargetPolicyMergesOverride(t *testing.T) {
	provider := ""
	model := ""
	maxTokens := int64(2048)
	config, err := ResolveConfig(BasicConfig{
		PolicyConfig: PolicyConfig{SummarizationProvider: &provider, SummarizationModel: &model, MaxTokens: &maxTokens},
		ModelPolicies: []ModelPolicyConfig{
			{PolicyConfig: PolicyConfig{MaxTokens: ptr(int64(4096))}, Provider: "deepseek", Model: "chat"},
		},
	})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	merged := ResolveTargetPolicy(config, Target{Provider: "deepseek", Model: "chat"})
	if merged.maxTokens != 4096 {
		t.Fatalf("override maxTokens wrong: %d", merged.maxTokens)
	}
	mergedOther := ResolveTargetPolicy(config, Target{Provider: "deepseek", Model: "reasoner"})
	if mergedOther.maxTokens != 2048 {
		t.Fatalf("default maxTokens wrong: %d", mergedOther.maxTokens)
	}
	if merged.Target.Provider != "deepseek" || merged.Target.Model != "chat" {
		t.Fatalf("target wrong: %+v", merged.Target)
	}
}

func TestResolveCompactSpecScaling(t *testing.T) {
	config, err := ResolveConfig(BasicConfig{})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	policy := ResolveTargetPolicy(config, Target{Provider: "deepseek", Model: "chat"})
	spec, err := ResolveCompactSpec(policy, 100000)
	if err != nil {
		t.Fatalf("spec failed: %v", err)
	}
	if spec.ThresholdTokens != 80000 || spec.RetainTokens != 16000 || spec.ContextWindow != 100000 {
		t.Fatalf("spec wrong: %+v", spec)
	}

	absolute := int64(40000)
	configAbsolute, err := ResolveConfig(BasicConfig{PolicyConfig: PolicyConfig{RetainTokens: &absolute}})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	specAbsolute, err := ResolveCompactSpec(ResolveTargetPolicy(configAbsolute, Target{Provider: "p", Model: "m"}), 100000)
	if err != nil {
		t.Fatalf("spec failed: %v", err)
	}
	if specAbsolute.RetainTokens != 40000 {
		t.Fatalf("absolute retention wrong: %d", specAbsolute.RetainTokens)
	}
	// Retain at or above threshold fails with the target-specific error.
	huge := int64(100000)
	conflict, err := ResolveConfig(BasicConfig{PolicyConfig: PolicyConfig{ThresholdRatio: ptr(0.9), RetainTokens: &huge}})
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	_, err = ResolveCompactSpec(ResolveTargetPolicy(conflict, Target{Provider: "p", Model: "m"}), 100000)
	var pressureErr *TargetPressureConfigError
	if err == nil || !asTargetPressureConfigError(err, &pressureErr) ||
		!strings.Contains(err.Error(), "retainTokens (100000) must be less than threshold tokens 90000") ||
		pressureErr.TargetKey != "p/m" {
		t.Fatalf("retain over threshold must fail typed: %v", err)
	}
	// Non-positive capacity fails typed.
	_, err = ResolveCompactSpec(ResolveTargetPolicy(config, Target{Provider: "p", Model: "m"}), 0)
	if err == nil || !strings.Contains(err.Error(), "contextWindow (0) must be a positive integer") {
		t.Fatalf("zero capacity must fail loud: %v", err)
	}
}
