// Tool registry: scoped registration shadowing globals, per-scope
// restriction masks, monotonic guards, presentation modes, and the single
// visibility resolver that feeds presentation, lookup, and dispatch. Port of
// the registry half of packages/core/tools/src/index.ts.
//
// Go adaptations: scope comes from explicit ScopeKey parameters instead of
// the registering context's tag (the agent package owns scoped contexts when
// it lands); the run_code transport and SDK renderers arrive with the PTC
// port, so a non-native mode fails assembly exactly as the source fails
// without a code runtime.
package tools

import (
	"errors"
	"fmt"
	"sync"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
)

// ReservedRunCodeName is the PTC mode presentation transport's tool name,
// reserved unconditionally: any agent may select a code mode for itself, so
// a name free to take under the deployment default would become a collision
// the moment a preset mounted.
const ReservedRunCodeName = "run_code"

// Presentation modes.
const (
	ModeNative = "native"
	ModePtc    = "ptc"
	ModeBoth   = "both"
)

// Config is the registry's plugin config.
type Config struct {
	// Mode is how registered tools are presented to the model: native sends
	// every visible schema; ptc sends only run_code plus a generated SDK
	// prompt; both sends both forms.
	Mode string
	// MaxParallelSubCalls caps a run_code program's overlapping sub-calls;
	// positive integer, default 10.
	MaxParallelSubCalls int
}

// resolveMaxParallelSubCalls resolves the overlap cap at the owning config
// boundary (direct construction bypasses the Loader schema).
func resolveMaxParallelSubCalls(value int) (int, error) {
	if value == 0 {
		return 10, nil
	}
	if value < 1 {
		return 0, errors.New("maxParallelSubCalls must be a positive integer")
	}
	return value, nil
}

// compiledRestriction is one restriction compiled at registration for
// repeated live-global lookup.
type compiledRestriction struct {
	allow map[string]bool
	deny  map[string]bool
}

// admits reports whether every filter in this restriction admits a name.
func (r *compiledRestriction) admits(name string) bool {
	if r.allow != nil && !r.allow[name] {
		return false
	}
	if r.deny != nil && r.deny[name] {
		return false
	}
	return true
}

// ToolGuard is a monotonic execution guard evaluated after every
// tools/pre-execute listener and before the tool body. Returning a reason
// denies the call; because guards have no allow result, listener ordering
// cannot turn a denial back into permission.
type ToolGuard func(execution *ToolExecution) (reason string, deny bool)

// toolLayer is one scope's complete registry contribution.
type toolLayer struct {
	scope        ScopeKey
	tools        *NamedEntries[*ToolDefinition]
	restrictions *AnonymousEntries[*compiledRestriction]
	guards       *AnonymousEntries[ToolGuard]
	// mode is the presentation this scope's agent declared for itself,
	// shadowing the deployment default. One cell rather than an entry
	// table: two answers to "which form does the model see" is a
	// contradiction, not a merge.
	mode string
}

func newToolLayer(scopeKey ScopeKey) *toolLayer {
	return &toolLayer{
		scope:        scopeKey,
		tools:        newNamedEntries[*ToolDefinition](),
		restrictions: &AnonymousEntries[*compiledRestriction]{},
		guards:       &AnonymousEntries[ToolGuard]{},
	}
}

func (l *toolLayer) isEmpty() bool {
	return l.tools.Len() == 0 && l.restrictions.IsEmpty() && l.guards.IsEmpty() && l.mode == ""
}

func (l *toolLayer) admits(name string) bool {
	for _, filter := range l.restrictions.Values() {
		if !filter.admits(name) {
			return false
		}
	}
	return true
}

// guardReason returns the first monotonic denial from this layer's live
// guard registrations.
func (l *toolLayer) guardReason(exec *ToolExecution) (string, bool) {
	for _, guard := range l.guards.Values() {
		if reason, deny := guard(exec); deny {
			return reason, true
		}
	}
	return "", false
}

// scopedLayers owns the global and exact-scope layers for the registry.
type scopedLayers = scope.Layers[toolLayer]

func newScopedLayers(onChange func()) *scopedLayers {
	return scope.NewLayers(newToolLayer, func(layer *toolLayer) bool { return layer.isEmpty() }, onChange)
}

