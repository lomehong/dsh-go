package toolgoal

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// --- pure units ---------------------------------------------------------------

func TestResolveConfigValidation(t *testing.T) {
	blockedAfter, err := ResolveConfig(Config{})
	if err != nil || blockedAfter != DefaultBlockedAfterConsecutiveRounds {
		t.Fatalf("default = %d, %v", blockedAfter, err)
	}
	seven := int64(7)
	blockedAfter, err = ResolveConfig(Config{BlockedAfterConsecutiveRounds: &seven})
	if err != nil || blockedAfter != 7 {
		t.Fatalf("override = %d, %v", blockedAfter, err)
	}
	zero := int64(0)
	if _, err = ResolveConfig(Config{BlockedAfterConsecutiveRounds: &zero}); err == nil ||
		err.Error() != "blockedAfterConsecutiveRounds must be a positive safe integer" {
		t.Fatalf("zero = %v; the official wording must surface", err)
	}
}

func TestGuidanceThreshold(t *testing.T) {
	text := Guidance(5)
	if !strings.Contains(text, "persists for at least 5 consecutive goal rounds") {
		t.Fatalf("guidance = %q; the deployment threshold must interpolate", text)
	}
}

func TestRenderWrapupContext(t *testing.T) {
	complete := RenderWrapupContext("ship it", nil)
	if len(complete) != 1 || complete[0].Type != llm.BlockText {
		t.Fatalf("complete blocks = %+v", complete)
	}
	if !strings.HasPrefix(complete[0].Text, "<goal_complete>\nObjective: \"ship it\"\n") ||
		!strings.HasSuffix(complete[0].Text, "</goal_complete>") ||
		!strings.Contains(complete[0].Text, "Report only what earlier rounds and tool results") {
		t.Fatalf("complete text = %q", complete[0].Text)
	}
	reason := "no credentials"
	blocked := RenderWrapupContext("ship it", &reason)
	if !strings.HasPrefix(blocked[0].Text, "<goal_blocked>\nObjective: \"ship it\"\nBlocked: \"no credentials\"\n") ||
		!strings.HasSuffix(blocked[0].Text, "</goal_blocked>") {
		t.Fatalf("blocked text = %q", blocked[0].Text)
	}
}

func TestGoalValueJSONShape(t *testing.T) {
	null, err := json.Marshal(goalValue(nil))
	if err != nil || string(null) != `{"goal":null}` {
		t.Fatalf("null shape = %s, %v", null, err)
	}
}

// --- fixture -------------------------------------------------------------------

type fakeDriver struct{ agent.Driver }

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

type fixture struct {
	registry *agent.AgentRegistry
	root     *cordis.Context
	sess     *session.Session
	live     *agent.Agent
	goals    *goal.Service
	runtime  *tools.ToolRuntime
	turn     int64
}

func newFixture(t *testing.T, config Config) *fixture {
	t.Helper()
	f := &fixture{registry: agent.NewAgentRegistry(nil, nil)}
	f.root = cordis.NewRoot(cordis.Discard{})
	t.Cleanup(func() { _ = f.root.Dispose() })
	header := &session.SessionHeader{ID: session.SessionID("sess-tool-goal")}
	sess, err := session.NewDetached(session.SessionID("sess-tool-goal"), nil, header)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	f.live = agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	f.live.SetDriver(fakeDriver{})
	detach, err := f.registry.Register(f.live)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detach)
	f.goals, err = goal.NewService(f.root, f.registry, goal.Config{})
	if err != nil {
		t.Fatalf("goal service: %v", err)
	}
	f.runtime, err = tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	if err := Apply(f.root, f.registry, f.goals, f.runtime, nil, config); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return f
}

// openTurnInLog appends one fresh turn/start boundary.
func (f *fixture) openTurnInLog(t *testing.T) {
	t.Helper()
	f.turn++
	if _, err := f.sess.Append(session.EventTurnStart, map[string]any{"turn": f.turn}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
}

// admitHuman appends one host-attested human message inside the open turn.
func (f *fixture) admitHuman(t *testing.T) {
	t.Helper()
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "do it"}}, llm.MessageSource{})
	intent := &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
	if _, err := f.sess.Append(session.EventUserMessage, message, intent); err != nil {
		t.Fatalf("user/message: %v", err)
	}
}

