package web

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"dshgo/cordis"
)

// The operational environment overrides. They feed the SAME config fields as
// the composition entry and are NOT a hidden priority chain.
const (
	envSearchProvider = "DSH_WEB_SEARCH_PROVIDER"
	envFetchProvider  = "DSH_WEB_FETCH_PROVIDER"
)

// Config for the web seam. SearchProvider / FetchProvider pin which provider
// wins for each capability; both are optional (a single registered usable
// provider auto-selects). Operational overrides such as environment
// variables feed these same fields rather than introduce a hidden priority
// chain.
type Config struct {
	// Explicit search provider id. Empty = auto-select when exactly one
	// usable.
	SearchProvider string
	// Explicit fetch provider id. Empty = auto-select when exactly one
	// usable.
	FetchProvider string
}

// Runtime is the web access service (official ctx.web, one instance per
// context): registries and provider-selecting execution for search and
// fetch. Duplicate ids are rejected. At execution time, a configured
// provider must exist and be usable; without one, exactly one usable
// provider is required, so selection never depends on registration order.
//
// Selection semantics (resolved at execution time, never order-dependent):
//   - A configured id that is registered and Available() → that provider.
//   - A configured id not registered → WEB_PROVIDER_CONFIGURED_MISSING.
//   - A configured id registered but unavailable →
//     WEB_PROVIDER_CONFIGURED_UNAVAILABLE.
//   - No id configured, exactly one registered usable provider → that
//     provider.
//   - No id configured, multiple usable providers → WEB_PROVIDER_AMBIGUOUS.
//   - No id configured, no usable provider → WEB_PROVIDER_UNAVAILABLE.
//
// Go mapping note: an explicitly empty provider id is treated exactly like
// omission (the official empty-string config walks the configured branch
// straight into CONFIGURED_MISSING; no sane composition pins an empty id).
type Runtime struct {
	ctx *cordis.Context

	mu              sync.Mutex
	searchProviders map[string]WebSearchProvider
	searchOrder     []string
	fetchProviders  map[string]WebFetchProvider
	fetchOrder      []string

	searchProviderID string
	fetchProviderID  string
}

// NewRuntime builds the seam over ctx. Provider selection config follows the
// official constructor: the explicit config wins, otherwise the operational
// environment variable supplies the same field.
func NewRuntime(ctx *cordis.Context, config Config) *Runtime {
	searchID := config.SearchProvider
	if searchID == "" {
		searchID = os.Getenv(envSearchProvider)
	}
	fetchID := config.FetchProvider
	if fetchID == "" {
		fetchID = os.Getenv(envFetchProvider)
	}
	return &Runtime{
		ctx:              ctx,
		searchProviders:  map[string]WebSearchProvider{},
		fetchProviders:   map[string]WebFetchProvider{},
		searchProviderID: searchID,
		fetchProviderID:  fetchID,
	}
}

// RegisterSearchProvider registers a search provider. It fails with a
// WEB_DUPLICATE_PROVIDER WebError when the id is already registered for
// search. The returned disposer unregisters the provider; it disposes with
// the owning context too.
func (r *Runtime) RegisterSearchProvider(provider WebSearchProvider) (cordis.Disposer, error) {
	r.mu.Lock()
	if _, exists := r.searchProviders[provider.ID()]; exists {
		r.mu.Unlock()
		return nil, NewWebError(CodeDuplicateProvider,
			fmt.Sprintf("a web provider with id %q is already registered", provider.ID()), nil)
	}
	r.mu.Unlock()
	var dispose cordis.Disposer
	err := r.ctx.Effect(func() (cordis.Disposer, error) {
		r.mu.Lock()
		r.searchProviders[provider.ID()] = provider
		r.searchOrder = append(r.searchOrder, provider.ID())
		r.mu.Unlock()
		dispose = func() {
			r.mu.Lock()
			delete(r.searchProviders, provider.ID())
			r.searchOrder = removeID(r.searchOrder, provider.ID())
			r.mu.Unlock()
		}
		return dispose, nil
	})
	if err != nil {
		return nil, err
	}
	// The official disposer is fire-and-forget; unregister is idempotent, so
	// a manual call that races the context disposal stays safe.
	return dispose, nil
}

