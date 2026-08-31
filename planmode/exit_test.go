package planmode

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/interaction/userquestions"
	"dshgo/session"
	"dshgo/tools"
)

// exitWorld wires the registry, user-questions service, tool runtime, and
// controller around one live agent.
type exitWorld struct {
	registry   *agent.AgentRegistry
	service    *userquestions.Service
	runtime    *tools.ToolRuntime
	controller *Controller
	tool       *tools.ToolDefinition
	dispose    func()
	agent      *agent.Agent
	sess       *session.Session
	claims     []userquestions.Request
	mu         sync.Mutex
	claim      func(request userquestions.Request) userquestions.AskUserQuestionAnswer
}

func newExitWorld(t *testing.T, active bool) *exitWorld {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	service := userquestions.NewService(registry)
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	controller, err := NewControllerWithRegistry(t, "Plan guidance stays inside the plan.")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	dispose, err := RegisterExitTool(runtime, service, controller)
	if err != nil {
		t.Fatalf("RegisterExitTool: %v", err)
	}
	t.Cleanup(dispose)
	tool, ok := runtime.Get(ExitToolName, nil)
	if !ok {
		t.Fatalf("tool %q not registered", ExitToolName)
	}
	sess, err := session.NewDetached(session.SessionID("root"), nil, &session.SessionHeader{ID: session.SessionID("root")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	if active {
		if _, err := sess.Append(EventPlanMode, PlanModeData{Active: true}, nil); err != nil {
			t.Fatalf("append plan/mode: %v", err)
		}
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	world := &exitWorld{
		registry: registry, service: service, runtime: runtime,
		controller: controller, tool: tool, dispose: dispose, agent: built, sess: sess,
	}
	userquestions.Requests(registry.Events()).On(built.Scope, world.listener)
	return world
}

func (w *exitWorld) listener(request userquestions.Request, next func(userquestions.Request) userquestions.QuestionDecision) userquestions.QuestionDecision {
	w.mu.Lock()
	w.claims = append(w.claims, request)
	claim := w.claim
	w.mu.Unlock()
	if claim == nil {
		return next(request)
	}
	return userquestions.QuestionDecision{Answer: claim(request)}
}

func (w *exitWorld) seenRequests() []userquestions.Request {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]userquestions.Request(nil), w.claims...)
}

func planArgs(t *testing.T, plan string) map[string]any {
	t.Helper()
	return map[string]any{"plan": plan}
}

func TestExitToolGatesBeforeAsking(t *testing.T) {
	world := newExitWorld(t, true)
	// No agent in scope: nothing to switch.
	if _, err := world.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{}); err == nil ||
		!strings.Contains(err.Error(), "requires a calling agent") {
		t.Fatalf("agent-less execute = %v", err)
	}
	// A heading-less plan is refused before any channel round-trip.
	if _, err := world.tool.Execute(planArgs(t, "just prose"), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: world.agent.Scope}}); err == nil ||
		!strings.Contains(err.Error(), "requires a non-empty markdown plan") {
		t.Fatalf("heading-less execute = %v", err)
	}
	if len(world.seenRequests()) != 0 {
		t.Fatal("gated rejections must not reach the review channel")
	}
	// Inactive mode: the tool stays registered but refuses.
	inactive := newExitWorld(t, false)
	if _, err := inactive.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: inactive.agent.Scope}}); err == nil ||
		!strings.Contains(err.Error(), "only available in plan mode") {
		t.Fatalf("inactive execute = %v", err)
	}
}

func TestExitToolApprovalQueuesSilentExit(t *testing.T) {
	world := newExitWorld(t, true)
	world.claim = func(request userquestions.Request) userquestions.AskUserQuestionAnswer {
		return userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{
			{ID: reviewID, Selected: []string{approveLabel}},
		}}
	}
	value, err := world.tool.Execute(planArgs(t, "# Ship it\n\nDetails."), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: world.agent.Scope}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	approved, ok := value.(map[string]any)["approved"].(bool)
	if !ok || !approved {
		t.Fatalf("value = %#v", value)
	}
	// The review question carries the plan detail and the plan-review intent.
	requests := world.seenRequests()
	if len(requests) != 1 {
		t.Fatalf("requests = %d", len(requests))
	}
	question := requests[0].Questions[0]
	if question.ID != reviewID || question.Question != "Approve this plan and leave plan mode?" ||
		question.Detail != "# Ship it\n\nDetails." || question.Intent == nil ||
		question.Intent.Kind != "plan-review" || question.Intent.Approve != approveLabel {
		t.Fatalf("question = %+v", question)
	}
	if requests[0].Agent != world.agent {
		t.Fatal("review must route through the calling agent")
	}
	// The approval queues a silent (non-narrated) exit pending the next
	// accepted pre-step, and the section hides immediately.
	active, pending, hasPending := world.controller.Get(world.sess)
	if !active || !hasPending || pending {
		t.Fatalf("get = %v %v %v", active, pending, hasPending)
	}
	if world.controller.SectionText(world.sess) != "" {
		t.Fatal("a pending exit must hide the guidance section")
	}
}

