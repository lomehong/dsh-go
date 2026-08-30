// Package toolsubagentreport ports @deepseek-ai/dsh-tool-subagent-report:
// the child-scoped `report` tool and its usage guidance, installed into
// every continuable in-process child's unpublished creation context. Roots,
// one-shot children, remote providers, and agentless executions never see
// the registration.
package toolsubagentreport

import (
	"fmt"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/subagent"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// ReportDelivery is how accepted reports are scheduled on the parent.
type ReportDelivery = subagent.SubagentReportDelivery

// Config is the deployment scheduling policy.
type Config struct {
	// ReportDelivery is the parent scheduling (default next-step):
	// next-step wakes the parent and enters at its nearest step boundary;
	// quiet adds the same context without waking, so a parked parent waits
	// for another waking input.
	ReportDelivery subagent.SubagentReportDelivery `json:"reportDelivery,omitempty"`
}

// ResolveConfig applies the default and validates the enum.
func ResolveConfig(config Config) (Config, error) {
	switch config.ReportDelivery {
	case "":
		config.ReportDelivery = subagent.DeliveryNextStep
	case subagent.DeliveryQuiet, subagent.DeliveryNextStep:
	default:
		return Config{}, fmt.Errorf("tool-subagent-report: reportDelivery must be \"quiet\" or \"next-step\" (got %q)", string(config.ReportDelivery))
	}
	return config, nil
}

// Deps carries the services the contribution composes. ResolveAgent maps an
// executing scope key to the live child agent at report time.
type Deps struct {
	Subagents    *subagent.SubagentRuntime
	Tools        *tools.ToolRuntime
	Prompt       *systemprompt.SystemPrompt
	ResolveAgent func(tools.ScopeKey) *agent.Agent
}

// Register validates the composition and contributes the report tool to
// every later continuable child. The returned disposer revokes the
// contribution; children already carrying the installation revoke it with
// their own disposal.
func Register(deps Deps, config Config) (func(), error) {
	if deps.Subagents == nil || deps.Tools == nil || deps.Prompt == nil || deps.ResolveAgent == nil {
		return nil, fmt.Errorf("tool-subagent-report: the subagent runtime, tool runtime, system prompt, and agent resolver are required")
	}
	config, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	undo, err := deps.Subagents.SetupRegistry().Register(func(childCtx *cordis.Context) func() {
		return installReportTool(agentFromContext(childCtx), deps, config.ReportDelivery)
	})
	if err != nil {
		return nil, err
	}
	return undo, nil
}

// installReportTool installs `report` and its usage guidance into one
// continuable child's scope. Both registrations are owned by that scope and
// are therefore invisible to the child's parent and siblings. A nil child
// or registration failure panics so the setup registry rolls the child
// creation back (the official contribution throws).
func installReportTool(child *agent.Agent, deps Deps, delivery subagent.SubagentReportDelivery) func() {
	if child == nil {
		panic(fmt.Errorf("tool-subagent-report: the continuable child context carries no agent"))
	}
	scope := child.Scope
	sectionUndo, err := deps.Prompt.Section(scope, systemprompt.PromptSection{
		Name:  "tool:report",
		Order: systemprompt.OrderToolReport,
		Text: "Deliver your result with the report tool before you finish: call it once with a self-contained " +
			"answer. The agent that started you shares your workspace but does not automatically receive your " +
			"transcript, tool output, or reasoning, so a closing remark such as \"done\" leaves it nothing it can " +
			"use. Report earlier as well whenever a partial finding changes what that agent should do next; " +
			"reporting never ends your turn.",
	})
	if err != nil {
		panic(fmt.Errorf("tool-subagent-report: %w", err))
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "report",
		Description: "Report selected content to the agent that started you. Call this once before you finish, with a " +
			"self-contained final result, and earlier for progress or findings that change what that agent does " +
			"next. That agent shares your workspace but does not automatically receive your transcript, tool " +
			"output, or reasoning, so finishing your work is not itself a result. Reporting does not end your " +
			"turn or finish your work, and only your direct parent receives it. A failed call may still have " +
			"arrived, so do not blindly repeat it.",
		Parameters: map[string]tools.PropSpec{
			"output": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Actionable content for your parent; summarize conclusions and reference relevant shared paths.",
			}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"messageId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: renderValue(value)}}
			},
		},
		// The report appends one message to the parent's inbox; calls are
		// independent insertions.
		IsConcurrencySafe: func(args map[string]any) bool { return true },
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			output, _ := args["output"].(string)
			if output == "" {
				return nil, fmt.Errorf("output is required")
			}
			caller := deps.ResolveAgent(exec.Agent)
			if caller == nil {
				return nil, fmt.Errorf("the report tool requires a calling agent")
			}
			content := []llm.ContentBlock{{Type: llm.BlockText, Text: output}}
			messageID, err := deps.Subagents.ReportFrom(caller, content, subagent.SubagentReportOptions{
				Delivery: delivery,
				Signal:   exec.Signal,
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"messageId": string(messageID)}, nil
		},
	})
	if err != nil {
		sectionUndo()
		panic(fmt.Errorf("tool-subagent-report: %w", err))
	}
	toolUndo, err := deps.Tools.RegisterIn(scope, definition)
	if err != nil {
		sectionUndo()
		panic(fmt.Errorf("tool-subagent-report: %w", err))
	}
	return func() {
		// Both registrations are scope-owned; their disposers cannot fail.
		toolUndo()
		sectionUndo()
	}
}

// agentFromContext reads the child agent materializing in this context.
func agentFromContext(childCtx *cordis.Context) *agent.Agent {
	built, _ := agent.ContextService.From(childCtx)
	return built
}

func boolPtr(b bool) *bool { return &b }

// renderValue projects the canonical value into model-facing text.
func renderValue(value any) string {
	outcome, ok := value.(map[string]any)
	if !ok {
		return fmt.Sprintf("%v", value)
	}
	return fmt.Sprintf("report accepted by the agent that started you as message %v", outcome["messageId"])
}
