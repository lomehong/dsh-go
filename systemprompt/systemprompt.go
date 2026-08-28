// Registry for ordered system sections, dynamic context, tool schemas, and
// prompt variables, assembled before each model step. Port of
// packages/core/system-prompt/src/index.ts.
//
// Go adaptations: registration methods take an explicit ScopeKey instead of
// deriving the scope from the registering Cordis context (the agent package
// owns scoped contexts); `text: string | provider` unions become a Text
// field plus an optional TextProvider that wins when non-nil; the prompt
// variables object is an insertion-ordered VariableSet so diagnostics and
// shadowing keep the source's first-registration order; registration
// disposers are plain functions the host can attach to cordis effects.
package systemprompt

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"dshgo/llm"
	"dshgo/scope"
)

// ScopeKey is an opaque, pointer-identity scope; nil denotes the global view.
type ScopeKey = scope.ScopeKey

// NewScopeKey mints one scope under an optional parent (nil = root scope).
func NewScopeKey(parent ScopeKey) ScopeKey { return scope.NewScopeKey(parent) }

// TOOL_ORDER_REST is the reserved Config.ToolOrder marker for unlisted tools.
const TOOL_ORDER_REST = "<unlisted-tools>"

// PERSONA_SECTION is the deployment persona's section name; a scoped section
// with this name shadows the configured persona.
const PERSONA_SECTION = "deployment:persona"

// Sparse integer placements for repository-owned prompt sections. Adjacent
// values differ by at least ten so accidental collisions are mechanically
// detectable; external plugins may use any finite order.
const (
	OrderHarnessIdentity   = -1000
	OrderHarnessSource     = -900
	OrderWebSurface        = -800
	OrderDeploymentPersona = 0
	OrderPlanPolicy        = 500
	OrderTeamPolicy        = 600
	OrderPtcOnly           = 800
	OrderFileReference     = 900
	OrderToolBash          = 1000
	OrderToolPwsh          = 1010
	OrderToolRead          = 1100
	OrderToolWrite         = 1200
	OrderToolEdit          = 1300
	OrderToolGlob          = 1400
	OrderToolGrep          = 1500
	OrderToolJobs          = 1600
	OrderToolPty           = 1700
	OrderToolWebSearch     = 2000
	OrderToolWebFetch      = 2100
	OrderToolLsp           = 2200
	OrderToolSessionQuery  = 2300
	OrderToolGoal          = 2400
	OrderToolCordis        = 2500
	OrderToolWorkflow      = 2600
	OrderToolRalph         = 2700
	OrderToolSubagent      = 2800
	OrderToolReport        = 2900
	OrderToolsSdk          = 5000
	OrderDeliverableRefs   = 9000
	OrderStructuredOutput  = 9900
)

// PERSONA_ORDER is the prompt order of the persona slot.
const PERSONA_ORDER = OrderDeploymentPersona

var (
	variableNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	groupAtRE      = regexp.MustCompile(`^\{\{([^{}]*)\}\}`)
)

// AssembleContext is the merge-extensible context for one prompt assembly.
type AssembleContext struct {
	// Scope whose providers and waterfall listeners participate; nil means
	// only global providers and subject-less listeners.
	Scope ScopeKey
	// Signal controls only this explicit assembly request; it must not be
	// retained to control later turns.
	Signal context.Context
}

// PromptSection is one contributed section of the system prompt.
type PromptSection struct {
	// Name is unique; a duplicate registration fails.
	Name string
	// Order places the section; equal orders use name order.
	Order float64
	// Text is the static text, or the fallback when TextProvider is set.
	Text string
	// TextProvider is evaluated at each assembly with that assembly's
	// context and wins over Text when non-nil.
	TextProvider func(context AssembleContext) string
	// Complete marks this contribution as the complete system prompt: the
	// assembly still runs the cooperative waterfall, then restores this
	// exact section as the sole prompt section.
	Complete bool
}

