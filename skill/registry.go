// The layered skill registry: host + per-scope contributions, the shape the
// tools registry established. A registration files into the layer of its
// explicit registration scope — host rows land in the global layer, a plugin
// mounted by an agent preset's standing composition lands in that preset's
// layer. A read merges the global layer with the viewing scope's chain: the
// nearest layer's entry wins a duplicate name outright, and rank order
// decides duplicates only within one layer.
package skill

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"dshgo/cordis"
	"dshgo/scope"
)

// Config is the registry configuration.
type Config struct {
	// CollectCacheMaxEntries is the maximum number of completed
	// cwd/provider catalogs kept in memory; zero applies the default.
	CollectCacheMaxEntries int
}

// indexedCandidate is one catalog row with its merge ordering facts.
type indexedCandidate struct {
	candidate     Candidate
	provider      Provider
	providerOrder int
	localOrder    int
	layer         *skillLayer
}

// registeredProvider is one provider registration retained by its layer.
type registeredProvider struct {
	provider Provider
	// order is the service-wide monotonic registration order, the
	// within-layer rank tiebreak.
	order int
}

// skillLayer is one scope's complete skill-registry contribution.
type skillLayer struct {
	providers *scope.NamedEntries[registeredProvider]
	runtime   map[string]Definition
}

func newSkillLayer(scopeKey scope.ScopeKey) *skillLayer {
	return &skillLayer{
		providers: scope.NewNamedEntries[registeredProvider](func(name string) error {
			if scopeKey == nil {
				return fmt.Errorf(`a skill provider named %q is already registered`, name)
			}
			return fmt.Errorf(`a skill provider named %q is already registered in this scope`, name)
		}),
		runtime: map[string]Definition{},
	}
}

func (l *skillLayer) isEmpty() bool {
	return l.providers.IsEmpty() && len(l.runtime) == 0
}

// collectResult is one merged catalog observation plus whether discovery
// completed within a stable revision.
type collectResult struct {
	entries   map[string]indexedCandidate
	cacheable bool
}

// Logger is the containment sink for skipped providers and duplicate
// registrations.
type Logger interface {
	Warn(msg string)
	Error(msg string)
	Info(msg string)
}

// Registry is the layered registry of skill providers and runtime skills.
// It is safe for concurrent use.
type Registry struct {
	logger                 cordis.Logger
	collectCacheMaxEntries int
	layers                 *scope.Layers[skillLayer]

	mu          sync.Mutex
	cache       map[string]map[string]indexedCandidate
	revision    int
	nextOrder   int
	scopeIDs    map[scope.ScopeKey]int
	nextScopeID int
	changeMu    sync.Mutex
	change      []func()
}

// NewRegistry builds an empty registry and validates the configuration.
func NewRegistry(logger cordis.Logger, config Config) (*Registry, error) {
	if logger == nil {
		logger = cordis.Discard{}
	}
	entries := config.CollectCacheMaxEntries
	if entries == 0 {
		entries = DefaultCollectCacheEntries
	}
	if entries < 1 {
		return nil, fmt.Errorf("skill: collectCacheMaxEntries must be an integer greater than or equal to 1")
	}
	registry := &Registry{
		logger:                 logger,
		collectCacheMaxEntries: entries,
		cache:                  map[string]map[string]indexedCandidate{},
		revision:               0,
		nextOrder:              0,
		scopeIDs:               map[scope.ScopeKey]int{},
		nextScopeID:            1,
	}
	registry.layers = scope.NewLayers(newSkillLayer, (*skillLayer).isEmpty, registry.invalidateCache)
	return registry, nil
}

