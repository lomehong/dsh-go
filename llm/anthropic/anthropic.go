// Package anthropic implements the Anthropic Messages wire protocol as an
// llm.Adapter family: POST {baseURL}/v1/messages with x-api-key +
// anthropic-version headers, SSE streaming, tool use through tool_use /
// tool_result content blocks, and thinking through thinking_delta. Port of
// the wire half of the official pi-ai anthropic provider (the Go host's
// multi-provider round).
package anthropic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"dshgo/llm"
)

// Options is the validated connection facts for one anthropic route.
type Options struct {
	// BaseURL is the endpoint base; `/v1/messages` is appended.
	BaseURL string
	// APIKeyEnv is the credential reference resolved per request.
	APIKeyEnv string
	// Version is the anthropic-version header value; empty defaults to
	// the 2023-06-01 GA wire.
	Version string
	// MaxTokens is the default per-request output cap (the anthropic wire
	// REQUIRES max_tokens on every request).
	MaxTokens int64
	// DefaultContextWindow is the fallback context capacity.
	DefaultContextWindow int64
	// Models is the advisory catalog.
	Models []Model
	// StreamIdleTimeoutMs bounds one stream read idle period.
	StreamIdleTimeoutMs int64
	// RetryPolicy is the provider-owned retry policy.
	RetryPolicy *llm.ResolvedRetryPolicy
}

// Model is one advisory catalog entry.
type Model struct {
	ID              string
	Name            string
	ContextWindow   int64
	MaxTokens       int64
	InputModalities []string
}

// AdapterOptions parameterizes one adapter.
type AdapterOptions struct {
	// Options returns the current validated connection facts.
	Options func() (*Options, error)
	// ResolveAPIKey resolves the bearer key for the facts of one request.
	ResolveAPIKey func(*Options) (string, error)
	// HTTPClient is optional; nil uses the default client.
	HTTPClient *http.Client
}

// Adapter is one anthropic-messages wire adapter over one route.
type Adapter struct {
	options AdapterOptions
	client  *http.Client
}

// NewAdapter builds one adapter.
func NewAdapter(options AdapterOptions) *Adapter {
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{options: options, client: client}
}

// ProviderInfo is the route's display metadata.
func (a *Adapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: provider}
}

// ProviderRetryPolicy is the provider-owned retry policy.
func (a *Adapter) ProviderRetryPolicy(provider string) *llm.ResolvedRetryPolicy { return nil }

// ListModels is the advisory catalog.
func (a *Adapter) ListModels(provider string) ([]llm.LlmModelInfo, error) {
	options, err := a.options.Options()
	if err != nil {
		return nil, err
	}
	models := make([]llm.LlmModelInfo, 0, len(options.Models))
	for _, model := range options.Models {
		name := model.Name
		if name == "" {
			name = model.ID
		}
		modalities := model.InputModalities
		if modalities == nil {
			modalities = []string{"text"}
		}
		models = append(models, llm.LlmModelInfo{
			Provider: provider, ID: model.ID, Name: name, InputModalities: modalities,
		})
	}
	return models, nil
}

// ResolveModel resolves one exact route's metadata.
func (a *Adapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	options, err := a.options.Options()
	if err != nil {
		return llm.LlmResolvedModelInfo{}, err
	}
	window := options.DefaultContextWindow
	maxTokens := options.MaxTokens
	for _, entry := range options.Models {
		if entry.ID == model {
			if entry.ContextWindow > 0 {
				window = entry.ContextWindow
			}
			if entry.MaxTokens > 0 {
				maxTokens = entry.MaxTokens
			}
			break
		}
	}
	info := llm.LlmResolvedModelInfo{
		LlmModelInfo:     llm.LlmModelInfo{Provider: provider, ID: model},
		Context:          &llm.LlmModelContext{ContextWindow: window},
		DefaultMaxTokens: &maxTokens,
	}
	return info, nil
}

func contextPointer(value int64) *int64 { return &value }

// maxTokensFloor is the wire's minimum max_tokens; the anthropic contract
// requires the field on every request.
const maxTokensFloor = int64(1024)

// Stream streams one model call as raw chunks.
func (a *Adapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	return func(yield func(llm.StreamChunk) bool) {
		ctx := options.Context
		if ctx == nil {
			ctx = context.Background()
		}
		aborted := ctx.Err() != nil
		fail := func(err error) bool {
			yield(llm.TerminalFailureChunk(err, aborted))
			return false
		}
		facts, err := a.options.Options()
		if err != nil {
			fail(err)
			return
		}
		apiKey, err := a.options.ResolveAPIKey(facts)
		if err != nil {
			fail(err)
			return
		}
		payload, err := json.Marshal(buildRequest(options, facts))
		if err != nil {
			fail(llm.NewLlmError("anthropic: request serialization failed", "INVALID_REQUEST", llm.LlmFailure{}))
			return
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimSuffix(facts.BaseURL, "/")+"/v1/messages", strings.NewReader(string(payload)))
		if err != nil {
			fail(llm.NewLlmError("anthropic: request build failed", "TRANSPORT", llm.LlmFailure{}))
			return
		}
		request.Header.Set("content-type", "application/json")
		request.Header.Set("accept", "text/event-stream")
		request.Header.Set("x-api-key", apiKey)
		version := facts.Version
		if version == "" {
			version = "2023-06-01"
		}
		request.Header.Set("anthropic-version", version)
		response, err := a.client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				fail(ctx.Err())
				return
			}
			fail(llm.NewLlmError(fmt.Sprintf("anthropic API request to %s failed", facts.BaseURL), "TRANSPORT", llm.LlmFailure{}))
			return
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			raw, _ := io.ReadAll(response.Body)
			response.Body.Close()
			message := fmt.Sprintf("anthropic API error (HTTP %d)", response.StatusCode)
			var parsed struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(raw, &parsed)
			if parsed.Error.Message != "" {
				message = parsed.Error.Message
			}
			code := "PROVIDER_ERROR"
			switch {
			case response.StatusCode == 401 || response.StatusCode == 403:
				code = "AUTH"
			case response.StatusCode == 429:
				code = "RATE_LIMITED"
			}
			fail(llm.NewLlmError(message, code, llm.LlmFailure{Status: response.StatusCode}))
			return
		}
		defer response.Body.Close()

		idle := time.NewTimer(time.Duration(facts.StreamIdleTimeoutMs) * time.Millisecond)
		defer idle.Stop()
		go func() {
			select {
			case <-ctx.Done():
				idle.Stop()
			case <-idle.C:
				idle.Stop()
			}
		}()

		translateSse(bufio.NewReader(response.Body), yield)
	}
}