// PromptContext is dynamic model context materialized as a durable user-role
// snapshot.
type PromptContext struct {
	// Name is unique; a duplicate registration fails.
	Name string
	// Order places the context; contexts are joined in ascending order.
	Order float64
	// Text is the static text, or the fallback when TextProvider is set.
	Text string
	// TextProvider is evaluated for each assembly.
	TextProvider func(context AssembleContext) string
}

// AssembledSection is one section of an assembly with its text resolved.
type AssembledSection struct {
	Name string
	// Text is the resolved (but not yet interpolated) section text.
	Text string
}

// AssembledContext is one resolved dynamic context contribution.
type AssembledContext struct {
	Name string
	// Text is the resolved text before variable interpolation.
	Text string
}

// ToolProviderResult is one provider's contribution to an assembly.
type ToolProviderResult struct {
	// Schemas are the schemas this provider contributes to THIS assembly.
	Schemas []llm.ToolSchema
	// KnownNames is the pre-restriction name universe for config
	// validation; nil defaults to Schemas' names.
	KnownNames []string
}

// ToolProvider supplies tool schemas for each assembly.
type ToolProvider func(context AssembleContext) ToolProviderResult

// VariableProvider supplies one prompt variable per assembly; defined=false
// registers the name without a value for this assembly.
type VariableProvider func(context AssembleContext) (value string, defined bool)

// VariableSet is the insertion-ordered variable map for one assembly: a
// scoped shadow of an existing name keeps that name's original position.
type VariableSet struct {
	keys []string
	data map[string]*string
}

func newVariableSet() *VariableSet {
	return &VariableSet{data: map[string]*string{}}
}

// Set registers or shadows one variable; value==nil means registered without
// a value for this assembly.
func (s *VariableSet) Set(name string, value *string) {
	if _, seen := s.data[name]; !seen {
		s.keys = append(s.keys, name)
	}
	s.data[name] = value
}

// Get reads one variable; ok is false when the name is unregistered.
func (s *VariableSet) Get(name string) (value *string, ok bool) {
	value, ok = s.data[name]
	return value, ok
}

// Keys returns the names in first-registration order.
func (s *VariableSet) Keys() []string {
	out := make([]string, len(s.keys))
	copy(out, s.keys)
	return out
}

// Clone copies the set; mutating the copy never affects the original. The
// value pointers are shared, matching the source's spread of string|undefined
// entries.
func (s *VariableSet) Clone() *VariableSet {
	out := &VariableSet{keys: append([]string{}, s.keys...), data: map[string]*string{}}
	for name, value := range s.data {
		out.data[name] = value
	}
	return out
}

// PromptAssembly is the merge-extensible assembled model input. Sections and
// contexts remain uninterpolated until rendered; tools are already in
// canonical order.
type PromptAssembly struct {
	Sections  []AssembledSection
	Contexts  []AssembledContext
	Tools     []llm.ToolSchema
	Variables *VariableSet
}

// Config is the plugin configuration: the deployment-authored fragment of the
// system prompt. Pointer booleans keep "omitted" distinct from false.
type Config struct {
	// IncludeHarnessIdentity includes the fixed DeepSeek Harness identity
	// before the deployment persona (default true).
	IncludeHarnessIdentity *bool
	// IncludeRuntimeContext includes dynamic runtime-context snapshots in
	// model history (default true).
	IncludeRuntimeContext *bool
	// Persona is the deployment-wide order-0 persona template. A scoped
	// section named PERSONA_SECTION shadows it.
	Persona string
	// ToolOrder is the model-facing tool-name order, with TOOL_ORDER_REST
	// exactly once. Nil means lexicographic order.
	ToolOrder []string
}

