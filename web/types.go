// Package web is the web access capability seam (official ctx.web): one
// owner for provider selection, cancellation, errors, and product
// configuration across search and fetch, with separate request and result
// types per capability. Ported from @deepseek-ai/dsh-web (types.ts +
// index.ts); the invariant companion registers nothing (provider maps stay
// private; selection and result caps are enforced on each call).
package web

import (
	"context"

	"dshgo/llm"
)

// WebSearchRequest is what one search-capable backend is asked to search.
// Each request carries one query; a consumer may issue several requests.
// MaxResults is a dsh-tool-web-layer bound passed through unchanged and
// enforced on the way back by the seam (see capSources).
type WebSearchRequest struct {
	Query string
	// Upper bound on returned sources; the seam truncates to it. Nil = no
	// bound. dsh-tool-web always sets it. A provider whose API supports a
	// result-count control should apply it at the request layer as a
	// cost/latency optimization; the seam enforces the bound regardless.
	MaxResults *int
}

// WebSearchResult is the normalized search outcome. Content is optional
// provider-generated answer text or summary (DeepSeek returns none;
// Perplexity-style providers return a generated answer). Sources is the
// portable citation shape. Truncated is set by the seam when it cut
// Sources down to MaxResults.
type WebSearchResult struct {
	// Optional provider-generated answer text, search context, or summary;
	// the empty string is the absent value.
	Content string
	// Citeable sources, already truncated to the request's MaxResults.
	Sources []WebSearchSource
	// True when the seam dropped sources to honor MaxResults.
	Truncated bool
}

// WebSearchSource is one citeable source. A source always has a URL; the
// other fields are absent as the empty string because not every provider
// returns them — forcing adapters to invent them would make the seam lie.
// dsh-tool-web renders the title with the hostname as the display fallback.
type WebSearchSource struct {
	URL string
	// Optional display title.
	Title string
	// Optional excerpt.
	Snippet string
	// Publication/crawl timestamp as a provider-supplied ISO-8601 string.
	PublishedAt string
}

// WebFetchRequest is what one fetch-capable backend is asked to retrieve.
// The request deliberately omits timeout, format, prompt, and extraction
// controls: cancellation is the execution context, while presentation and
// higher-level LLM concerns belong outside safe retrieval.
type WebFetchRequest struct {
	URL string
}

// WebFetchBodyKind discriminates a decoded fetch body. The union is CLOSED
// and owned by dsh-web: the provider decodes the kind and dsh-tool-web
// renders it, so a new kind is a coordinated change across known packages,
// not a plugin extension. Consumers switch on the kind and fail loud on an
// unknown value (the official assertNever default arm).
type WebFetchBodyKind string

// The closed body-kind union.
const (
	BodyHTML WebFetchBodyKind = "html"
	BodyText WebFetchBodyKind = "text"
)

// WebFetchBody is the decoded body of a fetched resource; Kind is one of
// the closed kinds above.
type WebFetchBody struct {
	Kind    WebFetchBodyKind
	Content string
}

// WebFetchResult is the normalized fetch outcome. A successful network
// fetch of a non-2xx response is a result, not an error: the status code is
// part of the fetched resource state. WebError is reserved for failures to
// safely retrieve or represent the resource.
type WebFetchResult struct {
	// The final URL after allowed redirects (the request URL is in the
	// request).
	URL string
	// HTTP status code of the fetched response.
	StatusCode int
	// Decoded body, classified by content kind.
	Body WebFetchBody
	// True when the provider capped the decoded body.
	Truncated bool
}

// WebSearchProvider is a search-capable backend, registered with
// Runtime.RegisterSearchProvider. The ID is a stable string, unique within
// the search capability kind.
type WebSearchProvider interface {
	ID() string
	// Cheap local usability check; must not make network calls.
	Available() bool
	// Run one search; honor ctx for cancellation.
	Search(ctx context.Context, request WebSearchRequest) (WebSearchResult, error)
}

// WebFetchProvider is a fetch-capable backend, registered with
// Runtime.RegisterFetchProvider. The ID is a stable string, unique within
// the fetch capability kind.
type WebFetchProvider interface {
	ID() string
	// Cheap local usability check; must not make network calls.
	Available() bool
	// Retrieve one URL; honor ctx for cancellation.
	Fetch(ctx context.Context, request WebFetchRequest) (WebFetchResult, error)
}

// The shared web failure codes: unavailable, missing, unusable, ambiguous, or
// duplicate providers. Providers add their own machine-routable codes on
// top (the fetch provider distinguishes invalid or blocked URLs, redirects,
// size and timeout limits, and unsupported content types); consumers route
// on code and tolerate provider-specific values.
const (
	CodeDuplicateProvider             = "WEB_DUPLICATE_PROVIDER"
	CodeProviderConfiguredMissing     = "WEB_PROVIDER_CONFIGURED_MISSING"
	CodeProviderConfiguredUnavailable = "WEB_PROVIDER_CONFIGURED_UNAVAILABLE"
	CodeProviderUnavailable           = "WEB_PROVIDER_UNAVAILABLE"
	CodeProviderAmbiguous             = "WEB_PROVIDER_AMBIGUOUS"
)

// NewWebError builds the typed web error: a machine-routable open-string
// code over the harness error base with chained cause.
func NewWebError(code, message string, cause error) *llm.Error {
	return llm.NewError(code, message, cause)
}