// ToolProviderResult is one scope's wire schemas and names for
// prompt-order validation, produced by WireSchemas for the system-prompt
// seam.
type ToolProviderResult struct {
	Schemas    []llm.ToolSchema
	KnownNames []string
}

// ToolRuntime is the tool registry and execution pipeline. Scoped
// registrations shadow globals; one visibility resolver feeds presentation,
// lookup, and dispatch.
type ToolRuntime struct {
	logger cordis.Logger

	mu                  sync.Mutex
	layers              *scopedLayers
	defaultMode         string
	maxParallelSubCalls int
	tokenCounter        uint64

	// events carry the pipeline waterfalls and the result/change emits.
	preExecEvent  waterfallEvent[*preExecuteCarrier, *preExecuteCarrier]
	execEvent     waterfallEvent[*executeCarrier, *executeCarrier]
	postExecEvent waterfallEvent[*postExecuteCarrier, *postExecuteCarrier]
	ptcEvent      waterfallEvent[*dispatchLogCarrier, *dispatchLogCarrier]
	resultEvents  []resultListener
	changeEvents  []changeListener

	// Approval resolves an `ask` pre-dispatch decision through the approval
	// seam, consumed opportunistically like the source's ctx.get('approval'):
	// nil (or a nil return) keeps the historical degrade to deny.
	Approval func() ApprovalService
}

type resultListener struct {
	scope ScopeKey
	id    uint64
	fn    func(exec *ToolExecution, result *ToolExecutionResult)
}

type changeListener struct {
	id uint64
	fn func()
}

// NewToolRuntime builds the registry. Mode defaults to native; the overlap
// cap defaults to 10.
func NewToolRuntime(logger cordis.Logger, config Config) (*ToolRuntime, error) {
	if logger == nil {
		logger = cordis.Discard{}
	}
	mode := config.Mode
	if mode == "" {
		mode = ModeNative
	}
	switch mode {
	case ModeNative, ModePtc, ModeBoth:
	default:
		return nil, fmt.Errorf("tools: unknown presentation mode %q", mode)
	}
	maxParallel, err := resolveMaxParallelSubCalls(config.MaxParallelSubCalls)
	if err != nil {
		return nil, err
	}
	runtime := &ToolRuntime{
		logger:              logger,
		defaultMode:         mode,
		maxParallelSubCalls: maxParallel,
	}
	runtime.layers = newScopedLayers(func() { runtime.emitChange() })
	return runtime, nil
}

// emitChange fires the unfiltered registry-subject notification: a global
// change concerns every agent's next assembly, so a scoped listener sees
// every change.
func (rt *ToolRuntime) emitChange() {
	rt.mu.Lock()
	listeners := make([]changeListener, len(rt.changeEvents))
	copy(listeners, rt.changeEvents)
	rt.mu.Unlock()
	for _, entry := range listeners {
		entry.fn()
	}
}

// OnChange subscribes to the tools/change emit.
func (rt *ToolRuntime) OnChange(listener func()) func() {
	rt.mu.Lock()
	id := scope.NextEntryID()
	rt.changeEvents = append(rt.changeEvents, changeListener{id: id, fn: listener})
	rt.mu.Unlock()
	return func() {
		rt.mu.Lock()
		for i, entry := range rt.changeEvents {
			if entry.id == id {
				rt.changeEvents = append(rt.changeEvents[:i], rt.changeEvents[i+1:]...)
				break
			}
		}
		rt.mu.Unlock()
	}
}

// Register registers a tool globally: schema and output validation fail
// loud, the reserved run_code name is rejected, and duplicates fail. The
// disposer unregisters the tool.
func (rt *ToolRuntime) Register(definition *ToolDefinition) (func(), error) {
	return rt.RegisterIn(nil, definition)
}

