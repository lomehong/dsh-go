// Connection options, plugin config, and the one explicit resolve step from
// raw config to validated connection facts. Port of the resolve half of
// index.ts plus the connection-options half of adapter.ts. The image /
// Files-API half of the official config is deferred with its serializer
// path; its bounds are not carried here.
package deepseek

import (
	"fmt"
	"os"

	"dshgo/llm"
)

// Public API default; the internal endpoint comes from $DEEPSEEK_BASE_URL.
const PublicBaseURL = "https://api.deepseek.com"

// BasURLEnv names the environment variable for this provider's endpoint,
// honored only from trusted layers.
const BaseURLEnv = "DEEPSEEK_BASE_URL"

// DefaultAPIKeyEnv is the default credential reference.
const DefaultAPIKeyEnv = "DEEPSEEK_API_KEY"

// ProviderRoute is the single provider route the plugin owns.
const ProviderRoute = "deepseek-official"

// Defaults ported from adapter.ts constants.
const (
	DefaultStreamIdleTimeoutMs = 300_000
	DefaultContextWindow       = 1_000_000
	DefaultMaxTokens           = 256_000
)

// maxTimerDelay mirrors the shared timer bound (MAX_TIMER_DELAY_MS).
const maxTimerDelay = 2_147_483_647

// CatalogModel is one optional model entry advertised by the adapter.
type CatalogModel struct {
	// ID is the wire model id accepted by the configured endpoint.
	ID string `json:"id"`
	// Name is the selector label; defaults to ID.
	Name string `json:"name,omitempty"`
	// Description is an optional selector detail for deployments with
	// similar model variants.
	Description string `json:"description,omitempty"`
	// ContextWindow is the known combined request/response context
	// capacity; omitted when deployment metadata is unavailable.
	ContextWindow *int64 `json:"contextWindow,omitempty"`
	// MaxTokens is the per-request output cap for this model; omission
	// falls back to ConnectionOptions.MaxTokens.
	MaxTokens *int64 `json:"maxTokens,omitempty"`
	// InputModalities are the accepted request modalities; omission is
	// text-only.
	InputModalities []string `json:"inputModalities,omitempty"`
}

// ConnectionOptions is the validated connection facts for one operation.
// ResolveAdapterOptions is the one explicit resolve step producing this
// value; the adapter trusts it and re-reads it per operation, which is what
// makes a configuration change reach the next request without
// re-registration.
type ConnectionOptions struct {
	// BaseURL is the endpoint base; `/chat/completions` is appended.
	BaseURL string
	// APIKeyEnv is the credential reference of this same resolution,
	// resolved per request. Travelling with the endpoint is the point: a
	// request can never pair one generation's URL with another generation's
	// secret.
	APIKeyEnv string
	// Defaults are the request defaults applied to every call.
	Defaults RequestDefaults
	// MaxTokens is the default per-request output cap; explicit request
	// values win.
	MaxTokens int64
	// DefaultContextWindow is the positive context capacity used when the
	// selected model has no exact value.
	DefaultContextWindow int64
	// Models is the advisory catalog exposed to discovery consumers;
	// requests remain unrestricted.
	Models []CatalogModel
	// StreamIdleTimeoutMs is the maximum provider idle time while one
	// stream read is outstanding.
	StreamIdleTimeoutMs int64
	// RetryPolicy is the provider-owned model-request retry policy,
	// already resolved.
	RetryPolicy *llm.ResolvedRetryPolicy
}

// Config is the plugin config shape, doubling as the `llm-deepseek`
// settings-section shape. Every field is optional in yml.
type Config struct {
	APIKeyEnv            string                 `json:"apiKeyEnv,omitempty"`
	BaseURL              string                 `json:"baseURL,omitempty"`
	Thinking             string                 `json:"thinking,omitempty"`
	ReasoningEffort      string                 `json:"reasoningEffort,omitempty"`
	MaxTokens            *int64                 `json:"maxTokens,omitempty"`
	DefaultContextWindow *int64                 `json:"defaultContextWindow,omitempty"`
	Models               []CatalogModel         `json:"models,omitempty"`
	StreamIdleTimeoutMs  *float64               `json:"streamIdleTimeoutMs,omitempty"`
	RetryPolicy          *llm.RetryPolicyConfig `json:"retryPolicy,omitempty"`
}

// defaultModels mirrors DEFAULT_MODELS.
func defaultModels() []CatalogModel {
	window := int64(DefaultContextWindow)
	return []CatalogModel{
		{
			ID: "deepseek-v4-flash", Name: "DeepSeek-V4-Flash",
			Description:   "Fast, efficient, and economical; suited to focused, routine, or parallel tasks.",
			ContextWindow: &window,
		},
		{
			ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro",
			Description:   "Stronger agentic coding, knowledge, and difficult reasoning; suited to complex or quality-critical tasks at higher cost.",
			ContextWindow: &window,
		},
		{
			ID: "deepseek-v4-flash-vision-exp", Name: "DeepSeek-V4-Flash-Vision-Exp",
			ContextWindow:   &window,
			InputModalities: []string{"text", "image"},
		},
	}
}

