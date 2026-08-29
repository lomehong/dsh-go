// `DeepSeekAdapter`: HTTP + SSE against a DeepSeek (OpenAI-compatible)
// chat-completions endpoint, emitting harness StreamChunks. The adapter is
// transport-only: connection facts arrive through a thunk resolved once per
// operation and the bearer token through a per-request resolver, so the
// registering plugin owns validation, layering, and credential policy.
// Port of adapter.ts (text-only path; the image / Files-API machinery is
// deferred with its serializer half).
package deepseek

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"dshgo/llm"
)

// Reasoning effort metadata ported from adapter.ts.
var reasoningEfforts = []llm.LlmReasoningEffortInfo{
	{ID: "off", Name: "Off", Description: "Use for simple tasks that do not need reasoning."},
	{ID: "low", Name: "Low", Description: "Prefer for routine or latency-sensitive tasks."},
	{ID: "high", Name: "High", Description: "The default balance for most tasks."},
	{ID: "max", Name: "Max", Description: "Reserve for the hardest quality-first tasks."},
}

var offOnlyReasoningEfforts = []llm.LlmReasoningEffortInfo{
	{ID: "off", Name: "Off", Description: "Use for simple tasks that do not need reasoning."},
}

// appIdentity is the static public application identity sent to the
// provider (attribution.ts). No secrets, paths, session ids, or per-user
// identifiers belong here.
var appIdentity = struct{ Product, Version, URL string }{
	Product: "deepseek-harness", Version: "0.1.2-alpha.1",
	URL: "https://github.com/deepseek-ai/deepseek-harness",
}

// UserAgent renders the standard `product/version (+url)` value.
func UserAgent() string {
	return fmt.Sprintf("%s/%s (+%s)", appIdentity.Product, appIdentity.Version, appIdentity.URL)
}

// attributionHeaders builds the attribution headers every provider request
// sends (currently just user-agent).
func attributionHeaders() map[string]string {
	return map[string]string{"user-agent": UserAgent()}
}

// AdapterOptions are the operation-local resolution hooks the plugin owns.
type AdapterOptions struct {
	// Options returns the current validated connection facts; called once
	// per operation.
	Options func() (*ConnectionOptions, error)
	// ResolveAPIKey resolves the bearer token for the connection facts of
	// one request. The snapshot is passed in — never re-read — so the key
	// can only ever come from the same resolution as the endpoint it is
	// sent to. Fails with LlmError MISSING_CREDENTIAL when no key is
	// available anywhere.
	ResolveAPIKey func(connection *ConnectionOptions) (string, error)
	// ResolveUserID resolves the harness-home anonymous id shared with
	// telemetry and feedback.
	ResolveUserID func() string
	// Extensions registers independently owned top-level request fields;
	// nil carries none.
	Extensions *ExtensionRegistry
	// HTTPClient overrides the transport (tests); nil uses the default.
	HTTPClient *http.Client
}

// Adapter is the DeepSeek chat-completions LLM adapter. One instance
// serves every model name it was registered under (the harness model name
// IS the wire model name). Caller aborts map to ABORTED; the configured
// per-read idle watchdog maps to TIMEOUT.
type Adapter struct {
	config AdapterOptions
	client *http.Client
}

// NewAdapter builds one adapter.
func NewAdapter(config AdapterOptions) *Adapter {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	return &Adapter{config: config, client: client}
}

// ProviderInfo is the DeepSeek display metadata.
func (a *Adapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: "DeepSeek"}
}

// ProviderRetryPolicy is the provider-owned retry policy from the current
// connection facts.
func (a *Adapter) ProviderRetryPolicy(provider string) *llm.ResolvedRetryPolicy {
	connection, err := a.config.Options()
	if err != nil {
		return nil
	}
	return connection.RetryPolicy
}

// ListModels is the advisory catalog.
func (a *Adapter) ListModels(provider string) ([]llm.LlmModelInfo, error) {
	connection, err := a.config.Options()
	if err != nil {
		return nil, err
	}
	models := make([]llm.LlmModelInfo, 0, len(connection.Models))
	for _, model := range connection.Models {
		models = append(models, catalogModelInfo(provider, model))
	}
	return models, nil
}

