// Package compactionbasic ports packages/compaction/compaction-basic: the
// dependency-light replay-aware compaction backend. It prices pressure and
// retention through the singleton token meter, selects balanced surface
// ranges, runs the durable compaction/start..end lock transaction with a
// cache-reusing summarization call, and offers automatic step-boundary
// pressure and context-overflow recovery listeners.
//
// Go adaptations: the async transaction collapses to a synchronous one
// (summarization blocks); WeakMaps become keyed maps owned by the engine;
// AbortSignal.any becomes a linked context; the Loader-decoded config object
// becomes typed structs with the same value validation (unknown-key
// rejection is a Loader-plane duty in Go and cannot occur on a typed struct).
package compactionbasic

import (
	"fmt"
	"math"
)

// PolicyConfig is the policy field set shared by the default policy and
// exact model overrides. Nil fields inherit.
type PolicyConfig struct {
	// ThresholdRatio compacts at this fraction of the model's context window.
	ThresholdRatio *float64
	// RetainRatio retains this fraction of the model's window as recent
	// context; mutually exclusive with RetainTokens.
	RetainRatio *float64
	// RetainTokens is an absolute recent-context budget; mutually exclusive
	// with RetainRatio.
	RetainTokens *int64
	// SummarizationProvider is the summary provider; set together with
	// SummarizationModel, or inherit the conversation target.
	SummarizationProvider *string
	// SummarizationModel is the summary model; set together with
	// SummarizationProvider.
	SummarizationModel *string
	// MaxTokens is the provider generation cap for summarization.
	MaxTokens *int64
	// CompactionRetries are extra attempts after the first compaction while
	// pressure remains above threshold.
	CompactionRetries *int64
	// MaxOverflowRetries caps retries after canonical context overflow; 0
	// disables recovery.
	MaxOverflowRetries *int64
}

// ModelPolicyConfig is one exact provider/model override merged over the
// default compaction policy.
type ModelPolicyConfig struct {
	PolicyConfig
	// Provider is the registered provider route to match.
	Provider string
	// Model is the exact routed model id to match within Provider.
	Model string
}

// BasicConfig is the basic compaction configuration with an optional
// exact-target policy table.
type BasicConfig struct {
	PolicyConfig
	// ModelPolicies are exact provider/model overrides; duplicate targets
	// fail plugin load.
	ModelPolicies []ModelPolicyConfig
	// Auto enables the automatic step-boundary pressure and
	// overflow-recovery listeners; nil defaults to true.
	Auto *bool
}

// Retention is exactly one validated retention form.
type Retention struct {
	// RetainRatio is set when retention is fractional (nil otherwise).
	RetainRatio *float64
	// RetainTokens is set when retention is absolute (nil otherwise).
	RetainTokens *int64
}

// policyFields are the validated policy fields shared before and after
// exact-target matching.
type policyFields struct {
	thresholdRatio        float64
	summarizationProvider string
	summarizationModel    string
	maxTokens             int64
	compactionRetries     int64
	maxOverflowRetries    int64
}

// Target is one exact durable provider/model route.
type Target struct {
	Provider string
	Model    string
}

// ResolvedConfig is the validated immutable config whose target-specific
// defaults remain unresolved.
type ResolvedConfig struct {
	policyFields
	// Retention is the resolved default retention form.
	Retention Retention
	// ModelPolicies are the validated exact-target overrides in declared
	// order.
	ModelPolicies []ModelPolicyConfig
	// Auto reports whether automatic listeners are enabled.
	Auto bool
}

// ResolvedTargetPolicy is the fully merged policy for one routed
// conversation target, before capacity scaling.
type ResolvedTargetPolicy struct {
	policyFields
	// Retention is the merged retention form for this target.
	Retention Retention
	// Target is the exact route this policy was merged for.
	Target Target
}

// ResolvedCompactSpec is one routed model's concrete pressure and retention
// budget.
type ResolvedCompactSpec struct {
	policyFields
	// ContextWindow is the adapter-owned capacity the budgets were scaled
	// against.
	ContextWindow int64
	// ThresholdTokens is the absolute request-pressure trigger.
	ThresholdTokens int64
	// RetainTokens is the absolute verbatim-tail budget.
	RetainTokens int64
	// Target is the exact route this spec was scaled for.
	Target Target
}

