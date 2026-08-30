package toolweb

import (
	"context"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/systemprompt"
	"dshgo/tools"
	"dshgo/web"
)

func newRuntime(t *testing.T) (*tools.ToolRuntime, *systemprompt.SystemPrompt) {
	t.Helper()
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("system prompt: %v", err)
	}
	return runtime, prompt
}

func TestParseSearchArgs(t *testing.T) {
	if _, err := ParseSearchArgs(nil, 4); err == nil {
		t.Fatal("empty queries accepted")
	}
	if _, err := ParseSearchArgs([]string{"a", "b", "c", "d", "e"}, 4); err == nil {
		t.Fatal("over-budget queries accepted")
	}
	if _, err := ParseSearchArgs([]string{"a", " "}, 4); err == nil {
		t.Fatal("blank query accepted")
	}
	queries, err := ParseSearchArgs([]string{"go release", "go release", " go release "}, 4)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Exact duplicates collapse; distinct strings (even after trimming) stay.
	if len(queries) != 2 || queries[0] != "go release" || queries[1] != " go release " {
		t.Fatalf("queries = %v", queries)
	}
}

func TestFormatSearchOutput(t *testing.T) {
	output := FormatSearchOutput(web.WebSearchResult{
		Content: "Answer text.",
		Sources: []web.WebSearchSource{
			{URL: "https://example.com/a", Title: "A", Snippet: "snippet", PublishedAt: "2026-01-01"},
			{URL: "https://example.org/b"},
		},
		Truncated: true,
	})
	for _, want := range []string{
		ExternalWebContentNotice,
		"Answer text.",
		"- [A](https://example.com/a) — snippet (2026-01-01)",
		"- [example.org](https://example.org/b)",
		"(Showing the first 2 sources.",
		"Cite the relevant URLs above",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	empty := FormatSearchOutput(web.WebSearchResult{})
	if !strings.Contains(empty, "No results found.") {
		t.Fatalf("empty output = %s", empty)
	}
}

type scriptedSearch struct {
	id      string
	results map[string]web.WebSearchResult
	errs    map[string]error
	queries []string
}

func (s *scriptedSearch) ID() string      { return s.id }
func (s *scriptedSearch) Available() bool { return true }
func (s *scriptedSearch) Search(_ context.Context, request web.WebSearchRequest) (web.WebSearchResult, error) {
	s.queries = append(s.queries, request.Query)
	if err, ok := s.errs[request.Query]; ok {
		return web.WebSearchResult{}, err
	}
	return s.results[request.Query], nil
}

func TestMergeSearchResultsRoundRobinDedup(t *testing.T) {
	merged := MergeSearchResults(
		[]string{"q1", "q2"},
		[]web.WebSearchResult{
			{Sources: []web.WebSearchSource{{URL: "u1"}, {URL: "u2"}, {URL: "u3"}}, Content: "one"},
			{Sources: []web.WebSearchSource{{URL: "u1"}, {URL: "u4"}}, Content: "two"},
		},
		8,
	)
	urls := make([]string, 0, len(merged.Sources))
	for _, source := range merged.Sources {
		urls = append(urls, source.URL)
	}
	// Round-robin rank order with u1 deduplicated.
	if strings.Join(urls, ",") != "u1,u2,u4,u3" {
		t.Fatalf("merge order = %v", urls)
	}
	if merged.Content != "### q1\n\none\n\n### q2\n\ntwo" {
		t.Fatalf("content = %q", merged.Content)
	}
	if merged.Truncated {
		t.Fatal("unexpected truncation")
	}

	// The cap drops and flags.
	capped := MergeSearchResults([]string{"q"}, []web.WebSearchResult{{Sources: []web.WebSearchSource{{URL: "a"}, {URL: "b"}}}}, 1)
	if len(capped.Sources) != 1 || !capped.Truncated {
		t.Fatalf("capped = %+v", capped)
	}
}

func TestApplyWebSearchToolEndToEnd(t *testing.T) {
	runtime, prompt := newRuntime(t)
	search := &scriptedSearch{id: "deepseek-official", results: map[string]web.WebSearchResult{
		"go release": {Sources: []web.WebSearchSource{{URL: "https://go.dev/doc/devel/release"}}},
	}}
	seam := web.NewRuntime(cordis.NewRoot(cordis.Discard{}), web.Config{SearchProvider: "deepseek-official"})
	if _, err := seam.RegisterSearchProvider(search); err != nil {
		t.Fatalf("register: %v", err)
	}
	options := SearchOptions{MaxResults: WebSearchMaxResults, MaxQueries: WebSearchMaxQueries, TimeoutMs: DefaultWebToolTimeoutMs}
	unregister, err := ApplyWebSearchTool(runtime, prompt, seam, options)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer unregister()

	if _, ok := runtime.Get(ToolNameWebSearch, nil); !ok {
		t.Fatal("web_search not registered")
	}
	run := runtime.Execute(&tools.ToolExecutionInput{
		Name:      ToolNameWebSearch,
		Arguments: map[string]any{"queries": []any{"go release"}},
		Signal:    context.Background(),
	})
	if run.IsError {
		t.Fatalf("execute failed: %v", run.Error)
	}
	text := run.Content[0].Text
	for _, want := range []string{ExternalWebContentNotice, "[go.dev](https://go.dev/doc/devel/release)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("render missing %q:\n%s", want, text)
		}
	}
	if len(search.queries) != 1 || search.queries[0] != "go release" {
		t.Fatalf("provider queries = %v", search.queries)
	}

	// Argument violations surface as tool errors, not panics.
	bad := runtime.Execute(&tools.ToolExecutionInput{
		Name:      ToolNameWebSearch,
		Arguments: map[string]any{"queries": []any{}},
		Signal:    context.Background(),
	})
	if !bad.IsError {
		t.Fatal("empty queries accepted")
	}
}

