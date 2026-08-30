// Package toolgoal is the model-facing get_goal, create_goal, and
// update_goal tools over the persisted same-session goal domain (official
// @deepseek-ai/dsh-tool-goal). Go adaptations: the official `exec.agent`
// object is re-resolved through AgentRegistry.ByScope over the execution's
// ScopeKey, and `ctx.agents.currentInitiator()` is the initiator context
// value read from exec.Signal (the whole driver chain runs inside
// AgentRegistry.WithInitiator). The official `presentCall` pending
// presentation has no Go registry seam — only the replayable
// PresentationMeta exists with different semantics — so it is omitted. The
// schemastery Config is a plain struct validated in ResolveConfig.
package toolgoal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Name is the plugin identity.
const Name = "tool-goal"

// Error codes for argument-shape and policy fences.
const (
	CodeGoalToolInvalidUpdate  = "GOAL_TOOL_INVALID_UPDATE"
	CodeGoalToolBlockThreshold = "GOAL_TOOL_BLOCK_THRESHOLD"
)

// Config is the model policy and hard lower bounds for goal-state updates.
type Config struct {
	// BlockedAfterConsecutiveRounds is the minimum admitted goal rounds
	// before the model may self-report `blocked`; nil means the default 3.
	BlockedAfterConsecutiveRounds *int64
}

// DefaultBlockedAfterConsecutiveRounds is the official Config default.
const DefaultBlockedAfterConsecutiveRounds = 3

// updateAction is one model-requested goal mutation.
type updateAction string

// The update actions.
const (
	actionEdit     updateAction = "edit"
	actionPause    updateAction = "pause"
	actionResume   updateAction = "resume"
	actionComplete updateAction = "complete"
	actionBlocked  updateAction = "blocked"
)

const createDescription = "Create one persisted same-session completion goal when the current direct human request " +
	"is a long-running objective that should continue across autonomous goal rounds. You may " +
	"infer that intent without requiring the user to say \"create a goal\". Do not use this for " +
	"trivial single-turn work. Execution rejects non-human and subagent authority."

const getDescription = "Read the current same-session goal, including its exact id/revision, objective, phase, completed " +
	"continuation rounds, round limit, blocker reason when present, and whether another continuation is armed. " +
	"Call this before updating a goal."

const updateDescription = "Update the exact current goal revision. edit, pause, and resume require a direct " +
	"top-level human request. During an automatic continuation of the current goal, complete " +
	"and blocked are also allowed. blocked is rejected before the configured minimum round count; the model remains " +
	"responsible for judging that the same condition persisted across those rounds and must explain it in blocked_reason."

// GoalToolBlockReasonValue is the model-facing blocker shape.
type GoalToolBlockReasonValue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// GoalToolGoalValue is the model-facing goal shape.
type GoalToolGoalValue struct {
	ID            string                    `json:"id"`
	Revision      int64                     `json:"revision"`
	Objective     string                    `json:"objective"`
	Phase         goal.GoalPhase            `json:"phase"`
	RoundsStarted int64                     `json:"roundsStarted"`
	MaxGoalRounds int64                     `json:"maxGoalRounds"`
	BlockedReason *GoalToolBlockReasonValue `json:"blockedReason,omitempty"`
}

// GoalToolValue is the canonical goal-tool output, matching the existing
// compact Native JSON.
type GoalToolValue struct {
	// Goal is nil exactly when no goal exists (renders `"goal": null`).
	Goal *GoalToolGoalValue `json:"goal"`
	// Activation rides only with an existing goal.
	Activation goal.GoalActivation `json:"activation,omitempty"`
}

func goalValueSchema() *tools.ValueSchemaSpec {
	additionalFalse := false
	return &tools.ValueSchemaSpec{
		OneOf: []*tools.ValueSchemaSpec{
			{
				Type:                 "object",
				AdditionalProperties: &additionalFalse,
				Properties: map[string]tools.PropSpec{
					"goal": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "null"}, Required: true},
				},
			},
			{
				Type:                 "object",
				AdditionalProperties: &additionalFalse,
				Properties: map[string]tools.PropSpec{
					"goal": {ValueSchemaSpec: tools.ValueSchemaSpec{
						Type:                 "object",
						AdditionalProperties: &additionalFalse,
						Properties: map[string]tools.PropSpec{
							"id":            {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
							"revision":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
							"objective":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
							"phase":         {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"active", "paused", "blocked", "complete"}}, Required: true},
							"roundsStarted": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
							"maxGoalRounds": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
							"blockedReason": {ValueSchemaSpec: tools.ValueSchemaSpec{
								Type:                 "object",
								AdditionalProperties: &additionalFalse,
								Properties: map[string]tools.PropSpec{
									"code":    {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
									"message": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
								},
							}},
						},
					}, Required: true},
					"activation": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"armed", "disarmed"}}, Required: true},
				},
			},
		},
	}
}