// OnChange subscribes a catalog-change observer: an unfiltered invalidation
// notification fired after any registration, disposal, or provider-driven
// invalidation. Listener failures are contained and cannot veto the registry
// mutation. The returned disposer removes the observer.
func (r *Registry) OnChange(fn func()) func() {
	r.changeMu.Lock()
	defer r.changeMu.Unlock()
	// Reuse the entry-id store for idempotent removal.
	r.change = append(r.change, fn)
	index := len(r.change) - 1
	return func() {
		r.changeMu.Lock()
		if index < len(r.change) && r.change[index] != nil {
			r.change[index] = nil
		}
		r.changeMu.Unlock()
	}
}

func (r *Registry) notifyChange() {
	r.changeMu.Lock()
	observers := make([]func(), 0, len(r.change))
	for _, fn := range r.change {
		if fn != nil {
			observers = append(observers, fn)
		}
	}
	r.changeMu.Unlock()
	for _, fn := range observers {
		r.contain(fn)
	}
}

// contain runs one observer, logging a panic instead of failing the mutation.
func (r *Registry) contain(fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logger.Warn(fmt.Sprintf("skills/change listener threw: %v", recovered))
		}
	}()
	fn()
}

// RegisterProviderIn registers a same-process provider synchronously, into
// the given layer (nil = global): a preset's standing mount registers for
// that scope alone. Duplicate names within one layer and the reserved
// runtime name fail loud. Remote initialization belongs in List. The
// returned disposer unregisters the provider, aborts its lifecycle context,
// and invalidates catalog caches.
func (r *Registry) RegisterProviderIn(regScope scope.ScopeKey, create func(control ProviderControl) (Provider, error)) (func(), error) {
	lifecycle, cancel := context.WithCancel(context.Background())
	registration := struct {
		layer    *skillLayer
		name     string
		provider Provider
	}{}
	var registrationMu sync.Mutex
	invalidate := func() {
		registrationMu.Lock()
		active := registration.layer != nil && registration.provider != nil
		var stillLive bool
		if active {
			if existing, ok := registration.layer.providers.Get(registration.name); ok && existing.provider == registration.provider {
				stillLive = true
			}
		}
		registrationMu.Unlock()
		if stillLive {
			r.invalidateCache()
		}
	}
	provider, err := create(ProviderControl{Context: lifecycle, Invalidate: invalidate})
	if err != nil {
		cancel()
		return nil, err
	}
	name := provider.Name()
	if name == RuntimeProvider {
		cancel()
		return nil, fmt.Errorf("%q is reserved for runtime skill registrations", RuntimeProvider)
	}
	order := r.nextProviderOrder()
	registrationMu.Lock()
	registration.layer = nil
	registration.name = name
	registration.provider = provider
	registrationMu.Unlock()
	undo, _, err := r.layers.Mutate(regScope, func(layer *skillLayer) (func(), error) {
		if err := layer.providers.Insert(name, registeredProvider{provider: provider, order: order}); err != nil {
			return nil, err
		}
		registrationMu.Lock()
		registration.layer = layer
		registrationMu.Unlock()
		return func() {
			registrationMu.Lock()
			registration.layer = nil
			registrationMu.Unlock()
			layer.providers.Remove(name)
			cancel()
		}, nil
	})
	if err != nil {
		cancel()
		return nil, err
	}
	// Any contribution mutation invalidates completed catalogs and
	// notifies observers — at registration as well as disposal.
	r.invalidateCache()
	return undo, nil
}

func (r *Registry) nextProviderOrder() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	order := r.nextOrder
	r.nextOrder++
	return order
}

