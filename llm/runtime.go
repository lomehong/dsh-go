// The LLM runtime: an adapter registry with a streaming model-call API.
// Port of the LlmRuntime class in packages/llm/llm/src/index.ts. The
// `llm/stream` waterfall becomes OnStream hooks; the Go stream type is
// iter.Seq[StreamChunk].
package llm

import (
	"iter"
	"sync"
)

// Adapter is the provider-wire adapter for the harness message and stream
// vocabulary. Register implementations with Runtime.RegisterAdapter. Embed
// BaseAdapter to inherit the official defaults. Adapters may optionally
// implement CallPreparer for generation-bound dispatch.
type Adapter interface {
	// Stream streams one model call as raw chunks — the only required
	// method. Implementations must honor cancellation through the
	// GenerateOptions Context.
	Stream(options GenerateOptions) iter.Seq[StreamChunk]
	// ProviderInfo describes one provider route owned by this adapter; the
	// id must equal the registered provider name.
	ProviderInfo(provider string) LlmProviderInfo
	// ProviderRetryPolicy returns the provider-owned retry policy, or nil
	// for the normal defaults.
	ProviderRetryPolicy(provider string) *ResolvedRetryPolicy
	// ListModels returns the advisory catalog in adapter-preferred order.
	ListModels(provider string) ([]LlmModelInfo, error)
	// ResolveModel resolves all metadata for one exact route; independent of
	// the advisory catalog, it never validates request routing.
	ResolveModel(provider, model string) (LlmResolvedModelInfo, error)
}

// CallPreparer is the optional generation-binding half of the adapter
// contract: PrepareCall binds exact model metadata and the eventual
// dispatch to one adapter generation, so settings changes between
// preparation and dispatch cannot combine one generation's capabilities
// with another's endpoint. Adapters without it get the default binding of
// ResolveModel to Stream.
type CallPreparer interface {
	PrepareCall(provider, model string) (LlmResolvedModelInfo, func(GenerateOptions) iter.Seq[StreamChunk], error)
}

// BaseAdapter supplies the official defaults; embed it in concrete
// adapters.
type BaseAdapter struct{}

// ProviderInfo defaults to the provider name as display metadata.
func (BaseAdapter) ProviderInfo(provider string) LlmProviderInfo {
	return LlmProviderInfo{ID: provider, Name: provider}
}

// ProviderRetryPolicy defaults to nil — the normal defaults apply.
func (BaseAdapter) ProviderRetryPolicy(string) *ResolvedRetryPolicy { return nil }

// ListModels defaults to an empty advisory catalog.
func (BaseAdapter) ListModels(string) ([]LlmModelInfo, error) { return nil, nil }

// ResolveModel defaults to the bare provider/model identity.
func (BaseAdapter) ResolveModel(provider, model string) (LlmResolvedModelInfo, error) {
	return LlmResolvedModelInfo{LlmModelInfo: LlmModelInfo{Provider: provider, ID: model, Name: model}}, nil
}

// defaultPrepare binds ResolveModel output to one generation's Stream — the
// fallback for adapters without a custom CallPreparer.
func defaultPrepare(adapter Adapter, provider, model string) (LlmResolvedModelInfo, func(GenerateOptions) iter.Seq[StreamChunk], error) {
	resolved, err := adapter.ResolveModel(provider, model)
	if err != nil {
		return LlmResolvedModelInfo{}, nil, err
	}
	return resolved, func(options GenerateOptions) iter.Seq[StreamChunk] { return adapter.Stream(options) }, nil
}

// Seq is the harness stream type: one ordered StreamChunk sequence.
type Seq = iter.Seq[StreamChunk]

// FromChunks builds an in-memory stream from a chunk slice — test and
// short-circuit helper.
func FromChunks(chunks []StreamChunk) Seq {
	return func(yield func(StreamChunk) bool) {
		for _, chunk := range chunks {
			if !yield(chunk) {
				return
			}
		}
	}
}