// Guidance renders the policy guidance with its deployment-selected blocked
// threshold.
func Guidance(blockedAfter int64) string {
	return "Use goal tools for one long-running completion objective in the current session. " +
		"create_goal may infer goal intent from a direct human request in any language; do not " +
		"create a goal for routine single-turn work. Call get_goal before update_goal and copy its " +
		"exact goal_id and revision. After session resume or fork, an active goal is disarmed: when " +
		"a human asks to continue or resume in any wording or language, use update_goal action " +
		"resume to rearm it. Mark complete only when the objective is actually achieved. Mark " +
		"blocked only after the same blocking condition persists for at least " +
		fmt.Sprintf("%d", blockedAfter) +
		" consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, " +
		"or useful remaining work is not blocked."
}

// ResolveConfig validates the policy even when apply is called directly
// outside Loader normalization.
func ResolveConfig(config Config) (int64, error) {
	blockedAfter := int64(DefaultBlockedAfterConsecutiveRounds)
	if config.BlockedAfterConsecutiveRounds != nil {
		blockedAfter = *config.BlockedAfterConsecutiveRounds
	}
	if blockedAfter < 1 {
		return 0, errors.New("blockedAfterConsecutiveRounds must be a positive safe integer")
	}
	return blockedAfter, nil
}

// hasText reports whether optional text is meaningful rather than a
// strict-schema empty filler.
func hasText(value string) bool {
	return value != ""
}

// hasRoundCap reports whether an optional round cap is meaningful rather
// than a strict-schema zero filler.
func hasRoundCap(value float64, present bool) bool {
	return present && value != 0
}

// goalRef builds the exact compare-and-set ref from model arguments.
func goalRef(goalID string, revision float64, revisionPresent bool) (goal.GoalRef, error) {
	if len(goalID) == 0 || goalID != strings.TrimSpace(goalID) ||
		!revisionPresent || revision != float64(int64(revision)) || revision < 1 {
		return goal.GoalRef{}, llm.NewError(CodeGoalToolInvalidUpdate,
			"goal_id must be non-empty and revision must be a positive safe integer", nil)
	}
	return goal.GoalRef{ID: goal.GoalID(goalID), Revision: int64(revision)}, nil
}

// goalValue renders the stable compact model result; activation is an
// observation, not replay state. The value stays JSON-native so the
// registry's lossless-output check passes untouched.
func goalValue(view *goal.GoalView) any {
	if view == nil {
		return map[string]any{"goal": nil}
	}
	goalMap := map[string]any{
		"id":            string(view.ID),
		"revision":      view.Revision,
		"objective":     view.Objective,
		"phase":         string(view.Phase),
		"roundsStarted": view.RoundsStarted,
		"maxGoalRounds": view.MaxGoalRounds,
	}
	if view.BlockedReason != nil {
		goalMap["blockedReason"] = map[string]any{
			"code":    view.BlockedReason.Code,
			"message": view.BlockedReason.Message,
		}
	}
	return map[string]any{"goal": goalMap, "activation": string(view.Activation)}
}

// goalOutputRender is the shared canonical output rendering: compact JSON.
func goalOutputRender(_ map[string]any, value any) []llm.ContentBlock {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte("{}")
	}
	return []llm.ContentBlock{{Type: llm.BlockText, Text: string(encoded)}}
}

// stringArg reads one optional string argument.
func stringArg(args map[string]any, key string) string {
	if raw, ok := args[key].(string); ok {
		return raw
	}
	return ""
}

// numberArg reads one optional numeric argument.
func numberArg(args map[string]any, key string) (float64, bool) {
	if raw, ok := args[key].(float64); ok {
		return raw, true
	}
	return 0, false
}