// validateToolOrder validates duplicate names and the required rest marker.
// Registered names are checked at assembly because plugins have not loaded
// at construction.
func validateToolOrder(toolOrder []string) ([]string, error) {
	if toolOrder == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, name := range toolOrder {
		if seen[name] {
			return nil, fmt.Errorf("toolOrder lists %q more than once", name)
		}
		seen[name] = true
	}
	if !seen[TOOL_ORDER_REST] {
		return nil, fmt.Errorf("toolOrder must contain the %q rest entry (where unlisted tools are inserted)", TOOL_ORDER_REST)
	}
	return toolOrder, nil
}

// compareNames is code-point name order — locale-independent, so the order is
// identical on every machine. (Go byte order equals code-point order; the
// JS source's UTF-16 order differs only for names mixing astral characters
// with U+E000..U+FFFF, which section names do not use.)
func compareNames(a, b string) int {
	return strings.Compare(a, b)
}

func comparePromptSections(a, b PromptSection) int {
	if a.Order != b.Order {
		if a.Order < b.Order {
			return -1
		}
		return 1
	}
	return compareNames(a.Name, b.Name)
}

func compareToolNames(a, b llm.ToolSchema) int {
	return compareNames(a.Name, b.Name)
}

// orderTools applies the configured tool order, inserting unlisted tools
// lexicographically at TOOL_ORDER_REST. Unknown configured names fail; known
// but restricted names may be absent.
func orderTools(tools []llm.ToolSchema, toolOrder []string, knownNames map[string]bool) ([]llm.ToolSchema, error) {
	for _, tool := range tools {
		if tool.Name == TOOL_ORDER_REST {
			return nil, fmt.Errorf("tool provider returned reserved tool name %q (reserved for toolOrder's rest entry)", TOOL_ORDER_REST)
		}
	}
	if toolOrder == nil {
		sorted := append([]llm.ToolSchema{}, tools...)
		sort.Slice(sorted, func(i, j int) bool { return compareToolNames(sorted[i], sorted[j]) < 0 })
		return sorted, nil
	}
	var unknown []string
	for _, name := range toolOrder {
		if name != TOOL_ORDER_REST && !knownNames[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		quoted := make([]string, len(unknown))
		for i, name := range unknown {
			quoted[i] = fmt.Sprintf("%q", name)
		}
		known := make([]string, 0, len(knownNames))
		for name := range knownNames {
			known = append(known, name)
		}
		sort.Strings(known)
		plural := ""
		if len(unknown) > 1 {
			plural = "s"
		}
		knownText := strings.Join(known, ", ")
		if knownText == "" {
			knownText = "(none)"
		}
		return nil, fmt.Errorf("toolOrder lists unregistered tool%s %s; known tools: %s", plural, strings.Join(quoted, ", "), knownText)
	}
	listed := map[string]bool{}
	for _, name := range toolOrder {
		listed[name] = true
	}
	var rest []llm.ToolSchema
	for _, tool := range tools {
		if !listed[tool.Name] {
			rest = append(rest, tool)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return compareToolNames(rest[i], rest[j]) < 0 })
	out := make([]llm.ToolSchema, 0, len(tools))
	for _, name := range toolOrder {
		if name == TOOL_ORDER_REST {
			out = append(out, rest...)
			continue
		}
		for _, tool := range tools {
			if tool.Name == name {
				out = append(out, tool)
			}
		}
	}
	return out, nil
}

// interpolate one section or context, attributing diagnostics to its owning
// input. Malformed, unknown, or undefined references fail; a lone `{{`
// without any later `}}` is literal prose, and substituted values are not
// scanned again.
func interpolate(name string, text string, variables *VariableSet, kind string) (string, error) {
	runes := []rune(text)
	var result []rune
	last := 0
	for {
		open := findDouble(runes, last, '{')
		if open < 0 {
			break
		}
		group := groupAtRE.FindStringSubmatch(string(runes[open:]))
		if group == nil {
			// A later closing pair makes this malformed; otherwise it is
			// literal prose.
			if findDouble(runes, open+2, '}') >= 0 {
				window := runes[open:]
				if len(window) > 16 {
					window = window[:16]
				}
				return "", fmt.Errorf("malformed prompt variable reference at \"%s\u2026\" in %s \"%s\" (references are complete simple {{name}} groups)", string(window), kind, name)
			}
			result = append(result, runes[last:open+2]...)
			last = open + 2
			continue
		}
		// An empty name follows the malformed-reference path.
		variable := group[1]
		if !variableNameRE.MatchString(variable) {
			return "", fmt.Errorf("malformed prompt variable reference \"{{%s}}\" in %s \"%s\" (variable names match /%s/)", variable, kind, name, variableNameRE.String())
		}
		value, registered := variables.Get(variable)
		if !registered {
			known := variables.Keys()
			joined := strings.Join(known, ", ")
			if len(known) == 0 {
				joined = "(none)"
			}
			return "", fmt.Errorf("unknown prompt variable \"{{%s}}\" in %s \"%s\"; registered variables: %s", variable, kind, name, joined)
		}
		if value == nil {
			return "", fmt.Errorf("prompt variable \"{{%s}}\" has no value for this assembly (%s \"%s\")", variable, kind, name)
		}
		result = append(result, runes[last:open]...)
		result = append(result, []rune(*value)...)
		last = open + len([]rune(group[0]))
	}
	if last < len(runes) {
		result = append(result, runes[last:]...)
	}
	return string(result), nil
}

// findDouble locates the first doubled ch from index `from`; -1 when absent.
func findDouble(runes []rune, from int, ch rune) int {
	for i := from; i+1 < len(runes); i++ {
		if runes[i] == ch && runes[i+1] == ch {
			return i
		}
	}
	return -1
}

// RenderPrompt interpolates strict variable references, drops empty sections,
// and joins the rest with blank lines. Malformed, unknown, or undefined
// references fail. All sections empty renders ”.
func RenderPrompt(assembly *PromptAssembly) (string, error) {
	var rendered []string
	for _, section := range assembly.Sections {
		text, err := interpolate(section.Name, section.Text, assembly.Variables, "section")
		if err != nil {
			return "", err
		}
		if len(text) > 0 {
			rendered = append(rendered, text)
		}
	}
	return strings.Join(rendered, "\n\n"), nil
}

// RenderContextSections renders the named dynamic-context contributions; one
// entry per contributing context whose text is non-empty.
func RenderContextSections(assembly *PromptAssembly) ([]llm.ContextSnapshotSection, error) {
	var sections []llm.ContextSnapshotSection
	for _, assembled := range assembly.Contexts {
		text, err := interpolate(assembled.Name, assembled.Text, assembly.Variables, "context")
		if err != nil {
			return nil, err
		}
		if len(text) > 0 {
			sections = append(sections, llm.ContextSnapshotSection{Name: assembled.Name, Text: text})
		}
	}
	return sections, nil
}

// JoinContextSections joins an already-rendered section list into the
// model-facing snapshot text, so a request does not interpolate every
// context twice.
func JoinContextSections(sections []llm.ContextSnapshotSection) string {
	body := make([]string, 0, len(sections))
	for _, section := range sections {
		body = append(body, section.Text)
	}
	joined := strings.Join(body, "\n\n")
	if len(joined) == 0 {
		return ""
	}
	return "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + joined
}

// RenderContextSnapshot renders the complete dynamic context snapshot.
func RenderContextSnapshot(assembly *PromptAssembly) (string, error) {
	sections, err := RenderContextSections(assembly)
	if err != nil {
		return "", err
	}
	return JoinContextSections(sections), nil
}

// promptLayer is one scope's aggregate prompt contribution.
type promptLayer struct {
	scope                     ScopeKey
	sections                  *scope.NamedEntries[PromptSection]
	contexts                  *scope.NamedEntries[PromptContext]
	runtimeContextSuppressors *scope.AnonymousEntries[bool]
	toolProviders             *scope.AnonymousEntries[ToolProvider]
	variables                 *scope.NamedEntries[VariableProvider]
}

func newPromptLayer(scopeKey ScopeKey) *promptLayer {
	global := scopeKey == nil
	suffix := " in this scope"
	if global {
		suffix = " (for a per-agent override, register through that agent's `agent.ctx` instead)"
	}
	kind := func(kind string) func(string) error {
		return func(name string) error {
			return fmt.Errorf("prompt %s %q is already registered%s", kind, name, suffix)
		}
	}
	return &promptLayer{
		scope:                     scopeKey,
		sections:                  scope.NewNamedEntries[PromptSection](kind("section")),
		contexts:                  scope.NewNamedEntries[PromptContext](kind("context")),
		runtimeContextSuppressors: &scope.AnonymousEntries[bool]{},
		toolProviders:             &scope.AnonymousEntries[ToolProvider]{},
		variables:                 scope.NewNamedEntries[VariableProvider](kind("variable")),
	}
}

// IsEmpty reports whether this layer owns no prompt registrations.
func (l *promptLayer) IsEmpty() bool {
	return l.sections.IsEmpty() &&
		l.contexts.IsEmpty() &&
		l.runtimeContextSuppressors.IsEmpty() &&
		l.toolProviders.IsEmpty() &&
		l.variables.IsEmpty()
}

type changeListener struct {
	id uint64
	fn func()
}

type assembleCarrier struct {
	Assembly *PromptAssembly
	Ctx      AssembleContext
}

// SystemPrompt is the registry service for the prompt inputs assembled before
// each model step.
type SystemPrompt struct {
	mu          sync.Mutex
	layers      *scope.Layers[promptLayer]
	assemble    scope.WaterfallEvent[assembleCarrier, assembleCarrier]
	changeEvent []changeListener
	toolOrder   []string
}

// NewSystemPrompt builds the registry with its harness-owned base sections.
func NewSystemPrompt(config Config) (*SystemPrompt, error) {
	toolOrder, err := validateToolOrder(config.ToolOrder)
	if err != nil {
		return nil, err
	}
	service := &SystemPrompt{toolOrder: toolOrder}
	service.layers = scope.NewLayers(newPromptLayer, func(layer *promptLayer) bool { return layer.IsEmpty() }, func() { service.emitChange() })
	// Keep harness-owned openers independent of the selected loop plugin.
	includeIdentity := config.IncludeHarnessIdentity == nil || *config.IncludeHarnessIdentity
	if includeIdentity {
		if _, err := service.Section(nil, PromptSection{
			Name:  "harness:identity",
			Order: OrderHarnessIdentity,
			Text:  "You are an AI agent powered by DeepSeek Harness.",
		}); err != nil {
			return nil, err
		}
	}
	if _, err := service.Section(nil, PromptSection{
		Name:  PERSONA_SECTION,
		Order: PERSONA_ORDER,
		Text:  config.Persona,
	}); err != nil {
		return nil, err
	}
	includeRuntimeContext := config.IncludeRuntimeContext == nil || *config.IncludeRuntimeContext
	if !includeRuntimeContext {
		if _, err := service.SuppressRuntimeContext(nil); err != nil {
			return nil, err
		}
	}
	return service, nil
}

func (sp *SystemPrompt) emitChange() {
	sp.mu.Lock()
	listeners := make([]changeListener, len(sp.changeEvent))
	copy(listeners, sp.changeEvent)
	sp.mu.Unlock()
	for _, entry := range listeners {
		entry.fn()
	}
}

// OnChange subscribes to the system-prompt/change notification; this
// registry notification is unfiltered because a change affects every scope.
func (sp *SystemPrompt) OnChange(listener func()) func() {
	sp.mu.Lock()
	id := nextID()
	sp.changeEvent = append(sp.changeEvent, changeListener{id: id, fn: listener})
	sp.mu.Unlock()
	return func() {
		sp.mu.Lock()
		for i, entry := range sp.changeEvent {
			if entry.id == id {
				sp.changeEvent = append(sp.changeEvent[:i], sp.changeEvent[i+1:]...)
				break
			}
		}
		sp.mu.Unlock()
	}
}

var idCounter atomic.Uint64

func nextID() uint64 { return idCounter.Add(1) }

// Section registers an ordered prompt section. A scoped section shadows a
// global section with the same name; duplicates within one layer and
// non-finite orders fail. Registration and disposal notify change.
func (sp *SystemPrompt) Section(scopeKey ScopeKey, section PromptSection) (func(), error) {
	if math.IsNaN(section.Order) || math.IsInf(section.Order, 0) {
		return nil, fmt.Errorf("prompt section %q order must be a finite number", section.Name)
	}
	sp.mu.Lock()
	dispose, changed, err := sp.layers.Mutate(scopeKey, func(layer *promptLayer) (func(), error) {
		if err := layer.sections.Insert(section.Name, section); err != nil {
			return nil, err
		}
		return func() { layer.sections.Remove(section.Name) }, nil
	})
	sp.mu.Unlock()
	if changed {
		sp.emitChange()
	}
	return dispose, err
}

// Context registers ordered dynamic context. Scoped entries shadow global
// entries with the same name.
func (sp *SystemPrompt) Context(scopeKey ScopeKey, promptContext PromptContext) (func(), error) {
	if math.IsNaN(promptContext.Order) || math.IsInf(promptContext.Order, 0) {
		return nil, fmt.Errorf("prompt context %q order must be a finite number", promptContext.Name)
	}
	sp.mu.Lock()
	dispose, changed, err := sp.layers.Mutate(scopeKey, func(layer *promptLayer) (func(), error) {
		if err := layer.contexts.Insert(promptContext.Name, promptContext); err != nil {
			return nil, err
		}
		return func() { layer.contexts.Remove(promptContext.Name) }, nil
	})
	sp.mu.Unlock()
	if changed {
		sp.emitChange()
	}
	return dispose, err
}

// SuppressRuntimeContext suppresses every dynamic runtime-context
// contribution in the scope without changing the services that own or
// enforce those facts. Multiple suppressors remain independently disposable.
func (sp *SystemPrompt) SuppressRuntimeContext(scopeKey ScopeKey) (func(), error) {
	sp.mu.Lock()
	dispose, changed, err := sp.layers.Mutate(scopeKey, func(layer *promptLayer) (func(), error) {
		return layer.runtimeContextSuppressors.Append(true), nil
	})
	sp.mu.Unlock()
	if changed {
		sp.emitChange()
	}
	return dispose, err
}

// Tools registers a tool-schema provider. Global and matching scoped
// providers both contribute; returning the reserved TOOL_ORDER_REST name
// makes assembly fail.
func (sp *SystemPrompt) Tools(scopeKey ScopeKey, provider ToolProvider) (func(), error) {
	sp.mu.Lock()
	dispose, changed, err := sp.layers.Mutate(scopeKey, func(layer *promptLayer) (func(), error) {
		return layer.toolProviders.Append(provider), nil
	})
	sp.mu.Unlock()
	if changed {
		sp.emitChange()
	}
	return dispose, err
}

// Variable registers a prompt variable. Scoped values shadow globals;
// invalid or duplicate names fail. A provider may register the name without
// a value, but rendering a section that references that value then fails.
func (sp *SystemPrompt) Variable(scopeKey ScopeKey, name string, provider VariableProvider) (func(), error) {
	if !variableNameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid prompt variable name \"%s\" (must match /%s/)", name, variableNameRE.String())
	}
	sp.mu.Lock()
	dispose, changed, err := sp.layers.Mutate(scopeKey, func(layer *promptLayer) (func(), error) {
		if err := layer.variables.Insert(name, provider); err != nil {
			return nil, err
		}
		return func() { layer.variables.Remove(name) }, nil
	})
	sp.mu.Unlock()
	if changed {
		sp.emitChange()
	}
	return dispose, err
}