// admitGoalRound appends one goal-sourced continuation message matching the
// current goal's exact admitted round.
func (f *fixture) admitGoalRound(t *testing.T, view *goal.GoalView) {
	t.Helper()
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "continue"}}, llm.MessageSource{
		Kind:         llm.SourceGoal,
		GoalID:       string(view.ID),
		GoalRevision: view.Revision,
		GoalRound:    view.RoundsStarted,
	})
	intent := &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
	if _, err := f.sess.Append(session.EventUserMessage, message, intent); err != nil {
		t.Fatalf("goal round message: %v", err)
	}
}

// endTurnInLog closes the current turn with a completed boundary.
func (f *fixture) endTurnInLog(t *testing.T) {
	t.Helper()
	if _, err := f.sess.Append(session.EventTurnEnd, session.TurnEndData{
		Turn: f.turn, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted},
	}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
}

// dispatch runs one tool call through the real registry pipeline inside the
// initiator boundary; withInitiator=false exercises the boundary-less path.
func (f *fixture) dispatch(t *testing.T, withInitiator bool, name string, args map[string]any) *tools.ToolExecutionResult {
	t.Helper()
	var result *tools.ToolExecutionResult
	run := func(ctx context.Context) error {
		prepared := f.runtime.Prepare(&tools.ToolExecutionInput{
			CallID:    "call-" + name,
			Name:      name,
			Arguments: args,
			Agent:     f.live.Scope,
			Signal:    ctx,
		})
		switch prepared.Kind {
		case tools.PreparedDispatch:
			result = f.runtime.Dispatch(prepared.Exec).Result
		default:
			result = prepared.Result
		}
		return nil
	}
	var err error
	if withInitiator {
		err = f.registry.WithInitiator(context.Background(), f.live, run)
	} else {
		err = run(context.Background())
	}
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if result == nil {
		t.Fatal("dispatch produced no result")
	}
	return result
}

// expectFailure asserts one structured policy failure verbatim.
func expectFailure(t *testing.T, result *tools.ToolExecutionResult, code, message string) {
	t.Helper()
	if !result.IsError || result.Error == nil {
		t.Fatalf("result = %+v; a structured failure was expected", result)
	}
	if result.Error.Message != message {
		t.Fatalf("message = %q; want %q", result.Error.Message, message)
	}
	if result.Error.Info == nil || result.Error.Info.Code != code {
		t.Fatalf("code = %+v; want %s", result.Error.Info, code)
	}
}

func valueOf(t *testing.T, result *tools.ToolExecutionResult) GoalToolValue {
	t.Helper()
	if result.IsError {
		t.Fatalf("result errored: %+v", result.Error)
	}
	encoded, err := json.Marshal(result.Value)
	if err != nil {
		t.Fatalf("encode value: %v", err)
	}
	var value GoalToolValue
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatalf("decode value %s: %v", encoded, err)
	}
	return value
}

// --- registration ---------------------------------------------------------------

func TestApplyRegistersToolsAndSection(t *testing.T) {
	f := newFixture(t, Config{})
	for _, name := range []string{"get_goal", "create_goal", "update_goal"} {
		if _, ok := f.runtime.Get(name, nil); !ok {
			t.Fatalf("%s missing after Apply", name)
		}
	}
}

func TestApplyRegistersPromptSection(t *testing.T) {
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("system prompt: %v", err)
	}
	f := newFixture(t, Config{})
	// A fresh runtime over the prompt registers the section once.
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := Apply(f.root, f.registry, f.goals, runtime, prompt, Config{}); err != nil {
		t.Fatalf("apply with prompt: %v", err)
	}
	assembled, err := prompt.Assemble(systemprompt.AssembleContext{})
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	var guidance string
	for _, section := range assembled.Sections {
		if section.Name == "tool:goal" {
			guidance = section.Text
		}
	}
	if !strings.Contains(guidance, "Use goal tools for one long-running completion objective") ||
		!strings.Contains(guidance, "at least 3 consecutive goal rounds") {
		t.Fatalf("tool:goal section = %q; the guidance must render with the default threshold", guidance)
	}
	// A second Apply over the same prompt fails loud on the duplicate section.
	runtime2, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	if err := Apply(f.root, f.registry, f.goals, runtime2, prompt, Config{}); err == nil {
		t.Fatal("a duplicate Apply must fail on the duplicate section registration")
	}
}

// --- execution fences --------------------------------------------------------------

