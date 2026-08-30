// Child LLM route selection for the delegation tool (official
// tool-subagent model-selection face): a settings-owned route list gates
// model-facing provider/model/reasoning_effort selection, the durable
// per-session policy event records the decision once, and `list_subagent_models`
// discovers authorized routes.
package toolsubagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// EventSubagentModelSelectionPolicy records that this session's delegation
// tool exposes child provider, model, and reasoning-effort selection.
// Appended before the first model request; absence means the fixed-route
// definition. Log-only: it never enters model history.
const EventSubagentModelSelectionPolicy = "subagent/model-selection-policy"

// AllowedModelRoute is one exact child LLM route authorized by a user
// setting.
type AllowedModelRoute struct {
	// Provider is the registered LLM provider id.
	Provider string `json:"provider"`
	// Model is the provider-owned exact model id.
	Model string `json:"model"`
}

// ModelSelectionPolicy is the route-selection authority captured for one
// delegation definition.
type ModelSelectionPolicy struct {
	// Routes are the exact provider/model routes authorized for explicit
	// selection.
	Routes []AllowedModelRoute `json:"allowedModels"`
}

// ModelRouteKey is the stable identity for one provider/model pair.
func ModelRouteKey(route AllowedModelRoute) string {
	return route.Provider + "\x00" + route.Model
}

// assertAllowedModelRoutes rejects malformed or duplicate route policy
// entries at a durable or configuration boundary.
func assertAllowedModelRoutes(routes []AllowedModelRoute) error {
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if route.Provider == "" || route.Model == "" {
			return fmt.Errorf("subagent model selection requires non-empty provider and model ids")
		}
		key := ModelRouteKey(route)
		if seen[key] {
			return fmt.Errorf("subagent model selection repeats route %q", route.Provider+"/"+route.Model)
		}
		seen[key] = true
	}
	return nil
}

// DelegationModelRequest is the model-facing child LLM route fields.
type DelegationModelRequest struct {
	Provider        *string `json:"provider,omitempty"`
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
}

// parseDelegationModelRequest extracts the optional route fields from raw
// tool arguments; string-typed fields only.
func parseDelegationModelRequest(args map[string]any) DelegationModelRequest {
	pointer := func(raw any) *string {
		value, _ := raw.(string)
		if _, ok := raw.(string); !ok {
			return nil
		}
		return &value
	}
	return DelegationModelRequest{
		Provider:        pointer(args["provider"]),
		Model:           pointer(args["model"]),
		ReasoningEffort: pointer(args["reasoning_effort"]),
	}
}

// hasDelegationModelRequest reports whether the call explicitly selects any
// child LLM value.
func hasDelegationModelRequest(request DelegationModelRequest) bool {
	return request.Provider != nil || request.Model != nil || request.ReasoningEffort != nil
}

// assertNonEmpty rejects an empty model-facing route value at the tool JSON
// boundary.
func assertNonEmpty(value *string, field string) error {
	if value != nil && len(*value) == 0 {
		return fmt.Errorf("child LLM `%s` must be non-empty", field)
	}
	return nil
}

// requestedAgentOptions merges model-supplied selection fields over
// configured child defaults. Provider and model form one route and must be
// supplied together. Changing that route without an effort clears the
// configured route-owned effort. The parent options supply missing
// baselines; the result preserves omission when no layer contributes one.
func requestedAgentOptions(parentOptions agent.AgentOptions, configured *agent.AgentOptions, request DelegationModelRequest, enabled bool) (*agent.AgentOptions, error) {
	if !hasDelegationModelRequest(request) {
		return configured, nil
	}
	if !enabled {
		return nil, fmt.Errorf("child model selection is disabled for this tool instance")
	}
	if err := assertNonEmpty(request.Provider, "provider"); err != nil {
		return nil, err
	}
	if err := assertNonEmpty(request.Model, "model"); err != nil {
		return nil, err
	}
	if err := assertNonEmpty(request.ReasoningEffort, "reasoning_effort"); err != nil {
		return nil, err
	}
	if (request.Provider == nil) != (request.Model == nil) {
		return nil, fmt.Errorf("child LLM `provider` and `model` must be supplied together")
	}

	baseline := agent.AgentOptions{}
	if configured != nil {
		baseline = *configured
	}
	if baseline.Provider == "" {
		baseline.Provider = parentOptions.Provider
	}
	if baseline.Model == "" {
		baseline.Model = parentOptions.Model
	}
	routeChanged := request.Provider != nil &&
		(*request.Provider != baseline.Provider || *request.Model != baseline.Model)

	merged := agent.AgentOptions{}
	if configured != nil {
		merged = *configured
	}
	if routeChanged && request.ReasoningEffort == nil {
		merged.ReasoningEffort = ""
	}
	if request.Provider != nil {
		merged.Provider = *request.Provider
		merged.Model = *request.Model
	}
	if request.ReasoningEffort != nil {
		merged.ReasoningEffort = llm.ReasoningEffortID(*request.ReasoningEffort)
	}
	return &merged, nil
}

