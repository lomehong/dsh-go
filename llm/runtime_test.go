package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func int64Ptr(v int64) *int64 { return &v }

func float64Ptr(v float64) *float64 { return &v }

// stubAdapter is a configurable Adapter for runtime contract tests.
type stubAdapter struct {
	BaseAdapter
	providers      map[string]LlmProviderInfo
	policies       map[string]*ResolvedRetryPolicy
	catalogs       map[string][]LlmModelInfo
	catalogErr     error
	resolved       map[string]LlmResolvedModelInfo
	resolveErr     error
	streams        []StreamChunk
	panicOnStream  bool
	receivedOption *GenerateOptions
	receivedMsgs   []Message
}

func (a *stubAdapter) Stream(options GenerateOptions) Seq {
	if a.panicOnStream {
		panic(errors.New("adapter exploded"))
	}
	if a.receivedOption == nil {
		captured := options
		a.receivedOption = &captured
		a.receivedMsgs = options.Messages
	}
	return FromChunks(a.streams)
}

func (a *stubAdapter) ProviderInfo(provider string) LlmProviderInfo {
	if info, ok := a.providers[provider]; ok {
		return info
	}
	return LlmProviderInfo{ID: provider, Name: provider}
}

func (a *stubAdapter) ProviderRetryPolicy(provider string) *ResolvedRetryPolicy {
	return a.policies[provider]
}

func (a *stubAdapter) ListModels(provider string) ([]LlmModelInfo, error) {
	if a.catalogErr != nil {
		return nil, a.catalogErr
	}
	return a.catalogs[provider], nil
}

func (a *stubAdapter) ResolveModel(provider, model string) (LlmResolvedModelInfo, error) {
	if a.resolveErr != nil {
		return LlmResolvedModelInfo{}, a.resolveErr
	}
	if resolved, ok := a.resolved[model]; ok {
		return resolved, nil
	}
	return LlmResolvedModelInfo{LlmModelInfo: LlmModelInfo{Provider: provider, ID: model, Name: model}}, nil
}

func reasonChunk(kind string, code string) StreamChunk {
	reason := &FinishReason{Kind: kind}
	if code != "" {
		reason.Failure = &LlmFailure{Message: "failed", Code: code}
	}
	return StreamChunk{Type: ChunkFinish, Reason: reason}
}

func collect(t *testing.T, seq Seq) []StreamChunk {
	t.Helper()
	var out []StreamChunk
	for chunk := range seq {
		out = append(out, chunk)
	}
	return out
}

// errorCode extracts the shared taxonomy code from a runtime error.
func errorCode(err error) string {
	var llmErr *LlmError
	if errors.As(err, &llmErr) {
		return llmErr.Code()
	}
	var harness *Error
	if errors.As(err, &harness) {
		return harness.Code()
	}
	return "<no-code>"
}

func TestRegisterAdapterAllOrNothing(t *testing.T) {
	rt := NewRuntime()
	first := &stubAdapter{}
	if _, err := rt.RegisterAdapter([]string{"alpha", "beta"}, first); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	second := &stubAdapter{}
	_, err := rt.RegisterAdapter([]string{"gamma", "alpha"}, second)
	if err == nil {
		t.Fatal("expected DUPLICATE_ADAPTER")
	}
	if got := errorCode(err); got != CodeDuplicateAdapter {
		t.Fatalf("code = %q, want DUPLICATE_ADAPTER", got)
	}
	// All-or-nothing: gamma must NOT be registered.
	for _, provider := range []string{"gamma"} {
		if _, err := rt.Registration(provider); err == nil {
			t.Fatalf("provider %q registered despite failed registration", provider)
		}
	}
	providers := rt.ListProviders()
	if len(providers) != 2 || providers[0].ID != "alpha" || providers[1].ID != "beta" {
		t.Fatalf("ListProviders = %v", providers)
	}
}