func TestGetGoalNullShape(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	result := f.dispatch(t, true, "get_goal", map[string]any{})
	value := valueOf(t, result)
	if value.Goal != nil {
		t.Fatalf("value = %+v; no goal exists", value)
	}
	if len(result.Content) != 1 || result.Content[0].Text != `{"goal":null}` {
		t.Fatalf("content = %+v; the compact Native JSON must render verbatim", result.Content)
	}
}

func TestAuthorityFences(t *testing.T) {
	f := newFixture(t, Config{})
	f.openTurnInLog(t)
	f.admitHuman(t)

	// A non-running agent is not inside its active driver.
	result := f.dispatch(t, true, "get_goal", map[string]any{})
	expectFailure(t, result, CodeGoalToolDriverRequired,
		"goal tools require the exact live calling agent inside its active driver")

	f.live.SetStatus(agent.AgentRunning)

	// Running without the initiator boundary is equally rejected.
	result = f.dispatch(t, false, "get_goal", map[string]any{})
	expectFailure(t, result, CodeGoalToolDriverRequired,
		"goal tools require the exact live calling agent inside its active driver")
}

func TestOpenTurnFence(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)

	// No turn boundary at all: no open model turn.
	result := f.dispatch(t, true, "get_goal", map[string]any{})
	expectFailure(t, result, CodeGoalToolDriverRequired, "goal tools require an open model turn")

	// A closed turn is equally unusable.
	f.openTurnInLog(t)
	if _, err := f.sess.Append(session.EventTurnEnd, session.TurnEndData{
		Turn: f.turn, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted},
	}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
	result = f.dispatch(t, true, "get_goal", map[string]any{})
	expectFailure(t, result, CodeGoalToolDriverRequired, "goal tools require an open model turn")
}

// --- create ----------------------------------------------------------------------

func TestCreateGoalRequiresDirectHuman(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	result := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	expectFailure(t, result, CodeGoalToolAuthorityRequired,
		"this goal operation requires a direct human turn on a top-level agent")
}

func TestCreateGoalRoundTrip(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	result := f.dispatch(t, true, "create_goal", map[string]any{
		"objective":       "ship it",
		"max_goal_rounds": float64(4),
	})
	value := valueOf(t, result)
	if value.Goal == nil || value.Goal.Objective != "ship it" || value.Goal.Phase != goal.PhaseActive ||
		value.Goal.MaxGoalRounds != 4 || value.Activation != goal.ActivationArmed {
		t.Fatalf("value = %+v", value)
	}
}

// --- update ----------------------------------------------------------------------

func TestUpdateGoalRefValidation(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	result := f.dispatch(t, true, "update_goal", map[string]any{
		"goal_id":  " padded ",
		"revision": float64(1),
		"action":   "pause",
	})
	expectFailure(t, result, CodeGoalToolInvalidUpdate,
		"goal_id must be non-empty and revision must be a positive safe integer")
}

func TestUpdateActionShapeFences(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	created := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	view := valueOf(t, created)
	args := func(action string, extra map[string]any) map[string]any {
		base := map[string]any{
			"goal_id":  string(view.Goal.ID),
			"revision": float64(view.Goal.Revision),
			"action":   action,
		}
		for key, value := range extra {
			base[key] = value
		}
		return base
	}

	expectFailure(t, f.dispatch(t, true, "update_goal", args("edit", map[string]any{"blocked_reason": "stuck"})),
		CodeGoalToolInvalidUpdate, "blocked_reason is valid only with action blocked")
	expectFailure(t, f.dispatch(t, true, "update_goal", args("pause", map[string]any{"objective": "other"})),
		CodeGoalToolInvalidUpdate,
		"objective and max_goal_rounds are valid only with action edit; blocked_reason is valid only with action blocked")
	expectFailure(t, f.dispatch(t, true, "update_goal", args("complete", map[string]any{"objective": "other"})),
		CodeGoalToolInvalidUpdate, "objective and max_goal_rounds are valid only with action edit")
	expectFailure(t, f.dispatch(t, true, "update_goal", args("complete", map[string]any{"blocked_reason": "stuck"})),
		CodeGoalToolInvalidUpdate, "blocked_reason is valid only with action blocked")
	expectFailure(t, f.dispatch(t, true, "update_goal", args("blocked", map[string]any{})),
		CodeGoalToolInvalidUpdate, "blocked_reason is required with action blocked")
}

func TestCompleteWithDirectHuman(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	created := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	view := valueOf(t, created)
	result := f.dispatch(t, true, "update_goal", map[string]any{
		"goal_id":  string(view.Goal.ID),
		"revision": float64(view.Goal.Revision),
		"action":   "complete",
	})
	value := valueOf(t, result)
	if value.Goal == nil || value.Goal.Phase != goal.PhaseComplete {
		t.Fatalf("value = %+v", value)
	}
}

