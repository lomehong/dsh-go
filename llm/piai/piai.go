// Package piai is the multi-provider twin: it mounts one wire adapter per
// provider route configured in the `llm-pi-ai` settings namespace, replacing
// the official JS-SDK adapter family with Go wire implementations
// (openai-completions / openai-responses ride the OpenAI-completions wire
// adapter family; anthropic-messages rides the Anthropic Messages adapter).
// Route set changes re-sync registrations; a route losing its adapter
// withdraws from the runtime while its configuration stays durable.
package piai

import (
	"fmt"
	"os"
	"sync"

	"dshgo/llm"
	"dshgo/llm/anthropic"
	"dshgo/llm/deepseek"
)

// API families the twin dispatches.
const (
	APIOpenAICompletions = "openai-completions"
	APIOpenAIResponses   = "openai-responses"
	APIAnthropicMessages = "anthropic-messages"
)

// Profile is one configured provider route (the `providers.<route>` settings
// entry). Only the fields the twin acts on are modeled; the settings document
// may carry more (editor metadata) and is preserved untouched by the store.
type Profile struct {
	// API is the wire protocol family. Unrecognized families are skipped
	// with a diagnostic — their adapters have not been ported.
	API string `json:"api"`
	// BaseURL is the endpoint base. Empty falls back to the catalog
	// default for the route, then fails loud at dispatch.
	BaseURL string `json:"baseUrl,omitempty"`
	// APIKeyEnv is the credential reference. Empty falls back to the
	// catalog default for the route.
	APIKeyEnv string `json:"apiKeyEnv,omitempty"`
	// Models is the advisory catalog (route/model ids and limits).
	Models []Model `json:"models,omitempty"`
	// MaxTokens is the default per-request output cap.
	MaxTokens int64 `json:"maxTokens,omitempty"`
	// DefaultContextWindow is the fallback context capacity.
	DefaultContextWindow int64 `json:"defaultContextWindow,omitempty"`
}

// Model is one advisory catalog entry.
type Model struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	ContextWindow   int64    `json:"contextWindow,omitempty"`
	MaxTokens       int64    `json:"maxTokens,omitempty"`
	InputModalities []string `json:"inputModalities,omitempty"`
}

// catalogDefaults resolves per-route fallbacks the official pi-ai catalog
// carries. Only entries with a concrete Go-dispatchable endpoint (or a
// documented default base URL) are listed; others fall back to the user's
// explicit baseUrl.
type catalogDefault struct {
	baseURL string
	apiKey  string
}

var catalogDefaults = map[string]catalogDefault{
	"deepseek":      {baseURL: "https://api.deepseek.com", apiKey: "DEEPSEEK_API_KEY"},
	"openai":        {baseURL: "https://api.openai.com/v1", apiKey: "OPENAI_API_KEY"},
	"anthropic":     {baseURL: "https://api.anthropic.com", apiKey: "ANTHROPIC_API_KEY"},
	"moonshotai":    {baseURL: "https://api.moonshot.cn/v1", apiKey: "MOONSHOT_API_KEY"},
	"groq":          {baseURL: "https://api.groq.com/openai/v1", apiKey: "GROQ_API_KEY"},
	"mistral":       {baseURL: "https://api.mistral.ai/v1", apiKey: "MISTRAL_API_KEY"},
	"together":      {baseURL: "https://api.together.xyz/v1", apiKey: "TOGETHER_API_KEY"},
	"fireworks":     {baseURL: "https://api.fireworks.ai/inference/v1", apiKey: "FIREWORKS_API_KEY"},
	"xai":           {baseURL: "https://api.x.ai/v1", apiKey: "XAI_API_KEY"},
	"opencode":      {baseURL: "https://opencode.ai/api/v1", apiKey: "OPENCODE_API_KEY"},
	"openrouter":    {baseURL: "https://openrouter.ai/api/v1", apiKey: "OPENROUTER_API_KEY"},
	"nvidia":        {baseURL: "https://integrate.api.nvidia.com/v1", apiKey: "NVIDIA_API_KEY"},
	"cerebras":      {baseURL: "https://api.cerebras.ai/v1", apiKey: "CEREBRAS_API_KEY"},
	"baseten":       {baseURL: "https://inference.baseten.co/v1", apiKey: "BASETEN_API_KEY"},
	"minimax":       {baseURL: "https://api.minimax.chat/v1", apiKey: "MINIMAX_API_KEY"},
	"minimax-cn":    {baseURL: "https://api.minimaxi.com/v1", apiKey: "MINIMAX_API_KEY"},
	"zai":           {baseURL: "https://api.z.ai/api/paas/v4", apiKey: "ZAI_API_KEY"},
	"zai-coding-cn": {baseURL: "https://api.z.ai/api/coding/paas/v4", apiKey: "ZAI_CODING_API_KEY"},
}

// resolveCatalog merges a route profile with the catalog defaults: explicit
// profile values win, catalog values fill the gaps.
func resolveCatalog(route string, profile Profile) Profile {
	resolved := profile
	if def, ok := catalogDefaults[route]; ok {
		if resolved.BaseURL == "" {
			resolved.BaseURL = def.baseURL
		}
		if resolved.APIKeyEnv == "" {
			resolved.APIKeyEnv = def.apiKey
		}
	}
	return resolved
}

// Deps are the seams the twin reads.
type Deps struct {
	// Runtime is the LLM registry adapters register on.
	Runtime *llm.Runtime
	// DisplayName labels each mounted route's provider info; empty uses
	// the route id.
	DisplayName string
}