func TestRunSearchQueriesFirstFailureWins(t *testing.T) {
	search := &scriptedSearch{id: "p", errs: map[string]error{"bad": context.DeadlineExceeded}}
	seam := web.NewRuntime(cordis.NewRoot(cordis.Discard{}), web.Config{SearchProvider: "p"})
	if _, err := seam.RegisterSearchProvider(search); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := RunSearchQueries(context.Background(), seam, []string{"good", "bad"}, 8); err == nil {
		t.Fatal("batch failure swallowed")
	}
}

func TestApplyWebFetchToolEndToEnd(t *testing.T) {
	runtime, prompt := newRuntime(t)
	provider := &stubFetch{id: "http", result: web.WebFetchResult{
		URL:        "https://example.com/page",
		StatusCode: 200,
		Body:       web.WebFetchBody{Kind: web.BodyText, Content: "plain page"},
	}}
	seam := web.NewRuntime(cordis.NewRoot(cordis.Discard{}), web.Config{FetchProvider: "http"})
	if _, err := seam.RegisterFetchProvider(provider); err != nil {
		t.Fatalf("register: %v", err)
	}
	unregister, err := ApplyWebFetchTool(runtime, prompt, seam, FetchOptions{MaxOutputChars: DefaultFetchMaxOutputChars, TimeoutMs: DefaultWebToolTimeoutMs})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	defer unregister()

	if _, ok := runtime.Get(ToolNameWebFetch, nil); !ok {
		t.Fatal("web_fetch not registered")
	}
	result := runtime.Execute(&tools.ToolExecutionInput{
		Name:      ToolNameWebFetch,
		Arguments: map[string]any{"url": "https://example.com/page"},
		Signal:    context.Background(),
	})
	if result.IsError {
		t.Fatalf("execute failed: %v", result.Error)
	}
	text := result.Content[0].Text
	if !strings.HasPrefix(text, "Fetched https://example.com/page (HTTP 200)") ||
		!strings.Contains(text, ExternalWebContentNotice) ||
		!strings.Contains(text, "plain page") {
		t.Fatalf("render = %s", text)
	}

	// A blank URL fails validation.
	bad := runtime.Execute(&tools.ToolExecutionInput{
		Name:      ToolNameWebFetch,
		Arguments: map[string]any{"url": "  "},
		Signal:    context.Background(),
	})
	if !bad.IsError {
		t.Fatal("blank url accepted")
	}
}