func TestExitToolKeepPlanningFeedback(t *testing.T) {
	world := newExitWorld(t, true)
	world.claim = func(userquestions.Request) userquestions.AskUserQuestionAnswer {
		return userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{
			{ID: reviewID, Selected: []string{keepPlanningLabel}, Custom: "cover the retry path"},
		}}
	}
	_, err := world.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: world.agent.Scope}})
	if err == nil || err.Error() != "The user chose to keep planning; their feedback: cover the retry path" {
		t.Fatalf("feedback execute = %v", err)
	}
	if _, _, hasPending := world.controller.Get(world.sess); hasPending {
		t.Fatal("a declined review must not queue an exit")
	}

	// A decline without prose keeps the generic text.
	world.claim = func(userquestions.Request) userquestions.AskUserQuestionAnswer {
		return userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{
			{ID: reviewID, Selected: []string{keepPlanningLabel}},
		}}
	}
	_, err = world.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: world.agent.Scope}})
	if err == nil || err.Error() != "The user chose to keep planning; revise the plan and present it again." {
		t.Fatalf("plain decline = %v", err)
	}
}

func TestExitToolDismissedReview(t *testing.T) {
	world := newExitWorld(t, true)
	// A dismissed review crosses as ASK_ABORTED (the ASK_CANCELLED analog);
	// the tool translates it so the model is not told about a channel it
	// never called. The service aborts before dispatch on a dead signal.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := world.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{
		ToolExecution: tools.ToolExecution{Agent: world.agent.Scope},
		Signal:        cancelled,
	})
	if err == nil || !strings.Contains(err.Error(), "dismissed the plan review") {
		t.Fatalf("dismissed execute = %v", err)
	}
	if len(world.seenRequests()) != 0 {
		t.Fatal("an aborted ask must not reach an answerer")
	}
}

func TestExitToolReloadDuringReview(t *testing.T) {
	world := newExitWorld(t, true)
	// Dispose while a review would be in flight: the approval path must fail
	// instead of queueing an exit no pre-step listener will ever append.
	world.claim = func(userquestions.Request) userquestions.AskUserQuestionAnswer {
		return userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{
			{ID: reviewID, Selected: []string{approveLabel}},
		}}
	}
	world.dispose()
	_, err := world.tool.Execute(planArgs(t, "# Plan"), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: world.agent.Scope}})
	if err == nil || !strings.Contains(err.Error(), "present the plan again") {
		t.Fatalf("post-dispose execute = %v", err)
	}
}