// assertAllowedModelSelection enforces a settings-owned route list at the
// operation that creates the child. Pure inheritance stays outside this
// policy because no model-facing choice occurred; any explicit route or
// effort field must resolve to an allowed route.
func assertAllowedModelSelection(policy *ModelSelectionPolicy, parentOptions agent.AgentOptions, requested *agent.AgentOptions, request DelegationModelRequest) error {
	if policy == nil || !hasDelegationModelRequest(request) {
		return nil
	}
	provider := parentOptions.Provider
	model := parentOptions.Model
	if requested != nil {
		if requested.Provider != "" {
			provider = requested.Provider
		}
		if requested.Model != "" {
			model = requested.Model
		}
	}
	if provider == "" || model == "" {
		return fmt.Errorf("cannot select child LLM values without an effective provider and model")
	}
	for _, route := range policy.Routes {
		if route.Provider == provider && route.Model == model {
			return nil
		}
	}
	return fmt.Errorf("child LLM route %q is not allowed for this Session", provider+"/"+model)
}

// hasConfiguredLlmSelection reports whether configured Agent options require
// route resolution before delegation.
func hasConfiguredLlmSelection(options *agent.AgentOptions) bool {
	return options != nil && (options.Provider != "" || options.Model != "" || options.ReasoningEffort != "")
}

// preflightChildLlmRoute resolves an effective child route through its live
// adapter before the child is created. The LLM runtime owns provider lookup,
// exact-model metadata, effort validation, and adapter defaults. An omitted
// effort inherits from the parent route only when the route itself is
// unchanged.
func preflightChildLlmRoute(rt *llm.Runtime, parentOptions agent.AgentOptions, requested *agent.AgentOptions) error {
	provider := parentOptions.Provider
	model := parentOptions.Model
	effort := llm.ReasoningEffortID("")
	if requested != nil {
		if requested.Provider != "" {
			provider = requested.Provider
		}
		if requested.Model != "" {
			model = requested.Model
		}
		effort = requested.ReasoningEffort
	}
	if provider == "" || model == "" {
		return fmt.Errorf("cannot select child LLM values without an effective provider and model")
	}
	routeChanged := provider != parentOptions.Provider || model != parentOptions.Model
	if effort == "" && !routeChanged {
		effort = parentOptions.ReasoningEffort
	}
	config := llm.LlmCallConfig{Provider: provider, Model: model, ReasoningEffort: effort}
	_, err := rt.ResolveCallConfig(config)
	return err
}

// sessionSubagentModelSelectionPolicy reads the exact route list captured
// for a model-selectable definition; nil marks the fixed-route definition.
func sessionSubagentModelSelectionPolicy(sess *session.Session) (*ModelSelectionPolicy, error) {
	for _, event := range sess.Events() {
		if event.Type != EventSubagentModelSelectionPolicy {
			continue
		}
		var payload struct {
			AllowedModels []AllowedModelRoute `json:"allowedModels"`
		}
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			return nil, fmt.Errorf("subagent/model-selection-policy payload: %w", err)
		}
		if err := assertAllowedModelRoutes(payload.AllowedModels); err != nil {
			return nil, err
		}
		if len(payload.AllowedModels) == 0 {
			return nil, fmt.Errorf("subagent/model-selection-policy requires at least one route")
		}
		return &ModelSelectionPolicy{Routes: payload.AllowedModels}, nil
	}
	return nil, nil
}

