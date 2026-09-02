package commandgoal

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/goal"
	"dshgo/llm"
	"dshgo/session"
)

// --- fake loop driver ----------------------------------------------------------

type commandFakeDriver struct {
	agent.Driver
	followup []llm.Message
}

func (d *commandFakeDriver) Followup(message llm.Message) {
	d.followup = append(d.followup, message)
}

func (d *commandFakeDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (d *commandFakeDriver) WhenIdle() <-chan struct{} {
	idle := make(chan struct{})
	close(idle)
	return idle
}
func (d *commandFakeDriver) Send(llm.Message, agent.InboxTarget, bool) {}
func (d *commandFakeDriver) Steer(llm.Message)                         {}
func (d *commandFakeDriver) Inject(llm.Message)                        {}

// --- fixture -------------------------------------------------------------------

type commandFixture struct {
	registry *agent.AgentRegistry
	sess     *session.Session
	agent    *agent.Agent
	goals    *goal.Service
	driver   *commandFakeDriver
}

type commandNoopNotifications struct{}

func (commandNoopNotifications) Inserted(llm.Message)       {}
func (commandNoopNotifications) Discarded(llm.Message)      {}
func (commandNoopNotifications) Claimed(llm.Message, int64) {}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	f := &commandFixture{registry: agent.NewAgentRegistry(nil, nil), driver: &commandFakeDriver{}}
	header := &session.SessionHeader{ID: session.SessionID("sess-command")}
	sess, err := session.NewDetached(session.SessionID("sess-command"), nil, header, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, commandNoopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	f.agent = agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	f.agent.SetDriver(f.driver)
	detach, err := f.registry.Register(f.agent)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detach)
	root := cordis.NewRoot(cordis.Discard{})
	t.Cleanup(func() { _ = root.Dispose() })
	f.goals, err = goal.NewService(root, f.registry, goal.Config{})
	if err != nil {
		t.Fatalf("goal service: %v", err)
	}
	return f
}

func (f *commandFixture) run(t *testing.T, rawInput string) commands.CommandResult {
	t.Helper()
	result, err := executeGoalCommand(f.goals, commands.Invocation{Agent: f.agent, RawInput: rawInput})
	if err != nil {
		t.Fatalf("execute %q: %v", rawInput, err)
	}
	return result
}

func (f *commandFixture) create(t *testing.T) {
	t.Helper()
	if result := f.run(t, "ship it"); result.Kind != commands.ResultSuccess {
		t.Fatalf("create = %+v", result)
	}
}

// --- grammar -------------------------------------------------------------------

func TestParseGoalCommandGrammar(t *testing.T) {
	cases := []struct {
		input string
		kind  string
		text  string
	}{
		{"", kindShow, ""},
		{"   ", kindShow, ""},
		{"clear", kindClear, ""},
		{"  CLEAR ", kindClear, ""},
		{"pause", kindPause, ""},
		{"Resume", kindResume, ""},
		{"edit", kindInvalidEdit, ""},
		{"edit ship it", kindEdit, "ship it"},
		{"EDIT\tship it", kindEdit, "ship it"},
		{"clear the table", kindCreate, "clear the table"},
		{"editors welcome", kindCreate, "editors welcome"},
		{"ship it", kindCreate, "ship it"},
	}
	for _, tc := range cases {
		command := parseGoalCommand(tc.input)
		if command.kind != tc.kind || command.objective != tc.text {
			t.Fatalf("parse %q = %+v, want kind %s text %q", tc.input, command, tc.kind, tc.text)
		}
	}
}

// --- flows -----------------------------------------------------------------------

func TestCommandShowWithoutGoal(t *testing.T) {
	f := newCommandFixture(t)
	result := f.run(t, "")
	if result.Kind != commands.ResultSuccess ||
		result.Text != "No goal is currently set.\nUsage: /goal [<objective>|clear|edit <objective>|pause|resume]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandCreateThenShow(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)

	result := f.run(t, "")
	if result.Kind != commands.ResultSuccess {
		t.Fatalf("result = %+v", result)
	}
	for _, line := range []string{
		"Goal",
		"Status: active",
		"Objective: ship it",
		"Rounds: 0/256",
		"Activation: armed",
		"Commands: /goal edit <objective>, /goal pause, /goal clear",
	} {
		if !strings.Contains(result.Text, line) {
			t.Fatalf("show missing %q:\n%s", line, result.Text)
		}
	}
}