// TargetPressureConfigError is a target-specific pressure configuration
// failure eligible for warning suppression.
type TargetPressureConfigError struct {
	// TargetKey is the exact provider/model route used as the warning key.
	TargetKey string
	// Message is the actionable configuration failure detail.
	Message string
}

func (e *TargetPressureConfigError) Error() string { return e.Message }

// Defaults from config.ts.
const (
	// DefaultThresholdRatio is the default request-pressure fraction for
	// every routed model.
	DefaultThresholdRatio = 0.8
	// DefaultRetainRatio is the default verbatim-tail fraction for every
	// routed model.
	DefaultRetainRatio = 0.16
	// DefaultSummarizationMaxTokens is the default provider generation cap.
	DefaultSummarizationMaxTokens = int64(8192)
)

// ResolveConfig validates service defaults plus exact-target partial
// overrides.
func ResolveConfig(config BasicConfig) (ResolvedConfig, error) {
	resolved := ResolvedConfig{}
	thresholdRatio := DefaultThresholdRatio
	if config.ThresholdRatio != nil {
		if err := assertRatio("BasicCompactionConfig.thresholdRatio", *config.ThresholdRatio); err != nil {
			return resolved, err
		}
		thresholdRatio = *config.ThresholdRatio
	}
	defaultRetention := Retention{RetainRatio: ptr(float64(DefaultRetainRatio))}
	retention, err := resolveRetention(config.PolicyConfig, "BasicCompactionConfig", defaultRetention)
	if err != nil {
		return resolved, err
	}
	if err := validateRatioRetention(thresholdRatio, retention, "BasicCompactionConfig"); err != nil {
		return resolved, err
	}
	fields, err := resolvePolicyFields(config.PolicyConfig, "BasicCompactionConfig")
	if err != nil {
		return resolved, err
	}
	fields.thresholdRatio = thresholdRatio

	policies, err := resolveModelPolicies(config.ModelPolicies)
	if err != nil {
		return resolved, err
	}
	for index := range policies {
		policy := &policies[index]
		policyThreshold := fields.thresholdRatio
		if policy.ThresholdRatio != nil {
			policyThreshold = *policy.ThresholdRatio
		}
		policyRetention, err := resolveRetention(policy.PolicyConfig, policyName(index), retention)
		if err != nil {
			return resolved, err
		}
		if err := validateRatioRetention(policyThreshold, policyRetention, policyName(index)); err != nil {
			return resolved, err
		}
	}

	auto := true
	if config.Auto != nil {
		auto = *config.Auto
	}
	resolved.policyFields = fields
	resolved.Retention = retention
	resolved.ModelPolicies = policies
	resolved.Auto = auto
	return resolved, nil
}

// ResolveTargetPolicy merges the exact provider/model override over the
// validated default policy.
func ResolveTargetPolicy(config ResolvedConfig, target Target) ResolvedTargetPolicy {
	var override *ModelPolicyConfig
	for index := range config.ModelPolicies {
		policy := &config.ModelPolicies[index]
		if policy.Provider == target.Provider && policy.Model == target.Model {
			override = policy
			break
		}
	}
	inheritedRetention := Retention{RetainRatio: config.Retention.RetainRatio}
	if config.Retention.RetainTokens != nil {
		inheritedRetention = Retention{RetainTokens: config.Retention.RetainTokens}
	}
	fields := config.policyFields
	retention := inheritedRetention
	if override != nil {
		if override.ThresholdRatio != nil {
			fields.thresholdRatio = *override.ThresholdRatio
		}
		if override.SummarizationProvider != nil {
			fields.summarizationProvider = *override.SummarizationProvider
		}
		if override.SummarizationModel != nil {
			fields.summarizationModel = *override.SummarizationModel
		}
		if override.MaxTokens != nil {
			fields.maxTokens = *override.MaxTokens
		}
		if override.CompactionRetries != nil {
			fields.compactionRetries = *override.CompactionRetries
		}
		if override.MaxOverflowRetries != nil {
			fields.maxOverflowRetries = *override.MaxOverflowRetries
		}
		retention = resolveRetentionValidated(override.PolicyConfig, inheritedRetention)
	}
	return ResolvedTargetPolicy{
		policyFields: fields,
		Retention:    retention,
		Target:       Target{Provider: target.Provider, Model: target.Model},
	}
}