// catalogModelInfo projects one catalog entry.
func catalogModelInfo(provider string, model CatalogModel) llm.LlmModelInfo {
	name := model.Name
	if name == "" {
		name = model.ID
	}
	return llm.LlmModelInfo{
		Provider: provider, ID: model.ID, Name: name,
		Description: model.Description, InputModalities: model.InputModalities,
	}
}

// ResolveModel resolves all metadata for one exact route; an uncatalogued
// endpoint is safely treated as text-only. Declaring an unverified image
// capability would let the host persist input that the endpoint may reject
// on every later turn.
func (a *Adapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	connection, err := a.config.Options()
	if err != nil {
		return llm.LlmResolvedModelInfo{}, err
	}
	return modelInfoFor(connection, provider, model), nil
}

// modelInfoFor resolves exact-route metadata from one connection snapshot.
func modelInfoFor(connection *ConnectionOptions, provider, model string) llm.LlmResolvedModelInfo {
	var configured *CatalogModel
	for i := range connection.Models {
		if connection.Models[i].ID == model {
			configured = &connection.Models[i]
			break
		}
	}
	contextWindow := connection.DefaultContextWindow
	var info llm.LlmModelInfo
	if configured == nil {
		info = llm.LlmModelInfo{Provider: provider, ID: model, Name: model, InputModalities: []string{"text"}}
	} else {
		info = catalogModelInfo(provider, *configured)
		if configured.ContextWindow != nil {
			contextWindow = *configured.ContextWindow
		}
	}
	defaultMaxTokens := connection.MaxTokens
	if configured != nil && configured.MaxTokens != nil {
		defaultMaxTokens = *configured.MaxTokens
	}
	reasoning := &llm.LlmModelReasoningInfo{Efforts: reasoningEfforts, DefaultEffort: "high"}
	if connection.Defaults.Thinking == "disabled" {
		reasoning = &llm.LlmModelReasoningInfo{Efforts: offOnlyReasoningEfforts, DefaultEffort: "off"}
	} else {
		switch connection.Defaults.ReasoningEffort {
		case "off":
			reasoning.DefaultEffort = "off"
		case "low":
			reasoning.DefaultEffort = "low"
		case "max":
			reasoning.DefaultEffort = "max"
		default:
			reasoning.DefaultEffort = "high"
		}
	}
	return llm.LlmResolvedModelInfo{
		LlmModelInfo:     info,
		Context:          &llm.LlmModelContext{ContextWindow: contextWindow},
		DefaultMaxTokens: &defaultMaxTokens,
		Reasoning:        reasoning,
	}
}

// PrepareCall binds exact model metadata and the eventual dispatch to one
// connection generation, so a settings change between preparation and
// dispatch cannot combine one generation's capabilities with another's
// endpoint.
func (a *Adapter) PrepareCall(provider, model string) (llm.LlmResolvedModelInfo, func(llm.GenerateOptions) llm.Seq, error) {
	connection, err := a.config.Options()
	if err != nil {
		return llm.LlmResolvedModelInfo{}, nil, err
	}
	return modelInfoFor(connection, provider, model), func(options llm.GenerateOptions) llm.Seq {
		return a.streamWithConnection(options, connection)
	}, nil
}

// Stream streams one model call against the current connection facts.
func (a *Adapter) Stream(options llm.GenerateOptions) llm.Seq {
	connection, err := a.config.Options()
	if err != nil {
		return llm.FromChunks([]llm.StreamChunk{llm.TerminalFailureChunk(err, false)})
	}
	return a.streamWithConnection(options, connection)
}

