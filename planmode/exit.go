package planmode

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/interaction/userquestions"
	"dshgo/llm"
	"dshgo/tools"
)

// The exit tool and the /plan command (official plan-mode index.ts): while
// plan mode is active, `exit_plan_mode` presents the completed plan for user
// review, and `/plan off` lets a user leave directly. The exit tool remains
// registered while plan mode is inactive, so entering or leaving plan mode
// changes only the prompt section, not the request tool catalog.

// ExitToolName is the model-facing exit tool's name. It stays registered
// while plan mode is inactive so the request tool catalog is stable across
// transitions.
const ExitToolName = "exit_plan_mode"

// reviewID is the review question's id, echoed in the answer the tool reads.
const reviewID = "plan-review"

// approveLabel and keepPlanningLabel are the review question's options.
const (
	approveLabel      = "Approve"
	keepPlanningLabel = "Keep planning"
)

const exitDescription = "Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. " +
	"Send the COMPLETE plan as markdown, starting with a # heading that names it. " +
	"The user may approve (carry out the plan from your next step) or keep " +
	"planning — their feedback comes back in the tool result; revise and present again."

// planHeadingRequirement rejects a plan without a leading markdown heading.
var planHeadingRequirement = regexp.MustCompile(`^#\s+\S`)

// RegisterPlanCommand registers `/plan` on a command runtime. The handler
// steers through the receiving agent's next-step inbox, mirroring
// agent.steer.
func RegisterPlanCommand(runtime *commands.CommandRuntime, controller *Controller) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("planmode: a command runtime is required")
	}
	if controller == nil {
		return nil, fmt.Errorf("planmode: a plan mode controller is required")
	}
	return runtime.Register(nil, commands.CommandDefinition{
		Name:        "plan",
		Description: "Enter or leave plan mode",
		Input:       &commands.CommandInputDescriptor{Hint: "[off|message]", Images: true},
		Handler: func(invocation commands.Invocation) (commands.CommandResult, error) {
			agentObj := invocation.Agent
			if agentObj == nil {
				return commands.CommandResult{}, fmt.Errorf("/plan requires a receiving agent")
			}
			message := strings.TrimSpace(invocation.RawInput)
			if message == "off" && len(invocation.Attachments) > 0 {
				return errorResult("Image attachments cannot accompany /plan off."), nil
			}
			if message == "off" {
				outcome, err := controller.Set(agentObj, false)
				if err != nil {
					return commands.CommandResult{}, err
				}
				switch outcome {
				case OutcomeCommitted:
					return successResult("Plan mode off."), nil
				case OutcomeQueued:
					return successResult("Leaving plan mode (applies from the next step)."), nil
				case OutcomeCancelled:
					return successResult("Plan mode entry cancelled."), nil
				case OutcomeNoop:
					// Repeat the queued wording while an exit still awaits
					// the next accepted pre-step; only a truly inactive
					// session reads idempotent.
					if FoldPlanMode(agentObj.Session.Events(), -1) {
						return successResult("Leaving plan mode (applies from the next step)."), nil
					}
					return successResult("Plan mode is already inactive."), nil
				}
			}
			outcome, err := controller.Set(agentObj, true)
			if err != nil {
				return commands.CommandResult{}, err
			}
			if message != "" || len(invocation.Attachments) > 0 {
				content := make([]llm.ContentBlock, 0, len(invocation.Attachments)+1)
				for _, attachment := range invocation.Attachments {
					if attachment.Block != nil {
						content = append(content, *attachment.Block)
					}
				}
				if message != "" {
					content = append(content, llm.ContentBlock{Type: llm.BlockText, Text: message})
				}
				if appendErr := agentObj.Inbox.Append(agent.InboxNextStep, llm.NewUserMessage(content, llm.MessageSource{Kind: llm.SourceUser})); appendErr != nil {
					agentObj.Ctx.Logger().Warn(fmt.Sprintf("plan-mode: the entry input for agent %q was not delivered: %v", agentObj.ID, appendErr))
				}
			}
			if outcome == OutcomeCommitted {
				return successResult("Plan mode on. Use /plan off to leave."), nil
			}
			return successResult("Entering plan mode (applies from the next step). Use /plan off to leave."), nil
		},
	})
}

func errorResult(text string) commands.CommandResult {
	return commands.CommandResult{Kind: commands.ResultError, HasText: true, Text: text}
}

func successResult(text string) commands.CommandResult {
	return commands.CommandResult{Kind: commands.ResultSuccess, HasText: true, Text: text}
}

