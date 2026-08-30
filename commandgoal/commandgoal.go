// Human-facing `/goal` command over the persisted same-session goal domain
// (official dsh-command-goal index.ts).
package commandgoal

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"dshgo/commands"
	"dshgo/goal"
	"dshgo/llm"
)

// Usage is the grammar hint shared by every usage-bearing result.
const Usage = "Usage: /goal [<objective>|clear|edit <objective>|pause|resume]"

// goalCommand kinds: the closed grammar owned by `/goal`.
const (
	kindShow        = "show"
	kindCreate      = "create"
	kindEdit        = "edit"
	kindInvalidEdit = "invalid-edit"
	kindPause       = "pause"
	kindResume      = "resume"
	kindClear       = "clear"
)

type goalCommand struct {
	kind      string
	objective string
}

// parseGoalCommand parses only the grammar owned by `/goal`; arbitrary
// other input is an objective.
func parseGoalCommand(rawInput string) goalCommand {
	input := strings.TrimSpace(rawInput)
	if len(input) == 0 {
		return goalCommand{kind: kindShow}
	}
	control := strings.ToLower(input)
	switch control {
	case "clear":
		return goalCommand{kind: kindClear}
	case "pause":
		return goalCommand{kind: kindPause}
	case "resume":
		return goalCommand{kind: kindResume}
	case "edit":
		return goalCommand{kind: kindInvalidEdit}
	}
	// The official lookahead: "edit" followed by whitespace, any case.
	if len(input) > 4 && strings.EqualFold(input[:4], "edit") && unicode.IsSpace(rune(input[4])) {
		return goalCommand{kind: kindEdit, objective: strings.TrimSpace(input[4:])}
	}
	return goalCommand{kind: kindCreate, objective: input}
}

// phaseLabel renders the human label for one durable goal phase.
func phaseLabel(phase goal.GoalPhase) string {
	switch phase {
	case goal.PhaseActive, goal.PhasePaused, goal.PhaseBlocked, goal.PhaseComplete:
		return string(phase)
	default:
		panic(fmt.Sprintf("unknown goal phase: %s", phase))
	}
}

// commandHint lists the commands that are meaningful from one exact live
// state.
func commandHint(view *goal.GoalView) string {
	if view.Phase == goal.PhaseActive {
		if view.Activation == goal.ActivationArmed {
			return "/goal edit <objective>, /goal pause, /goal clear"
		}
		return "/goal edit <objective>, /goal resume, /goal clear"
	}
	switch view.Phase {
	case goal.PhasePaused, goal.PhaseBlocked:
		return "/goal edit <objective>, /goal resume, /goal clear"
	case goal.PhaseComplete:
		return "/goal <objective>, /goal clear"
	default:
		panic(fmt.Sprintf("unknown goal phase: %s", view.Phase))
	}
}

// renderGoal renders direct UI output without exposing compare-and-set
// internals.
func renderGoal(title string, view *goal.GoalView) commands.CommandResult {
	reason := view.BlockedReason
	if view.Phase == goal.PhaseBlocked && reason == nil {
		panic("blocked goal is missing its reason")
	}
	lines := []string{
		title,
		fmt.Sprintf("Status: %s", phaseLabel(view.Phase)),
	}
	if reason != nil {
		lines = append(lines, fmt.Sprintf("Blocker: %s: %s", reason.Code, reason.Message))
	}
	lines = append(lines,
		fmt.Sprintf("Objective: %s", view.Objective),
		fmt.Sprintf("Rounds: %d/%d", view.RoundsStarted, view.MaxGoalRounds),
		fmt.Sprintf("Activation: %s", view.Activation),
		"",
		fmt.Sprintf("Commands: %s", commandHint(view)),
	)
	return commands.CommandResult{Kind: commands.ResultSuccess, Text: strings.Join(lines, "\n")}
}

// goalRefOf is the exact current compare-and-set ref.
func goalRefOf(view *goal.GoalView) goal.GoalRef {
	return goal.GoalRef{ID: view.ID, Revision: view.Revision}
}

// missingGoal renders the direct error for an operation that requires a
// current goal.
func missingGoal(action string) commands.CommandResult {
	return commands.CommandResult{
		Kind: commands.ResultError,
		Text: fmt.Sprintf("No goal is currently set; /goal %s requires one. %s", action, Usage),
	}
}

func errorResult(text string) commands.CommandResult {
	return commands.CommandResult{Kind: commands.ResultError, Text: text}
}

func successResult(text string) commands.CommandResult {
	return commands.CommandResult{Kind: commands.ResultSuccess, Text: text}
}

// isGoalError recognizes one goal boundary error (the official GoalError
// class): a harness error carrying one of the stable GOAL_* codes.
func isGoalError(err error) bool {
	var harnessErr *llm.Error
	return errors.As(err, &harnessErr) && strings.HasPrefix(harnessErr.Code(), "GOAL_")
}