// ResolveCompactSpec scales one routed policy into concrete token budgets
// for its model capacity.
func ResolveCompactSpec(policy ResolvedTargetPolicy, contextWindow int64) (ResolvedCompactSpec, error) {
	spec := ResolvedCompactSpec{}
	targetKey := policy.Target.Provider + "/" + policy.Target.Model
	if contextWindow <= 0 {
		return spec, &TargetPressureConfigError{
			TargetKey: targetKey,
			Message: fmt.Sprintf(
				"BasicCompactionConfig: contextWindow (%d) must be a positive integer", contextWindow),
		}
	}
	thresholdTokens := int64(math.Floor(float64(contextWindow) * policy.thresholdRatio))
	retainTokens := int64(0)
	if policy.Retention.RetainTokens != nil {
		retainTokens = *policy.Retention.RetainTokens
	} else {
		retainTokens = int64(math.Floor(float64(contextWindow) * (*policy.Retention.RetainRatio)))
	}
	if retainTokens >= thresholdTokens {
		return spec, &TargetPressureConfigError{
			TargetKey: targetKey,
			Message: fmt.Sprintf(
				"BasicCompactionConfig: %s retainTokens (%d) must be less than threshold tokens %d",
				targetKey, retainTokens, thresholdTokens),
		}
	}
	spec.policyFields = policy.policyFields
	spec.Target = policy.Target
	spec.ContextWindow = contextWindow
	spec.ThresholdTokens = thresholdTokens
	spec.RetainTokens = retainTokens
	return spec, nil
}

// resolveRetention chooses an explicit retention form or inherits the
// already-resolved fallback, validating value shape on the way.
func resolveRetention(config PolicyConfig, name string, fallback Retention) (Retention, error) {
	if config.RetainTokens != nil {
		if err := assertNonNegativeInteger(name+".retainTokens", *config.RetainTokens); err != nil {
			return Retention{}, err
		}
		if config.RetainRatio != nil {
			return Retention{}, fmt.Errorf("%s: retainRatio and retainTokens are mutually exclusive", name)
		}
		return Retention{RetainTokens: config.RetainTokens}, nil
	}
	if config.RetainRatio != nil {
		if err := assertRatio(name+".retainRatio", *config.RetainRatio); err != nil {
			return Retention{}, err
		}
		return Retention{RetainRatio: config.RetainRatio}, nil
	}
	return fallback, nil
}

// resolveRetentionValidated inherits an already-validated override retention
// form (resolveTargetPolicy runs over policies validated at load).
func resolveRetentionValidated(config PolicyConfig, fallback Retention) Retention {
	if config.RetainTokens != nil {
		return Retention{RetainTokens: config.RetainTokens}
	}
	if config.RetainRatio != nil {
		return Retention{RetainRatio: config.RetainRatio}
	}
	return fallback
}

// validateRatioRetention rejects a capacity-independent retention conflict
// at plugin load.
func validateRatioRetention(thresholdRatio float64, retention Retention, name string) error {
	if retention.RetainRatio != nil && *retention.RetainRatio >= thresholdRatio {
		return fmt.Errorf(
			"%s: retainRatio (%g) must be less than the resolved thresholdRatio (%g)",
			name, *retention.RetainRatio, thresholdRatio)
	}
	return nil
}

// resolvePolicyFields validates the shared fields with their defaults.
func resolvePolicyFields(config PolicyConfig, name string) (policyFields, error) {
	fields := policyFields{
		thresholdRatio:        DefaultThresholdRatio,
		summarizationProvider: "",
		summarizationModel:    "",
		maxTokens:             DefaultSummarizationMaxTokens,
		compactionRetries:     1,
		maxOverflowRetries:    1,
	}
	if config.MaxTokens != nil {
		if err := assertPositiveInteger(name+".maxTokens", *config.MaxTokens); err != nil {
			return fields, err
		}
		fields.maxTokens = *config.MaxTokens
	}
	if config.CompactionRetries != nil {
		if err := assertNonNegativeInteger(name+".compactionRetries", *config.CompactionRetries); err != nil {
			return fields, err
		}
		fields.compactionRetries = *config.CompactionRetries
	}
	if config.MaxOverflowRetries != nil {
		if err := assertNonNegativeInteger(name+".maxOverflowRetries", *config.MaxOverflowRetries); err != nil {
			return fields, err
		}
		fields.maxOverflowRetries = *config.MaxOverflowRetries
	}
	if err := validateSummarizationPair(config, name); err != nil {
		return fields, err
	}
	if config.SummarizationProvider != nil {
		fields.summarizationProvider = *config.SummarizationProvider
	}
	if config.SummarizationModel != nil {
		fields.summarizationModel = *config.SummarizationModel
	}
	return fields, nil
}