// streamWithConnection runs one request against a frozen connection
// snapshot: one resolution per stream call — connection facts and the
// credential freeze here and hold for this whole request, so an in-flight
// stream never observes a configuration change and the next call
// re-resolves.
func (a *Adapter) streamWithConnection(options llm.GenerateOptions, connection *ConnectionOptions) llm.Seq {
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
		// The image path is deferred: image content is rejected before any
		// wire bytes are built.
		for _, message := range options.Messages {
			if err := assertTextOnly(message.Content); err != nil {
				fail(err)
				return
			}
		}
		apiKey, err := a.config.ResolveAPIKey(connection)
		if err != nil {
			fail(err)
			return
		}
		userID := a.config.ResolveUserID()
		body, err := SerializeRequest(options, connection.Defaults)
		if err != nil {
			fail(err)
			return
		}
		payload, err := json.Marshal(body)
		if err != nil {
			fail(llm.NewLlmError("DeepSeek request serialization failed", "INVALID_REQUEST", llm.LlmFailure{}))
			return
		}
		// Extension fields merge into the serialized body: preparation
		// failure or a base-field collision fails the dispatch before any
		// HTTP traffic; acceptance commits after the 2xx status.
		accept := func() error { return nil }
		if a.config.Extensions != nil {
			facts := RequestFacts{Signal: ctx, SessionID: options.SessionID, Purpose: options.Purpose}
			if json.Unmarshal(payload, &facts.Body) != nil {
				fail(llm.NewLlmError("DeepSeek request serialization failed", "INVALID_REQUEST", llm.LlmFailure{}))
				return
			}
			// Providers see a detached clone: they retain no mutable alias
			// to the outgoing request.
			visible := cloneJSONValue(facts.Body).(map[string]any)
			prepared, err := a.config.Extensions.Prepare(ctx, RequestFacts{
				Body: visible, SessionID: facts.SessionID, Purpose: facts.Purpose, Signal: facts.Signal,
			})
			if err != nil {
				fail(llm.NewLlmError("DeepSeek request extension preparation failed", "REQUEST_EXTENSION", llm.LlmFailure{}))
				return
			}
			for field := range prepared.Fields {
				if _, taken := facts.Body[field]; taken {
					fail(llm.NewLlmError(fmt.Sprintf("DeepSeek request extension field %q collides with the base request", field), "REQUEST_EXTENSION", llm.LlmFailure{}))
					return
				}
			}
			for field, value := range prepared.Fields {
				facts.Body[field] = value
			}
			merged, err := json.Marshal(facts.Body)
			if err != nil {
				fail(llm.NewLlmError("DeepSeek request serialization failed", "INVALID_REQUEST", llm.LlmFailure{}))
				return
			}
			payload = merged
			accept = prepared.Accept
		}

		// The idle watchdog: one stable signal reaches both the initial
		// fetch and body reads. Caller aborts map to ABORTED; the
		// configured per-read idle timeout maps to TIMEOUT.
		watchCtx, watchCancel := context.WithCancel(ctx)
		defer watchCancel()
		idle := time.NewTimer(time.Duration(connection.StreamIdleTimeoutMs) * time.Millisecond)
		defer idle.Stop()
		timedOut := false
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-watchCtx.Done():
			case <-idle.C:
				timedOut = true
				watchCancel()
			}
		}()
		pulse := func() {
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(time.Duration(connection.StreamIdleTimeoutMs) * time.Millisecond)
		}

		chunks, err := a.request(watchCtx, options, connection, apiKey, userID, payload, accept, pulse)
		watchCancel()
		<-watchDone
		if err != nil {
			if timedOut {
				fail(llm.NewLlmError(
					fmt.Sprintf("DeepSeek stream idle timeout after %dms", connection.StreamIdleTimeoutMs),
					"TIMEOUT", llm.LlmFailure{}))
				return
			}
			if ctx.Err() != nil {
				fail(llm.NewLlmError("DeepSeek request aborted by caller", "ABORTED", llm.LlmFailure{}))
				return
			}
			fail(err)
			return
		}
		for chunk := range chunks {
			pulse()
			if !yield(chunk) {
				return
			}
			if chunk.Type == llm.ChunkFinish {
				return
			}
		}
	}
}