// OnAssemble registers a system-prompt/assemble waterfall listener. Scoped
// listeners receive only that scope's assemblies; the returned value is
// authoritative. A registered complete section is restored after this
// waterfall, so listeners cannot add to or replace that scope's system
// prompt.
func (sp *SystemPrompt) OnAssemble(scopeKey ScopeKey, handler func(assembly *PromptAssembly, assembleContext AssembleContext, next func() *PromptAssembly) *PromptAssembly) func() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.assemble.On(scopeKey, func(c assembleCarrier, next func(assembleCarrier) assembleCarrier) assembleCarrier {
		c.Assembly = handler(c.Assembly, c.Ctx, func() *PromptAssembly {
			return next(c).Assembly
		})
		return c
	})
}

// Assemble resolves global and scoped providers, detaches tool parameters,
// applies canonical ordering, then runs the assembly waterfall. Scoped
// sections and variables shadow globals. The returned waterfall value is
// authoritative except that an effective complete section is restored
// afterwards as the sole prompt section.
func (sp *SystemPrompt) Assemble(assembleContext AssembleContext) (*PromptAssembly, error) {
	scopeKey := assembleContext.Scope
	sp.mu.Lock()
	scopeLayers := sp.layers.ChainLayers(scopeKey)
	runtimeContextSuppressed := !sp.layers.Global.runtimeContextSuppressors.IsEmpty()
	for _, layer := range scopeLayers {
		if !layer.runtimeContextSuppressors.IsEmpty() {
			runtimeContextSuppressed = true
		}
	}
	// Scoped variables shadow globals; farthest first so the nearest wins.
	variables := newVariableSet()
	for _, entry := range sp.layers.Global.variables.Entries() {
		value, defined := entry.Value(assembleContext)
		if defined {
			variables.Set(entry.Name, &value)
		} else {
			variables.Set(entry.Name, nil)
		}
	}
	for _, layer := range scopeLayers {
		for _, entry := range layer.variables.Entries() {
			value, defined := entry.Value(assembleContext)
			if defined {
				variables.Set(entry.Name, &value)
			} else {
				variables.Set(entry.Name, nil)
			}
		}
	}
	sectionByName := scope.MergeLayers(sp.layers, scopeKey, func(layer *promptLayer) *scope.NamedEntries[PromptSection] { return layer.sections })
	contextByName := scope.MergeLayers(sp.layers, scopeKey, func(layer *promptLayer) *scope.NamedEntries[PromptContext] { return layer.contexts })
	// Validate order against pre-restriction names while collecting.
	providers := append([]ToolProvider{}, sp.layers.Global.toolProviders.Values()...)
	for _, layer := range scopeLayers {
		providers = append(providers, layer.toolProviders.Values()...)
	}
	var collected []llm.ToolSchema
	knownNames := map[string]bool{}
	for _, provider := range providers {
		result := provider(assembleContext)
		for _, schema := range result.Schemas {
			collected = append(collected, llm.ToolSchema{
				Name:        schema.Name,
				Description: schema.Description,
				Parameters:  deepCloneParameters(schema.Parameters),
			})
		}
		if result.KnownNames != nil {
			for _, name := range result.KnownNames {
				knownNames[name] = true
			}
		} else {
			for _, schema := range result.Schemas {
				knownNames[schema.Name] = true
			}
		}
	}
	sectionDefinitions := sectionByName.Values()
	sort.SliceStable(sectionDefinitions, func(i, j int) bool { return comparePromptSections(sectionDefinitions[i], sectionDefinitions[j]) < 0 })
	var completeSections []PromptSection
	for _, section := range sectionDefinitions {
		if section.Complete {
			completeSections = append(completeSections, section)
		}
	}
	if len(completeSections) > 1 {
		quoted := make([]string, len(completeSections))
		for i, section := range completeSections {
			quoted[i] = fmt.Sprintf("%q", section.Name)
		}
		sp.mu.Unlock()
		return nil, fmt.Errorf("multiple complete prompt sections are active: %s", strings.Join(quoted, ", "))
	}
	var completeSection *AssembledSection
	sections := make([]AssembledSection, 0, len(sectionDefinitions))
	for _, section := range sectionDefinitions {
		text := section.Text
		if section.TextProvider != nil {
			text = section.TextProvider(assembleContext)
		}
		assembled := AssembledSection{Name: section.Name, Text: text}
		if section.Complete {
			copied := assembled
			completeSection = &copied
		}
		sections = append(sections, assembled)
	}
	var contexts []AssembledContext
	if !runtimeContextSuppressed {
		contextDefinitions := contextByName.Values()
		// Equal orders keep merge insertion order: the source sorts by the
		// numeric difference alone.
		sort.SliceStable(contextDefinitions, func(i, j int) bool { return contextDefinitions[i].Order < contextDefinitions[j].Order })
		contexts = make([]AssembledContext, 0, len(contextDefinitions))
		for _, entry := range contextDefinitions {
			text := entry.Text
			if entry.TextProvider != nil {
				text = entry.TextProvider(assembleContext)
			}
			contexts = append(contexts, AssembledContext{Name: entry.Name, Text: text})
		}
	}
	tools, err := orderTools(collected, sp.toolOrder, knownNames)
	if err != nil {
		sp.mu.Unlock()
		return nil, err
	}
	assembly := &PromptAssembly{Sections: sections, Contexts: contexts, Tools: tools, Variables: variables}
	carrier := assembleCarrier{Assembly: assembly, Ctx: assembleContext}
	listeners := sp.assemble.Snapshot(scopeKey)
	sp.mu.Unlock()
	transformed := scope.RunWaterfall(listeners, carrier, func(c assembleCarrier) assembleCarrier { return c }).Assembly
	if completeSection == nil && !runtimeContextSuppressed {
		return transformed, nil
	}
	result := *transformed
	if completeSection != nil {
		result.Sections = []AssembledSection{*completeSection}
	}
	if runtimeContextSuppressed {
		result.Contexts = nil
	}
	return &result, nil
}