// RegisterExitTool defines the exit_plan_mode tool on the runtime (global
// registration). It returns the tool's registration disposer.
func RegisterExitTool(runtime *tools.ToolRuntime, questions *userquestions.Service, controller *Controller) (func(), error) {
	closedObject := func() *bool { value := false; return &value }
	if runtime == nil {
		return nil, fmt.Errorf("planmode: a tool runtime is required")
	}
	if controller == nil {
		return nil, fmt.Errorf("planmode: a plan mode controller is required")
	}
	// A review outliving its registration fails the keep-planning way; the
	// disposer flips this before unregistering.
	var disposed atomic.Bool
	tool, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        ExitToolName,
		Description: exitDescription,
		Parameters: map[string]tools.PropSpec{
			"plan": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "The complete plan, as markdown, starting with a # heading that names it.",
			}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"approved": {ValueSchemaSpec: tools.ValueSchemaSpec{
						Type:  "boolean",
						Const: true,
					}, Required: true},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText,
					Text: "Plan approved — plan mode exited; carry out the plan starting with your next step."}}
			},
		},
		PresentationMeta: func(args map[string]any, _ any) any {
			heading := FirstHeading(planText(args))
			title := heading
			if title == "" {
				// The official `?? 'Plan'` mapping: a heading-less plan still
				// names its card.
				title = "Plan"
			}
			return map[string]any{
				"card":    "generic",
				"title":   title,
				"kind":    "other",
				"content": []map[string]any{{"type": "text", "text": planText(args)}},
			}
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			if questions == nil {
				return nil, fmt.Errorf("no user-questions channel is available to review the plan; ask the user to switch the session mode instead")
			}
			agentObj := questions.AgentByScope(exec.Agent)
			if agentObj == nil {
				return nil, fmt.Errorf("%s requires a calling agent (no session to switch)", ExitToolName)
			}
			if !FoldPlanMode(agentObj.Session.Events(), -1) {
				return nil, fmt.Errorf("%s is only available in plan mode", ExitToolName)
			}
			plan := planText(args)
			if !planHeadingRequirement.MatchString(strings.TrimSpace(plan)) {
				return nil, fmt.Errorf("%s requires a non-empty markdown plan starting with a # heading", ExitToolName)
			}
			approve := approveLabel
			answer, err := questions.Ask(userquestions.Request{
				Questions: []userquestions.AskUserQuestionItem{{
					ID:       reviewID,
					Header:   "Plan review",
					Question: "Approve this plan and leave plan mode?",
					Detail:   plan,
					Options: []userquestions.AskUserQuestionOption{
						{Label: approveLabel, Description: "Leave plan mode; the plan is carried out from the next step."},
						{Label: keepPlanningLabel, Description: "Stay in plan mode; feedback goes back to the model."},
					},
					// Presentation only: a capable UI renders the plan as a
					// review decision instead of a generic question, and
					// answers with one of the labels above either way.
					Intent: &userquestions.AskUserQuestionIntent{Kind: "plan-review", Approve: approve},
				}},
				Agent:  agentObj,
				Signal: exec.Signal,
			})
			if err != nil {
				var questionErr userquestions.UserQuestionError
				// A dismissed review is not a failed one: the user took the
				// turn back to say something the two options do not cover.
				// An abort (turn cancel, provider teardown) keeps its own
				// message — there is no user to wait for.
				if errors.As(err, &questionErr) && questionErr.Code() == userquestions.CodeAskAborted {
					return nil, fmt.Errorf("The user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message.")
				}
				return nil, err
			}
			// A review may outlive this registration. Without its pre-step
			// listener, an approved selection could never be appended, so
			// fail and keep planning.
			if disposed.Load() {
				return nil, fmt.Errorf("the plan-mode service was reloaded while the plan was under review; present the plan again")
			}
			reviews := 0
			var item *userquestions.AskUserQuestionAnswerItem
			for index := range answer.Answers {
				if answer.Answers[index].ID == reviewID {
					reviews++
					item = &answer.Answers[index]
				}
			}
			if reviews != 1 || len(item.Selected) != 1 || item.Selected[0] != approveLabel || item.Custom != "" {
				// The official feedback rides the custom text only; declined
				// labels carry no prose.
				if item == nil || item.Custom == "" {
					return nil, fmt.Errorf("The user chose to keep planning; revise the plan and present it again.")
				}
				return nil, fmt.Errorf("The user chose to keep planning; their feedback: %s", item.Custom)
			}
			controller.QueueExit(agentObj.Session)
			return map[string]any{"approved": true}, nil
		},
		IsConcurrencySafe: func(map[string]any) bool { return false },
	})
	if err != nil {
		return nil, err
	}
	dispose, err := runtime.Register(tool)
	if err != nil {
		return nil, err
	}
	return func() {
		disposed.Store(true)
		dispose()
	}, nil
}

// planText reads the tool's plan argument.
func planText(args map[string]any) string {
	plan, _ := args["plan"].(string)
	return plan
}
