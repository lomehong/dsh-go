package toolralph

import (
	"encoding/json"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/subagent"
	"dshgo/systemprompt"
	"dshgo/tools"
	"dshgo/workflow"
)

// Description is the model-facing tool description (verbatim).
const Description = "Run a foreground fresh-agent Ralph loop toward one immutable objective. " +
	"Use only when the direct human explicitly asks for Ralph or fresh-agent iteration. Each round " +
	"opens a new child with no parent conversation or prior child session; the shared workspace is " +
	"long-term memory, and only a bounded structured report crosses rounds. The call returns when " +
	"a worker reports completion or a concrete blocker, or at the round limit. Ordinary long-running same-session work " +
	"belongs to goal tools."

// SectionText is the explicit-ask usage policy (verbatim).
const SectionText = "Use the ralph tool ONLY when the direct human explicitly asks for a Ralph loop or fresh-agent iterative execution. Each Ralph round starts a fresh child with no conversation seed and uses the shared workspace as durable memory. Completion and blockers are worker reports, not independent evaluation. Use same-session goal tools for ordinary long-running objectives, and plain subagents or workflows for bounded delegation and fan-out."

// truncationNotice is the bounded-text truncation marker.
const truncationNotice = "\n… [truncated]"

// boundResult bounds complete parent-facing text, including its envelope
// and truncation marker.
func boundResult(text string, maxChars int64) string {
	if int64(len(text)) <= maxChars {
		return text
	}
	if maxChars <= int64(len(truncationNotice)) {
		return truncationNotice[:maxChars]
	}
	return text[:maxChars-int64(len(truncationNotice))] + truncationNotice
}

// mustJSON renders the final report envelope; report values are plain JSON
// data and cannot fail to encode.
func mustJSON(report RalphRoundReport) string {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Sprintf("%%!unrenderable report: %v", err)
	}
	return string(encoded)
}

// RenderResult renders the fixed terminal envelope without presenting
// self-report as certification.
func RenderResult(result RunResult, maxChars int64) string {
	rounds := fmt.Sprintf("%d round", result.RoundsStarted)
	if result.RoundsStarted != 1 {
		rounds += "s"
	}
	var text string
	switch result.Status {
	case RunComplete:
		text = fmt.Sprintf("Ralph worker reported completion after %s.\nFinal report:\n%s", rounds, mustJSON(result.Report))
	case RunBlocked:
		text = fmt.Sprintf("Ralph worker reported a blocker after %s.\nFinal report:\n%s", rounds, mustJSON(result.Report))
	case RunBudgetLimited:
		text = fmt.Sprintf("Ralph reached its %s limit; the worker reported work remaining.\nFinal report:\n%s", rounds, mustJSON(result.Report))
	}
	return boundResult(text, maxChars)
}

// RenderRoundFailure renders an ordinary child failure with the most
// recent durable handoff.
func RenderRoundFailure(result RoundFailure, maxChars int64) string {
	header := fmt.Sprintf("Ralph round %d child failed before producing a structured report.", result.RoundsStarted)
	var text string
	if result.LastReport == nil {
		text = header + "\nNo previous handoff was available."
	} else {
		text = header + "\nLast successful handoff:\n" + mustJSON(*result.LastReport)
	}
	return boundResult(text, maxChars)
}

// requireFreshProvider requires the configured route to mean a genuinely
// fresh structured child (the official execute-time gate).
func requireFreshProvider(subagents *subagent.SubagentRuntime, name string) error {
	provider, ok := subagents.GetProvider(name)
	if !ok {
		return fmt.Errorf("Ralph subagent provider %q is not registered", name)
	}
	capabilities := provider.Capabilities()
	if !capabilities.OutputSchema {
		return fmt.Errorf("Ralph subagent provider %q does not support structured output", name)
	}
	if provider.InheritsParentContext() {
		return fmt.Errorf("Ralph subagent provider %q inherits parent context; Ralph requires a fresh provider", name)
	}
	return nil
}