// submitObjectiveAttachments submits the invocation's admitted composer
// images as one model-visible user message ahead of the goal's next round.
// The images precede a fixed text block naming their role, so a later goal
// round reads them from ordinary session history without the goal domain
// storing attachment state.
func submitObjectiveAttachments(invocation commands.Invocation) error {
	if len(invocation.Attachments) == 0 {
		return nil
	}
	content := make([]llm.ContentBlock, 0, len(invocation.Attachments)+1)
	for _, attachment := range invocation.Attachments {
		if attachment.Block != nil {
			content = append(content, *attachment.Block)
		}
	}
	content = append(content, llm.ContentBlock{Type: llm.BlockText, Text: "Reference images for the goal objective."})
	driver := invocation.Agent.Driver()
	if driver == nil {
		return fmt.Errorf("agent %q has no driver installed", invocation.Agent.ID)
	}
	driver.Followup(llm.NewUserMessage(content, llm.MessageSource{Kind: llm.SourceUser}))
	return nil
}

// executeGoalCommand executes one parsed human command through the domain
// that owns persistence. Any goal boundary error renders the official
// state-invalid wording; every other failure propagates.
func executeGoalCommand(goals *goal.Service, invocation commands.Invocation) (commands.CommandResult, error) {
	command := parseGoalCommand(invocation.RawInput)
	if len(invocation.Attachments) > 0 && command.kind != kindCreate && command.kind != kindEdit {
		return errorResult("Image attachments only accompany a goal objective: /goal <objective> or /goal edit <objective>."), nil
	}
	result, err := runGoalCommand(goals, invocation, command)
	if err != nil {
		if isGoalError(err) {
			return errorResult("The goal command is not valid for the current state. Run /goal to view available commands."), nil
		}
		return commands.CommandResult{}, err
	}
	return result, nil
}

func runGoalCommand(goals *goal.Service, invocation commands.Invocation, command goalCommand) (commands.CommandResult, error) {
	agentRef := invocation.Agent
	current, err := goals.Get(agentRef)
	if err != nil {
		return commands.CommandResult{}, err
	}
	switch command.kind {
	case kindShow:
		if current == nil {
			return successResult(fmt.Sprintf("No goal is currently set.\n%s", Usage)), nil
		}
		return renderGoal("Goal", current), nil
	case kindInvalidEdit:
		return errorResult(fmt.Sprintf("Goal editing requires a replacement objective.\n%s", Usage)), nil
	case kindCreate:
		if current != nil && current.Phase != goal.PhaseComplete {
			return errorResult(fmt.Sprintf(
				"A goal is already %s. Use /goal edit <objective> to change it or /goal clear before replacing it.",
				phaseLabel(current.Phase))), nil
		}
		created, err := goals.Create(agentRef, goal.CreateGoalRequest{Objective: command.objective})
		if err != nil {
			return commands.CommandResult{}, err
		}
		if err := submitObjectiveAttachments(invocation); err != nil {
			return commands.CommandResult{}, err
		}
		return renderGoal("Goal created", created), nil
	case kindEdit:
		if current == nil {
			return missingGoal("edit"), nil
		}
		if current.Phase == goal.PhaseComplete {
			replaced, err := goals.Create(agentRef, goal.CreateGoalRequest{Objective: command.objective})
			if err != nil {
				return commands.CommandResult{}, err
			}
			if err := submitObjectiveAttachments(invocation); err != nil {
				return commands.CommandResult{}, err
			}
			return renderGoal("Goal created", replaced), nil
		}
		edited, err := goals.Edit(agentRef, goalRefOf(current), goal.EditGoalRequest{Objective: &command.objective})
		if err != nil {
			return commands.CommandResult{}, err
		}
		if err := submitObjectiveAttachments(invocation); err != nil {
			return commands.CommandResult{}, err
		}
		return renderGoal("Goal updated", edited), nil
	case kindPause:
		if current == nil {
			return missingGoal("pause"), nil
		}
		paused, err := goals.Pause(agentRef, goalRefOf(current))
		if err != nil {
			return commands.CommandResult{}, err
		}
		return renderGoal("Goal paused", paused), nil
	case kindResume:
		if current == nil {
			return missingGoal("resume"), nil
		}
		resumed, err := goals.Resume(agentRef, goalRefOf(current))
		if err != nil {
			return commands.CommandResult{}, err
		}
		return renderGoal("Goal resumed", resumed), nil
	case kindClear:
		if current == nil {
			return successResult("No goal to clear."), nil
		}
		if _, err := goals.Clear(agentRef, goalRefOf(current)); err != nil {
			return commands.CommandResult{}, err
		}
		return successResult("Goal cleared."), nil
	default:
		panic(fmt.Sprintf("unknown goal command: %s", command.kind))
	}
}

// Register installs the Codex-shaped `/goal` command onto the composed
// command runtime; the returned disposer unregisters it.
func Register(runtime *commands.CommandRuntime, goals *goal.Service) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("command-goal: a command runtime is required")
	}
	if goals == nil {
		return nil, fmt.Errorf("command-goal: a goal service is required")
	}
	return runtime.Register(nil, commands.CommandDefinition{
		Name:        "goal",
		Description: "set or view the goal for a long-running task",
		Input:       &commands.CommandInputDescriptor{Hint: "[<objective>|clear|edit <objective>|pause|resume]", Images: true},
		Handler: func(invocation commands.Invocation) (commands.CommandResult, error) {
			return executeGoalCommand(goals, invocation)
		},
	})
}
