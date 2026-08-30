package websearchdeepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dshgo/credentials"
	"dshgo/llm"
	"dshgo/web"
)

func deepseekResponse(content []map[string]any) map[string]any {
	return map[string]any{"content": content}
}

func resultBlock(url string) map[string]any {
	return map[string]any{
		"type": "web_search_tool_result",
		"content": []map[string]any{
			{"type": "web_search_result", "url": url, "title": "Example", "page_age": "2 days ago"},
		},
	}
}

func textBlockWithCitation(url, cited string) map[string]any {
	return map[string]any{
		"type": "text",
		"text": "Some prose.",
		"citations": []map[string]any{
			{"url": url, "cited_text": cited},
		},
	}
}

func TestMapAnthropicResponseJoinsCitationsAndDedupes(t *testing.T) {
	payload := messagesResponse{}
	raw, _ := json.Marshal(deepseekResponse([]map[string]any{
		textBlockWithCitation("u1", "excerpt one"),
		textBlockWithCitation("u2", "excerpt two"),
		resultBlock("u1"),
		resultBlock("u1"), // same URL across searches dedupes
	}))
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	result, err := mapAnthropicResponse(payload)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if result.Truncated {
		t.Fatal("provider owns truncation, seam does")
	}
	if len(result.Sources) != 1 {
		t.Fatalf("sources = %+v", result.Sources)
	}
	source := result.Sources[0]
	if source.URL != "u1" || source.Title != "Example" || source.Snippet != "excerpt one" || source.PublishedAt != "2 days ago" {
		t.Fatalf("source = %+v", source)
	}
}

func TestMapAnthropicResponseFailsLoudWithoutResultBlocks(t *testing.T) {
	payload := messagesResponse{}
	raw, _ := json.Marshal(deepseekResponse([]map[string]any{textBlockWithCitation("u1", "excerpt")}))
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if _, err := mapAnthropicResponse(payload); err == nil {
		t.Fatal("prose-only response accepted")
	}
}

func staticServer(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewProvider(func() Options {
		return Options{APIKey: "k", BaseURL: server.URL, Model: DefaultModel, APIVersion: DefaultAPIVersion, MaxTokens: DefaultMaxTokens, MaxUses: DefaultMaxUses}
	})
}

