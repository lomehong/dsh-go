// Package guard ports @deepseek-ai/dsh-repeat-tool-reminder and
// @deepseek-ai/dsh-tool-call-timeout-policy: loop-hygiene plugins. The
// repeat detector is advisory per-agent enrichment (it never vetoes or
// rewrites calls); the timeout policy is the cooperative enforcer mapping a
// declared tool budget to a structured TOOL_TIMEOUT result.
package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/tools"
)

// PluginName is the source label stamped on every reminder. It is
// load-bearing: an unlabeled context would render as a user prompt in
// derived history.
const PluginName = "repeat-tool-reminder"

// Defaults for RepeatConfig.
const (
	DefaultRepeatThresholds      = 3 // first tier of the default escalation
	defaultArgumentsPreviewChars = 500
)

// gentleReminder is keyed to thresholds[0], not a literal count, so a
// custom first threshold keeps the gentle-then-detailed escalation.
const gentleReminder = "You are repeating the exact same tool call with identical arguments. " +
	"Carefully analyze the previous result before calling again: if the task is " +
	"not complete, try a different approach or different arguments instead of " +
	"repeating the call."

// RepeatConfig configures the advisory repeat-call detector. include/exclude
// entries are `*`-wildcard predicates over tool names at call time — a
// pattern matching no registered tool is valid.
type RepeatConfig struct {
	// Thresholds: consecutive-repeat counts that trigger a reminder.
	Thresholds []int
	// Include: tool-name patterns to track; empty means every tool.
	Include []string
	// Exclude: tool-name patterns transparent to the chain (neither count
	// nor reset).
	Exclude []string
	// ArgumentsPreviewChars caps the canonical arguments quoted in the
	// DETAILED reminder. The cap bounds the reminder, never the detection.
	ArgumentsPreviewChars int
}

// ValidateRepeatConfig applies defaults and enforces the fail-loud contract:
// a non-empty, duplicate-free threshold list of integers >= 2, sorted
// ascending (the escalation rule reads thresholds[0] as the gentle tier), and
// a preview cap that is an integer >= 1. A nil threshold list means
// "unconfigured"; an empty non-nil list is a loud misconfiguration.
func ValidateRepeatConfig(config RepeatConfig) (RepeatConfig, error) {
	if config.Thresholds == nil {
		config.Thresholds = []int{3, 5, 8}
	}
	if len(config.Thresholds) == 0 {
		return RepeatConfig{}, fmt.Errorf("repeat-tool-reminder: `thresholds` must not be empty")
	}
	if config.ArgumentsPreviewChars == 0 {
		config.ArgumentsPreviewChars = defaultArgumentsPreviewChars
	}
	seen := map[int]bool{}
	for _, value := range config.Thresholds {
		if value < 2 {
			return RepeatConfig{}, fmt.Errorf("repeat-tool-reminder: invalid threshold %d — every threshold must be an integer >= 2", value)
		}
		if seen[value] {
			return RepeatConfig{}, fmt.Errorf("repeat-tool-reminder: `thresholds` must not contain duplicates")
		}
		seen[value] = true
	}
	sorted := append([]int(nil), config.Thresholds...)
	sort.Ints(sorted)
	config.Thresholds = sorted
	if config.ArgumentsPreviewChars < 1 {
		return RepeatConfig{}, fmt.Errorf("repeat-tool-reminder: invalid argumentsPreviewChars %d — must be an integer >= 1", config.ArgumentsPreviewChars)
	}
	return config, nil
}

// wildcardToRegexp compiles one `*`-wildcard pattern to an anchored regexp;
// every other regex metacharacter matches literally.
func wildcardToRegexp(pattern string) (*regexp.Regexp, error) {
	var builder strings.Builder
	builder.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			builder.WriteString(".*")
		case '|', '\\', '{', '}', '(', ')', '[', ']', '^', '$', '+', '?', '.':
			builder.WriteString("\\")
			builder.WriteRune(r)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteString("$")
	return regexp.Compile(builder.String())
}

// RepeatToolReminder is one detector over one tool runtime.
type RepeatToolReminder struct {
	thresholds   []int
	thresholdSet map[int]bool
	include      []*regexp.Regexp
	exclude      []*regexp.Regexp
	previewChars int
	mu           sync.Mutex
	chains       map[scope.ScopeKey]*repeatChain
}

// repeatChain is one agent's consecutive-repeat run: the last tracked call's
// identity key and its length.
type repeatChain struct {
	key   string
	count int
}

// NewRepeatToolReminder validates the config and builds the detector.
func NewRepeatToolReminder(config RepeatConfig) (*RepeatToolReminder, error) {
	resolved, err := ValidateRepeatConfig(config)
	if err != nil {
		return nil, err
	}
	guard := &RepeatToolReminder{
		thresholds:   resolved.Thresholds,
		thresholdSet: map[int]bool{},
		previewChars: resolved.ArgumentsPreviewChars,
		chains:       map[scope.ScopeKey]*repeatChain{},
	}
	for _, value := range resolved.Thresholds {
		guard.thresholdSet[value] = true
	}
	for _, pattern := range resolved.Include {
		compiled, err := wildcardToRegexp(pattern)
		if err != nil {
			return nil, fmt.Errorf("repeat-tool-reminder: invalid include pattern %q: %w", pattern, err)
		}
		guard.include = append(guard.include, compiled)
	}
	for _, pattern := range resolved.Exclude {
		compiled, err := wildcardToRegexp(pattern)
		if err != nil {
			return nil, fmt.Errorf("repeat-tool-reminder: invalid exclude pattern %q: %w", pattern, err)
		}
		guard.exclude = append(guard.exclude, compiled)
	}
	return guard, nil
}