// adapterRegistration is one provider route's captured registration state.
type adapterRegistration struct {
	adapter     Adapter
	provider    LlmProviderInfo
	retryPolicy *ResolvedRetryPolicy
}

// RegistrationHandle is what RegisterAdapter returns: the disposer, plus an
// atomic route replacement for the same adapter instance.
type RegistrationHandle struct {
	disposed bool
	owned    map[string]bool
	adapter  Adapter
	rt       *Runtime
}

// Dispose releases every route this registration currently holds.
func (h *RegistrationHandle) Dispose() {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()
	if h.disposed {
		return
	}
	h.disposed = true
	for provider := range h.owned {
		delete(h.rt.adapters, provider)
		h.rt.removeFromOrder(provider)
	}
	h.owned = map[string]bool{}
}

// Replace swaps this registration's routes in one synchronous section. The
// candidate set is validated in full first — a rejected candidate leaves the
// current routes untouched. An empty slice legally holds zero routes while
// staying registered. A disposed registration refuses with
// REGISTRATION_DISPOSED.
func (h *RegistrationHandle) Replace(providers []string) error {
	h.rt.mu.Lock()
	defer h.rt.mu.Unlock()
	if h.disposed {
		return NewLlmError("a disposed adapter registration cannot replace its routes", "REGISTRATION_DISPOSED", LlmFailure{})
	}
	registrations, err := h.rt.prepareRoutes(providers, h.adapter, h.owned)
	if err != nil {
		return err
	}
	h.rt.commitRoutes(h.owned, registrations)
	return nil
}

// PreparedCall is one model call whose config and adapter registration were
// resolved together. Dispatch is one-shot and config-checked.
type PreparedCall struct {
	// Config is the detached config with any adapter-owned default
	// materialized.
	Config LlmCallConfig
	// RetryPolicy is the policy captured with the adapter registration.
	RetryPolicy *ResolvedRetryPolicy
	// AdapterDefaults marks config fields materialized by the captured
	// adapter rather than proposed by the caller.
	AdapterDefaults *LlmCallConfigAdapterDefaults
	// dispatch runs at most once, through the captured generation.
	dispatch func(GenerateOptions) iter.Seq[StreamChunk]

	rt         *Runtime
	dispatched bool
	mu         sync.Mutex
}

// Stream dispatches this call once through the captured registration; reuse
// or config drift fails with INVALID_PREPARED_CALL.
func (p *PreparedCall) Stream(options GenerateOptions) iter.Seq[StreamChunk] {
	return p.rt.streamPrepared(options, p)
}

func (p *PreparedCall) claim(options GenerateOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dispatched {
		return NewLlmError("a prepared LLM call can only be dispatched once", "INVALID_PREPARED_CALL", LlmFailure{})
	}
	if !optionsConfigMatches(options, p.Config) {
		return NewLlmError("prepared LLM call config changed before adapter dispatch", "INVALID_PREPARED_CALL", LlmFailure{})
	}
	p.dispatched = true
	return nil
}

// Runtime is the llm service: an adapter registry plus a streaming
// model-call API, interceptable via registered stream hooks (the official
// `llm/stream` waterfall).
type Runtime struct {
	mu       sync.Mutex
	adapters map[string]*adapterRegistration
	order    []string
	hooks    []*streamHookEntry
	// configurable holds discovery-facing provider entries (the
	// `registerConfigurableProviders` table: which provider routes expose a
	// user settings section).
	configurable      map[string]ConfigurableProvider
	configurableOrder []string
}

// ConfigurableProvider declares one provider route whose connection facts
// users configure through a settings section.
type ConfigurableProvider struct {
	// Provider is the registered provider route.
	Provider string
	// DisplayName is the human label (the web Models page shows it).
	DisplayName string
	// SettingsNs is the settings namespace backing this provider.
	SettingsNs string
	// SettingsPath is the document path of the section inside the namespace.
	SettingsPath []string
}