// outputSchema is the canonical Ralph result fields (runId/agentsStarted/
// result), shared by schema and rendering.
func outputSchema() *tools.ValueSchemaSpec {
	object := false
	return &tools.ValueSchemaSpec{
		Type: "object",
		Properties: map[string]tools.PropSpec{
			"runId":         {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			"agentsStarted": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
			"result":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "json"}, Required: true},
		},
		AdditionalProperties: &object,
	}
}

// Register defines and registers the fixed Ralph tool and its
// explicit-ask usage policy. The engine and subagent runtime seams come
// from the composition (the official inject
// ['tools', 'workflowEngine', 'subagents', 'systemPrompt']).
func Register(toolRuntime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, agents *agent.AgentRegistry, engine workflow.Engine, subagents *subagent.SubagentRuntime, config Config) (func(), error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	undoSection, err := prompt.Section(nil, systemprompt.PromptSection{
		Name:  "tool:ralph",
		Order: systemprompt.OrderToolRalph,
		TextProvider: func(systemprompt.AssembleContext) string {
			return SectionText
		},
	})
	if err != nil {
		return nil, err
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "ralph",
		Description: Description,
		Parameters: map[string]tools.PropSpec{
			"objective": {
				ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The immutable completion objective for every fresh Ralph round."},
				Required:        true,
			},
			"maxRounds": {
				ValueSchemaSpec: tools.ValueSchemaSpec{Type: "number", Description: "Optional positive safe-integer round cap, bounded by the deployment ceiling."},
			},
		},
		Output: tools.ToolOutput{
			Schema: outputSchema(),
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				encoded, _ := json.Marshal(value)
				var rendered struct {
					Result RunResult `json:"result"`
				}
				_ = json.Unmarshal(encoded, &rendered)
				return []llm.ContentBlock{{Type: llm.BlockText, Text: RenderResult(rendered.Result, resolved.MaxResultChars)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			var parent *agent.Agent
			if exec.Agent != nil {
				if resolvedAgent := agents.ByScope(exec.Agent); resolvedAgent != nil {
					parent = resolvedAgent
				}
			}
			if parent == nil {
				return nil, fmt.Errorf("Ralph tool requires a calling agent (exec.agent was undefined)")
			}
			rawObjective, _ := args["objective"].(string)
			objective := strings.TrimSpace(rawObjective)
			if objective == "" {
				return nil, fmt.Errorf("Ralph objective must be a non-empty string")
			}
			var requested *int64
			if raw, ok := args["maxRounds"].(float64); ok {
				value := int64(raw)
				requested = &value
			}
			maxRounds, err := ResolveMaxRounds(requested, resolved.MaxRounds)
			if err != nil {
				return nil, err
			}
			if err := requireFreshProvider(subagents, resolved.SubagentProvider); err != nil {
				return nil, err
			}
			run, err := engine.Start(workflow.StartRequest{
				Program:          Program(objective, maxRounds, resolved.MaxHandoffChars),
				Meta:             RalphMeta(),
				SubagentProvider: resolved.SubagentProvider,
				MaxTotalAgents:   &maxRounds,
				Parent:           parent,
				Signal:           exec.Signal,
			})
			if err != nil {
				return nil, err
			}
			settled := <-run.Result()
			if settled.StopReason != workflow.StopReasonCompleted {
				return nil, fmt.Errorf("Ralph workflow failed: %s", settled.Error)
			}
			value, err := DecodeRunResult(settled.Value, maxRounds, resolved.MaxHandoffChars)
			if err != nil {
				return nil, err
			}
			if value.Status == RunBudgetLimited && value.RoundsStarted != maxRounds {
				return nil, fmt.Errorf("Ralph workflow returned budget-limited before the round limit")
			}
			return map[string]any{
				"runId":         string(run.ID()),
				"agentsStarted": settled.AgentsStarted,
				"result":        value,
			}, nil
		},
	})
	if err != nil {
		undoSection()
		return nil, err
	}
	undoTool, err := toolRuntime.Register(definition)
	if err != nil {
		undoSection()
		return nil, err
	}
	return func() {
		undoTool()
		undoSection()
	}, nil
}

// Compile-time interface checks for the seams this package consumes.
var (
	_ = func(engine workflow.Engine) {}
	_ agent.Agent
	_ = subagent.SubagentRuntime{}
)