// --- goal-round authority -----------------------------------------------------------

func TestBlockedThresholdUnderGoalRound(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	created := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	view := valueOf(t, created)

	// A fresh continuation turn carrying only the goal-sourced message:
	// authority is goal-round, never direct-human.
	f.endTurnInLog(t)
	f.openTurnInLog(t)
	f.admitGoalRound(t, &goal.GoalView{GoalSnapshot: goal.GoalSnapshot{
		ID: goal.GoalID(view.Goal.ID), Revision: view.Goal.Revision,
	}, RoundsStarted: 1})
	result := f.dispatch(t, true, "update_goal", map[string]any{
		"goal_id":        string(view.Goal.ID),
		"revision":       float64(view.Goal.Revision),
		"action":         "blocked",
		"blocked_reason": "the registry is unreachable",
	})
	expectFailure(t, result, CodeGoalToolBlockThreshold,
		"blocked requires at least 3 consecutive goal rounds; current round is 1")
}

func TestBlockedAtThresholdDefersWrapup(t *testing.T) {
	f := newFixture(t, Config{BlockedAfterConsecutiveRounds: ptrInt64(1)})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	created := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	view := valueOf(t, created)
	f.endTurnInLog(t)
	f.openTurnInLog(t)
	f.admitGoalRound(t, &goal.GoalView{GoalSnapshot: goal.GoalSnapshot{
		ID: goal.GoalID(view.Goal.ID), Revision: view.Goal.Revision,
	}, RoundsStarted: 1})
	result := f.dispatch(t, true, "update_goal", map[string]any{
		"goal_id":        string(view.Goal.ID),
		"revision":       float64(view.Goal.Revision),
		"action":         "blocked",
		"blocked_reason": "the registry is unreachable",
	})
	value := valueOf(t, result)
	if value.Goal == nil || value.Goal.Phase != goal.PhaseBlocked ||
		value.Goal.BlockedReason == nil || value.Goal.BlockedReason.Code != "model-reported" ||
		value.Goal.BlockedReason.Message != "the registry is unreachable" {
		t.Fatalf("value = %+v", value)
	}
	if len(result.AdditionalContexts) != 1 {
		t.Fatalf("deferred contexts = %+v; the goal round must receive one wrapup notice", result.AdditionalContexts)
	}
	notice := result.AdditionalContexts[0]
	if notice.Source.Kind != llm.SourcePlugin || notice.Source.Plugin != Name ||
		notice.Source.Form != llm.FormNotice {
		t.Fatalf("notice source = %+v", notice.Source)
	}
	if !strings.Contains(notice.Source.Summary, "blocked: ship it") {
		t.Fatalf("notice summary = %q", notice.Source.Summary)
	}
	if len(notice.Content) != 1 || !strings.HasPrefix(notice.Content[0].Text, "<goal_blocked>") {
		t.Fatalf("notice content = %+v", notice.Content)
	}
}

func TestCompleteUnderGoalRoundDefersWrapup(t *testing.T) {
	f := newFixture(t, Config{})
	f.live.SetStatus(agent.AgentRunning)
	f.openTurnInLog(t)
	f.admitHuman(t)
	created := f.dispatch(t, true, "create_goal", map[string]any{"objective": "ship it"})
	view := valueOf(t, created)
	f.endTurnInLog(t)
	f.openTurnInLog(t)
	f.admitGoalRound(t, &goal.GoalView{GoalSnapshot: goal.GoalSnapshot{
		ID: goal.GoalID(view.Goal.ID), Revision: view.Goal.Revision,
	}, RoundsStarted: 1})
	result := f.dispatch(t, true, "update_goal", map[string]any{
		"goal_id":  string(view.Goal.ID),
		"revision": float64(view.Goal.Revision),
		"action":   "complete",
	})
	value := valueOf(t, result)
	if value.Goal == nil || value.Goal.Phase != goal.PhaseComplete {
		t.Fatalf("value = %+v", value)
	}
	if len(result.AdditionalContexts) != 1 ||
		!strings.HasPrefix(result.AdditionalContexts[0].Content[0].Text, "<goal_complete>") {
		t.Fatalf("deferred contexts = %+v", result.AdditionalContexts)
	}
}

func ptrInt64(value int64) *int64 { return &value }