// RegisterConfigurableProviders records discovery-facing provider entries.
// Duplicate provider routes fail loud: two owners of one route is a
// composition error.
func (rt *Runtime) RegisterConfigurableProviders(entries []ConfigurableProvider) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.configurable == nil {
		rt.configurable = map[string]ConfigurableProvider{}
	}
	for _, entry := range entries {
		if entry.Provider == "" {
			return NewLlmError("a configurable provider must name its provider route", CodeInvalidAdapter, LlmFailure{})
		}
		if _, exists := rt.configurable[entry.Provider]; exists {
			return NewLlmError("configurable provider \""+entry.Provider+"\" is already registered", CodeDuplicateAdapter, LlmFailure{})
		}
		rt.configurable[entry.Provider] = entry
		rt.configurableOrder = append(rt.configurableOrder, entry.Provider)
	}
	return nil
}

// ListConfigurableProviders returns the discovery table in declaration
// order; nil when nothing registered it.
func (rt *Runtime) ListConfigurableProviders() []ConfigurableProvider {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.configurableOrder) == 0 {
		return nil
	}
	out := make([]ConfigurableProvider, 0, len(rt.configurableOrder))
	for _, provider := range rt.configurableOrder {
		out = append(out, rt.configurable[provider])
	}
	return out
}

type streamHookEntry struct {
	fn StreamHook
}

// NewRuntime builds an empty runtime.
func NewRuntime() *Runtime {
	return &Runtime{adapters: map[string]*adapterRegistration{}}
}

// StreamHook is one `llm/stream` waterfall listener. It receives the full
// request and next; calling next reaches the resolved adapter's stream, and
// returning a different sequence short-circuits. A hook failure stays a
// thrown error (a panic), never a terminal chunk.
type StreamHook func(options GenerateOptions, next func(GenerateOptions) iter.Seq[StreamChunk]) iter.Seq[StreamChunk]

// OnStream registers one waterfall listener; the returned disposer removes
// it exactly once.
func (rt *Runtime) OnStream(hook StreamHook) (dispose func()) {
	entry := &streamHookEntry{fn: hook}
	rt.mu.Lock()
	rt.hooks = append(rt.hooks, entry)
	rt.mu.Unlock()
	return func() {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		for i, candidate := range rt.hooks {
			if candidate == entry {
				rt.hooks = append(rt.hooks[:i], rt.hooks[i+1:]...)
				return
			}
		}
	}
}