// Apply registers the three Codex-shaped goal tools and their shared policy
// section. The prompt service is optional (section skipped without it), the
// rest is required.
func Apply(ctx *cordis.Context, registry *agent.AgentRegistry, goals *goal.Service,
	runtime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, config Config,
) error {
	blockedAfter, err := ResolveConfig(config)
	if err != nil {
		return err
	}
	var undoSection func()
	if prompt != nil {
		undo, sectionErr := prompt.Section(nil, systemprompt.PromptSection{
			Name:  "tool:goal",
			Order: systemprompt.OrderToolGoal,
			Text:  Guidance(blockedAfter),
		})
		if sectionErr != nil {
			return sectionErr
		}
		undoSection = undo
	} else {
		undoSection = func() {}
	}

	undoGet, err := registerGetGoal(runtime, registry, goals)
	if err != nil {
		undoSection()
		return err
	}
	undoCreate, err := registerCreateGoal(runtime, registry, goals)
	if err != nil {
		undoGet()
		undoSection()
		return err
	}
	undoUpdate, err := registerUpdateGoal(runtime, registry, goals, blockedAfter)
	if err != nil {
		undoCreate()
		undoGet()
		undoSection()
		return err
	}
	ctx.Effect(func() (cordis.Disposer, error) {
		return cordis.Disposer(func() {
			undoUpdate()
			undoCreate()
			undoGet()
			undoSection()
		}), nil
	})
	return nil
}