// resolveModelPolicies validates, detaches, and rejects duplicate
// exact-target policies.
func resolveModelPolicies(configured []ModelPolicyConfig) ([]ModelPolicyConfig, error) {
	if len(configured) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	resolved := make([]ModelPolicyConfig, 0, len(configured))
	for index := range configured {
		source := &configured[index]
		name := policyName(index)
		if err := validatePolicyConfig(source.PolicyConfig, name); err != nil {
			return nil, err
		}
		if source.Provider == "" {
			return nil, fmt.Errorf("%s.provider must be a non-empty string", name)
		}
		if source.Model == "" {
			return nil, fmt.Errorf("%s.model must be a non-empty string", name)
		}
		key := source.Provider + "\x00" + source.Model
		if seen[key] {
			return nil, fmt.Errorf(
				"BasicCompactionConfig: duplicate model policy for %s/%s", source.Provider, source.Model)
		}
		seen[key] = true
		resolved = append(resolved, *source)
	}
	return resolved, nil
}

// policyName renders one model-policy entry's diagnostic name.
func policyName(index int) string {
	return fmt.Sprintf("BasicCompactionConfig: modelPolicies[%d]", index)
}

// validatePolicyConfig validates the fields common to defaults and
// exact-target partial overrides.
func validatePolicyConfig(config PolicyConfig, name string) error {
	if config.ThresholdRatio != nil {
		if err := assertRatio(name+".thresholdRatio", *config.ThresholdRatio); err != nil {
			return err
		}
	}
	if config.RetainRatio != nil && config.RetainTokens != nil {
		return fmt.Errorf("%s: retainRatio and retainTokens are mutually exclusive", name)
	}
	if config.MaxTokens != nil {
		if err := assertPositiveInteger(name+".maxTokens", *config.MaxTokens); err != nil {
			return err
		}
	}
	if config.CompactionRetries != nil {
		if err := assertNonNegativeInteger(name+".compactionRetries", *config.CompactionRetries); err != nil {
			return err
		}
	}
	if config.MaxOverflowRetries != nil {
		if err := assertNonNegativeInteger(name+".maxOverflowRetries", *config.MaxOverflowRetries); err != nil {
			return err
		}
	}
	return validateSummarizationPair(config, name)
}

// validateSummarizationPair requires one scope to omit, clear, or replace
// the summarization target as a pair.
func validateSummarizationPair(config PolicyConfig, name string) error {
	provider := config.SummarizationProvider
	model := config.SummarizationModel
	if provider == nil && model == nil {
		return nil
	}
	if provider == nil || model == nil {
		return fmt.Errorf(
			"%s: summarizationProvider and summarizationModel must be set together as an empty or non-empty pair", name)
	}
	if (len(*provider) == 0) != (len(*model) == 0) {
		return fmt.Errorf(
			"%s: summarizationProvider and summarizationModel must be set together as an empty or non-empty pair", name)
	}
	return nil
}

func assertPositiveInteger(name string, value int64) error {
	if value <= 0 {
		return fmt.Errorf("%s (%d) must be a positive integer", name, value)
	}
	return nil
}

func assertNonNegativeInteger(name string, value int64) error {
	if value < 0 {
		return fmt.Errorf("%s (%d) must be a non-negative integer", name, value)
	}
	return nil
}

func assertRatio(name string, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 1 {
		return fmt.Errorf("%s (%g) must be a number in (0, 1]", name, value)
	}
	return nil
}

func ptr[T any](value T) *T {
	return &value
}