// RegisterAdapter registers an adapter for the given provider routes.
// All-or-nothing: any validation failure leaves the registry untouched.
func (rt *Runtime) RegisterAdapter(providers []string, adapter Adapter) (*RegistrationHandle, error) {
	if len(providers) == 0 {
		return nil, NewLlmError("an adapter must register at least one provider", CodeInvalidAdapter, LlmFailure{})
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	registrations, err := rt.prepareRoutes(providers, adapter, nil)
	if err != nil {
		return nil, err
	}
	handle := &RegistrationHandle{owned: map[string]bool{}, adapter: adapter, rt: rt}
	rt.commitRoutes(handle.owned, registrations)
	return handle, nil
}

// prepareRoutes validates one candidate route set for adapter, treating
// routes this registration already holds as available. Nothing is mutated.
func (rt *Runtime) prepareRoutes(providers []string, adapter Adapter, owned map[string]bool) ([]adapterRegistration, error) {
	unique := map[string]bool{}
	registrations := make([]adapterRegistration, 0, len(providers))
	for _, provider := range providers {
		if provider == "" {
			return nil, NewLlmError("adapter provider names must be non-empty", CodeInvalidAdapter, LlmFailure{})
		}
		if unique[provider] || (rt.adapters[provider] != nil && !owned[provider]) {
			return nil, NewLlmError("an adapter for provider \""+provider+"\" is already registered", CodeDuplicateAdapter, LlmFailure{})
		}
		info := adapter.ProviderInfo(provider)
		if info.ID != provider || info.Name == "" {
			return nil, NewLlmError("adapter metadata for provider \""+provider+"\" must preserve its id and have a non-empty name", CodeInvalidAdapter, LlmFailure{})
		}
		unique[provider] = true
		policy, err := ResolveRetryPolicy(nil, "llm: provider \""+provider+"\" retryPolicy")
		if err != nil {
			return nil, err
		}
		if owned := adapter.ProviderRetryPolicy(provider); owned != nil {
			policy = owned
		}
		registrations = append(registrations, adapterRegistration{adapter: adapter, provider: info, retryPolicy: policy})
	}
	return registrations, nil
}

// commitRoutes swaps route sets in one synchronous section (the caller
// holds rt.mu).
func (rt *Runtime) commitRoutes(owned map[string]bool, registrations []adapterRegistration) {
	for provider := range owned {
		delete(rt.adapters, provider)
		rt.removeFromOrder(provider)
	}
	for provider := range owned {
		delete(owned, provider)
	}
	for _, registration := range registrations {
		captured := registration
		rt.adapters[captured.provider.ID] = &captured
		owned[captured.provider.ID] = true
		rt.order = append(rt.order, captured.provider.ID)
	}
}

func (rt *Runtime) removeFromOrder(provider string) {
	for i, name := range rt.order {
		if name == provider {
			rt.order = append(rt.order[:i], rt.order[i+1:]...)
			return
		}
	}
}

// ListProviders describes provider routes with a registered adapter, in
// registration order.
func (rt *Runtime) ListProviders() []LlmProviderInfo {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]LlmProviderInfo, 0, len(rt.order))
	for _, provider := range rt.order {
		if registration := rt.adapters[provider]; registration != nil {
			out = append(out, registration.provider)
		}
	}
	return out
}

// Registration returns one provider's captured registration state.
func (rt *Runtime) Registration(provider string) (*adapterRegistration, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	registration := rt.adapters[provider]
	if registration == nil {
		return nil, NewLlmError("no adapter registered for provider \""+provider+"\"", CodeNoAdapter, LlmFailure{})
	}
	return registration, nil
}

// ProviderRetryPolicy resolves the retry policy captured when one provider
// route was registered.
func (rt *Runtime) ProviderRetryPolicy(provider string) (*ResolvedRetryPolicy, error) {
	registration, err := rt.Registration(provider)
	if err != nil {
		return nil, err
	}
	return registration.retryPolicy, nil
}

// ListModels discovers models advertised by one registered provider.
// Catalog membership is advisory and never changes routing or request
// validation. Invalid or duplicate metadata fails loud as INVALID_CATALOG.
func (rt *Runtime) ListModels(provider string) ([]LlmModelInfo, error) {
	registration, err := rt.Registration(provider)
	if err != nil {
		return nil, err
	}
	models, err := registration.adapter.ListModels(provider)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]LlmModelInfo, 0, len(models))
	for _, model := range models {
		if model.Provider != provider || model.ID == "" || model.Name == "" || seen[model.ID] {
			return nil, NewLlmError("adapter returned invalid or duplicate model metadata for provider \""+provider+"\"", CodeInvalidCatalog, LlmFailure{})
		}
		seen[model.ID] = true
		out = append(out, model)
	}
	return out, nil
}

// ResolveModelInfo resolves and validates all metadata from the adapter
// that owns one exact route.
func (rt *Runtime) ResolveModelInfo(provider, model string) (LlmResolvedModelInfo, error) {
	registration, err := rt.Registration(provider)
	if err != nil {
		return LlmResolvedModelInfo{}, err
	}
	return rt.resolveModelInfoFor(registration, model)
}

func (rt *Runtime) resolveModelInfoFor(registration *adapterRegistration, model string) (LlmResolvedModelInfo, error) {
	resolved, err := registration.adapter.ResolveModel(registration.provider.ID, model)
	if err != nil {
		return LlmResolvedModelInfo{}, err
	}
	return normalizeModelInfo(registration, model, resolved)
}