// ValidateAssembly is the package invariant: the authoritative assembly must
// carry unique non-empty section/context names with string text, non-empty
// tool names, and valid variable names with string-or-absent values.
func ValidateAssembly(assembly *PromptAssembly) []string {
	var failures []string
	sectionNames := map[string]bool{}
	for _, section := range assembly.Sections {
		if section.Name == "" {
			failures = append(failures, "assembled section names must be non-empty")
		}
		if sectionNames[section.Name] {
			failures = append(failures, fmt.Sprintf("assembled section name %q is duplicated", section.Name))
		}
		sectionNames[section.Name] = true
	}
	contextNames := map[string]bool{}
	for _, assembled := range assembly.Contexts {
		if assembled.Name == "" {
			failures = append(failures, "assembled context names must be non-empty")
		}
		if contextNames[assembled.Name] {
			failures = append(failures, fmt.Sprintf("assembled context name %q is duplicated", assembled.Name))
		}
		contextNames[assembled.Name] = true
	}
	for _, tool := range assembly.Tools {
		if tool.Name == "" {
			failures = append(failures, "assembled tool names must be non-empty")
		}
	}
	if assembly.Variables != nil {
		for _, name := range assembly.Variables.Keys() {
			if !variableNameRE.MatchString(name) {
				failures = append(failures, fmt.Sprintf("assembled variable name %q is invalid", name))
			}
		}
	}
	return failures
}

// deepCloneParameters detaches one tool schema's parameter object through a
// wire round trip (structuredClone in the source).
func deepCloneParameters(parameters map[string]any) map[string]any {
	if parameters == nil {
		return nil
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return parameters
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return parameters
	}
	return out
}