// request performs the HTTP round trip and returns the translated chunk
// stream. Terminal failures come back through err; chunk-payload failures
// inside the SSE stream arrive as terminal finish chunks.
func (a *Adapter) request(
	ctx context.Context,
	options llm.GenerateOptions,
	connection *ConnectionOptions,
	apiKey, userID string,
	payload []byte,
	acceptExtensions func() error,
	onActivity func(),
) (llm.Seq, error) {
	headers := map[string]string{
		"authorization": "Bearer " + apiKey,
		"content-type":  "application/json",
		"accept":        "text/event-stream",
	}
	for name, value := range attributionHeaders() {
		headers[name] = value
	}
	if userID != "" {
		headers["x-deepseek-harness-user-id"] = userID
	}
	if options.SessionID != "" {
		headers["x-deepseek-harness-session-id"] = options.SessionID
	}
	if options.Purpose == llm.PurposeCompaction {
		headers["x-deepseek-harness-compact"] = "1"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, connection.BaseURL+"/chat/completions", strings.NewReader(string(payload)))
	if err != nil {
		return nil, llm.NewLlmError(
			fmt.Sprintf("DeepSeek API request to %s failed", connection.BaseURL), "TRANSPORT", llm.LlmFailure{})
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := a.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, llm.NewLlmError(
			fmt.Sprintf("DeepSeek API request to %s failed", connection.BaseURL), "TRANSPORT", llm.LlmFailure{})
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		message := fmt.Sprintf("DeepSeek API error (HTTP %d)", response.StatusCode)
		var parsed WireError
		if json.Unmarshal(raw, &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
			message = parsed.Error.Message
		}
		code := httpErrorCode(response.StatusCode, parsed)
		failure := llm.LlmFailure{Status: response.StatusCode}
		if delay := providerRetryAfterMs(response.Header.Get("Retry-After")); delay > 0 {
			failure.ProviderRetryAfterMs = delay
		}
		if id := requestID(response.Header); id != "" {
			failure.RequestID = llm.ProviderRequestID(id)
		}
		return nil, llm.NewLlmError(message, code, failure)
	}
	// Acceptance commits only after the 2xx status; its failure fails the
	// request as REQUEST_EXTENSION even though the provider answered.
	if acceptExtensions != nil {
		if err := acceptExtensions(); err != nil {
			response.Body.Close()
			return nil, llm.NewLlmError("DeepSeek request extension acceptance failed", "REQUEST_EXTENSION", llm.LlmFailure{})
		}
	}
	if response.Body == nil {
		return nil, llm.NewLlmError("DeepSeek API returned no response body", llm.EmptyResponseCode, llm.LlmFailure{})
	}
	chunks := Translate(ParseSse(response.Body, func(string) { onActivity() }))
	return chunks, nil
}

// httpErrorCode maps an HTTP status to a stable LlmError code.
func httpErrorCode(status int, err WireError) string {
	if status == 401 || status == 403 {
		return "AUTH"
	}
	if status == 413 {
		return "INVALID_REQUEST"
	}
	detail := ""
	if err.Error != nil {
		parts := []string{}
		for _, part := range []string{err.Error.Code, err.Error.Type, err.Error.Message} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		detail = strings.Join(parts, " ")
	}
	if llm.IsQuotaExceededError(detail) {
		return llm.QuotaExceededCode
	}
	if status == 429 {
		return "RATE_LIMIT"
	}
	if status == 400 {
		if llm.IsContextWindowExceededError(detail) {
			return llm.ContextWindowExceededCode
		}
		return "INVALID_REQUEST"
	}
	if status >= 500 {
		return "SERVER"
	}
	return fmt.Sprintf("HTTP_%d", status)
}

// providerRetryAfterMs parses the Retry-After header: integer seconds or
// an HTTP date; only positive delays survive.
func providerRetryAfterMs(value string) int64 {
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		delay := seconds * 1_000
		if delay > 0 {
			return delay
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil {
		delay := time.Until(when).Milliseconds()
		if delay > 0 {
			return delay
		}
	}
	return 0
}

// requestID reads the provider request id from the response headers.
func requestID(header http.Header) string {
	value := header.Get("x-request-id")
	if value == "" {
		value = header.Get("x-deepseek-request-id")
	}
	return value
}