// normalizeModelInfo validates and detaches one adapter-returned exact
// model result.
func normalizeModelInfo(registration *adapterRegistration, model string, resolved LlmResolvedModelInfo) (LlmResolvedModelInfo, error) {
	provider := registration.provider.ID
	if resolved.Provider != provider || resolved.ID != model || resolved.Name == "" {
		return LlmResolvedModelInfo{}, NewLlmError("adapter returned invalid exact model metadata for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_INFO", LlmFailure{})
	}
	if resolved.Context != nil && resolved.Context.ContextWindow <= 0 {
		return LlmResolvedModelInfo{}, NewLlmError("adapter returned invalid context metadata for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_CONTEXT", LlmFailure{})
	}
	if resolved.DefaultMaxTokens != nil && *resolved.DefaultMaxTokens <= 0 {
		return LlmResolvedModelInfo{}, NewLlmError("adapter returned invalid default maxTokens for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_MAX_TOKENS", LlmFailure{})
	}
	info := resolved
	info.LlmModelInfo = LlmModelInfo{
		Provider: provider, ID: model, Name: resolved.Name,
		Description: resolved.Description, InputModalities: resolved.InputModalities,
	}
	if resolved.Reasoning == nil {
		return info, nil
	}
	if len(resolved.Reasoning.Efforts) == 0 {
		return LlmResolvedModelInfo{}, NewLlmError("adapter returned invalid reasoning metadata for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_REASONING", LlmFailure{})
	}
	seen := map[string]bool{}
	for _, effort := range resolved.Reasoning.Efforts {
		if effort.ID == "" || effort.Name == "" || seen[string(effort.ID)] {
			return LlmResolvedModelInfo{}, NewLlmError("adapter returned invalid or duplicate reasoning effort metadata for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_REASONING", LlmFailure{})
		}
		seen[string(effort.ID)] = true
	}
	if resolved.Reasoning.DefaultEffort != "" && !seen[string(resolved.Reasoning.DefaultEffort)] {
		return LlmResolvedModelInfo{}, NewLlmError("adapter returned an unknown default reasoning effort for provider \""+provider+"\" model \""+model+"\"", "INVALID_MODEL_REASONING", LlmFailure{})
	}
	return info, nil
}

// ResolveCallConfig validates a conversation call config against its exact
// model capability and materializes adapter-configured defaults.
// Unsupported explicit efforts reject before provider I/O; no clamping or
// aliasing is performed.
func (rt *Runtime) ResolveCallConfig(config LlmCallConfig) (LlmCallConfig, error) {
	registration, err := rt.Registration(config.Provider)
	if err != nil {
		return LlmCallConfig{}, err
	}
	info, err := rt.resolveModelInfoFor(registration, config.Model)
	if err != nil {
		return LlmCallConfig{}, err
	}
	return resolveCallWithInfo(config, info)
}

// resolveCallWithInfo validates request controls against one already-bound
// exact model result.
func resolveCallWithInfo(config LlmCallConfig, info LlmResolvedModelInfo) (LlmCallConfig, error) {
	if config.MaxTokens == nil && info.DefaultMaxTokens != nil {
		materialized := config
		materialized.MaxTokens = info.DefaultMaxTokens
		config = materialized
	}
	reasoning := info.Reasoning
	requested := config.ReasoningEffort
	if reasoning == nil {
		if requested != "" {
			return LlmCallConfig{}, NewLlmError("provider \""+config.Provider+"\" model \""+config.Model+"\" does not support reasoning effort \""+requested+"\"", "UNSUPPORTED_REASONING_EFFORT", LlmFailure{})
		}
		return config, nil
	}
	effective := requested
	if effective == "" {
		effective = reasoning.DefaultEffort
	}
	if effective != "" {
		supported := false
		for _, effort := range reasoning.Efforts {
			if effort.ID == effective {
				supported = true
				break
			}
		}
		if !supported {
			return LlmCallConfig{}, NewLlmError("provider \""+config.Provider+"\" model \""+config.Model+"\" does not support reasoning effort \""+effective+"\"", "UNSUPPORTED_REASONING_EFFORT", LlmFailure{})
		}
		if requested != effective {
			config.ReasoningEffort = effective
		}
	}
	return config, nil
}

