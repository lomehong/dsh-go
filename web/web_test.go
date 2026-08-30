package web

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/llm"
)

// fakeSearchProvider is a scripted search backend for the seam tests.
type fakeSearchProvider struct {
	id        string
	available bool
	result    WebSearchResult
	err       error
	requests  []WebSearchRequest
}

func (f *fakeSearchProvider) ID() string      { return f.id }
func (f *fakeSearchProvider) Available() bool { return f.available }
func (f *fakeSearchProvider) Search(ctx context.Context, request WebSearchRequest) (WebSearchResult, error) {
	f.requests = append(f.requests, request)
	return f.result, f.err
}

// fakeFetchProvider is a scripted fetch backend for the seam tests.
type fakeFetchProvider struct {
	id        string
	available bool
	result    WebFetchResult
	err       error
}

func (f *fakeFetchProvider) ID() string      { return f.id }
func (f *fakeFetchProvider) Available() bool { return f.available }
func (f *fakeFetchProvider) Fetch(ctx context.Context, request WebFetchRequest) (WebFetchResult, error) {
	return f.result, f.err
}

func newTestRuntime(t *testing.T, config Config) (*Runtime, *cordis.Context) {
	t.Helper()
	ctx := cordis.NewRoot(cordis.Discard{})
	t.Cleanup(func() { _ = ctx.Dispose() })
	return NewRuntime(ctx, config), ctx
}

func webErrorCode(t *testing.T, err error) string {
	t.Helper()
	var webErr *llm.Error
	if !errors.As(err, &webErr) {
		t.Fatalf("expected a WebError, got %v", err)
	}
	return webErr.Code()
}

func TestRegisterRejectsDuplicateIDs(t *testing.T) {
	runtime, _ := newTestRuntime(t, Config{})
	if _, err := runtime.RegisterSearchProvider(&fakeSearchProvider{id: "deepseek-official", available: true}); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := runtime.RegisterSearchProvider(&fakeSearchProvider{id: "deepseek-official", available: true})
	if got := webErrorCode(t, err); got != CodeDuplicateProvider {
		t.Fatalf("duplicate code = %q", got)
	}
	if want := `a web provider with id "deepseek-official" is already registered`; err.Error() != want {
		t.Fatalf("duplicate wording = %q", err.Error())
	}
	// The fetch registry is a separate capability kind: the same id joins it
	// without complaint.
	if _, err := runtime.RegisterFetchProvider(&fakeFetchProvider{id: "deepseek-official", available: true}); err != nil {
		t.Fatalf("cross-kind registration: %v", err)
	}
	if _, err := runtime.RegisterFetchProvider(&fakeFetchProvider{id: "deepseek-official", available: true}); err == nil {
		t.Fatal("duplicate fetch registration accepted")
	}
}

func TestRegisterDisposersUnregister(t *testing.T) {
	runtime, ctx := newTestRuntime(t, Config{})
	dispose, err := runtime.RegisterSearchProvider(&fakeSearchProvider{id: "one", available: true})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	dispose()
	// Unregistered: the capability now reports no usable provider, and the
	// id can be registered again.
	if _, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"}); err == nil ||
		webErrorCode(t, err) != CodeProviderUnavailable {
		t.Fatalf("post-dispose search = %v", err)
	}
	if _, err := runtime.RegisterSearchProvider(&fakeSearchProvider{id: "one", available: true}); err != nil {
		t.Fatalf("re-register after dispose: %v", err)
	}
	// Context disposal unregisters the remaining provider.
	if err := ctx.Dispose(); err != nil {
		t.Fatalf("dispose ctx: %v", err)
	}
	if _, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"}); err == nil ||
		webErrorCode(t, err) != CodeProviderUnavailable {
		t.Fatalf("post-context-disposal search = %v", err)
	}
}