func TestCommandCreateRejectsExisting(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	result := f.run(t, "another")
	if result.Kind != commands.ResultError || result.Text !=
		"A goal is already active. Use /goal edit <objective> to change it or /goal clear before replacing it." {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandEditUpdates(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	result := f.run(t, "edit ship it well")
	if result.Kind != commands.ResultSuccess || !strings.HasPrefix(result.Text, "Goal updated") ||
		!strings.Contains(result.Text, "Objective: ship it well") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandEditWithoutGoal(t *testing.T) {
	f := newCommandFixture(t)
	result := f.run(t, "edit ship it")
	if result.Kind != commands.ResultError || result.Text !=
		"No goal is currently set; /goal edit requires one. Usage: /goal [<objective>|clear|edit <objective>|pause|resume]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandEditOnCompleteCreates(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	if result := f.run(t, "pause"); result.Kind != commands.ResultSuccess {
		t.Fatalf("pause = %+v", result)
	}
	if result := f.run(t, "resume"); result.Kind != commands.ResultSuccess {
		t.Fatalf("resume = %+v", result)
	}
	view, err := f.goals.Get(f.agent)
	if err != nil || view == nil {
		t.Fatalf("get = %+v, %v", view, err)
	}
	if _, err := f.goals.Complete(f.agent, goal.GoalRef{ID: view.ID, Revision: view.Revision}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	result := f.run(t, "edit next objective")
	if result.Kind != commands.ResultSuccess || !strings.HasPrefix(result.Text, "Goal created") ||
		!strings.Contains(result.Text, "Objective: next objective") {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandLifecycleAndClear(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	paused := f.run(t, "pause")
	if paused.Kind != commands.ResultSuccess || !strings.HasPrefix(paused.Text, "Goal paused") ||
		!strings.Contains(paused.Text, "Status: paused") ||
		!strings.Contains(paused.Text, "Commands: /goal edit <objective>, /goal resume, /goal clear") {
		t.Fatalf("paused = %+v", paused)
	}
	resumed := f.run(t, "resume")
	if resumed.Kind != commands.ResultSuccess || !strings.HasPrefix(resumed.Text, "Goal resumed") {
		t.Fatalf("resumed = %+v", resumed)
	}
	cleared := f.run(t, "clear")
	if cleared.Kind != commands.ResultSuccess || cleared.Text != "Goal cleared." {
		t.Fatalf("cleared = %+v", cleared)
	}
	again := f.run(t, "clear")
	if again.Kind != commands.ResultSuccess || again.Text != "No goal to clear." {
		t.Fatalf("again = %+v", again)
	}
}

func TestCommandMissingGoalWording(t *testing.T) {
	f := newCommandFixture(t)
	for _, action := range []string{"pause", "resume"} {
		result := f.run(t, action)
		if result.Kind != commands.ResultError || result.Text !=
			"No goal is currently set; /goal "+action+" requires one. Usage: /goal [<objective>|clear|edit <objective>|pause|resume]" {
			t.Fatalf("%s = %+v", action, result)
		}
	}
}

func TestCommandInvalidEditWording(t *testing.T) {
	f := newCommandFixture(t)
	result := f.run(t, "edit")
	if result.Kind != commands.ResultError || result.Text !=
		"Goal editing requires a replacement objective.\nUsage: /goal [<objective>|clear|edit <objective>|pause|resume]" {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandStateInvalidRendersOfficialWording(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	// Active-and-armed resume is a goal boundary error: the human wording
	// hides the compare-and-set detail.
	result := f.run(t, "resume")
	if result.Kind != commands.ResultError || result.Text !=
		"The goal command is not valid for the current state. Run /goal to view available commands." {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandBlockedRendersBlocker(t *testing.T) {
	f := newCommandFixture(t)
	f.create(t)
	view, err := f.goals.Get(f.agent)
	if err != nil || view == nil {
		t.Fatalf("get = %+v, %v", view, err)
	}
	if _, err := f.goals.Block(f.agent, goal.GoalRef{ID: view.ID, Revision: view.Revision},
		goal.GoalBlockReason{Code: "waiting-user", Message: "needs a decision"}); err != nil {
		t.Fatalf("block: %v", err)
	}
	result := f.run(t, "")
	if result.Kind != commands.ResultSuccess ||
		!strings.Contains(result.Text, "Status: blocked") ||
		!strings.Contains(result.Text, "Blocker: waiting-user: needs a decision") {
		t.Fatalf("result = %+v", result)
	}
}

// --- attachments -----------------------------------------------------------------

func TestCommandImagesOnlyWithObjective(t *testing.T) {
	f := newCommandFixture(t)
	image := llm.ContentBlock{Type: llm.BlockImage}
	result, err := executeGoalCommand(f.goals, commands.Invocation{
		Agent: f.agent, RawInput: "pause",
		Attachments: []commands.ImageAttachment{{Block: &image}},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Kind != commands.ResultError || result.Text !=
		"Image attachments only accompany a goal objective: /goal <objective> or /goal edit <objective>." {
		t.Fatalf("result = %+v", result)
	}
}

func TestCommandCreateSubmitsAttachmentsAheadOfRounds(t *testing.T) {
	f := newCommandFixture(t)
	image := llm.ContentBlock{Type: llm.BlockImage}
	result, err := executeGoalCommand(f.goals, commands.Invocation{
		Agent: f.agent, RawInput: "ship it",
		Attachments: []commands.ImageAttachment{{Block: &image}},
	})
	if err != nil || result.Kind != commands.ResultSuccess {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(f.driver.followup) != 1 {
		t.Fatalf("followup = %d", len(f.driver.followup))
	}
	message := f.driver.followup[0]
	if message.Source.Kind != llm.SourceUser || len(message.Content) != 2 ||
		message.Content[0].Type != llm.BlockImage ||
		message.Content[1].Text != "Reference images for the goal objective." {
		t.Fatalf("message = %+v", message)
	}
}

// --- registration -----------------------------------------------------------------

func TestRegisterInstallsTheGoalCommand(t *testing.T) {
	f := newCommandFixture(t)
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	undo, err := Register(runtime, f.goals)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	undo()
	if _, err := Register(nil, f.goals); err == nil {
		t.Fatal("a nil runtime must be rejected")
	}
	if _, err := Register(runtime, nil); err == nil {
		t.Fatal("a nil goal service must be rejected")
	}
}
