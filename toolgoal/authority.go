package toolgoal

import (
	"encoding/json"

	"dshgo/agent"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// GoalToolExecution is the current open turn plus the events accepted after
// its start boundary.
type GoalToolExecution struct {
	Agent  *agent.Agent
	Start  session.Event
	Events []session.Event
}

// GoalToolAuthority is the hard authority granted to one state-changing call.
type GoalToolAuthority struct {
	// Kind is "direct-human" or "goal-round".
	Kind string
	// Goal carries the exact matching goal for a goal-round grant.
	Goal *goal.GoalView
}

// Authority kinds.
const (
	authorityDirectHuman = "direct-human"
	authorityGoalRound   = "goal-round"
)

// Error codes for the goal-tool policy fences.
const (
	CodeGoalToolAgentRequired     = "GOAL_TOOL_AGENT_REQUIRED"
	CodeGoalToolDriverRequired    = "GOAL_TOOL_DRIVER_REQUIRED"
	CodeGoalToolAuthorityRequired = "GOAL_TOOL_AUTHORITY_REQUIRED"
)

// reject builds one structured tool-policy failure.
func reject(message string, codes ...string) error {
	code := CodeGoalToolAuthorityRequired
	if len(codes) > 0 {
		code = codes[0]
	}
	return llm.NewError(code, message, nil)
}

// openTurn locates the open turn enclosing a model tool call.
func openTurn(a *agent.Agent) (session.Event, []session.Event, error) {
	events := a.Session.Events()
	for index := len(events) - 1; index >= 0; index-- {
		boundary := events[index]
		if boundary.Type == session.EventTurnEnd {
			return session.Event{}, nil, reject("goal tools require an open model turn", CodeGoalToolDriverRequired)
		}
		if boundary.Type == session.EventTurnStart {
			return boundary, events[index+1:], nil
		}
	}
	return session.Event{}, nil, reject("goal tools require an open model turn", CodeGoalToolDriverRequired)
}

// GoalToolExecution resolves and authenticates the calling agent and its
// driver boundary, returning the authenticated agent and its current turn
// window. Go adaptation: the official registry-level `currentInitiator()` is
// the initiator context value, visible through exec.Signal because the whole
// driver chain runs inside AgentRegistry.WithInitiator; the official
// `exec.agent` object is re-resolved from the execution's ScopeKey.
func goalToolExecution(registry *agent.AgentRegistry, exec *tools.ToolRunContext) (*GoalToolExecution, error) {
	calling := registry.ByScope(exec.Agent)
	if calling == nil {
		return nil, reject("goal tools require a calling agent", CodeGoalToolAgentRequired)
	}
	if registry.Get(calling.ID) != calling || calling.Status() != agent.AgentRunning ||
		agent.CurrentInitiator(exec.Signal) != calling {
		return nil, reject(
			"goal tools require the exact live calling agent inside its active driver",
			CodeGoalToolDriverRequired,
		)
	}
	start, events, err := openTurn(calling)
	if err != nil {
		return nil, err
	}
	return &GoalToolExecution{Agent: calling, Start: start, Events: events}, nil
}

// userMessageSource decodes one user/message event's source attribution.
func userMessageSource(event session.Event) (llm.MessageSource, bool) {
	var data struct {
		Source llm.MessageSource `json:"source"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return llm.MessageSource{}, false
	}
	return data.Source, true
}

// hasDirectHumanInput reports whether host-attested human input appears in
// the current root-agent turn. An omitted followup/steer source resolves to
// `user`, so non-human producers must supply their own source rather than
// inheriting this authority.
func hasDirectHumanInput(registry *agent.AgentRegistry, execution *GoalToolExecution) bool {
	isRoot := false
	for _, root := range registry.Roots() {
		if root == execution.Agent {
			isRoot = true
			break
		}
	}
	if !isRoot {
		return false
	}
	for _, event := range execution.Events {
		if event.Type != session.EventUserMessage {
			continue
		}
		if source, ok := userMessageSource(event); ok && source.Kind == llm.SourceUser {
			return true
		}
	}
	return false
}

// isMatchingGoalRound reports whether this turn is the current goal's exact
// admitted round.
func isMatchingGoalRound(execution *GoalToolExecution, view *goal.GoalView) bool {
	for _, event := range execution.Events {
		if event.Type != session.EventUserMessage {
			continue
		}
		source, ok := userMessageSource(event)
		if !ok || source.Kind != llm.SourceGoal {
			continue
		}
		if source.GoalID == string(view.ID) && source.GoalRevision == view.Revision &&
			source.GoalRound == view.RoundsStarted {
			return true
		}
	}
	return false
}

// requireDirectHuman requires authority originating in a human message
// accepted by a runtime root.
func requireDirectHuman(registry *agent.AgentRegistry, execution *GoalToolExecution) error {
	if hasDirectHumanInput(registry, execution) {
		return nil
	}
	return reject("this goal operation requires a direct human turn on a top-level agent")
}

// completionAuthority resolves completion authority from either direct human
// input or the exact goal round.
func completionAuthority(goals *goal.Service, registry *agent.AgentRegistry, execution *GoalToolExecution) (*GoalToolAuthority, error) {
	if hasDirectHumanInput(registry, execution) {
		return &GoalToolAuthority{Kind: authorityDirectHuman}, nil
	}
	view, err := goals.Get(execution.Agent)
	if err != nil {
		return nil, err
	}
	if view != nil && isMatchingGoalRound(execution, view) {
		return &GoalToolAuthority{Kind: authorityGoalRound, Goal: view}, nil
	}
	return nil, reject("complete and blocked require a direct human turn or the current goal round")
}