// resolveModels validates and detaches the advisory model catalog.
func resolveModels(models []CatalogModel) ([]CatalogModel, error) {
	if models == nil {
		models = defaultModels()
	}
	out := make([]CatalogModel, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		if model.ID == "" {
			return nil, fmt.Errorf("llm-deepseek: catalog model ids must be non-empty")
		}
		if model.Name == "" {
			return nil, fmt.Errorf("llm-deepseek: catalog model %q has an empty name", model.ID)
		}
		if model.ContextWindow != nil && *model.ContextWindow <= 0 {
			return nil, fmt.Errorf("llm-deepseek: catalog model %q contextWindow must be a positive integer", model.ID)
		}
		if model.MaxTokens != nil && *model.MaxTokens <= 0 {
			return nil, fmt.Errorf("llm-deepseek: catalog model %q maxTokens must be a positive integer", model.ID)
		}
		modalities := model.InputModalities
		if modalities == nil {
			modalities = []string{"text"}
		}
		if len(modalities) == 0 {
			return nil, fmt.Errorf("llm-deepseek: catalog model %q inputModalities must not be empty", model.ID)
		}
		modalitySeen := map[string]bool{}
		for _, modality := range modalities {
			if modality != "text" && modality != "image" {
				return nil, fmt.Errorf("llm-deepseek: catalog model %q inputModalities must contain only \"text\" and \"image\"", model.ID)
			}
			if modalitySeen[modality] {
				return nil, fmt.Errorf("llm-deepseek: catalog model %q inputModalities must not contain duplicates", model.ID)
			}
			modalitySeen[modality] = true
		}
		if seen[model.ID] {
			return nil, fmt.Errorf("llm-deepseek: duplicate catalog model %q", model.ID)
		}
		seen[model.ID] = true
		captured := model
		captured.InputModalities = append([]string(nil), modalities...)
		out = append(out, captured)
	}
	return out, nil
}

// environmentValue reads one environment layer. The product trusts the
// project it is launched in, so a checkout can point its own agent at the
// gateway that checkout is meant to use.
type environmentValue func(name string) (string, bool)

// osEnvironment reads the process environment.
func osEnvironment(name string) (string, bool) {
	value, ok := os.LookupEnv(name)
	return value, ok
}

// ResolveAdapterOptions validates raw config into connection facts,
// re-judging every default and bound (programmatic construction may bypass
// schema normalization). Fails loud at load and at each settings snapshot's
// first use. The env argument may be nil outside the product CLI.
func ResolveAdapterOptions(config Config, env environmentValue) (*ConnectionOptions, error) {
	if config.Thinking != "" && config.Thinking != "enabled" && config.Thinking != "disabled" {
		return nil, fmt.Errorf("llm-deepseek: thinking must be \"enabled\" or \"disabled\"")
	}
	if config.ReasoningEffort != "" &&
		config.ReasoningEffort != "off" && config.ReasoningEffort != "low" &&
		config.ReasoningEffort != "high" && config.ReasoningEffort != "max" {
		return nil, fmt.Errorf("llm-deepseek: reasoningEffort must be one of \"off\", \"low\", \"high\", \"max\"")
	}
	if config.Thinking == "disabled" && config.ReasoningEffort != "" && config.ReasoningEffort != "off" {
		return nil, fmt.Errorf("llm-deepseek: only reasoningEffort \"off\" can be configured when thinking is disabled")
	}
	if config.DefaultContextWindow != nil && *config.DefaultContextWindow <= 0 {
		return nil, fmt.Errorf("llm-deepseek: defaultContextWindow must be a positive integer")
	}
	if config.MaxTokens != nil && *config.MaxTokens <= 0 {
		return nil, fmt.Errorf("llm-deepseek: maxTokens must be a positive safe integer")
	}
	streamIdleTimeoutMs := int64(DefaultStreamIdleTimeoutMs)
	if config.StreamIdleTimeoutMs != nil {
		streamIdleTimeoutMs = int64(*config.StreamIdleTimeoutMs)
	}
	if streamIdleTimeoutMs <= 0 || streamIdleTimeoutMs > maxTimerDelay {
		return nil, fmt.Errorf("llm-deepseek: streamIdleTimeoutMs must be a positive finite number no greater than %d", maxTimerDelay)
	}
	models, err := resolveModels(config.Models)
	if err != nil {
		return nil, err
	}
	policy, err := llm.ResolveRetryPolicy(config.RetryPolicy, "llm-deepseek: retryPolicy")
	if err != nil {
		return nil, err
	}
	baseURL := config.BaseURL
	if baseURL == "" {
		if env != nil {
			if value, ok := env(BaseURLEnv); ok && value != "" {
				baseURL = value
			}
		}
		if baseURL == "" {
			baseURL = PublicBaseURL
		}
	}
	apiKeyEnv := config.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = DefaultAPIKeyEnv
	}
	maxTokens := int64(DefaultMaxTokens)
	if config.MaxTokens != nil {
		maxTokens = *config.MaxTokens
	}
	defaultContextWindow := int64(DefaultContextWindow)
	if config.DefaultContextWindow != nil {
		defaultContextWindow = *config.DefaultContextWindow
	}
	return &ConnectionOptions{
		APIKeyEnv:            apiKeyEnv,
		BaseURL:              baseURL,
		Defaults:             RequestDefaults{Thinking: config.Thinking, ReasoningEffort: config.ReasoningEffort},
		MaxTokens:            maxTokens,
		DefaultContextWindow: defaultContextWindow,
		Models:               models,
		StreamIdleTimeoutMs:  streamIdleTimeoutMs,
		RetryPolicy:          policy,
	}, nil
}