// RegisterIn registers a runtime skill into the given layer (nil = global).
// Same-name runtime entries in one layer are first-wins: a duplicate logs a
// warning and receives a no-op disposer so it cannot remove the winner.
func (r *Registry) RegisterIn(regScope scope.ScopeKey, skill Registration) (func(), error) {
	if err := validateRuntimeSkill(skill); err != nil {
		return nil, err
	}
	layer := r.layers.Global
	if regScope != nil {
		layer = r.layers.Peek(regScope)
	}
	if layer != nil {
		if _, exists := layer.runtime[skill.Name]; exists {
			r.logger.Warn(fmt.Sprintf("runtime skill %q ignored because it is already registered", skill.Name))
			return func() {}, nil
		}
	}
	invocation := InvocationPolicy{ModelInvocable: true, UserInvocable: true}
	if skill.Invocation != nil {
		invocation = *skill.Invocation
	}
	provider := skill.Provider
	if provider == "" {
		provider = RuntimeProvider
	}
	definition := Definition{
		Summary: Summary{
			Name:         skill.Name,
			Description:  skill.Description,
			WhenToUse:    skill.WhenToUse,
			Invocation:   invocation,
			Source:       skill.Source,
			Provider:     provider,
			ResourceBase: skill.ResourceBase,
		},
		Content:  skill.Content,
		Path:     skill.Path,
		Metadata: skill.Metadata,
	}
	undo, _, err := r.layers.Mutate(regScope, func(target *skillLayer) (func(), error) {
		target.runtime[definition.Name] = definition
		return func() { delete(target.runtime, definition.Name) }, nil
	})
	if err != nil {
		return nil, err
	}
	r.invalidateCache()
	return undo, nil
}

// List returns the invocation-neutral skill summaries for a workspace,
// sorted by name. Consumers apply invocation policy at their own boundary.
func (r *Registry) List(options ViewOptions) ([]Summary, error) {
	snapshot, err := r.Snapshot(options)
	if err != nil {
		return nil, err
	}
	return snapshot.Skills, nil
}

// CatalogSnapshot is one catalog observation plus whether discovery
// completed within a stable catalog revision.
type CatalogSnapshot struct {
	// Skills are the sorted invocation-neutral summaries of this
	// observation.
	Skills []Summary
	// Complete reports whether every registered provider completed without
	// a concurrent catalog revision.
	Complete bool
}

// Snapshot observes the current catalog and its discovery-completeness
// state. Incomplete observations are never cached, allowing consumers to
// retain last-good state and retry at their next request boundary.
func (r *Registry) Snapshot(options ViewOptions) (CatalogSnapshot, error) {
	collected, err := r.collect(options)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	summaries := make([]Summary, 0, len(collected.entries))
	for _, entry := range collected.entries {
		summaries = append(summaries, toSummary(entry.candidate))
	}
	sortSummaries(summaries)
	return CatalogSnapshot{Skills: summaries, Complete: collected.cacheable}, nil
}

// Get loads and validates the winning candidate, passing its opaque
// discovery locator back to the provider. Cancellation is rechecked after
// selection, including cache hits. An unknown or invalid name is absent.
func (r *Registry) Get(name string, options ViewOptions) (*Definition, error) {
	if !IsSkillName(name) {
		return nil, nil
	}
	if err := options.Context.Err(); err != nil {
		return nil, err
	}
	collected, err := r.collect(options)
	if err != nil {
		return nil, err
	}
	if err := options.Context.Err(); err != nil {
		return nil, err
	}
	match, ok := collected.entries[name]
	if !ok {
		return nil, nil
	}
	definition, err := match.provider.Get(match.candidate, options.LookupOptions)
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return nil, nil
	}
	if err := validateDefinition(*definition); err != nil {
		return nil, err
	}
	if definition.Name != match.candidate.Name {
		r.invalidateEntry(match)
		return nil, nil
	}
	return definition, nil
}

