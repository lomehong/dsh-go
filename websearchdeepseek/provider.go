package websearchdeepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"dshgo/credentials"
	"dshgo/web"
)

// Options carries one search operation's fully resolved configuration (the
// official DeepSeekSearchProviderOptions). The provider reads them through a
// per-operation thunk so a settings change landing between searches never
// mixes one search's key with another search's endpoint.
type Options struct {
	// APIKey is a literal DeepSeek API key; when non-empty it wins over
	// ResolveAPIKey.
	APIKey string
	// ResolveAPIKey resolves the current key for one search; nil means the
	// ambient fallback only.
	ResolveAPIKey func() (string, error)
	// APIKeyEnv is the credential reference named by missing-credential
	// diagnostics.
	APIKeyEnv string
	// BaseURL is the endpoint base; `/messages` is appended.
	BaseURL string
	// Model is the Anthropic-format model name.
	Model string
	// APIVersion is the `anthropic-version` header value.
	APIVersion string
	// MaxTokens bounds generated tokens for the Messages request.
	MaxTokens int
	// MaxUses bounds `web_search` server-tool uses per request.
	MaxUses int
	// RecordRequest records the exact secret-free request immediately before
	// dispatch; a non-nil throw prevents dispatch so model-visible auxiliary
	// input cannot escape logging. The session-append face (official
	// `web/deepseek-search-llm-request`) is a host-injected seam.
	RecordRequest func(RecordedRequest)
}

// RecordedRequest is the secret-free Messages request recorded immediately
// before one auxiliary search dispatch.
type RecordedRequest struct {
	Endpoint   string              `json:"endpoint"`
	APIVersion string              `json:"apiVersion"`
	Body       RecordedRequestBody `json:"body"`
}

// RecordedRequestBody mirrors the Messages body shape exactly.
type RecordedRequestBody struct {
	Model     string                   `json:"model"`
	MaxTokens int                      `json:"max_tokens"`
	Messages  []RecordedRequestMessage `json:"messages"`
	Tools     []RecordedRequestTool    `json:"tools"`
}

// RecordedRequestMessage is one user turn of the recorded body.
type RecordedRequestMessage struct {
	Role    string                   `json:"role"`
	Content []RecordedRequestContent `json:"content"`
}

// RecordedRequestContent is one text block of the recorded body.
type RecordedRequestContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// RecordedRequestTool is the native server-tool declaration of the recorded
// body.
type RecordedRequestTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses"`
}

// Available reports whether one search can resolve a key and a sane
// endpoint: a literal key or a resolver, a parseable base URL, and positive
// integer bounds.
func (o Options) Available() bool {
	hasKey := len(o.APIKey) > 0 || o.ResolveAPIKey != nil
	_, err := url.Parse(o.BaseURL)
	return hasKey && err == nil && o.MaxTokens > 0 && o.MaxUses > 0
}

// Provider is the DeepSeek-backed search provider: an Anthropic-compatible
// Messages model call with the native `web_search_20250305` server tool. The
// wire client is provider-private and does not use ctx.llm. HTTP redirects
// fail as WEB_PROVIDER_ERROR.
type Provider struct {
	id             string
	resolveOptions func() Options
	client         *http.Client
	// nowquirk reserved for future injection seams.
}

// NewProvider builds the provider over an options thunk. Each search
// snapshots the options once at entry so one search never mixes two settings
// sections.
func NewProvider(resolveOptions func() Options) *Provider {
	return &Provider{
		id:             ProviderID,
		resolveOptions: resolveOptions,
		// Redirects are refused before following: the credential-bearing
		// request must not be replayed to another origin.
		client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// ID implements the seam's provider identity.
func (p *Provider) ID() string { return p.id }

// Available reports whether the current options can dispatch one search.
func (p *Provider) Available() bool { return p.resolveOptions().Available() }

// messagesRequest is the fixed-shape Messages request body.
type messagesRequest struct {
	Model    string           `json:"model"`
	MaxToken int              `json:"max_tokens"`
	Messages []requestMessage `json:"messages"`
	Tools    []requestTool    `json:"tools"`
}

type requestMessage struct {
	Role    string         `json:"role"`
	Content []requestBlock `json:"content"`
}

type requestBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type requestTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses"`
}

// Search dispatches one Messages search and normalizes the response. The API
// key resolves per operation without being retained on the provider.
func (p *Provider) Search(ctx context.Context, request web.WebSearchRequest) (web.WebSearchResult, error) {
	if err := ctx.Err(); err != nil {
		return web.WebSearchResult{}, searchAborted()
	}
	options := p.resolveOptions()
	apiKey, err := p.apiKey(options)
	if err != nil {
		return web.WebSearchResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return web.WebSearchResult{}, searchAborted()
	}
	endpoint := strings.TrimSuffix(options.BaseURL, "/") + "/messages"
	body := messagesRequest{
		Model:    options.Model,
		MaxToken: options.MaxTokens,
		Messages: []requestMessage{{
			Role: "user",
			Content: []requestBlock{{
				Type: "text",
				Text: fmt.Sprintf("Perform a web search for the query: %s", request.Query),
			}},
		}},
		Tools: []requestTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: options.MaxUses}},
	}
	if options.RecordRequest != nil {
		options.RecordRequest(recordedRequest(endpoint, options.APIVersion, body))
	}
	if err := ctx.Err(); err != nil {
		return web.WebSearchResult{}, searchAborted()
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return web.WebSearchResult{}, webError(CodeProviderError, "DeepSeek search request failed: %v", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return web.WebSearchResult{}, webError(CodeProviderError, "DeepSeek search request failed: %v", err)
	}
	// Official DeepSeek expects `x-api-key`; an Anthropic-compatible proxy
	// may expect `Authorization: Bearer` — send both so either resolves.
	httpRequest.Header.Set("x-api-key", apiKey)
	httpRequest.Header.Set("authorization", "Bearer "+apiKey)
	httpRequest.Header.Set("anthropic-version", options.APIVersion)
	httpRequest.Header.Set("content-type", "application/json")
	httpRequest.Header.Set("accept", "application/json")
	httpRequest.Header.Set("user-agent", UserAgent)

	httpResponse, err := p.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return web.WebSearchResult{}, searchAborted()
		}
		return web.WebSearchResult{}, webError(CodeProviderError, "DeepSeek search request failed: %v", err)
	}
	defer httpResponse.Body.Close()
	// A refused redirect settles as the 3xx response (ErrUseLastResponse);
	// following it would leak the credential to another origin.
	if httpResponse.StatusCode >= 300 && httpResponse.StatusCode < 400 {
		return web.WebSearchResult{}, webError(CodeProviderError, "DeepSeek search request failed: redirect refused (HTTP %d)", httpResponse.StatusCode)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode > 299 {
		return web.WebSearchResult{}, httpError(httpResponse)
	}
	var payload messagesResponse
	if err := json.NewDecoder(io.LimitReader(httpResponse.Body, 32<<20)).Decode(&payload); err != nil {
		if ctx.Err() != nil {
			return web.WebSearchResult{}, searchAborted()
		}
		return web.WebSearchResult{}, webError(CodeProviderError, "DeepSeek returned an unprocessable response body: %v", err)
	}
	return mapAnthropicResponse(payload)
}

