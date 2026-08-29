// Registration of the model-facing `skill` tool and its visibility-matched
// durable session catalog on an agent registry's pre-step waterfall.
package toolskill

import (
	"context"
	"fmt"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/skill"
	"dshgo/tools"
)

// Config is the model-facing skill catalog configuration.
type Config struct {
	// CatalogDescriptionMaxLength is the maximum normalized description
	// length rendered in the session catalog; zero applies the default,
	// and values below 3 fail loud.
	CatalogDescriptionMaxLength int
}

// Register defines the `skill` tool on the runtime and attaches the gesture
// and catalog listeners to the registry's pre-step waterfall. The catalog is
// emitted only when the calling agent resolves this plugin's exact tool
// registration: a restriction or scoped same-name shadow removes both the
// schema and its call guidance. The returned disposer tears down in reverse
// registration order (catalog, gesture, tool).
func Register(runtime *tools.ToolRuntime, skills *skill.Registry, agents *agent.AgentRegistry, logger cordis.Logger, config Config) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("tool-skill: a tool runtime is required")
	}
	if skills == nil {
		return nil, fmt.Errorf("tool-skill: a skill registry is required")
	}
	if logger == nil {
		logger = cordis.Discard{}
	}
	maxLength := config.CatalogDescriptionMaxLength
	if maxLength == 0 {
		maxLength = DefaultCatalogDescriptionMaxLength
	}
	if maxLength < 3 {
		return nil, fmt.Errorf("tool-skill: catalogDescriptionMaxLength must be an integer greater than or equal to 3")
	}

	closedObject := func() *bool { value := false; return &value }
	resourceBaseBranches := []*tools.ValueSchemaSpec{
		{
			Type:                 "object",
			AdditionalProperties: closedObject(),
			Properties: map[string]tools.PropSpec{
				"kind": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "directory"}, Required: true},
				"path": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			},
		},
		{
			Type:                 "object",
			AdditionalProperties: closedObject(),
			Properties: map[string]tools.PropSpec{
				"kind": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "url"}, Required: true},
				"url":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			},
		},
		{
			Type:                 "object",
			AdditionalProperties: closedObject(),
			Properties: map[string]tools.PropSpec{
				"kind":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Const: "opaque"}, Required: true},
				"description": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			},
		},
	}
	toolDef, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        Name,
		Description: "Load the full instructions for an available skill. Call this with the exact skill name from the session skill catalog before acting on a task that names or clearly matches that skill.",
		Parameters: map[string]tools.PropSpec{
			"name": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "The exact skill name from the available skills list.",
				},
				Required: true,
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"name":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"provider": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"resourceBase": {
						ValueSchemaSpec: tools.ValueSchemaSpec{OneOf: resourceBaseBranches},
					},
					"content": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				loaded, ok := value.(LoadedSkill)
				if !ok {
					return []llm.ContentBlock{{Type: llm.BlockText, Text: "Loaded skill."}}
				}
				text := skill.RenderSkillContent(skill.Definition{
					Summary: skill.Summary{
						Name:         loaded.Name,
						Provider:     loaded.Provider,
						ResourceBase: loaded.ResourceBase,
					},
					Content: loaded.Content,
				})
				return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			raw, _ := args["name"].(string)
			if !skill.IsSkillName(raw) {
				return nil, fmt.Errorf("invalid skill name %q", raw)
			}
			var signal context.Context
			if exec != nil {
				signal = exec.Signal
			}
			lookup := skillView{
				registry: skills,
				cwd:      sessionCWD(agents, exec.Agent),
				scope:    exec.Agent,
				signal:   signal,
				logger:   logger,
			}
			summaries, err := lookup.list()
			if err != nil {
				return nil, err
			}
			var summary *skill.Summary
			for index := range summaries {
				if summaries[index].Name == raw {
					summary = &summaries[index]
					break
				}
			}
			if summary == nil {
				return nil, fmt.Errorf("skill %q is unknown or no longer available", raw)
			}
			if !skill.IsModelInvocable(*summary) {
				return nil, fmt.Errorf("skill %q is not available for model invocation", raw)
			}
			definition, err := lookup.get(raw)
			if err != nil {
				return nil, err
			}
			if definition == nil {
				return nil, fmt.Errorf("skill %q is unknown or no longer available", raw)
			}
			if !skill.IsModelInvocable(definition.Summary) {
				return nil, fmt.Errorf("skill %q is not available for model invocation", raw)
			}
			return LoadedSkill{
				Name:         definition.Name,
				Provider:     definition.Provider,
				ResourceBase: definition.ResourceBase,
				Content:      definition.Content,
			}, nil
		},
		IsConcurrencySafe: func(map[string]any) bool { return true },
	})
	if err != nil {
		return nil, err
	}
	toolUndo, err := runtime.Register(toolDef)
	if err != nil {
		return nil, err
	}
	// The agent is its own scope key, so a registry lookup resolves the
	// layered skills exactly as this agent's composition sees it.
	cwd := func(p agent.PreStepPayload) string {
		if p.Agent == nil || p.Agent.Session == nil {
			return ""
		}
		return p.Agent.Session.Header().CWD
	}
	lookup := func(p agent.PreStepPayload) skillView {
		return skillView{
			registry: skills,
			cwd:      cwd(p),
			scope:    agentScope(p),
			signal:   p.Signal,
			logger:   logger,
		}
	}
	// User-explicit invocation: a claimed user message whose token is
	// `/name` naming a user-invocable skill is a deterministic load
	// gesture. Only direct user messages are scanned — external text
	// cannot forge the gesture — and a token naming no user-invocable
	// skill stays ordinary prose. This is the only entry point for skills
	// the model may not invoke itself; the catalog and the `skill` tool
	// never see those.
	gestureDetach := agents.Events().PreStep().On(nil, func(p agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		decision := next(p)
		if decision.Kind != "enter" {
			return decision
		}
		names := invokedSkillNames(p.Messages)
		if len(names) == 0 {
			return decision
		}
		if p.Signal != nil && p.Signal.Err() != nil {
			return decision
		}
		view := lookup(p)
		var injections []llm.Message
		for _, name := range names {
			definition, err := view.get(name)
			if err != nil {
				logger.Warn(fmt.Sprintf("tool-skill: skill %q gesture load failed: %v", name, err))
				continue
			}
			// Unknown names and user-disabled skills stay plain prose: the
			// gesture was never a claim this boundary recognizes.
			if definition == nil || !skill.IsUserInvocable(definition.Summary) {
				continue
			}
			injections = append(injections, llm.NewUserMessage(
				[]llm.ContentBlock{{Type: llm.BlockText, Text: skill.RenderSkillContent(*definition)}},
				llm.MessageSource{
					Kind:    SourceKindSkillInvocation,
					Plugin:  PluginName,
					Form:    llm.FormInstructions,
					Summary: name,
				},
			))
		}
		if len(injections) == 0 {
			return decision
		}
		decision.Messages = append(decision.Messages, injections...)
		return decision
	})
	// The durable session catalog: published only when this agent resolves
	// the exact registration above. The comparison is against the
	// registered definition pointer, not a name lookup, so a scoped shadow
	// merely named `skill` cannot inherit this catalog.
	catalogDetach := agents.Events().PreStep().On(nil, func(p agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		decision := next(p)
		if decision.Kind != "enter" {
			return decision
		}
		if p.Signal != nil && p.Signal.Err() != nil {
			return decision
		}
		var snapshot skill.CatalogSnapshot
		toolVisible := false
		if resolved, found := runtime.Get(Name, agentScope(p)); found && resolved == toolDef {
			toolVisible = true
		}
		if toolVisible {
			view := lookup(p)
			published, err := view.snapshot()
			if err != nil {
				logger.Warn(fmt.Sprintf("tool-skill: catalog snapshot failed: %v", err))
				return decision
			}
			snapshot = published
		}
		if !snapshot.Complete {
			return decision
		}
		var modelInvocable []skill.Summary
		for _, entry := range snapshot.Skills {
			if skill.IsModelInvocable(entry) {
				modelInvocable = append(modelInvocable, entry)
			}
		}
		entries := catalogSourceEntries(modelInvocable, maxLength)
		digest := digestCatalogEntries(entries)
		historyDigest, historyVisible, published := catalogHistory(p.Agent.Session)
		existing := catalogMessage(decision.Messages)
		if historyVisible && historyDigest == digest {
			if existing == nil {
				return decision
			}
			return enterWithout(decision, existing.message.ID)
		}
		if existing != nil && digestCatalogEntries(existing.entries) == digest {
			return decision
		}
		if !published && len(modelInvocable) == 0 {
			if existing == nil {
				return decision
			}
			return enterWithout(decision, existing.message.ID)
		}
		var catalog llm.Message
		if published {
			catalog = renderCatalogUpdate(entries)
		} else {
			catalog = renderCatalogMessage(entries)
		}
		if existing == nil {
			decision.Messages = append(decision.Messages, catalog)
			return decision
		}
		messages := make([]llm.Message, 0, len(decision.Messages))
		for _, message := range decision.Messages {
			if message.ID == existing.message.ID {
				messages = append(messages, catalog)
			} else {
				messages = append(messages, message)
			}
		}
		decision.Messages = messages
		return decision
	})
	return func() {
		catalogDetach()
		gestureDetach()
		toolUndo()
	}, nil
}