// collect resolves the merged catalog for one view, consulting and filling
// the completed-catalog cache keyed by cwd, scope chain, and revision.
func (r *Registry) collect(options ViewOptions) (collectResult, error) {
	if err := options.Context.Err(); err != nil {
		return collectResult{}, err
	}
	attempt := 1
	for {
		r.mu.Lock()
		revision := r.revision
		r.mu.Unlock()
		// The chain is part of the key rather than assumed stable: a
		// recompose can re-parent an existing scope without touching this
		// registry, and only a chain-bearing key makes the next read see
		// the new preset.
		key := r.collectCacheKey(options.CWD, options.Scope, revision)
		r.mu.Lock()
		cached, hit := r.cache[key]
		r.mu.Unlock()
		if hit {
			return collectResult{entries: cached, cacheable: true}, nil
		}

		result, err := r.collectFresh(options)
		if err != nil {
			return collectResult{}, err
		}
		if err := options.Context.Err(); err != nil {
			return collectResult{}, err
		}
		r.mu.Lock()
		stale := revision != r.revision
		if !stale && result.cacheable {
			r.cache[key] = result.entries
			if len(r.cache) > r.collectCacheMaxEntries {
				r.evictOldestLocked()
			}
		}
		r.mu.Unlock()
		if stale {
			if attempt < MaxCollectAttempts {
				attempt++
				continue
			}
			return collectResult{entries: result.entries, cacheable: false}, nil
		}
		return result, nil
	}
}

// evictOldestLocked drops one arbitrary oldest entry (Go maps have no
// insertion order; the official FIFO eviction is approximated by dropping
// the first key in iteration order).
func (r *Registry) evictOldestLocked() {
	for key := range r.cache {
		delete(r.cache, key)
		return
	}
}

// collectFresh merges the global layer and the existing chain overlays,
// farthest ancestor first and the exact scope last, so the nearest layer's
// same-name entry replaces the farther ones. Rank decides duplicates only
// within one layer.
func (r *Registry) collectFresh(options ViewOptions) (collectResult, error) {
	layers := []*skillLayer{r.layers.Global}
	layers = append(layers, r.layers.ChainLayers(options.Scope)...)
	merged := map[string]indexedCandidate{}
	cacheable := true
	for _, layer := range layers {
		collected, err := r.collectLayer(layer, options)
		if err != nil {
			return collectResult{}, err
		}
		if !collected.cacheable {
			cacheable = false
		}
		for _, entry := range collected.entries {
			merged[entry.candidate.Name] = entry
		}
	}
	return collectResult{entries: merged, cacheable: cacheable}, nil
}

// collectLayer orders one layer's candidates and drops same-name duplicates
// behind the winner.
func (r *Registry) collectLayer(layer *skillLayer, options ViewOptions) (layerCollectResult, error) {
	collected, err := r.listLayerCandidates(layer, options)
	if err != nil {
		return layerCollectResult{}, err
	}
	entries := collected.entries
	sort.SliceStable(entries, func(i, j int) bool {
		return compareIndexedCandidates(entries[i], entries[j])
	})
	seen := map[string]bool{}
	result := make([]indexedCandidate, 0, len(entries))
	for _, entry := range entries {
		if seen[entry.candidate.Name] {
			r.logger.Warn(fmt.Sprintf("skill %q from %s ignored because a higher-priority skill already exists", entry.candidate.Name, entry.candidate.Source))
			continue
		}
		seen[entry.candidate.Name] = true
		result = append(result, entry)
	}
	return layerCollectResult{entries: result, cacheable: collected.cacheable}, nil
}

type layerCollectResult struct {
	entries   []indexedCandidate
	cacheable bool
}