type stubFetch struct {
	id     string
	result web.WebFetchResult
}

func (s *stubFetch) ID() string      { return s.id }
func (s *stubFetch) Available() bool { return true }
func (s *stubFetch) Fetch(_ context.Context, request web.WebFetchRequest) (web.WebFetchResult, error) {
	return s.result, nil
}

func TestRenderFetchOutputCaps(t *testing.T) {
	// A source cut past the output cap flags truncation, per official
	// renderBody semantics (the sliced content is the source cut).
	result := web.WebFetchResult{
		URL:        "https://example.com",
		StatusCode: 200,
		Body:       web.WebFetchBody{Kind: web.BodyText, Content: strings.Repeat("x", 500)},
	}
	text, truncated := renderFetchResult(result, 600)
	if !truncated || len(text) != 600 || !strings.HasSuffix(text, truncationFooter) {
		t.Fatalf("cap = %d chars, truncated=%v", len(text), truncated)
	}
	if !strings.HasPrefix(text, "Fetched https://example.com (HTTP 200)") {
		t.Fatalf("header missing: %s", text)
	}

	// An output cap smaller than the footer squeezes the prefix hard.
	squeezed, squeezedTruncated := renderFetchResult(result, 100)
	if !squeezedTruncated || len(squeezed) != 100 || !strings.HasSuffix(squeezed, truncationFooter) {
		t.Fatalf("squeeze = %d chars, truncated=%v", len(squeezed), squeezedTruncated)
	}

	// Content wholly inside the cap renders without a footer.
	small := web.WebFetchResult{
		URL:        "https://example.com",
		StatusCode: 200,
		Body:       web.WebFetchBody{Kind: web.BodyText, Content: "short"},
	}
	text, truncated = renderFetchResult(small, DefaultFetchMaxOutputChars)
	if truncated || strings.HasSuffix(text, truncationFooter) {
		t.Fatalf("small output falsely truncated: %v %q", truncated, text)
	}

	// A provider-side cut propagates even when the whole text fits.
	cut := small
	cut.Truncated = true
	text, truncated = renderFetchResult(cut, DefaultFetchMaxOutputChars)
	if !truncated || !strings.HasSuffix(text, truncationFooter) {
		t.Fatalf("provider truncation lost: %v %s", truncated, text)
	}
}

func TestHTMLConversion(t *testing.T) {
	converted := RenderHTMLBody("<script>evil()</script><h1>Title</h1><p>Hello <strong>bold</strong> and <a href=\"https://x\">link</a>.</p><ul><li>one</li><li>two</li></ul>")
	for _, want := range []string{"# Title", "**bold**", "[link](https://x)", "- one", "- two"} {
		if !strings.Contains(converted, want) {
			t.Fatalf("conversion missing %q:\n%s", want, converted)
		}
	}
	if strings.Contains(converted, "evil") {
		t.Fatal("script content survived")
	}
	if got := RenderHTMLBody(""); got != "" {
		t.Fatalf("empty conversion = %q", got)
	}
	entities := RenderHTMLBody("<p>fish &amp; chips &lt;3</p>")
	if !strings.Contains(entities, "fish & chips <3") {
		t.Fatalf("entities = %s", entities)
	}
}

func TestSearchMetaProjection(t *testing.T) {
	meta := SearchMeta(web.WebSearchResult{
		Content:   "answer",
		Truncated: true,
		Sources:   []web.WebSearchSource{{URL: "u", Title: "t", Snippet: "s", PublishedAt: "d"}, {URL: "v"}},
	})
	sources := meta["sources"].([]any)
	first := sources[0].(map[string]any)
	if first["title"] != "t" || first["snippet"] != "s" || first["publishedAt"] != "d" {
		t.Fatalf("source meta = %v", first)
	}
	if _, has := sources[1].(map[string]any)["title"]; has {
		t.Fatal("absent field projected")
	}
	if meta["answer"] != "answer" || meta["truncated"] != true {
		t.Fatalf("meta = %v", meta)
	}
}
