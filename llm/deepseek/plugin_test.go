package deepseek

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/credentials"
	"dshgo/llm"
	"dshgo/settings"
)

type recordingLogger struct {
	warns []string
}

func (l *recordingLogger) Info(...any) {}
func (l *recordingLogger) Warn(args ...any) {
	if text, ok := args[0].(string); ok {
		l.warns = append(l.warns, text)
	}
}
func (l *recordingLogger) Error(...any) {}

func staticEnv(values map[string]string) environmentValue {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

func newTestRuntime() *llm.Runtime { return llm.NewRuntime() }

// --- registration ------------------------------------------------------------

func TestApplyRegistersRouteAndDiscovery(t *testing.T) {
	runtime := newTestRuntime()
	plugin, err := Apply(PluginDeps{Runtime: runtime}, Config{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()
	providers := runtime.ListProviders()
	if len(providers) != 1 || providers[0].ID != ProviderRoute || providers[0].Name != "DeepSeek" {
		t.Fatalf("providers = %+v", providers)
	}
	configurable := runtime.ListConfigurableProviders()
	if len(configurable) != 1 || configurable[0].SettingsNs != "llm-deepseek" || configurable[0].DisplayName != "DeepSeek" {
		t.Fatalf("configurable = %+v", configurable)
	}
	// The load-time resolve ran: the default facts are already memoized.
	options, err := plugin.Options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if options.BaseURL != PublicBaseURL || options.APIKeyEnv != DefaultAPIKeyEnv {
		t.Fatalf("options = %+v", options)
	}
	if options.RetryPolicy == nil || options.RetryPolicy.Mode != "normal" || options.RetryPolicy.MaxRetries != 5 {
		t.Fatalf("retry policy = %+v", options.RetryPolicy)
	}
	// Duplicate registration fails loud.
	if _, err := Apply(PluginDeps{Runtime: runtime}, Config{}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("dup apply err = %v", err)
	}
	plugin.Dispose()
	if len(runtime.ListProviders()) != 0 {
		t.Fatal("dispose left the route registered")
	}
}

// --- per-request resolution --------------------------------------------------

func TestSettingsChangeReachesNextRequest(t *testing.T) {
	runtime := newTestRuntime()
	store := settings.NewStore(nil)
	plugin, err := Apply(PluginDeps{Runtime: runtime, Settings: store, Logger: &recordingLogger{}}, Config{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()

	// A committed settings snapshot re-points the config source; the next
	// Options call resolves from it.
	scope := plugin.Section()
	if scope == nil {
		t.Fatal("section missing")
	}
	if err := scope.Update(map[string]any{"baseURL": "https://gateway.internal", "apiKeyEnv": "GATEWAY_KEY"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	options, err := plugin.Options()
	if err != nil {
		t.Fatalf("options after update: %v", err)
	}
	if options.BaseURL != "https://gateway.internal" || options.APIKeyEnv != "GATEWAY_KEY" {
		t.Fatalf("stale facts: %+v", options)
	}
}

func TestInvalidSnapshotKeepsLastGoodFacts(t *testing.T) {
	runtime := newTestRuntime()
	store := settings.NewStore(nil)
	logger := &recordingLogger{}
	plugin, err := Apply(PluginDeps{Runtime: runtime, Settings: store, Logger: logger}, Config{MaxTokens: int64ptr(1000)})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()
	scope := plugin.Section()
	// The schema Validate refuses the bad section outright.
	if err := scope.Update(map[string]any{"streamIdleTimeoutMs": -5}); err == nil {
		t.Fatal("invalid section accepted by the schema")
	}
	// A wholesale Replace bypasses nothing: validation still holds.
	if err := scope.Replace(map[string]any{"streamIdleTimeoutMs": -5}); err == nil {
		t.Fatal("invalid replace accepted")
	}
	options, err := plugin.Options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if options.MaxTokens != 1000 {
		t.Fatalf("last good facts lost: %+v", options)
	}
}

func int64ptr(v int64) *int64 { return &v }

func TestMissingCredentialFailsWithTypedError(t *testing.T) {
	runtime := newTestRuntime()
	plugin, err := Apply(PluginDeps{Runtime: runtime, Environment: staticEnv(map[string]string{})}, Config{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()
	options, err := plugin.Options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	_, err = plugin.ResolveAPIKey(options)
	var llmErr *llm.LlmError
	if !errors.As(err, &llmErr) || llmErr.Code() != "MISSING_CREDENTIAL" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(llmErr.Error(), "DEEPSEEK_API_KEY") {
		t.Fatalf("message = %q", llmErr.Error())
	}
}

func TestCredentialSeamRanksBeforeEnvironment(t *testing.T) {
	runtime := newTestRuntime()
	provider := credentials.NewMemoryProvider(map[string]string{"DEEPSEEK_API_KEY": "stored-key"})
	plugin, err := Apply(PluginDeps{
		Runtime:     runtime,
		Credentials: provider,
		Environment: staticEnv(map[string]string{"DEEPSEEK_API_KEY": "env-key"}),
	}, Config{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()
	options, _ := plugin.Options()
	key, err := plugin.ResolveAPIKey(options)
	if err != nil || key != "stored-key" {
		t.Fatalf("key = %q, %v (the managed store ranks first)", key, err)
	}
	// Without the seam the ambient environment is the whole plane.
	fallback, err := Apply(PluginDeps{Runtime: llm.NewRuntime(), Environment: staticEnv(map[string]string{"DEEPSEEK_API_KEY": "env-key"})}, Config{})
	if err != nil {
		t.Fatalf("apply fallback: %v", err)
	}
	defer fallback.Dispose()
	fallbackOptions, _ := fallback.Options()
	key, err = fallback.ResolveAPIKey(fallbackOptions)
	if err != nil || key != "env-key" {
		t.Fatalf("ambient key = %q, %v", key, err)
	}
}

// --- retry policy re-registration ---------------------------------------------

func TestRetryPolicyChangeReplacesRouteInPlace(t *testing.T) {
	runtime := newTestRuntime()
	store := settings.NewStore(nil)
	plugin, err := Apply(PluginDeps{Runtime: runtime, Settings: store, Logger: &recordingLogger{}}, Config{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer plugin.Dispose()
	prepared, err := runtime.PrepareCall(llm.LlmCallConfig{Provider: ProviderRoute, Model: "deepseek-v4-flash"})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if prepared == nil || prepared.RetryPolicy == nil || prepared.RetryPolicy.MaxRetries != 5 {
		t.Fatalf("initial policy = %+v", prepared)
	}

	scope := plugin.Section()
	if err := scope.Update(map[string]any{
		"retryPolicy": map[string]any{"mode": "normal", "maxRetries": 9},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	// The registry captured the policy at registration; the change
	// re-registers the route in place (the handle stays valid).
	prepared, err = runtime.PrepareCall(llm.LlmCallConfig{Provider: ProviderRoute, Model: "deepseek-v4-flash"})
	if err != nil {
		t.Fatalf("prepare after change: %v", err)
	}
	if prepared.RetryPolicy == nil || prepared.RetryPolicy.MaxRetries != 9 {
		t.Fatalf("policy after change = %+v", prepared.RetryPolicy)
	}
	// An unchanged snapshot does not churn the registry.
	plugin.EnsureRegistrationFacts()
	prepared, err = runtime.PrepareCall(llm.LlmCallConfig{Provider: ProviderRoute, Model: "deepseek-v4-flash"})
	if err != nil || prepared.RetryPolicy.MaxRetries != 9 {
		t.Fatalf("idempotent check = %+v, %v", prepared, err)
	}
}

// --- config resolution guards (plugin level) -----------------------------------

func TestApplyFailsLoudOnInvalidStaticConfig(t *testing.T) {
	runtime := newTestRuntime()
	if _, err := Apply(PluginDeps{Runtime: runtime}, Config{Thinking: "disabled", ReasoningEffort: "high"}); err == nil ||
		!strings.Contains(err.Error(), "only reasoningEffort \"off\"") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Apply(PluginDeps{Runtime: nil}, Config{}); err == nil {
		t.Fatal("missing runtime accepted")
	}
}

func TestConfigRoundTripsThroughSectionJSON(t *testing.T) {
	// The settings layer uses JSON as its transport; the Config shape must
	// round-trip the fields the adapter consumes.
	source := Config{
		APIKeyEnv: "K", BaseURL: "https://x", Thinking: "enabled",
		ReasoningEffort: "max", MaxTokens: int64ptr(7), DefaultContextWindow: int64ptr(9),
		StreamIdleTimeoutMs: float64ptr(1234),
		RetryPolicy:         &llm.RetryPolicyConfig{Mode: "normal", MaxRetries: int64ptr(2)},
	}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Config
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	resolved, err := ResolveAdapterOptions(decoded, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.MaxTokens != 7 || resolved.DefaultContextWindow != 9 || resolved.StreamIdleTimeoutMs != 1234 {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.RetryPolicy == nil || resolved.RetryPolicy.MaxRetries != 2 {
		t.Fatalf("policy = %+v", resolved.RetryPolicy)
	}
}

func float64ptr(v float64) *float64 { return &v }