func TestSearchDispatchesMessagesWireShape(t *testing.T) {
	var seen struct {
		auth     string
		apiKey   string
		version  string
		agent    string
		endpoint string
		body     map[string]any
	}
	provider := staticServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen.auth = r.Header.Get("authorization")
		seen.apiKey = r.Header.Get("x-api-key")
		seen.version = r.Header.Get("anthropic-version")
		seen.agent = r.Header.Get("user-agent")
		seen.endpoint = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&seen.body); err != nil {
			t.Errorf("body: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(deepseekResponse([]map[string]any{resultBlock("u1")}))
	})
	result, err := provider.Search(context.Background(), web.WebSearchRequest{Query: "go release"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Sources) != 1 || result.Sources[0].URL != "u1" {
		t.Fatalf("result = %+v", result)
	}
	if seen.auth != "Bearer k" || seen.apiKey != "k" || seen.version != DefaultAPIVersion {
		t.Fatalf("headers = %q %q %q", seen.auth, seen.apiKey, seen.version)
	}
	if seen.endpoint != "/messages" {
		t.Fatalf("endpoint = %s", seen.endpoint)
	}
	if seen.agent != UserAgent {
		t.Fatalf("user-agent = %q", seen.agent)
	}
	if seen.body["model"] != DefaultModel || seen.body["max_tokens"] != float64(DefaultMaxTokens) {
		t.Fatalf("body = %v", seen.body)
	}
	tools, _ := seen.body["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search_20250305" || tool["name"] != "web_search" || tool["max_uses"] != float64(DefaultMaxUses) {
		t.Fatalf("tool = %v", tool)
	}
	messages, _ := seen.body["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("message = %v", first)
	}
}

func TestSearchRecordsRequestBeforeDispatch(t *testing.T) {
	var recorded []RecordedRequest
	provider := NewProvider(func() Options {
		return Options{
			APIKey: "k", BaseURL: "http://127.0.0.1:1", Model: DefaultModel,
			APIVersion: DefaultAPIVersion, MaxTokens: DefaultMaxTokens, MaxUses: DefaultMaxUses,
			RecordRequest: func(request RecordedRequest) { recorded = append(recorded, request) },
		}
	})
	if _, err := provider.Search(context.Background(), web.WebSearchRequest{Query: "q"}); err == nil {
		t.Fatal("unreachable endpoint accepted")
	}
	if len(recorded) != 1 || recorded[0].Endpoint == "" || recorded[0].Body.Model != DefaultModel {
		t.Fatalf("recorded = %+v", recorded)
	}
	if recorded[0].Body.MaxTokens != DefaultMaxTokens || len(recorded[0].Body.Tools) != 1 {
		t.Fatalf("recorded body = %+v", recorded[0].Body)
	}
}

func TestSearchCredentialMissing(t *testing.T) {
	provider := NewProvider(func() Options {
		return Options{BaseURL: DefaultBaseURL, APIKeyEnv: DefaultAPIKeyEnv, Model: DefaultModel, APIVersion: DefaultAPIVersion, MaxTokens: DefaultMaxTokens, MaxUses: DefaultMaxUses}
	})
	if provider.Available() {
		t.Fatal("provider available without any key path")
	}
	_, err := provider.Search(context.Background(), web.WebSearchRequest{Query: "q"})
	var webErr *llm.Error
	if err == nil {
		t.Fatal("missing credential accepted")
	}
	webErr = asError(err)
	if webErr.Code() != CodeCredentialMissing || !strings.Contains(webErr.Error(), `"DEEPSEEK_API_KEY"`) {
		t.Fatalf("err = %+v", webErr)
	}
}

func asError(err error) *llm.Error {
	return err.(*llm.Error)
}

func TestSearchResolvesKeyFromCredentials(t *testing.T) {
	provider := credentials.NewMemoryProvider(map[string]string{
		"DEEPSEEK_API_KEY": "from-credentials",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(deepseekResponse([]map[string]any{resultBlock("u1")}))
	}))
	defer server.Close()
	search := NewProvider(func() Options {
		return Options{
			ResolveAPIKey: ResolveAPIKeyFromCredentials(provider, "DEEPSEEK_API_KEY"),
			BaseURL:       server.URL, Model: DefaultModel,
			APIVersion: DefaultAPIVersion, MaxTokens: DefaultMaxTokens, MaxUses: DefaultMaxUses,
		}
	})
	if !search.Available() {
		t.Fatal("provider unavailable with credential resolver")
	}
	result, err := search.Search(context.Background(), web.WebSearchRequest{Query: "q"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(result.Sources) != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestSearchHTTPErrorUsesProviderDetail(t *testing.T) {
	provider := staticServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	_, err := provider.Search(context.Background(), web.WebSearchRequest{Query: "q"})
	webErr := asError(err)
	if webErr.Code() != CodeProviderError || webErr.Error() != "slow down" {
		t.Fatalf("err = %+v", webErr)
	}

	// A non-JSON error body falls back to the status line, never the body.
	binary := staticServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("gateway garbage"))
	})
	_, err = binary.Search(context.Background(), web.WebSearchRequest{Query: "q"})
	webErr = asError(err)
	if webErr.Code() != CodeProviderError || !strings.Contains(webErr.Error(), "HTTP 502") {
		t.Fatalf("err = %+v", webErr)
	}
}

func TestSearchRefusesRedirect(t *testing.T) {
	redirect := staticServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/messages", http.StatusFound)
	})
	_, err := redirect.Search(context.Background(), web.WebSearchRequest{Query: "q"})
	webErr := asError(err)
	if webErr.Code() != CodeProviderError || !strings.Contains(webErr.Error(), "redirect refused") {
		t.Fatalf("err = %+v", webErr)
	}
}

func TestSearchAbortedBeforeDispatch(t *testing.T) {
	provider := NewProvider(func() Options {
		return Options{APIKey: "k", BaseURL: "http://127.0.0.1:1", Model: DefaultModel, APIVersion: DefaultAPIVersion, MaxTokens: DefaultMaxTokens, MaxUses: DefaultMaxUses}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := provider.Search(ctx, web.WebSearchRequest{Query: "q"})
	if asError(err).Code() != CodeAborted {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveAPIKeyFromEnv(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "from-env")
	resolved, err := ResolveAPIKeyFromEnv("DEEPSEEK_API_KEY")()
	if err != nil || resolved != "from-env" {
		t.Fatalf("resolved = %q %v", resolved, err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	resolved, err = ResolveAPIKeyFromEnv("DEEPSEEK_API_KEY")()
	if err != nil || resolved != "" {
		t.Fatalf("resolved = %q %v", resolved, err)
	}
}