func TestRegisterAdapterEmptyProviders(t *testing.T) {
	rt := NewRuntime()
	_, err := rt.RegisterAdapter(nil, &stubAdapter{})
	if err == nil || errorCode(err) != CodeInvalidAdapter {
		t.Fatalf("err = %v, want INVALID_ADAPTER", err)
	}
	_, err = rt.RegisterAdapter([]string{""}, &stubAdapter{})
	if err == nil || errorCode(err) != CodeInvalidAdapter {
		t.Fatalf("empty name err = %v, want INVALID_ADAPTER", err)
	}
}

func TestRegisterAdapterInvalidMetadata(t *testing.T) {
	rt := NewRuntime()
	bad := &stubAdapter{providers: map[string]LlmProviderInfo{
		"beta": {ID: "wrong", Name: "Beta"},
	}}
	if _, err := rt.RegisterAdapter([]string{"alpha", "beta"}, bad); err == nil || errorCode(err) != CodeInvalidAdapter {
		t.Fatalf("err = %v, want INVALID_ADAPTER", err)
	}
	if _, err := rt.Registration("alpha"); err == nil {
		t.Fatal("alpha registered despite all-or-nothing failure")
	}
}

func TestReplaceRoutes(t *testing.T) {
	rt := NewRuntime()
	adapter := &stubAdapter{}
	handle, err := rt.RegisterAdapter([]string{"alpha"}, adapter)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Legal grow.
	if err := handle.Replace([]string{"alpha", "beta"}); err != nil {
		t.Fatalf("replace grow: %v", err)
	}
	if len(rt.ListProviders()) != 2 {
		t.Fatalf("providers after grow = %v", rt.ListProviders())
	}
	// Rejected candidate leaves routes untouched.
	other := &stubAdapter{}
	if _, err := rt.RegisterAdapter([]string{"zeta"}, other); err != nil {
		t.Fatalf("register zeta: %v", err)
	}
	if err := handle.Replace([]string{"alpha", "zeta"}); err == nil || errorCode(err) != CodeDuplicateAdapter {
		t.Fatalf("replace dup err = %v", err)
	}
	providers := rt.ListProviders()
	if len(providers) != 3 {
		t.Fatalf("providers after rejected replace = %v", providers)
	}
	// Empty array legal.
	if err := handle.Replace(nil); err != nil {
		t.Fatalf("replace empty: %v", err)
	}
	for _, provider := range []string{"alpha", "beta"} {
		if _, err := rt.Registration(provider); err == nil {
			t.Fatalf("%s still registered after empty replace", provider)
		}
	}
	// Handle still usable to re-add routes (not disposed).
	if err := handle.Replace([]string{"alpha"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	// Dispose then use fails.
	handle.Dispose()
	if err := handle.Replace([]string{"omega"}); err == nil || errorCode(err) != "REGISTRATION_DISPOSED" {
		t.Fatalf("disposed replace err = %v, want REGISTRATION_DISPOSED", err)
	}
	if _, err := rt.Registration("alpha"); err == nil {
		t.Fatal("alpha registered after dispose")
	}
	// Double dispose is a no-op.
	handle.Dispose()
}

func TestListModelsCatalogValidation(t *testing.T) {
	adapter := &stubAdapter{catalogs: map[string][]LlmModelInfo{
		"alpha": {
			{Provider: "alpha", ID: "m1", Name: "Model One"},
			{Provider: "alpha", ID: "m2", Name: "Model Two"},
		},
	}}
	rt := NewRuntime()
	if _, err := rt.RegisterAdapter([]string{"alpha"}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	models, err := rt.ListModels("alpha")
	if err != nil || len(models) != 2 {
		t.Fatalf("ListModels = %v, %v", models, err)
	}
	// Foreign provider in catalog.
	adapter.catalogs["alpha"] = []LlmModelInfo{{Provider: "elsewhere", ID: "m1", Name: "X"}}
	if _, err := rt.ListModels("alpha"); err == nil || errorCode(err) != CodeInvalidCatalog {
		t.Fatalf("foreign-provider catalog err = %v", err)
	}
	// Duplicate ids.
	adapter.catalogs["alpha"] = []LlmModelInfo{
		{Provider: "alpha", ID: "m1", Name: "A"},
		{Provider: "alpha", ID: "m1", Name: "B"},
	}
	if _, err := rt.ListModels("alpha"); err == nil || errorCode(err) != CodeInvalidCatalog {
		t.Fatalf("duplicate catalog err = %v", err)
	}
	// Missing name.
	adapter.catalogs["alpha"] = []LlmModelInfo{{Provider: "alpha", ID: "m1", Name: ""}}
	if _, err := rt.ListModels("alpha"); err == nil || errorCode(err) != CodeInvalidCatalog {
		t.Fatalf("missing-name catalog err = %v", err)
	}
	// Adapter throw.
	adapter.catalogErr = errors.New("boom")
	if _, err := rt.ListModels("alpha"); err == nil {
		t.Fatal("expected adapter catalog error to propagate")
	}
	// Unregistered provider → NO_ADAPTER.
	if _, err := rt.ListModels("ghost"); err == nil || errorCode(err) != CodeNoAdapter {
		t.Fatalf("ghost err = %v", err)
	}
}

func TestNormalizeModelInfoCodes(t *testing.T) {
	provider, model := "alpha", "m1"
	base := LlmModelInfo{Provider: provider, ID: model, Name: "M"}
	cases := []struct {
		name     string
		resolved LlmResolvedModelInfo
		wantCode string
	}{
		{"wrong provider", LlmResolvedModelInfo{LlmModelInfo: LlmModelInfo{Provider: "other", ID: model, Name: "M"}}, "INVALID_MODEL_INFO"},
		{"wrong id", LlmResolvedModelInfo{LlmModelInfo: LlmModelInfo{Provider: provider, ID: "other", Name: "M"}}, "INVALID_MODEL_INFO"},
		{"missing name", LlmResolvedModelInfo{LlmModelInfo: LlmModelInfo{Provider: provider, ID: model}}, "INVALID_MODEL_INFO"},
		{"zero context", LlmResolvedModelInfo{LlmModelInfo: base, Context: &LlmModelContext{ContextWindow: 0}}, "INVALID_MODEL_CONTEXT"},
		{"negative context", LlmResolvedModelInfo{LlmModelInfo: base, Context: &LlmModelContext{ContextWindow: -5}}, "INVALID_MODEL_CONTEXT"},
		{"zero maxTokens", LlmResolvedModelInfo{LlmModelInfo: base, DefaultMaxTokens: int64Ptr(0)}, "INVALID_MODEL_MAX_TOKENS"},
		{"empty efforts", LlmResolvedModelInfo{LlmModelInfo: base, Reasoning: &LlmModelReasoningInfo{}}, "INVALID_MODEL_REASONING"},
		{"effort missing name", LlmResolvedModelInfo{LlmModelInfo: base, Reasoning: &LlmModelReasoningInfo{
			Efforts: []LlmReasoningEffortInfo{{ID: "high"}},
		}}, "INVALID_MODEL_REASONING"},
		{"duplicate efforts", LlmResolvedModelInfo{LlmModelInfo: base, Reasoning: &LlmModelReasoningInfo{
			Efforts: []LlmReasoningEffortInfo{{ID: "high", Name: "H"}, {ID: "high", Name: "H2"}},
		}}, "INVALID_MODEL_REASONING"},
		{"unknown default effort", LlmResolvedModelInfo{LlmModelInfo: base, Reasoning: &LlmModelReasoningInfo{
			Efforts: []LlmReasoningEffortInfo{{ID: "high", Name: "H"}}, DefaultEffort: "low",
		}}, "INVALID_MODEL_REASONING"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRuntime()
			adapter := &stubAdapter{resolved: map[string]LlmResolvedModelInfo{model: tc.resolved}}
			if _, err := rt.RegisterAdapter([]string{provider}, adapter); err != nil {
				t.Fatalf("register: %v", err)
			}
			_, err := rt.ResolveModelInfo(provider, model)
			if err == nil || errorCode(err) != tc.wantCode {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
		})
	}
	// Valid reasoning metadata passes.
	rt := NewRuntime()
	adapter := &stubAdapter{resolved: map[string]LlmResolvedModelInfo{model: {
		LlmModelInfo: base,
		Context:      &LlmModelContext{ContextWindow: 128000},
		Reasoning: &LlmModelReasoningInfo{
			Efforts: []LlmReasoningEffortInfo{{ID: "low", Name: "Low"}, {ID: "high", Name: "High"}},
		},
	}}}
	if _, err := rt.RegisterAdapter([]string{provider}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	resolved, err := rt.ResolveModelInfo(provider, model)
	if err != nil {
		t.Fatalf("valid resolve: %v", err)
	}
	if resolved.Context.ContextWindow != 128000 || len(resolved.Reasoning.Efforts) != 2 {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestResolveCallConfig(t *testing.T) {
	provider := "alpha"
	reasoning := &LlmModelReasoningInfo{
		Efforts:       []LlmReasoningEffortInfo{{ID: "low", Name: "Low"}, {ID: "high", Name: "High"}},
		DefaultEffort: "low",
	}
	rt := NewRuntime()
	adapter := &stubAdapter{resolved: map[string]LlmResolvedModelInfo{
		"m1": {LlmModelInfo: LlmModelInfo{Provider: provider, ID: "m1", Name: "M1"}, Reasoning: reasoning, DefaultMaxTokens: int64Ptr(4096)},
		"m2": {LlmModelInfo: LlmModelInfo{Provider: provider, ID: "m2", Name: "M2"}},
	}}
	if _, err := rt.RegisterAdapter([]string{provider}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Default effort materialized.
	config, err := rt.ResolveCallConfig(LlmCallConfig{Provider: provider, Model: "m1"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if config.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort = %q, want low", config.ReasoningEffort)
	}
	if config.MaxTokens == nil || *config.MaxTokens != 4096 {
		t.Fatalf("MaxTokens = %v, want 4096", config.MaxTokens)
	}
	// Supported explicit effort preserved.
	config, err = rt.ResolveCallConfig(LlmCallConfig{Provider: provider, Model: "m1", ReasoningEffort: "high"})
	if err != nil || config.ReasoningEffort != "high" {
		t.Fatalf("explicit effort = %v, %v", config, err)
	}
	// Unsupported explicit effort.
	_, err = rt.ResolveCallConfig(LlmCallConfig{Provider: provider, Model: "m1", ReasoningEffort: "turbo"})
	if err == nil || errorCode(err) != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("turbo err = %v", err)
	}
	// Effort on non-reasoning model.
	_, err = rt.ResolveCallConfig(LlmCallConfig{Provider: provider, Model: "m2", ReasoningEffort: "low"})
	if err == nil || errorCode(err) != "UNSUPPORTED_REASONING_EFFORT" {
		t.Fatalf("non-reasoning err = %v", err)
	}
	// No effort on non-reasoning model passes untouched.
	config, err = rt.ResolveCallConfig(LlmCallConfig{Provider: provider, Model: "m2"})
	if err != nil || config.ReasoningEffort != "" || config.MaxTokens != nil {
		t.Fatalf("plain config = %+v, %v", config, err)
	}
}

func TestPrepareCallOneShot(t *testing.T) {
	provider, model := "alpha", "m1"
	rt := NewRuntime()
	adapter := &stubAdapter{resolved: map[string]LlmResolvedModelInfo{model: {
		LlmModelInfo:     LlmModelInfo{Provider: provider, ID: model, Name: "M1"},
		DefaultMaxTokens: int64Ptr(512),
	}}, streams: []StreamChunk{
		{Type: ChunkTextDelta, Text: "hi"},
		reasonChunk(FinishStop, ""),
	}}
	if _, err := rt.RegisterAdapter([]string{provider}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	prepared, err := rt.PrepareCall(LlmCallConfig{Provider: provider, Model: model})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if !prepared.AdapterDefaults.MaxTokens {
		t.Fatalf("AdapterDefaults = %+v, want MaxTokens=true", prepared.AdapterDefaults)
	}
	options := GenerateOptions{Provider: provider, Model: model, MaxTokens: int64Ptr(512)}
	chunks := collect(t, prepared.Stream(options))
	if len(chunks) != 2 || chunks[0].Type != ChunkTextDelta {
		t.Fatalf("chunks = %v", chunks)
	}
	if adapter.receivedOption == nil || adapter.receivedOption.MaxTokens == nil || *adapter.receivedOption.MaxTokens != 512 {
		t.Fatalf("adapter received %+v", adapter.receivedOption)
	}
	// Second dispatch of the same handle → INVALID_PREPARED_CALL.
	adapter.receivedOption = nil
	chunks = collect(t, prepared.Stream(options))
	if len(chunks) != 1 || chunks[0].Reason == nil || chunks[0].Reason.Failure == nil || chunks[0].Reason.Failure.Code != "INVALID_PREPARED_CALL" {
		t.Fatalf("reuse chunks = %v", chunks)
	}
	// Config drift.
	prepared2, err := rt.PrepareCall(LlmCallConfig{Provider: provider, Model: model})
	if err != nil {
		t.Fatalf("prepare2: %v", err)
	}
	chunks = collect(t, prepared2.Stream(GenerateOptions{Provider: provider, Model: model, Temperature: float64Ptr(0.7)}))
	if len(chunks) != 1 || chunks[0].Reason == nil || chunks[0].Reason.Failure == nil || chunks[0].Reason.Failure.Code != "INVALID_PREPARED_CALL" {
		t.Fatalf("drift chunks = %v", chunks)
	}
}

func TestStreamNoAdapter(t *testing.T) {
	rt := NewRuntime()
	chunks := collect(t, rt.Stream(GenerateOptions{Provider: "ghost", Model: "m"}))
	if len(chunks) != 1 || chunks[0].Type != ChunkFinish || chunks[0].Reason == nil ||
		chunks[0].Reason.Failure == nil || chunks[0].Reason.Failure.Code != CodeNoAdapter {
		t.Fatalf("chunks = %v", chunks)
	}
	if chunks[0].Reason.Kind != FinishError {
		t.Fatalf("kind = %v, want error", chunks[0].Reason.Kind)
	}
}

func TestStreamAdapterPanicBecomesTerminalChunk(t *testing.T) {
	rt := NewRuntime()
	if _, err := rt.RegisterAdapter([]string{"alpha"}, &stubAdapter{panicOnStream: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	chunks := collect(t, rt.Stream(GenerateOptions{Provider: "alpha", Model: "m"}))
	if len(chunks) != 1 || chunks[0].Type != ChunkFinish {
		t.Fatalf("chunks = %v", chunks)
	}
	failure := chunks[0].Reason.Failure
	if failure == nil || failure.Code != "UNKNOWN" || !strings.Contains(failure.Message, "exploded") {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestStreamAborted(t *testing.T) {
	rt := NewRuntime()
	// A failing dispatch with an already-canceled context classifies as
	// aborted (the official AbortError / signal.aborted detection).
	if _, err := rt.RegisterAdapter([]string{"alpha"}, &stubAdapter{panicOnStream: true}); err != nil {
		t.Fatalf("register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	chunks := collect(t, rt.Stream(GenerateOptions{
		Provider: "alpha", Model: "m",
		Context: ctx,
	}))
	if len(chunks) != 1 || chunks[0].Reason == nil || chunks[0].Reason.Kind != FinishAborted {
		t.Fatalf("chunks = %v", chunks)
	}
}

func TestStreamWaterfall(t *testing.T) {
	rt := NewRuntime()
	adapter := &stubAdapter{streams: []StreamChunk{{Type: ChunkTextDelta, Text: "core"}}}
	if _, err := rt.RegisterAdapter([]string{"alpha"}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Hook A wraps; hook B (registered later) sees the request too.
	var order []string
	disposeA := rt.OnStream(func(options GenerateOptions, next func(GenerateOptions) Seq) Seq {
		order = append(order, "A")
		return next(options)
	})
	disposeB := rt.OnStream(func(options GenerateOptions, next func(GenerateOptions) Seq) Seq {
		order = append(order, "B")
		return next(options)
	})
	chunks := collect(t, rt.Stream(GenerateOptions{Provider: "alpha", Model: "m"}))
	if len(chunks) != 1 || chunks[0].Text != "core" {
		t.Fatalf("chunks = %v", chunks)
	}
	// First-registered hook is outermost (official waterfall order).
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("order = %v", order)
	}
	// Short-circuit: hook returns its own sequence without next.
	disposeB()
	disposeShort := rt.OnStream(func(options GenerateOptions, next func(GenerateOptions) Seq) Seq {
		order = append(order, "short")
		return FromChunks([]StreamChunk{{Type: ChunkTextDelta, Text: "hooked"}})
	})
	chunks = collect(t, rt.Stream(GenerateOptions{Provider: "alpha", Model: "m"}))
	if len(chunks) != 1 || chunks[0].Text != "hooked" {
		t.Fatalf("short-circuit chunks = %v", chunks)
	}
	if len(order) != 4 || order[2] != "A" || order[3] != "short" {
		t.Fatalf("short order = %v", order)
	}
	// Hook failure is thrown, not a terminal chunk.
	disposeShort()
	disposeBroken := rt.OnStream(func(options GenerateOptions, next func(GenerateOptions) Seq) Seq {
		panic(errors.New("hook broken"))
	})
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("hook panic did not propagate")
			}
			if !strings.Contains(fmt.Sprint(r), "hook broken") {
				t.Fatalf("panic = %v", r)
			}
		}()
		for range rt.Stream(GenerateOptions{Provider: "alpha", Model: "m"}) {
		}
	}()
	// Dispose removes hooks.
	disposeBroken()
	disposeA()
	adapter.receivedOption = nil
	for range rt.Stream(GenerateOptions{Provider: "alpha", Model: "m"}) {
	}
	if adapter.receivedOption == nil {
		t.Fatal("adapter not reached after hook dispose")
	}
}

func TestStripForeignReplayState(t *testing.T) {
	own := &Message{Role: RoleAssistant, Source: MessageSource{
		Kind: SourceModel, Provider: "alpha", Model: "m", ReplayState: json.RawMessage(`{"k":1}`),
	}}
	foreign := &Message{Role: RoleAssistant, Source: MessageSource{
		Kind: SourceModel, Provider: "beta", Model: "m", ReplayState: json.RawMessage(`{"k":2}`),
	}}
	out := StripForeignReplayState([]Message{*own, *foreign}, "alpha")
	if out[0].Source.ReplayState == nil {
		t.Fatal("own replay state stripped")
	}
	if out[1].Source.ReplayState != nil {
		t.Fatal("foreign replay state kept")
	}
}

func TestProviderRetryPolicyCapture(t *testing.T) {
	rt := NewRuntime()
	policy := &ResolvedRetryPolicy{Mode: "always", MaxRetries: 2}
	adapter := &stubAdapter{policies: map[string]*ResolvedRetryPolicy{"alpha": policy}}
	if _, err := rt.RegisterAdapter([]string{"alpha", "beta"}, adapter); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, err := rt.ProviderRetryPolicy("alpha")
	if err != nil || got != policy {
		t.Fatalf("alpha policy = %v, %v", got, err)
	}
	got, err = rt.ProviderRetryPolicy("beta")
	if err != nil || got == nil || got.Mode != "normal" || got.MaxRetries != 5 {
		t.Fatalf("beta default policy = %+v, %v", got, err)
	}
	if _, err := rt.ProviderRetryPolicy("ghost"); err == nil || errorCode(err) != CodeNoAdapter {
		t.Fatalf("ghost policy err = %v", err)
	}
}