// recordSubagentModelSelection appends the route policy once, before its
// definition can reach a model request.
func recordSubagentModelSelection(sess *session.Session, allowedModels []AllowedModelRoute) error {
	if existing, err := sessionSubagentModelSelectionPolicy(sess); err != nil || existing != nil {
		return err
	}
	routes := make([]AllowedModelRoute, len(allowedModels))
	copy(routes, allowedModels)
	_, err := sess.Append(EventSubagentModelSelectionPolicy, map[string]any{"allowedModels": routes}, nil)
	return err
}

// ParseModelSelectionSection reads the settings-section document shape
// ({enabled, allowedModels}) into the typed preference; malformed route
// entries surface through Validate.
func ParseModelSelectionSection(value map[string]any) ModelSelectionSettings {
	parsed := ModelSelectionSettings{}
	parsed.Enabled, _ = value["enabled"].(bool)
	raw, _ := value["allowedModels"].([]any)
	for _, item := range raw {
		entry, _ := item.(map[string]any)
		provider, _ := entry["provider"].(string)
		model, _ := entry["model"].(string)
		parsed.AllowedModels = append(parsed.AllowedModels, AllowedModelRoute{Provider: provider, Model: model})
	}
	return parsed
}

// ModelSelectionSettings is the stored user preference; the shipped
// composition defaults it off.
type ModelSelectionSettings struct {
	// Enabled gates newly composed top-level Sessions' model selection.
	Enabled bool `json:"enabled"`
	// AllowedModels are the exact child LLM routes offered to them.
	AllowedModels []AllowedModelRoute `json:"allowedModels"`
}

// SubagentModelSelectionConfig is the settings owner read by delegation
// tools when an Agent is published (official SubagentModelSelectionConfig).
type SubagentModelSelectionConfig struct {
	source func() ModelSelectionSettings
}

// NewModelSelectionConfig builds the preference with an explicit initial
// value (the composition default; the shipped default is disabled).
func NewModelSelectionConfig(initial ModelSelectionSettings) (*SubagentModelSelectionConfig, error) {
	config := &SubagentModelSelectionConfig{}
	if err := config.Validate(initial); err != nil {
		return nil, err
	}
	config.source = func() ModelSelectionSettings { return initial }
	return config, nil
}

// SetSource installs the live user-settings source; the settings section's
// setSource seam.
func (c *SubagentModelSelectionConfig) SetSource(source func() ModelSelectionSettings) {
	c.source = source
}

// Current reads a detached selection preference for the next eligible Agent
// publication.
func (c *SubagentModelSelectionConfig) Current() ModelSelectionSettings {
	current := c.source()
	routes := make([]AllowedModelRoute, len(current.AllowedModels))
	copy(routes, current.AllowedModels)
	return ModelSelectionSettings{Enabled: current.Enabled, AllowedModels: routes}
}

// Validate enforces the route-array contract and the enabled-needs-routes
// rule at the settings boundary.
func (c *SubagentModelSelectionConfig) Validate(value ModelSelectionSettings) error {
	if err := assertAllowedModelRoutes(value.AllowedModels); err != nil {
		return err
	}
	if value.Enabled && len(value.AllowedModels) == 0 {
		return fmt.Errorf("enabled subagent model selection requires at least one allowed model")
	}
	return nil
}

// resolveDelegationPolicy resolves the selection authority for one executing
// agent: its durable session event first, then a live settings sample
// recorded once. A disabled or unconfigured preference yields nil — the
// model then cannot select (the official definition simply omits the
// parameters; the Go tool face is static, so the call is rejected).
func resolveDelegationPolicy(selection *SubagentModelSelectionConfig, parent *agent.Agent) (*ModelSelectionPolicy, error) {
	if policy, err := sessionSubagentModelSelectionPolicy(parent.Session); err != nil || policy != nil {
		return policy, err
	}
	if selection == nil {
		return nil, nil
	}
	settings := selection.Current()
	if !settings.Enabled {
		return nil, nil
	}
	if err := recordSubagentModelSelection(parent.Session, settings.AllowedModels); err != nil {
		return nil, err
	}
	return &ModelSelectionPolicy{Routes: settings.AllowedModels}, nil
}