func TestSearchSelectionSemantics(t *testing.T) {
	provider := &fakeSearchProvider{
		id:        "deepseek-official",
		available: true,
		result:    WebSearchResult{Sources: []WebSearchSource{{URL: "https://example.test/a"}}},
	}

	// No provider at all.
	runtime, _ := newTestRuntime(t, Config{})
	if _, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"}); err == nil ||
		webErrorCode(t, err) != CodeProviderUnavailable ||
		err.Error() != "no usable web provider is registered" {
		t.Fatalf("no-provider search = %q (%v)", err, err)
	}

	// Exactly one usable provider auto-selects.
	if _, err := runtime.RegisterSearchProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	result, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"})
	if err != nil || len(result.Sources) != 1 {
		t.Fatalf("auto-select search = %+v, %v", result, err)
	}

	// A configured id that is not registered fails loud.
	configured, _ := newTestRuntime(t, Config{SearchProvider: "ghost"})
	if _, err := configured.RegisterSearchProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = configured.Search(context.Background(), WebSearchRequest{Query: "q"})
	if got := webErrorCode(t, err); got != CodeProviderConfiguredMissing {
		t.Fatalf("configured missing = %q", got)
	}
	if want := `configured web provider "ghost" is not registered`; err.Error() != want {
		t.Fatalf("configured missing wording = %q", err.Error())
	}

	// A configured id that is registered but unavailable fails loud.
	unavailable := &fakeSearchProvider{id: "down", available: false}
	configuredTwo, _ := newTestRuntime(t, Config{SearchProvider: "down"})
	if _, err := configuredTwo.RegisterSearchProvider(unavailable); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err = configuredTwo.Search(context.Background(), WebSearchRequest{Query: "q"})
	if got := webErrorCode(t, err); got != CodeProviderConfiguredUnavailable {
		t.Fatalf("configured unavailable = %q", got)
	}
	if want := `configured web provider "down" is registered but unavailable`; err.Error() != want {
		t.Fatalf("configured unavailable wording = %q", err.Error())
	}

	// Multiple usable providers without a configured id fail ambiguous, in
	// registration order.
	ambiguous, _ := newTestRuntime(t, Config{})
	if _, err := ambiguous.RegisterSearchProvider(&fakeSearchProvider{id: "alpha", available: true}); err != nil {
		t.Fatalf("register alpha: %v", err)
	}
	if _, err := ambiguous.RegisterSearchProvider(&fakeSearchProvider{id: "beta", available: true}); err != nil {
		t.Fatalf("register beta: %v", err)
	}
	_, err = ambiguous.Search(context.Background(), WebSearchRequest{Query: "q"})
	if got := webErrorCode(t, err); got != CodeProviderAmbiguous {
		t.Fatalf("ambiguous code = %q", got)
	}
	if want := "multiple usable web providers are registered (alpha, beta); configure one explicitly"; err.Error() != want {
		t.Fatalf("ambiguous wording = %q", err.Error())
	}
	// Unavailable registrations do not count toward ambiguity.
	if _, err := ambiguous.RegisterSearchProvider(&fakeSearchProvider{id: "gamma", available: false}); err != nil {
		t.Fatalf("register gamma: %v", err)
	}
	if _, err := ambiguous.Search(context.Background(), WebSearchRequest{Query: "q"}); err == nil ||
		webErrorCode(t, err) != CodeProviderAmbiguous {
		t.Fatalf("ambiguity ignored the unavailable provider: %v", err)
	}

	// Pinning one of the ambiguous providers resolves the selection.
	pinned, _ := newTestRuntime(t, Config{SearchProvider: "beta"})
	if _, err := pinned.RegisterSearchProvider(&fakeSearchProvider{id: "alpha", available: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	beta := &fakeSearchProvider{id: "beta", available: true, result: WebSearchResult{Sources: []WebSearchSource{{URL: "https://example.test/b"}}}}
	if _, err := pinned.RegisterSearchProvider(beta); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := pinned.Search(context.Background(), WebSearchRequest{Query: "q"}); err != nil || len(beta.requests) != 1 {
		t.Fatalf("pinned search reached %d requests: %v", len(beta.requests), err)
	}
}

func TestSearchCapsSourcesAtTheSeam(t *testing.T) {
	overReturning := &fakeSearchProvider{
		id:        "deepseek-official",
		available: true,
		result: WebSearchResult{
			Content: "answer",
			Sources: []WebSearchSource{
				{URL: "https://example.test/1"},
				{URL: "https://example.test/2"},
				{URL: "https://example.test/3"},
			},
		},
	}
	runtime, _ := newTestRuntime(t, Config{})
	if _, err := runtime.RegisterSearchProvider(overReturning); err != nil {
		t.Fatalf("register: %v", err)
	}

	limit := 2
	capped, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q", MaxResults: &limit})
	if err != nil {
		t.Fatalf("capped search: %v", err)
	}
	if !capped.Truncated || len(capped.Sources) != 2 || capped.Sources[1].URL != "https://example.test/2" || capped.Content != "answer" {
		t.Fatalf("capped = %+v", capped)
	}

	// Within the bound: untouched, no truncation flag.
	fits := 5
	untouched, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q", MaxResults: &fits})
	if err != nil || untouched.Truncated || len(untouched.Sources) != 3 {
		t.Fatalf("untouched = %+v, %v", untouched, err)
	}

	// No bound: untouched.
	unbounded, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"})
	if err != nil || unbounded.Truncated || len(unbounded.Sources) != 3 {
		t.Fatalf("unbounded = %+v, %v", unbounded, err)
	}
}

func TestSearchPropagatesProviderErrors(t *testing.T) {
	failing := &fakeSearchProvider{id: "deepseek-official", available: true, err: errors.New("upstream refused")}
	runtime, _ := newTestRuntime(t, Config{})
	if _, err := runtime.RegisterSearchProvider(failing); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"})
	if err == nil || !strings.Contains(err.Error(), "upstream refused") {
		t.Fatalf("provider error = %v", err)
	}
}

func TestFetchSelectionAndResults(t *testing.T) {
	provider := &fakeFetchProvider{
		id:        "http",
		available: true,
		result: WebFetchResult{
			URL:        "https://example.test/final",
			StatusCode: 404,
			Body:       WebFetchBody{Kind: BodyHTML, Content: "<html>missing</html>"},
		},
	}
	runtime, _ := newTestRuntime(t, Config{FetchProvider: "http"})
	if _, err := runtime.RegisterFetchProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A non-2xx response is a result, not an error.
	result, err := runtime.Fetch(context.Background(), WebFetchRequest{URL: "https://example.test/gone"})
	if err != nil || result.StatusCode != 404 || result.Body.Kind != BodyHTML {
		t.Fatalf("fetch = %+v, %v", result, err)
	}

	// The configured search id never bleeds into fetch selection.
	if _, err := runtime.Fetch(context.Background(), WebFetchRequest{URL: "https://example.test/x"}); err != nil {
		t.Fatalf("fetch repeat: %v", err)
	}
}

func TestConfigFallsBackToTheOperationalEnvironment(t *testing.T) {
	t.Setenv(envSearchProvider, "env-search")
	t.Setenv(envFetchProvider, "env-fetch")
	runtime, _ := newTestRuntime(t, Config{})
	if _, err := runtime.RegisterSearchProvider(&fakeSearchProvider{id: "other", available: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The env-pinned id is not registered: the configured-missing path
	// proves the env value fed the selection field.
	_, err := runtime.Search(context.Background(), WebSearchRequest{Query: "q"})
	if got := webErrorCode(t, err); got != CodeProviderConfiguredMissing {
		t.Fatalf("env search selection = %q", got)
	}
	_, err = runtime.Fetch(context.Background(), WebFetchRequest{URL: "https://example.test"})
	if got := webErrorCode(t, err); got != CodeProviderConfiguredMissing {
		t.Fatalf("env fetch selection = %q", got)
	}

	// Explicit config wins over the environment.
	explicit, _ := newTestRuntime(t, Config{SearchProvider: "explicit"})
	_, err = explicit.Search(context.Background(), WebSearchRequest{Query: "q"})
	if want := `configured web provider "explicit" is not registered`; err.Error() != want {
		t.Fatalf("explicit config wording = %q", err.Error())
	}
}