// RegisterIn registers a tool in one scope; scoped tools shadow globals.
func (rt *ToolRuntime) RegisterIn(scope ScopeKey, definition *ToolDefinition) (func(), error) {
	if definition == nil {
		return nil, errors.New("tools: definition is required")
	}
	name := definition.Name
	if definition.render == nil {
		return nil, fmt.Errorf("tool %q must declare output { schema, render }", name)
	}
	if err := AssertSupportedJsonSchema(definition.OutputSchema); err != nil {
		return nil, err
	}
	if definition.TimeoutMs != 0 && (definition.TimeoutMs <= 0 || definition.TimeoutMs != definition.TimeoutMs) {
		return nil, fmt.Errorf("tool %q timeoutMs must be a positive finite number", name)
	}
	if name == ReservedRunCodeName {
		return nil, fmt.Errorf("tool name %q is reserved for the PTC mode presentation transport and cannot be registered or shadowed", ReservedRunCodeName)
	}
	rt.mu.Lock()
	dispose, changed, err := rt.layers.Mutate(scope, func(layer *toolLayer) (func(), error) {
		if err := layer.tools.Insert(name, definition); err != nil {
			if scope == nil {
				return nil, fmt.Errorf("tool %q is already registered (for a per-agent variant, register through that agent's scope instead)", name)
			}
			return nil, fmt.Errorf("tool %q is already registered in this scope", name)
		}
		return func() { layer.tools.Remove(name) }, nil
	})
	rt.mu.Unlock()
	if changed {
		rt.emitChange()
	}
	return dispose, err
}

// RestrictIn restricts global tools for one agent scope. Empty filters,
// unknown names, and the reserved transport name fail. Restrictions
// intersect; scoped registrations remain visible.
func (rt *ToolRuntime) RestrictIn(scope ScopeKey, allow, deny []string) (func(), error) {
	if scope == nil {
		return nil, errors.New("tools: restrict() requires a scope: a context-global restriction would mask every agent — restrict the intended agent instead")
	}
	if allow == nil && deny == nil {
		return nil, errors.New("tools: an empty restriction is a no-op: pass allow and/or deny (an empty filter is almost always a materialized-empty-config bug)")
	}
	compiled := &compiledRestriction{}
	if allow != nil {
		compiled.allow = make(map[string]bool, len(allow))
		for _, name := range allow {
			compiled.allow[name] = true
		}
	}
	if deny != nil {
		compiled.deny = make(map[string]bool, len(deny))
		for _, name := range deny {
			compiled.deny[name] = true
		}
	}
	for _, list := range [][]string{allow, deny} {
		for _, name := range list {
			if name == ReservedRunCodeName {
				return nil, fmt.Errorf("tools: restrict() cannot name reserved PTC mode presentation transport %q; restrict end-capability tools instead", ReservedRunCodeName)
			}
		}
	}
	rt.mu.Lock()
	known := rt.viewLocked(scope).restrictableNames
	var unknown []string
	for _, list := range [][]string{allow, deny} {
		for _, name := range list {
			if !known[name] {
				unknown = append(unknown, name)
			}
		}
	}
	if len(unknown) > 0 {
		knownList := make([]string, 0, len(known))
		for name := range known {
			knownList = append(knownList, name)
		}
		sortStrings(knownList)
		rt.mu.Unlock()
		return nil, fmt.Errorf("tools: restrict() names unknown global tools %v; known global tools: %v", unknown, knownList)
	}
	dispose, changed, err := rt.layers.Mutate(scope, func(layer *toolLayer) (func(), error) {
		return layer.restrictions.Append(compiled), nil
	})
	rt.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		rt.emitChange()
	}
	return dispose, nil
}

// GuardIn registers a monotonic guard for one scope (nil = global). Any
// matching guard may deny; no guard can force-allow a call another guard
// denied. Guard registration is deliberately not a tools/change
// notification: the available tool set did not change.
func (rt *ToolRuntime) GuardIn(scope ScopeKey, guard ToolGuard) (func(), error) {
	rt.mu.Lock()
	dispose, _, err := rt.layers.Mutate(scope, func(layer *toolLayer) (func(), error) {
		return layer.guards.Append(guard), nil
	})
	rt.mu.Unlock()
	return dispose, err
}

// Guard registers a global monotonic guard.
func (rt *ToolRuntime) Guard(guard ToolGuard) (func(), error) {
	return rt.GuardIn(nil, guard)
}