// RegisterFetchProvider registers a fetch provider. It fails with a
// WEB_DUPLICATE_PROVIDER WebError when the id is already registered for
// fetch. The returned disposer unregisters the provider; it disposes with
// the owning context too.
func (r *Runtime) RegisterFetchProvider(provider WebFetchProvider) (cordis.Disposer, error) {
	r.mu.Lock()
	if _, exists := r.fetchProviders[provider.ID()]; exists {
		r.mu.Unlock()
		return nil, NewWebError(CodeDuplicateProvider,
			fmt.Sprintf("a web provider with id %q is already registered", provider.ID()), nil)
	}
	r.mu.Unlock()
	var dispose cordis.Disposer
	err := r.ctx.Effect(func() (cordis.Disposer, error) {
		r.mu.Lock()
		r.fetchProviders[provider.ID()] = provider
		r.fetchOrder = append(r.fetchOrder, provider.ID())
		r.mu.Unlock()
		dispose = func() {
			r.mu.Lock()
			delete(r.fetchProviders, provider.ID())
			r.fetchOrder = removeID(r.fetchOrder, provider.ID())
			r.mu.Unlock()
		}
		return dispose, nil
	})
	if err != nil {
		return nil, err
	}
	return dispose, nil
}

// Search runs one search through the selected provider. The provider is
// resolved at call time with the selection rules above; the call fails with
// a WebError when the capability cannot run. The seam enforces
// request.MaxResults on the result: when the provider over-returns, Sources
// is truncated and Truncated set.
func (r *Runtime) Search(ctx context.Context, request WebSearchRequest) (WebSearchResult, error) {
	r.mu.Lock()
	configured := r.searchProviderID
	ids := append([]string(nil), r.searchOrder...)
	providers := make(map[string]WebSearchProvider, len(r.searchProviders))
	for id, provider := range r.searchProviders {
		providers[id] = provider
	}
	r.mu.Unlock()
	provider, err := resolveProvider(configured, ids, providers)
	if err != nil {
		return WebSearchResult{}, err
	}
	result, err := provider.Search(ctx, request)
	if err != nil {
		return WebSearchResult{}, err
	}
	return capSources(result, request.MaxResults), nil
}

// Fetch retrieves one URL through the selected provider. The provider is
// resolved at call time with the selection rules above; the call fails with
// a WebError when the capability cannot run. A non-2xx response is a
// result, not an error.
func (r *Runtime) Fetch(ctx context.Context, request WebFetchRequest) (WebFetchResult, error) {
	r.mu.Lock()
	configured := r.fetchProviderID
	ids := append([]string(nil), r.fetchOrder...)
	providers := make(map[string]WebFetchProvider, len(r.fetchProviders))
	for id, provider := range r.fetchProviders {
		providers[id] = provider
	}
	r.mu.Unlock()
	provider, err := resolveProvider(configured, ids, providers)
	if err != nil {
		return WebFetchResult{}, err
	}
	return provider.Fetch(ctx, request)
}

// resolvable is the selection-time view of either provider kind.
type resolvable interface {
	ID() string
	Available() bool
}

// resolveProvider resolves the selected provider or returns the matching
// WebError. The ids slice carries registration order so the ambiguous
// wording lists providers deterministically (the official Map iteration
// order).
func resolveProvider[P resolvable](configuredID string, ids []string, providers map[string]P) (P, error) {
	var none P
	if configuredID != "" {
		provider, ok := providers[configuredID]
		if !ok {
			return none, NewWebError(CodeProviderConfiguredMissing,
				fmt.Sprintf("configured web provider %q is not registered", configuredID), nil)
		}
		if !provider.Available() {
			return none, NewWebError(CodeProviderConfiguredUnavailable,
				fmt.Sprintf("configured web provider %q is registered but unavailable", configuredID), nil)
		}
		return provider, nil
	}
	var usable []P
	for _, id := range ids {
		if provider := providers[id]; provider.Available() {
			usable = append(usable, provider)
		}
	}
	if len(usable) == 0 {
		return none, NewWebError(CodeProviderUnavailable, "no usable web provider is registered", nil)
	}
	if len(usable) > 1 {
		names := make([]string, 0, len(usable))
		for _, provider := range usable {
			names = append(names, provider.ID())
		}
		return none, NewWebError(CodeProviderAmbiguous,
			fmt.Sprintf("multiple usable web providers are registered (%s); configure one explicitly",
				strings.Join(names, ", ")), nil)
	}
	return usable[0], nil
}

// capSources enforces MaxResults on a search result: truncate Sources and
// flag it. Nil leaves the result untouched.
func capSources(result WebSearchResult, maxResults *int) WebSearchResult {
	if maxResults == nil || len(result.Sources) <= *maxResults {
		return result
	}
	capped := result
	capped.Sources = append([]WebSearchSource(nil), result.Sources[:*maxResults]...)
	capped.Truncated = true
	return capped
}

// removeID drops one id from an ordered registration list.
func removeID(ids []string, id string) []string {
	out := ids[:0]
	for _, entry := range ids {
		if entry != id {
			out = append(out, entry)
		}
	}
	return out
}