// tracked reports whether a tool participates in the chain; untracked calls
// are transparent — they neither count nor reset.
func (g *RepeatToolReminder) tracked(toolName string) bool {
	if len(g.include) > 0 {
		matched := false
		for _, pattern := range g.include {
			if pattern.MatchString(toolName) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, pattern := range g.exclude {
		if pattern.MatchString(toolName) {
			return false
		}
	}
	return true
}

// sortJsonValue deep key-sorts a parsed-JSON value so two argument objects
// differing only in property order canonicalize identically. The loop's
// parsed-JSON domain is the whole input domain.
func sortJsonValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = sortJsonValue(item)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		sorted := make(map[string]any, len(typed))
		for _, key := range keys {
			sorted[key] = sortJsonValue(typed[key])
		}
		return sorted
	default:
		return value
	}
}

// canonicalize is the canonical string form of a call's arguments: deep
// key-sort, then stringify.
func canonicalize(argumentsValue any) string {
	encoded, err := json.Marshal(sortJsonValue(argumentsValue))
	if err != nil {
		// Lossless-JSON arguments always marshal; the error branch exists
		// for the raw-string fallback shape only.
		return fmt.Sprintf("%v", argumentsValue)
	}
	return string(encoded)
}

// previewArguments head-truncates the canonical arguments for quoting in the
// detailed reminder, marking how much was omitted. It bounds only the
// model-visible text.
func previewArguments(canonical string, capChars int) string {
	if len(canonical) <= capChars {
		return canonical
	}
	return fmt.Sprintf("%s… (+%d more chars)", canonical[:capChars], len(canonical)-capChars)
}

// detailedReminder names the tool, the run length, and the canonical
// arguments.
func detailedReminder(toolName string, count int, canonicalArguments string) string {
	return "Repeated tool call detected:\n" +
		fmt.Sprintf("- tool: %s\n", toolName) +
		fmt.Sprintf("- consecutive_calls: %d\n", count) +
		fmt.Sprintf("- arguments: %s\n", canonicalArguments) +
		"The repeated calls are not making progress. Do not call this tool with " +
		"these exact arguments again. Inspect the latest result and choose a " +
		"different action, different arguments, or finish the task if enough " +
		"evidence has been gathered."
}

// Observe advances the keyed agent's chain for one attempt and returns the
// reminder to deliver when this attempt's run length hits a configured
// threshold. Counting happens in post-execute because denied calls also flow
// through that waterfall, and a model hammering a denied call is exactly the
// loop worth breaking. A nil agent key (a direct execute caller has no model
// to remind) skips observation.
func (g *RepeatToolReminder) Observe(agentKey scope.ScopeKey, exec *tools.ToolExecution) *llm.Message {
	if agentKey == nil {
		return nil
	}
	if !g.tracked(exec.Name) {
		return nil
	}
	canonical := canonicalize(exec.Arguments)
	keyBytes, _ := json.Marshal([]string{exec.Name, canonical})
	key := string(keyBytes)
	g.mu.Lock()
	chain := g.chains[agentKey]
	count := 1
	if chain != nil && chain.key == key {
		count = chain.count + 1
	}
	g.chains[agentKey] = &repeatChain{key: key, count: count}
	g.mu.Unlock()
	if !g.thresholdSet[count] {
		return nil
	}
	text := detailedReminder(exec.Name, count, previewArguments(canonical, g.previewChars))
	if count == g.thresholds[0] {
		text = gentleReminder
	}
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: "text", Text: text}},
		llm.MessageSource{Kind: "plugin", Plugin: PluginName, Form: "notice", Summary: fmt.Sprintf("%s × %d", exec.Name, count)},
	)
	return &message
}

// Reset drops one agent's chain. A user interjection changes the context;
// repetition across it is not a loop.
func (g *RepeatToolReminder) Reset(agentKey scope.ScopeKey) {
	if agentKey == nil {
		return
	}
	g.mu.Lock()
	delete(g.chains, agentKey)
	g.mu.Unlock()
}

// AttachPreStepReset wires the pure reset hook onto one agent registry: a
// user interjection in the claimed step's input drops that agent's chain,
// because repetition across a context change is not a loop. Always delegates
// — attaching nothing, vetoing nothing. The returned disposer detaches it.
func (g *RepeatToolReminder) AttachPreStepReset(agents *agent.AgentRegistry) func() {
	return agents.Events().OnWaterfall(agent.EventPreStep, nil, func(payload any, next func(any) any) any {
		if preStep, ok := payload.(agent.PreStepPayload); ok {
			for _, message := range preStep.Messages {
				if message.Source.Kind == llm.SourceUser {
					g.Reset(preStep.Agent.Scope)
					break
				}
			}
		}
		return next(payload)
	})
}

// Attach installs the post-execute observe-and-enrich listener: count first
// (state advances regardless of the downstream outcome), delegate so a later
// listener can still block or replace, then fold the reminder onto whatever
// came back — additionalContexts rides both decision variants, so a blocked
// call still gets the nudge. The returned disposer detaches it.
func (g *RepeatToolReminder) Attach(runtime *tools.ToolRuntime) func() {
	detach := runtime.OnPostExecute(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult, next func(*tools.ToolExecutionResult) *tools.PostToolDecision) *tools.PostToolDecision {
		reminder := g.Observe(exec.Agent, exec)
		downstream := next(result)
		if reminder == nil {
			return downstream
		}
		contexts := make([]llm.Message, 0, len(downstream.AdditionalContexts)+1)
		contexts = append(contexts, *reminder)
		contexts = append(contexts, downstream.AdditionalContexts...)
		downstream.AdditionalContexts = contexts
		return downstream
	})
	return detach
}