// PresentAs presents the calling scope's tools in `mode` instead of the
// deployment default. Scoped only, and one declaration per scope.
func (rt *ToolRuntime) PresentAs(scope ScopeKey, mode string) (func(), error) {
	if scope == nil {
		return nil, errors.New(`tools: PresentAs() requires a scope: a context-global presentation is the "mode" config field`)
	}
	switch mode {
	case ModeNative, ModePtc, ModeBoth:
	default:
		return nil, fmt.Errorf("tools: unknown presentation mode %q", mode)
	}
	rt.mu.Lock()
	dispose, changed, err := rt.layers.Mutate(scope, func(layer *toolLayer) (func(), error) {
		if layer.mode != "" {
			return nil, fmt.Errorf("tools: PresentAs(%q) conflicts with %q already declared for this scope; one composition selects one presentation", mode, layer.mode)
		}
		layer.mode = mode
		return func() { layer.mode = "" }, nil
	})
	rt.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if changed {
		rt.emitChange()
	}
	return dispose, nil
}

// toolView is one scope's complete registry view, derived in a single layer
// traversal.
type toolView struct {
	// visible holds the definitions after restrictions, scoped shadowing,
	// and transport insertion.
	visible map[string]*ToolDefinition
	// knownNames are the pre-restriction capability names used by
	// prompt-order validation.
	knownNames map[string]bool
	// restrictableNames are the current global names a scoped restriction
	// may name.
	restrictableNames map[string]bool
}

// viewLocked resolves every registry fact one scope needs. A restriction
// filters what a scope inherits — the global layer and every ancestor layer
// on its chain — and never what its OWN layer registers: that exemption is
// what a per-child capability filter has to keep intact.
func (rt *ToolRuntime) viewLocked(scope ScopeKey) toolView {
	layers := rt.layers.ChainLayers(scope)
	own := rt.layers.Peek(scope)
	// Inherited surface, nearest ancestor last: a nearer scope's same-name
	// entry shadows a farther one, and the global layer is the farthest.
	inherited := map[string]*ToolDefinition{}
	var inheritedOrder []string
	for _, entry := range rt.layers.Global.tools.Entries() {
		if _, seen := inherited[entry.Name]; !seen {
			inheritedOrder = append(inheritedOrder, entry.Name)
		}
		inherited[entry.Name] = entry.Value
	}
	for _, layer := range layers {
		if layer == own {
			continue
		}
		for _, entry := range layer.tools.Entries() {
			if _, seen := inherited[entry.Name]; !seen {
				inheritedOrder = append(inheritedOrder, entry.Name)
			}
			inherited[entry.Name] = entry.Value
		}
	}
	view := toolView{
		visible:           map[string]*ToolDefinition{},
		knownNames:        map[string]bool{},
		restrictableNames: map[string]bool{},
	}
	for _, name := range inheritedOrder {
		definition := inherited[name]
		view.knownNames[name] = true
		view.restrictableNames[name] = true
		// Restrictions intersect across the whole chain: any scope on it
		// may mask an inherited name for everything nested inside it.
		admitted := true
		for _, layer := range layers {
			if !layer.admits(name) {
				admitted = false
				break
			}
		}
		if admitted {
			view.visible[name] = definition
		}
	}
	// The scope's own registrations last, shadowing an inherited name and
	// outside the filter above.
	if own != nil {
		for _, entry := range own.tools.Entries() {
			view.knownNames[entry.Name] = true
			view.visible[entry.Name] = entry.Value
		}
	}
	// The reserved transport joins only scopes whose mode presents it.
	// Without the PTC port the insertion is skipped and assembly surfaces
	// the missing-code-runtime error at the seam that needs it.
	return view
}

// modeFor resolves the presentation one scope's agent sees: its own
// declaration, else the nearest ancestor's, else the deployment default.
func (rt *ToolRuntime) modeFor(scope ScopeKey) string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.modeForLocked(scope)
}

func (rt *ToolRuntime) modeForLocked(scope ScopeKey) string {
	layers := rt.layers.ChainLayers(scope)
	for i := len(layers) - 1; i >= 0; i-- {
		if layers[i].mode != "" {
			return layers[i].mode
		}
	}
	return rt.defaultMode
}

