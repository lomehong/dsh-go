package websearchdeepseek

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
	"dshgo/web"
)

// Stable id this provider registers under.
const ProviderID = "deepseek-official"

// Defaults carried by the official Config schema.
const (
	// DefaultBaseURL is DeepSeek's Anthropic-compatible API, `/v1` included
	// (`/messages` is appended). This is NOT the chat-completions base the
	// llm-deepseek adapter uses, so DEEPSEEK_BASE_URL is deliberately not
	// reused — only the API key is shared.
	DefaultBaseURL = "https://api.deepseek.com/anthropic/v1"
	// DefaultModel is the default Anthropic-format model name.
	DefaultModel = "deepseek-v4-flash"
	// DefaultAPIVersion is the default `anthropic-version` header value.
	DefaultAPIVersion = "2023-06-01"
	// DefaultMaxTokens bounds generated tokens for one Messages request.
	DefaultMaxTokens = 4096
	// DefaultMaxUses bounds `web_search` server-tool uses per request.
	DefaultMaxUses = 5
	// DefaultAPIKeyEnv is the credential reference named by
	// missing-credential diagnostics.
	DefaultAPIKeyEnv = "DEEPSEEK_API_KEY"
	// SearchBaseURLEnv names this provider's endpoint override. Deliberately
	// distinct from DEEPSEEK_BASE_URL: search speaks the Anthropic-compatible
	// Messages API, so one variable cannot serve both.
	SearchBaseURLEnv = "DEEPSEEK_SEARCH_BASE_URL"
	// UserAgent is the attribution header sent on every request.
	UserAgent = "deepseek-harness/0.0.1"
)

// Error codes this provider fails with (official WebError union members).
const (
	CodeProviderError     = "WEB_PROVIDER_ERROR"
	CodeAborted           = "WEB_ABORTED"
	CodeCredentialMissing = "WEB_PROVIDER_CREDENTIAL_MISSING"
)

func webError(code, format string, args ...any) *llm.Error {
	return web.NewWebError(code, fmt.Sprintf(format, args...), nil)
}

// searchResultItem is one `web_search_result` inside a
// `web_search_tool_result` block.
type searchResultItem struct {
	Type    string  `json:"type"`
	URL     string  `json:"url"`
	Title   *string `json:"title,omitempty"`
	PageAge *string `json:"page_age,omitempty"`
}

// searchToolResultBlock is the citeable-result content block.
type searchToolResultBlock struct {
	Type    string             `json:"type"`
	Content []searchResultItem `json:"content,omitempty"`
}

// citationLocation is one citation inside a `text` block — the snippet
// source.
type citationLocation struct {
	URL       *string `json:"url,omitempty"`
	CitedText *string `json:"cited_text,omitempty"`
}

// textBlock is the model's prose plus per-URL citations.
type textBlock struct {
	Type      string             `json:"type"`
	Text      *string            `json:"text,omitempty"`
	Citations []citationLocation `json:"citations,omitempty"`
}

// contentBlock is any response content block; only
// `web_search_tool_result` and `text` are consumed.
type contentBlock struct {
	Type string `json:"type"`
}

// messagesResponse is the Anthropic Messages response envelope.
type messagesResponse struct {
	Content []json.RawMessage `json:"content,omitempty"`
}

// messagesError is the error envelope (best-effort; fields vary).
type messagesError struct {
	Error   json.RawMessage `json:"error,omitempty"`
	Message string          `json:"message,omitempty"`
}

// blockType reads one content block's discriminant; "" means unreadable.
func blockType(raw json.RawMessage) string {
	var probe contentBlock
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return probe.Type
}

// rawBlocks decodes the envelope's blocks into the two consumed shapes.
func rawBlocks(response messagesResponse) (resultBlocks []searchToolResultBlock, textBlocks []textBlock) {
	for _, raw := range response.Content {
		switch blockType(raw) {
		case "web_search_tool_result":
			var typed searchToolResultBlock
			if json.Unmarshal(raw, &typed) == nil {
				resultBlocks = append(resultBlocks, typed)
			}
		case "text":
			var typed textBlock
			if json.Unmarshal(raw, &typed) == nil {
				textBlocks = append(textBlocks, typed)
			}
		}
	}
	return resultBlocks, textBlocks
}

// citationSnippets builds the `url → cited_text` map from every `text`
// block's citations. Anthropic `web_search_result` items carry
// url/title/page_age but typically NO inline snippet — the excerpt lives in a
// separate text block's citation keyed by url (first occurrence wins).
func citationSnippets(response messagesResponse) map[string]string {
	_, textBlocks := rawBlocks(response)
	snippets := map[string]string{}
	for _, block := range textBlocks {
		for _, cite := range block.Citations {
			if cite.URL == nil || cite.CitedText == nil || *cite.URL == "" || *cite.CitedText == "" {
				continue
			}
			if _, seen := snippets[*cite.URL]; !seen {
				snippets[*cite.URL] = *cite.CitedText
			}
		}
	}
	return snippets
}

// mapAnthropicResponse maps a Messages response to a normalized search
// result: walks `web_search_tool_result` blocks, joins each item to its
// citation excerpt, dedupes by url (max_uses > 1 can surface the same URL
// across searches). Truncated stays false — the seam owns final truncation.
func mapAnthropicResponse(response messagesResponse) (web.WebSearchResult, error) {
	resultBlocks, _ := rawBlocks(response)
	if len(resultBlocks) == 0 {
		return web.WebSearchResult{}, webError(CodeProviderError,
			"DeepSeek returned no web_search_tool_result blocks; the request may not have triggered native web search")
	}
	snippets := citationSnippets(response)
	seen := map[string]bool{}
	sources := make([]web.WebSearchSource, 0)
	for _, block := range resultBlocks {
		for _, item := range block.Content {
			if item.Type != "web_search_result" || item.URL == "" || seen[item.URL] {
				continue
			}
			seen[item.URL] = true
			source := web.WebSearchSource{URL: item.URL}
			if item.Title != nil && *item.Title != "" {
				source.Title = *item.Title
			}
			if snippet, ok := snippets[item.URL]; ok && snippet != "" {
				source.Snippet = snippet
			}
			if item.PageAge != nil && *item.PageAge != "" {
				source.PublishedAt = *item.PageAge
			}
			sources = append(sources, source)
		}
	}
	return web.WebSearchResult{Sources: sources, Truncated: false}, nil
}