// PrepareCall resolves one call under its current adapter registration. The
// returned one-shot handle keeps that registration across header logging
// and dispatch, so a concurrent re-registration cannot combine one adapter's
// capability result with another adapter.
func (rt *Runtime) PrepareCall(config LlmCallConfig) (*PreparedCall, error) {
	registration, err := rt.Registration(config.Provider)
	if err != nil {
		return nil, err
	}
	modelInfo, dispatch, err := prepareCallOn(registration.adapter, config.Provider, config.Model)
	if err != nil {
		return nil, err
	}
	modelInfo, err = normalizeModelInfo(registration, config.Model, modelInfo)
	if err != nil {
		return nil, err
	}
	resolvedConfig, err := resolveCallWithInfo(config, modelInfo)
	if err != nil {
		return nil, err
	}
	defaults := &LlmCallConfigAdapterDefaults{}
	if config.ReasoningEffort == "" && resolvedConfig.ReasoningEffort != "" {
		defaults.ReasoningEffort = true
	}
	if config.MaxTokens == nil && resolvedConfig.MaxTokens != nil {
		defaults.MaxTokens = true
	}
	return &PreparedCall{
		Config: resolvedConfig, RetryPolicy: registration.retryPolicy,
		AdapterDefaults: defaults, dispatch: dispatch, rt: rt,
	}, nil
}

// prepareCallOn dispatches to an adapter's CallPreparer when it implements
// one, else the default ResolveModel-to-Stream binding.
func prepareCallOn(adapter Adapter, provider, model string) (LlmResolvedModelInfo, func(GenerateOptions) iter.Seq[StreamChunk], error) {
	if preparer, ok := adapter.(CallPreparer); ok {
		return preparer.PrepareCall(provider, model)
	}
	return defaultPrepare(adapter, provider, model)
}

// Stream streams one model call as raw chunks. Adapter selection, dispatch,
// and iteration failures become terminal finish chunks; hook failures stay
// thrown panics.
func (rt *Runtime) Stream(options GenerateOptions) iter.Seq[StreamChunk] {
	return rt.runHooks(options, nil)
}

func (rt *Runtime) streamPrepared(options GenerateOptions, prepared *PreparedCall) iter.Seq[StreamChunk] {
	return rt.runHooks(options, prepared)
}

func (rt *Runtime) runHooks(options GenerateOptions, prepared *PreparedCall) iter.Seq[StreamChunk] {
	rt.mu.Lock()
	hooks := make([]*streamHookEntry, len(rt.hooks))
	copy(hooks, rt.hooks)
	rt.mu.Unlock()
	next := func(opts GenerateOptions) iter.Seq[StreamChunk] {
		return rt.adapterStream(opts, prepared)
	}
	for i := len(hooks) - 1; i >= 0; i-- {
		hook := hooks[i].fn
		innerNext := next
		next = func(opts GenerateOptions) iter.Seq[StreamChunk] {
			return hook(opts, innerNext)
		}
	}
	return next(options)
}