// modelLine renders one advertised or resolved model.
func modelLine(provider string, model llm.LlmModelInfo) string {
	line := provider + "/" + model.ID + " — " + model.Name
	if model.Description != "" {
		line += ": " + model.Description
	}
	return line
}

// listSubagentModels reads the requested provider, its advertised models, or
// an exact model's reasoning efforts, all filtered through the live policy.
func listSubagentModels(rt *llm.Runtime, policy *ModelSelectionPolicy, args map[string]any) (string, error) {
	request := struct {
		Provider *string
		Model    *string
	}{}
	if raw, ok := args["provider"].(string); ok {
		request.Provider = &raw
	}
	if raw, ok := args["model"].(string); ok {
		request.Model = &raw
	}
	if policy == nil {
		policy = &ModelSelectionPolicy{}
	}
	if request.Model != nil && request.Provider == nil {
		return "", fmt.Errorf("`model` requires `provider`")
	}
	if request.Provider == nil {
		var lines []string
		for _, provider := range rt.ListProviders() {
			for _, route := range policy.Routes {
				if route.Provider == provider.ID {
					lines = append(lines, provider.ID+" — "+provider.Name)
					break
				}
			}
		}
		if len(lines) == 0 {
			return "(no LLM providers)", nil
		}
		return strings.Join(lines, "\n"), nil
	}
	if len(*request.Provider) == 0 {
		return "", fmt.Errorf("`provider` must be non-empty")
	}
	var allowedRoutes []AllowedModelRoute
	for _, route := range policy.Routes {
		if route.Provider == *request.Provider {
			allowedRoutes = append(allowedRoutes, route)
		}
	}
	if len(allowedRoutes) == 0 {
		return "", fmt.Errorf("LLM provider %q is not allowed for this Session", *request.Provider)
	}
	registered := false
	var available []string
	for _, provider := range rt.ListProviders() {
		for _, route := range policy.Routes {
			if route.Provider == provider.ID {
				available = append(available, provider.ID)
				break
			}
		}
		if provider.ID == *request.Provider {
			registered = true
		}
	}
	if !registered {
		joined := strings.Join(available, ", ")
		if joined == "" {
			joined = "(none)"
		}
		return "", fmt.Errorf("LLM provider %q is not registered; available providers: %s", *request.Provider, joined)
	}
	if request.Model == nil {
		models, err := rt.ListModels(*request.Provider)
		if err != nil {
			return "", err
		}
		var lines []string
		for _, model := range models {
			for _, route := range allowedRoutes {
				if route.Model == model.ID {
					lines = append(lines, modelLine(*request.Provider, model))
					break
				}
			}
		}
		if len(lines) == 0 {
			return "(no advertised models for " + *request.Provider + ")", nil
		}
		return strings.Join(lines, "\n"), nil
	}
	if len(*request.Model) == 0 {
		return "", fmt.Errorf("`model` must be non-empty")
	}
	allowedModel := false
	for _, route := range allowedRoutes {
		if route.Model == *request.Model {
			allowedModel = true
			break
		}
	}
	if !allowedModel {
		return "", fmt.Errorf("child LLM route %q is not allowed for this Session", *request.Provider+"/"+*request.Model)
	}
	model, err := rt.ResolveModelInfo(*request.Provider, *request.Model)
	if err != nil {
		return "", err
	}
	head := modelLine(*request.Provider, model.LlmModelInfo)
	if model.Reasoning == nil || len(model.Reasoning.Efforts) == 0 {
		return head + "\nReasoning efforts:\n(no advertised reasoning efforts)", nil
	}
	lines := make([]string, 0, len(model.Reasoning.Efforts))
	for _, effort := range model.Reasoning.Efforts {
		line := string(effort.ID)
		if effort.ID == model.Reasoning.DefaultEffort {
			line += " (default)"
		}
		line += " — " + effort.Name
		if effort.Description != "" {
			line += ": " + effort.Description
		}
		lines = append(lines, line)
	}
	return head + "\nReasoning efforts:\n" + strings.Join(lines, "\n"), nil
}