// httpError extracts the provider's error detail best-effort; a
// malformed/non-JSON error body (normal for gateway 5xx/429s) costs only a
// richer message, never the real error.
func httpError(response *http.Response) error {
	message := fmt.Sprintf("DeepSeek API error (HTTP %d)", response.StatusCode)
	var parsed messagesError
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&parsed) == nil {
		if detail := errorMessage(parsed); detail != "" {
			message = detail
		}
	}
	return webError(CodeProviderError, "%s", message)
}

// errorMessage digs the detail out of the varying error envelope: `error`
// may be a string or {message}, else top-level `message`.
func errorMessage(parsed messagesError) string {
	if len(parsed.Error) > 0 {
		var asString string
		if json.Unmarshal(parsed.Error, &asString) == nil && asString != "" {
			return asString
		}
		var asObject struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(parsed.Error, &asObject) == nil && asObject.Message != "" {
			return asObject.Message
		}
	}
	return parsed.Message
}

// apiKey resolves one operation's credential. The literal key wins; the
// resolver runs next; the diagnostic names the configured reference.
func (p *Provider) apiKey(options Options) (string, error) {
	ref := options.APIKeyEnv
	if ref == "" {
		ref = DefaultAPIKeyEnv
	}
	if options.APIKey != "" {
		return options.APIKey, nil
	}
	if options.ResolveAPIKey != nil {
		resolved, err := options.ResolveAPIKey()
		if err != nil {
			return "", webError(CodeProviderError, "DeepSeek search credential resolution failed: %v", err)
		}
		if resolved != "" {
			return resolved, nil
		}
	}
	return "", webError(CodeCredentialMissing,
		"DeepSeek search has no API key for %q; store it through the credentials service"+
			" (the web Models page writes it), export it in the launching environment, or set a literal"+
			" \"apiKey\" in the web-search-deepseek config", ref)
}

func searchAborted() error {
	return webError(CodeAborted, "DeepSeek search aborted")
}

func recordedRequest(endpoint, apiVersion string, body messagesRequest) (recorded RecordedRequest) {
	recorded.Endpoint = endpoint
	recorded.APIVersion = apiVersion
	recorded.Body.Model = body.Model
	recorded.Body.MaxTokens = body.MaxToken
	for _, message := range body.Messages {
		entry := RecordedRequestMessage{Role: message.Role}
		for _, block := range message.Content {
			entry.Content = append(entry.Content, RecordedRequestContent{Type: block.Type, Text: block.Text})
		}
		recorded.Body.Messages = append(recorded.Body.Messages, entry)
	}
	for _, tool := range body.Tools {
		recorded.Body.Tools = append(recorded.Body.Tools, RecordedRequestTool{Type: tool.Type, Name: tool.Name, MaxUses: tool.MaxUses})
	}
	return recorded
}

// ResolveAPIKeyFromCredentials adapts the credentials service into the
// per-search key resolver: the service resolves per call and consumers must
// not cache across operations (official `credentials.resolve`).
func ResolveAPIKeyFromCredentials(provider credentials.Provider, ref string) func() (string, error) {
	return func() (string, error) {
		resolved, err := provider.Resolve(credentials.Ref(ref))
		if err != nil {
			return "", err
		}
		if resolved == nil {
			return "", nil
		}
		return resolved.Value, nil
	}
}

// ResolveAPIKeyFromEnv reads the launching environment; without the
// credential service the environment is the whole credential plane (official
// launchEnvironmentOf fallback).
func ResolveAPIKeyFromEnv(ref string) func() (string, error) {
	return func() (string, error) {
		value := os.Getenv(ref)
		if value == "" {
			return "", nil
		}
		return value, nil
	}
}