// listLayerCandidates gathers runtime skills and every provider's discovery
// output from one layer. A provider failure is contained into a warning and
// marks the layer's observation incomplete; an aborted caller still
// propagates.
func (r *Registry) listLayerCandidates(layer *skillLayer, options ViewOptions) (layerCollectResult, error) {
	if err := options.Context.Err(); err != nil {
		return layerCollectResult{}, err
	}
	var candidates []indexedCandidate
	cacheable := true
	runtimeOrder := 0
	runtimeNames := make([]string, 0, len(layer.runtime))
	for name := range layer.runtime {
		runtimeNames = append(runtimeNames, name)
	}
	sort.Strings(runtimeNames)
	for _, name := range runtimeNames {
		candidates = append(candidates, indexedCandidate{
			candidate:     runtimeCandidate(layer.runtime[name]),
			provider:      runtimeSkillProvider{},
			providerOrder: -1,
			localOrder:    runtimeOrder,
			layer:         layer,
		})
		runtimeOrder++
	}
	for _, entry := range layer.providers.Entries() {
		localOrder := 0
		observation, err := entry.Value.provider.List(options.LookupOptions)
		if err != nil {
			if options.Context.Err() != nil {
				return layerCollectResult{}, options.Context.Err()
			}
			cacheable = false
			r.logger.Warn(fmt.Sprintf("skill provider %q skipped: %v", entry.Value.provider.Name(), err))
			continue
		}
		if !observation.Complete {
			cacheable = false
		}
		for _, candidate := range observation.Candidates {
			if err := validateCandidate(candidate, entry.Value.provider.Name()); err != nil {
				return layerCollectResult{}, err
			}
			candidates = append(candidates, indexedCandidate{
				candidate:     candidate,
				provider:      entry.Value.provider,
				providerOrder: entry.Value.order,
				localOrder:    localOrder,
				layer:         layer,
			})
			localOrder++
		}
	}
	return layerCollectResult{entries: candidates, cacheable: cacheable}, nil
}

// runtimeSkillProvider serves runtime skills: injected directly by the
// registry, its Get unwraps the definition stored as the locator.
type runtimeSkillProvider struct{}

func (runtimeSkillProvider) Name() string { return RuntimeProvider }

func (runtimeSkillProvider) List(options LookupOptions) (ProviderObservation, error) {
	_ = options
	return ProviderObservation{Candidates: nil, Complete: true}, nil
}

func (runtimeSkillProvider) Get(candidate Candidate, options LookupOptions) (*Definition, error) {
	_ = options
	definition, _ := candidate.Locator.(Definition)
	return &definition, nil
}

func runtimeCandidate(definition Definition) Candidate {
	return Candidate{
		Summary:  definition.Summary,
		Rank:     RuntimeRank,
		Locator:  definition,
		Path:     definition.Path,
		Metadata: definition.Metadata,
	}
}

// invalidateCache bumps the revision, drops completed catalogs, and notifies
// observers.
func (r *Registry) invalidateCache() {
	r.mu.Lock()
	r.revision++
	r.cache = map[string]map[string]indexedCandidate{}
	r.mu.Unlock()
	r.notifyChange()
}

// invalidateEntry invalidates after a stale definition load, only while the
// exact registration that produced the entry is still live.
func (r *Registry) invalidateEntry(entry indexedCandidate) {
	if existing, ok := entry.layer.providers.Get(entry.provider.Name()); ok && existing.provider == entry.provider {
		r.invalidateCache()
	}
}

func (r *Registry) collectCacheKey(cwd string, viewScope scope.ScopeKey, revision int) string {
	chain := scope.ChainOf(viewScope)
	ids := make([]int, 0, len(chain))
	for _, key := range chain {
		ids = append(ids, r.scopeID(key))
	}
	return fmt.Sprintf("%s|%v|%d", cwd, ids, revision)
}

// scopeID mints a stable integer identity for one scope key: scope keys are
// opaque pointer-identity objects.
func (r *Registry) scopeID(key scope.ScopeKey) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.scopeIDs[key]
	if !ok {
		id = r.nextScopeID
		r.nextScopeID++
		r.scopeIDs[key] = id
	}
	return id
}

// compareIndexedCandidates orders duplicates within one layer: rank, then
// provider registration order (runtime = -1 wins), then local order.
func compareIndexedCandidates(left, right indexedCandidate) bool {
	if left.candidate.Rank != right.candidate.Rank {
		return left.candidate.Rank < right.candidate.Rank
	}
	if left.providerOrder != right.providerOrder {
		return left.providerOrder < right.providerOrder
	}
	return left.localOrder < right.localOrder
}