// newPlanCommandWorld builds a command runtime, controller, and idle agent.
func newPlanCommandWorld(t *testing.T) (*commands.CommandRuntime, *Controller, *agent.Agent, *session.Session) {
	t.Helper()
	controller, err := NewControllerWithRegistry(t, "Plan guidance stays inside the plan.")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	if _, err := RegisterPlanCommand(runtime, controller); err != nil {
		t.Fatalf("RegisterPlanCommand: %v", err)
	}
	sess, err := session.NewDetached(session.SessionID("plan-cmd"), nil, &session.SessionHeader{ID: session.SessionID("plan-cmd")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, registry.Events())
	if _, err := registry.Enter(built, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	return runtime, controller, built, sess
}

func planDone(t *testing.T, sess *session.Session) commands.CommandDoneData {
	t.Helper()
	events := sess.Events()
	var done commands.CommandDoneData
	if err := json.Unmarshal(events[len(events)-1].Data, &done); err != nil {
		t.Fatalf("done: %v", err)
	}
	return done
}

func TestPlanCommandEnterAndLeave(t *testing.T) {
	runtime, _, agentObj, sess := newPlanCommandWorld(t)

	// Entering with a message commits and steers the text into the next step.
	execution, err := runtime.ExecuteForAgent(context.Background(), agentObj, nil, sess, "/plan focus the tests", nil)
	if err != nil || execution == nil {
		t.Fatalf("enter = %v %v", execution, err)
	}
	done := planDone(t, sess)
	if done.Kind != commands.ResultSuccess || done.Text == nil || *done.Text != "Plan mode on. Use /plan off to leave." {
		t.Fatalf("enter done = %+v", done)
	}
	if !FoldPlanMode(sess.Events(), -1) {
		t.Fatal("an idle session commits the entry immediately")
	}
	steered := agentObj.Inbox.NextStep()
	if len(steered) != 1 || len(steered[0].Content) != 1 || steered[0].Content[0].Text != "focus the tests" {
		t.Fatalf("steered = %+v", steered)
	}
	if steered[0].Source.Kind != "user" {
		t.Fatalf("steered source = %+v", steered[0].Source)
	}

	// Leaving commits the exit.
	if execution, err = runtime.ExecuteForAgent(context.Background(), agentObj, nil, sess, "/plan off", nil); err != nil || execution == nil {
		t.Fatalf("leave = %v %v", execution, err)
	}
	done = planDone(t, sess)
	if done.Text == nil || *done.Text != "Plan mode off." {
		t.Fatalf("leave done = %+v", done)
	}
	if FoldPlanMode(sess.Events(), -1) {
		t.Fatal("the exit must be logged")
	}

	// Leaving again is idempotent.
	if execution, err = runtime.ExecuteForAgent(context.Background(), agentObj, nil, sess, "/plan off", nil); err != nil || execution == nil {
		t.Fatalf("leave again = %v %v", execution, err)
	}
	done = planDone(t, sess)
	if done.Text == nil || *done.Text != "Plan mode is already inactive." {
		t.Fatalf("idempotent done = %+v", done)
	}
}

func TestPlanCommandOffWithAttachmentsRefused(t *testing.T) {
	runtime, _, agentObj, sess := newPlanCommandWorld(t)
	// Without a composed attachment store the runtime refuses the images
	// before the handler; compose a store so the handler's own refusal is
	// what the test observes.
	runtime.SetImageAdmitter(func([]any) ([]commands.ImageAttachment, error) {
		return []commands.ImageAttachment{{Reference: "ref-1"}}, nil
	})
	execution, err := runtime.ExecuteForAgent(context.Background(), agentObj, nil, sess, "/plan off", []any{"png-bytes"})
	if err != nil || execution == nil {
		t.Fatalf("execute = %v %v", execution, err)
	}
	done := planDone(t, sess)
	if done.Kind != commands.ResultError || done.Text == nil || *done.Text != "Image attachments cannot accompany /plan off." {
		t.Fatalf("done = %+v", done)
	}
}

func TestPlanCommandEntryDuringOpenTurnQueues(t *testing.T) {
	runtime, controller, agentObj, sess := newPlanCommandWorld(t)
	// Open a turn: the selection defers to the next accepted pre-step.
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("open turn: %v", err)
	}
	execution, err := runtime.ExecuteForAgent(context.Background(), agentObj, nil, sess, "/plan", nil)
	if err != nil || execution == nil {
		t.Fatalf("execute = %v %v", execution, err)
	}
	done := planDone(t, sess)
	if done.Text == nil || *done.Text != "Entering plan mode (applies from the next step). Use /plan off to leave." {
		t.Fatalf("queued done = %+v", done)
	}
	if FoldPlanMode(sess.Events(), -1) {
		t.Fatal("a queued selection must not log inside the open turn")
	}
	if _, pending, hasPending := controller.Get(sess); !hasPending || !pending {
		t.Fatalf("get = pending %v hasPending %v", pending, hasPending)
	}
}

func TestPlanCommandUndoUnregisters(t *testing.T) {
	controller, err := NewControllerWithRegistry(t, "Plan guidance stays inside the plan.")
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	undo, err := RegisterPlanCommand(runtime, controller)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	undo()
	if _, err := RegisterPlanCommand(runtime, controller); err != nil {
		t.Fatalf("re-register after undo: %v", err)
	}
}