// enterWithout drops one message from an enter decision by id.
func enterWithout(decision agent.PreStepDecision, id llm.MessageID) agent.PreStepDecision {
	messages := make([]llm.Message, 0, len(decision.Messages))
	for _, message := range decision.Messages {
		if message.ID != id {
			messages = append(messages, message)
		}
	}
	decision.Messages = messages
	return decision
}

// agentScope returns the payload agent's scope key (nil-safe).
func agentScope(p agent.PreStepPayload) tools.ScopeKey {
	if p.Agent == nil {
		return nil
	}
	return p.Agent.Scope
}

// sessionCWD resolves the live agent's session cwd by its tools scope key.
func sessionCWD(agents *agent.AgentRegistry, target tools.ScopeKey) string {
	if agents == nil || target == nil {
		return ""
	}
	for _, candidate := range agents.List() {
		if candidate.Scope == target && candidate.Session != nil {
			return candidate.Session.Header().CWD
		}
	}
	return ""
}

// skillView bundles one pre-step's skill-registry view.
type skillView struct {
	registry *skill.Registry
	cwd      string
	scope    tools.ScopeKey
	signal   context.Context
	logger   cordis.Logger
}

func (v skillView) options() skill.ViewOptions {
	ctx := v.signal
	if ctx == nil {
		ctx = context.Background()
	}
	return skill.ViewOptions{
		LookupOptions: skill.LookupOptions{Context: ctx, CWD: v.cwd},
		Scope:         v.scope,
	}
}

func (v skillView) list() ([]skill.Summary, error) {
	return v.registry.List(v.options())
}

func (v skillView) get(name string) (*skill.Definition, error) {
	return v.registry.Get(name, v.options())
}

func (v skillView) snapshot() (skill.CatalogSnapshot, error) {
	return v.registry.Snapshot(v.options())
}

// LoadedSkill is the tool's output value: the identity, optional resource
// base, and full instructions of one loaded skill.
type LoadedSkill struct {
	Name         string              `json:"name"`
	Provider     string              `json:"provider"`
	ResourceBase *skill.ResourceBase `json:"resourceBase,omitempty"`
	Content      string              `json:"content"`
}