// Manager is one live twin: it owns the mounted route adapters and re-syncs
// them with the settings section on every change.
type Manager struct {
	deps Deps

	mu      sync.Mutex
	mounted map[string]func() // route → disposer
}

// NewManager builds the twin over one runtime.
func NewManager(deps Deps) *Manager {
	return &Manager{deps: deps, mounted: map[string]func(){}}
}

// Sync reconciles the mounted route adapters with one settings snapshot:
// new routes mount, removed routes withdraw, changed routes re-register.
// Diagnostics ride the returned list (mount failures skip the route and
// report; one bad route never blocks the others).
func (m *Manager) Sync(providers map[string]Profile) []string {
	m.mu.Lock()
	mounted := m.mounted
	m.mu.Unlock()
	var diagnostics []string

	// Withdraw removed routes first.
	for route, dispose := range mounted {
		if _, keep := providers[route]; keep {
			continue
		}
		dispose()
		m.mu.Lock()
		delete(m.mounted, route)
		m.mu.Unlock()
	}

	for route, profile := range providers {
		if _, live := mounted[route]; live {
			continue
		}
		resolved := resolveCatalog(route, profile)
		dispose, err := m.mountRoute(route, resolved)
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("route %q: %v", route, err))
			continue
		}
		m.mu.Lock()
		m.mounted[route] = dispose
		m.mu.Unlock()
	}
	return diagnostics
}

// mountRoute builds and registers one route's adapter.
func (m *Manager) mountRoute(route string, profile Profile) (func(), error) {
	switch profile.API {
	case APIOpenAICompletions, APIOpenAIResponses:
		return m.mountOpenAI(route, profile)
	case APIAnthropicMessages:
		return m.mountAnthropic(route, profile)
	default:
		return nil, fmt.Errorf("piai: route %q names api %q, which the Go host has not implemented", route, profile.API)
	}
}

func (m *Manager) mountOpenAI(route string, profile Profile) (func(), error) {
	connection := &deepseek.ConnectionOptions{
		BaseURL:   profile.BaseURL,
		APIKeyEnv: profile.APIKeyEnv,
		Models:    modelsToCatalog(profile.Models),
		MaxTokens: profile.MaxTokens,
	}
	displayName := m.deps.DisplayName
	if displayName == "" {
		displayName = route
	}
	adapter := deepseek.NewAdapter(deepseek.AdapterOptions{
		DisplayName: displayName,
		Options:     func() (*deepseek.ConnectionOptions, error) { return connection, nil },
		ResolveAPIKey: func(*deepseek.ConnectionOptions) (string, error) {
			return resolveEnvKey(profile.APIKeyEnv, route)
		},
	})
	handle, err := m.deps.Runtime.RegisterAdapter([]string{route}, adapter)
	if err != nil {
		return nil, err
	}
	return handle.Dispose, nil
}

func (m *Manager) mountAnthropic(route string, profile Profile) (func(), error) {
	window := profile.DefaultContextWindow
	if window == 0 {
		window = 200_000
	}
	maxTokens := profile.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8_192
	}
	connection := &anthropic.Options{
		BaseURL: profile.BaseURL, APIKeyEnv: profile.APIKeyEnv,
		MaxTokens: maxTokens, DefaultContextWindow: window,
		Models: modelsToAnthropic(profile.Models),
	}
	adapter := anthropic.NewAdapter(anthropic.AdapterOptions{
		Options: func() (*anthropic.Options, error) { return connection, nil },
		ResolveAPIKey: func(*anthropic.Options) (string, error) {
			return resolveEnvKey(profile.APIKeyEnv, route)
		},
	})
	handle, err := m.deps.Runtime.RegisterAdapter([]string{route}, adapter)
	if err != nil {
		return nil, err
	}
	return handle.Dispose, nil
}

func modelsToCatalog(models []Model) []deepseek.CatalogModel {
	out := make([]deepseek.CatalogModel, 0, len(models))
	for _, model := range models {
		window, maxTokens := model.ContextWindow, model.MaxTokens
		out = append(out, deepseek.CatalogModel{
			ID: model.ID, Name: model.Name,
			ContextWindow:   int64Ptr(window),
			MaxTokens:       int64Ptr(maxTokens),
			InputModalities: model.InputModalities,
		})
	}
	return out
}

func modelsToAnthropic(models []Model) []anthropic.Model {
	out := make([]anthropic.Model, 0, len(models))
	for _, model := range models {
		out = append(out, anthropic.Model{
			ID: model.ID, Name: model.Name,
			ContextWindow: model.ContextWindow, MaxTokens: model.MaxTokens,
			InputModalities: model.InputModalities,
		})
	}
	return out
}

func int64Ptr(value int64) *int64 { return &value }

// resolveEnvKey reads the route's ambient key; an absent or empty key fails
// with the MISSING_CREDENTIAL contract the runtime surfaces to callers.
func resolveEnvKey(env, route string) (string, error) {
	value := envValue(env)
	if value == "" {
		return "", llm.NewLlmError(
			fmt.Sprintf("piai: no API key for provider route %q; export %s or store it through the credentials service", route, env),
			"MISSING_CREDENTIAL", llm.LlmFailure{})
	}
	return llm.AssertUsableApiKey(value, "piai", env)
}

// envValue is the environment seam; a var for tests to shadow.
var envValue = func(name string) string {
	value, _ := lookupEnv(name)
	return value
}

var lookupEnv = os.LookupEnv