// Get looks up a tool as one scope sees it (scoped shadows global; a
// restricted-away global reads as absent).
func (rt *ToolRuntime) Get(name string, scope ScopeKey) (*ToolDefinition, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	definition, ok := rt.viewLocked(scope).visible[name]
	return definition, ok
}

// Schemas projects visible definitions onto the allowlisted model-facing
// schema fields, one deep-cloned schema per tool.
func (rt *ToolRuntime) Schemas(scope ScopeKey) []llm.ToolSchema {
	rt.mu.Lock()
	view := rt.viewLocked(scope)
	rt.mu.Unlock()
	out := make([]llm.ToolSchema, 0, len(view.visible))
	for _, name := range sortVisibleNames(view.visible) {
		out = append(out, schemaOf(view.visible[name], true))
	}
	return out
}

// WireSchemas builds one scope's wire schemas and names for prompt-order
// validation. Restrictions do not make known tools invalid, but a mode
// collapse does; a non-native mode fails loud without a code runtime.
func (rt *ToolRuntime) WireSchemas(scope ScopeKey) (ToolProviderResult, error) {
	rt.mu.Lock()
	view := rt.viewLocked(scope)
	mode := rt.modeForLocked(scope)
	rt.mu.Unlock()
	names := sortVisibleNames(view.visible)
	if mode == ModeNative {
		result := ToolProviderResult{KnownNames: make([]string, 0, len(view.knownNames))}
		for name := range view.knownNames {
			result.KnownNames = append(result.KnownNames, name)
		}
		sortStrings(result.KnownNames)
		for _, name := range names {
			result.Schemas = append(result.Schemas, schemaOf(view.visible[name], false))
		}
		return result, nil
	}
	// PTC collapse and SDK projection need the run_code transport and the
	// code-runtime seam; they arrive with the PTC port.
	return ToolProviderResult{}, fmt.Errorf("tools: mode %q requires a code runtime — no implementation is registered in this build yet", mode)
}

func sortVisibleNames(visible map[string]*ToolDefinition) []string {
	names := make([]string, 0, len(visible))
	for name := range visible {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func schemaOf(definition *ToolDefinition, detachParameters bool) llm.ToolSchema {
	parameters := definition.Parameters
	if detachParameters {
		parameters = deepCloneJSON(parameters).(map[string]any)
	}
	return llm.ToolSchema{Name: definition.Name, Description: definition.Description, Parameters: parameters}
}

// deepCloneJSON detaches one lossless JSON value through a wire round trip.
func deepCloneJSON(value any) any {
	detached, ok := snapshotJSONValue(value)
	if !ok {
		return value
	}
	return detached
}

// ResolveExecution resolves the definition that MAY EXECUTE for a call,
// applying the mode collapse at the operation boundary that owns it: a
// model-direct call under `ptc` may only name the reserved run_code
// transport, while a nested sub-dispatch may call any visible tool.
func (rt *ToolRuntime) ResolveExecution(name string, scope ScopeKey, nested bool) (*ToolDefinition, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	tool, ok := rt.viewLocked(scope).visible[name]
	if !ok {
		return nil, false
	}
	if rt.collapsesLocked(name, scope, nested) {
		return nil, false
	}
	return tool, true
}

// collapsesLocked is the security-relevant predicate shared by execution
// resolution: only a model-direct call (no parent) under an effective `ptc`
// naming anything but run_code is denied. Resolved through modeFor, NOT the
// deployment default — an agent given `ptc` by a preset under a native
// deployment must not stay uncollapsed, which would announce one surface
// while executing another.
func (rt *ToolRuntime) collapsesLocked(name string, scope ScopeKey, nested bool) bool {
	return !nested && rt.modeForLocked(scope) == ModePtc && name != ReservedRunCodeName
}

// ExecutionMode classifies a pending call through the caller's visible tool
// definition. Only an exact `true` is parallel; unknown, hidden, undeclared,
// or invalid classifiers are exclusive.
func (rt *ToolRuntime) ExecutionMode(input *ToolExecutionInput) string {
	tool, ok := rt.ResolveExecution(input.Name, input.Agent, input.Parent != nil)
	if !ok {
		return ModeExclusive
	}
	if tool.IsConcurrencySafe(asArgsMap(input.Arguments)) {
		return ModeParallel
	}
	return ModeExclusive
}