func registerGetGoal(runtime *tools.ToolRuntime, registry *agent.AgentRegistry, goals *goal.Service) (func(), error) {
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "get_goal",
		Description: getDescription,
		Parameters:  map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: goalValueSchema(),
			Render: goalOutputRender,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			execution, execErr := goalToolExecution(registry, exec)
			if execErr != nil {
				return nil, execErr
			}
			view, getErr := goals.Get(execution.Agent)
			if getErr != nil {
				return nil, getErr
			}
			return goalValue(view), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

func registerCreateGoal(runtime *tools.ToolRuntime, registry *agent.AgentRegistry, goals *goal.Service) (func(), error) {
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "create_goal",
		Description: createDescription,
		Parameters: map[string]tools.PropSpec{
			"objective": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "The concrete completion objective inferred from the direct human request.",
			}, Required: true},
			"max_goal_rounds": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "number",
				Description: "Optional positive safe-integer limit on automatic continuation rounds.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: goalValueSchema(),
			Render: goalOutputRender,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			execution, execErr := goalToolExecution(registry, exec)
			if execErr != nil {
				return nil, execErr
			}
			if humanErr := requireDirectHuman(registry, execution); humanErr != nil {
				return nil, humanErr
			}
			request := goal.CreateGoalRequest{Objective: stringArg(args, "objective")}
			if capValue, present := numberArg(args, "max_goal_rounds"); present {
				capInt := int64(capValue)
				request.MaxGoalRounds = &capInt
			}
			view, createErr := goals.Create(execution.Agent, request)
			if createErr != nil {
				return nil, createErr
			}
			return goalValue(view), nil
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

func registerUpdateGoal(runtime *tools.ToolRuntime, registry *agent.AgentRegistry, goals *goal.Service,
	blockedAfter int64,
) (func(), error) {
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "update_goal",
		Description: updateDescription,
		Parameters: map[string]tools.PropSpec{
			"goal_id": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string", Description: "Exact id returned by get_goal.",
			}, Required: true},
			"revision": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "number", Description: "Exact positive revision returned by get_goal.",
			}, Required: true},
			"action": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Enum:        []any{"edit", "pause", "resume", "complete", "blocked"},
				Description: "edit | pause | resume | complete | blocked",
			}, Required: true},
			"objective": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string", Description: "Replacement objective; valid only with action edit.",
			}},
			"max_goal_rounds": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "number", Description: "Replacement cap; valid only with action edit.",
			}},
			"blocked_reason": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string", Description: "Concrete blocking condition; required only with action blocked.",
			}},
		},
		Output: tools.ToolOutput{
			Schema: goalValueSchema(),
			Render: goalOutputRender,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			execution, execErr := goalToolExecution(registry, exec)
			if execErr != nil {
				return nil, execErr
			}
			revisionValue, revisionPresent := numberArg(args, "revision")
			ref, refErr := goalRef(stringArg(args, "goal_id"), revisionValue, revisionPresent)
			if refErr != nil {
				return nil, refErr
			}
			objective := stringArg(args, "objective")
			blockedReason := stringArg(args, "blocked_reason")
			roundCap, capPresent := numberArg(args, "max_goal_rounds")
			var replacements goal.EditGoalRequest
			if hasText(objective) {
				replacement := objective
				replacements.Objective = &replacement
			}
			if hasRoundCap(roundCap, capPresent) {
				capInt := int64(roundCap)
				replacements.MaxGoalRounds = &capInt
			}
			action := updateAction(stringArg(args, "action"))
			switch action {
			case actionEdit:
				if humanErr := requireDirectHuman(registry, execution); humanErr != nil {
					return nil, humanErr
				}
				if hasText(blockedReason) {
					return nil, llm.NewError(CodeGoalToolInvalidUpdate,
						"blocked_reason is valid only with action blocked", nil)
				}
				view, editErr := goals.Edit(execution.Agent, ref, replacements)
				if editErr != nil {
					return nil, editErr
				}
				return goalValue(view), nil
			case actionPause, actionResume:
				if humanErr := requireDirectHuman(registry, execution); humanErr != nil {
					return nil, humanErr
				}
				if hasText(objective) || hasRoundCap(roundCap, capPresent) || hasText(blockedReason) {
					return nil, llm.NewError(CodeGoalToolInvalidUpdate,
						"objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked", nil)
				}
				var (
					view     *goal.GoalView
					stateErr error
				)
				if action == actionPause {
					view, stateErr = goals.Pause(execution.Agent, ref)
				} else {
					view, stateErr = goals.Resume(execution.Agent, ref)
				}
				if stateErr != nil {
					return nil, stateErr
				}
				return goalValue(view), nil
			case actionComplete, actionBlocked:
				authority, authorityErr := completionAuthority(goals, registry, execution)
				if authorityErr != nil {
					return nil, authorityErr
				}
				if hasText(objective) || hasRoundCap(roundCap, capPresent) {
					return nil, llm.NewError(CodeGoalToolInvalidUpdate,
						"objective and max_goal_rounds are valid only with action edit", nil)
				}
				if action == actionComplete && hasText(blockedReason) {
					return nil, llm.NewError(CodeGoalToolInvalidUpdate,
						"blocked_reason is valid only with action blocked", nil)
				}
				if action == actionBlocked && strings.TrimSpace(blockedReason) == "" {
					return nil, llm.NewError(CodeGoalToolInvalidUpdate,
						"blocked_reason is required with action blocked", nil)
				}
				if action == actionBlocked && authority.Kind == authorityGoalRound &&
					authority.Goal.RoundsStarted < blockedAfter {
					return nil, llm.NewError(CodeGoalToolBlockThreshold, fmt.Sprintf(
						"blocked requires at least %d consecutive goal rounds; current round is %d",
						blockedAfter, authority.Goal.RoundsStarted), nil)
				}
				var (
					view     *goal.GoalView
					stateErr error
				)
				if action == actionComplete {
					view, stateErr = goals.Complete(execution.Agent, ref)
				} else {
					view, stateErr = goals.Block(execution.Agent, ref, goal.GoalBlockReason{
						Code:    "model-reported",
						Message: blockedReason,
					})
				}
				if stateErr != nil {
					return nil, stateErr
				}
				if authority.Kind == authorityGoalRound {
					wrapup := RenderWrapupContext(view.Objective, nil)
					if action == actionBlocked {
						wrapup = RenderWrapupContext(view.Objective, &blockedReason)
					}
					exec.DeferContext(llm.NewUserMessage(wrapup, llm.MessageSource{
						Kind:    llm.SourcePlugin,
						Plugin:  Name,
						Form:    llm.FormNotice,
						Summary: llm.BoundContextSummary(string(action) + ": " + view.Objective),
					}))
				}
				return goalValue(view), nil
			}
			// The action enum is schema-validated; anything else is a
			// developer-visible contract break, not a model error.
			return nil, fmt.Errorf("update_goal: unknown action %q", string(action))
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}