// adapterStream is the final adapter boundary: adapter selection, dispatch,
// and iteration failures become one terminal failure chunk; a consumer
// stopping iteration (yield false) ends the sequence without a terminal
// chunk.
func (rt *Runtime) adapterStream(options GenerateOptions, prepared *PreparedCall) iter.Seq[StreamChunk] {
	return func(yield func(StreamChunk) bool) {
		aborted := options.aborted()
		if prepared != nil {
			if err := prepared.claim(options); err != nil {
				yield(adapterFailureChunk(err, aborted))
				return
			}
		}
		registration, err := rt.Registration(options.Provider)
		if err != nil {
			yield(adapterFailureChunk(err, aborted))
			return
		}
		var dispatch func(GenerateOptions) iter.Seq[StreamChunk]
		if prepared == nil {
			var modelInfo LlmResolvedModelInfo
			modelInfo, dispatch, err = prepareCallOn(registration.adapter, options.Provider, options.Model)
			if err == nil {
				modelInfo, err = normalizeModelInfo(registration, options.Model, modelInfo)
			}
			if err == nil {
				resolvedConfig, configErr := resolveCallWithInfo(optionsConfig(options), modelInfo)
				if configErr != nil {
					err = configErr
				} else if !optionsConfigMatches(options, resolvedConfig) {
					// Materialize adapter-owned defaults into the dispatch.
					adjusted := options
					adjusted.ReasoningEffort = resolvedConfig.ReasoningEffort
					adjusted.MaxTokens = resolvedConfig.MaxTokens
					options = adjusted
				}
			}
			if err != nil {
				yield(adapterFailureChunk(err, aborted))
				return
			}
		} else {
			dispatch = prepared.dispatch
		}
		// Replay state whose historical route is owned by another adapter is
		// stripped: it is adapter-private and only its owner can read it.
		dispatchOptions := options
		dispatchOptions.Messages = StripForeignReplayState(options.Messages, options.Provider)
		rt.drainPrepared(dispatch, dispatchOptions, yield, aborted)
	}
}

// drainPrepared creates and iterates one adapter sequence inside the
// official adapterStream boundary: a panic escaping the adapter — at
// creation or during iteration — becomes one terminal failure chunk (the
// official try/catch), a consumer stop (yield false) ends quietly, and the
// terminal finish chunk ends the sequence.
func (rt *Runtime) drainPrepared(dispatch func(GenerateOptions) Seq, options GenerateOptions, yield func(StreamChunk) bool, aborted bool) {
	defer func() {
		if r := recover(); r != nil {
			yield(adapterFailureChunk(NewLlmError(panicMessage(r), "UNKNOWN", LlmFailure{}), aborted))
		}
	}()
	for chunk := range dispatch(options) {
		if !yield(chunk) {
			return
		}
		if chunk.Type == ChunkFinish {
			return
		}
	}
}

// panicMessage renders a recovered panic as an error message.
func panicMessage(r any) string {
	if err, ok := r.(error); ok {
		return "LLM adapter failed: " + err.Error()
	}
	return "LLM adapter failed: panic"
}

// StripForeignReplayState removes replay state whose historical route is
// owned by another adapter than the target provider.
func StripForeignReplayState(messages []Message, provider string) []Message {
	changed := false
	out := make([]Message, len(messages))
	copy(out, messages)
	for i, message := range out {
		if message.Role != RoleAssistant || message.Source.Kind != SourceModel || message.Source.ReplayState == nil {
			continue
		}
		if message.Source.Provider == provider {
			continue
		}
		stripped := message
		stripped.Source.ReplayState = nil
		out[i] = stripped
		changed = true
	}
	if !changed {
		return messages
	}
	return out
}

// optionsConfig projects a request onto its config fields.
func optionsConfig(options GenerateOptions) LlmCallConfig {
	return LlmCallConfig{
		Provider: options.Provider, Model: options.Model,
		ReasoningEffort: options.ReasoningEffort,
		Temperature:     options.Temperature, MaxTokens: options.MaxTokens, Stop: options.Stop,
	}
}

// optionsConfigMatches reports whether a request's config fields equal the
// prepared config.
func optionsConfigMatches(options GenerateOptions, config LlmCallConfig) bool {
	return CallConfigEquals(optionsConfig(options), config)
}

// aborted reads the request's cancellation state.
func (options GenerateOptions) aborted() bool {
	return options.Context != nil && options.Context.Err() != nil
}
