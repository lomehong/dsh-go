package tmuxcontext

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// fakeShell serves canned stdout or an error for the query command.
type fakeShell struct {
	stdout   string
	exitCode int
	err      error
	commands []string
}

func (f *fakeShell) Run(command string, signal context.Context) (ShellRunResult, error) {
	f.commands = append(f.commands, command)
	if f.err != nil {
		return ShellRunResult{}, f.err
	}
	return ShellRunResult{ExitCode: f.exitCode, Stdout: f.stdout}, nil
}

func inPaneStdout() string {
	fields := []string{"main", "3", "build", "1", "%7", "1", "1", "bb02a1f0,238x58,0,0,0,238x58,1,1,7,238x58"}
	return strings.Join(fields, fieldSep) + "\n"
}

func locationState() string {
	return RenderState(&TmuxLocation{
		SessionName: "main", WindowIndex: "3", WindowName: "build",
		PaneIndex: "1", PaneID: "%7", WindowActive: "1", PaneActive: "1",
		WindowLayout: "bb02a1f0,238x58,0,0,0,238x58,1,1,7,238x58",
	})
}

// newLoopAgent builds one live agent whose session appends through the
// surface validator.
func newLoopAgent(t *testing.T, id string) (*agent.Agent, *session.Session) {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built, sess
}

func runPreStep(t *testing.T, registry *agent.AgentRegistry, built *agent.Agent, turn int64, step int64, proposed []llm.Message) agent.PreStepDecision {
	t.Helper()
	return registry.Events().PreStep().Dispatch(nil, agent.PreStepPayload{
		Agent: built, Messages: proposed, Turn: turn, Step: step, Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(proposed) })
}

func TestQueryRejectsInheritedEnvironmentAndBadOutput(t *testing.T) {
	cases := map[string]*fakeShell{
		"no TMUX_PANE":  {exitCode: 1},
		"tty mismatch":  {exitCode: 1},
		"executor err":  {err: errors.New("denied by policy")},
		"garbled line":  {stdout: "only-one-field\n"},
		"empty pane id": {stdout: "\t\t\t\t\t\t\t\n"},
	}
	for name, shell := range cases {
		if location := QueryTmuxLocation(shell, cordis.Discard{}, 4242, context.Background()); location != nil {
			t.Fatalf("%s produced %+v", name, location)
		}
	}
	ok := &fakeShell{stdout: inPaneStdout()}
	location := QueryTmuxLocation(ok, cordis.Discard{}, 4242, context.Background())
	if location == nil || location.SessionName != "main" || location.PaneID != "%7" || location.WindowLayout == "" {
		t.Fatalf("healthy query = %+v", location)
	}
	// The single command bundles the pane/tty check and the field query,
	// whose format opens with session_name and ends with window_layout.
	if len(ok.commands) != 1 || !strings.Contains(ok.commands[0], `ps -o tty= -p 4242`) ||
		!strings.Contains(ok.commands[0], `#{pane_tty}`) ||
		!strings.HasPrefix(strings.Join(tmuxFields, fieldSep), "#{session_name}") ||
		!strings.HasSuffix(strings.Join(tmuxFields, fieldSep), "#{window_layout}") ||
		!strings.Contains(ok.commands[0], strings.Join(tmuxFields, fieldSep)) {
		t.Fatalf("command = %q", ok.commands[0])
	}
}

func TestRenderStateExcludesTurnPreamble(t *testing.T) {
	location := &TmuxLocation{
		SessionName: "main", WindowIndex: "3", WindowName: "build work",
		PaneIndex: "1", PaneID: "%7", WindowActive: "1", PaneActive: "1",
		WindowLayout: "layout-1",
	}
	state := RenderState(location)
	if !strings.HasPrefix(state, "session main, window 3 \"build work\", pane 1 %7\n") ||
		!strings.Contains(state, "window active=1, pane active=1, layout layout-1") {
		t.Fatalf("state = %q", state)
	}
	reading := RenderReading(location, 9)
	if reading != "tmux location (turn 9):\n"+state {
		t.Fatalf("reading = %q", reading)
	}
}

func TestAttachInjectsDedupesAndRespectsRefreshFloor(t *testing.T) {
	shell := &fakeShell{stdout: inPaneStdout()}
	registry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	detach, err := Attach(registry, shell, cordis.Discard{}, Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	built, sess := newLoopAgent(t, "tmux-1")

	first := runPreStep(t, registry, built, 4, 1, nil)
	if len(first.Messages) != 1 || !strings.HasPrefix(first.Messages[0].Content[0].Text, "tmux location (turn 4):\n") {
		t.Fatalf("first = %+v", first.Messages)
	}
	// Source attribution makes the reading durable and recognized.
	if first.Messages[0].Source.Plugin != Name || first.Messages[0].Source.Form != "snapshot" ||
		len(first.Messages[0].Source.Sections) != 1 || first.Messages[0].Source.Sections[0].Name != Name {
		t.Fatalf("source = %+v", first.Messages[0].Source)
	}

	// Record the injection durably; an unchanged state injects nothing.
	if _, err := sess.Append(session.EventUserMessage, first.Messages[0], &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	second := runPreStep(t, registry, built, 5, 1, nil)
	if len(second.Messages) != 0 {
		t.Fatalf("dedupe failed: %+v", second.Messages)
	}

	// A changed pane re-injects, driven by state not loop position.
	shell.stdout = strings.Replace(inPaneStdout(), "%7", "%9", 1)
	third := runPreStep(t, registry, built, 6, 1, nil)
	if len(third.Messages) != 1 || !strings.Contains(third.Messages[0].Content[0].Text, "%9") {
		t.Fatalf("changed = %+v", third.Messages)
	}

	// The refresh floor suppresses re-injection inside the window.
	if err := ValidateConfig(Config{RefreshIntervalMs: -1}); err == nil {
		t.Fatal("negative interval accepted")
	}
	floorRegistry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	moveClock(1_000)
	detachFloor, err := Attach(floorRegistry, &fakeShell{stdout: inPaneStdout()}, cordis.Discard{}, Config{RefreshIntervalMs: 5_000})
	if err != nil {
		t.Fatalf("attach floor: %v", err)
	}
	defer detachFloor()
	floorBuilt, floorSess := newLoopAgent(t, "tmux-2")
	injected := runPreStep(t, floorRegistry, floorBuilt, 1, 1, nil)
	if len(injected.Messages) != 1 {
		t.Fatalf("floor first = %+v", injected.Messages)
	}
	if _, err := floorSess.Append(session.EventUserMessage, injected.Messages[0], &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	moveClock(3_000)
	if again := runPreStep(t, floorRegistry, floorBuilt, 2, 1, nil); len(again.Messages) != 0 {
		t.Fatalf("floor skipped nothing: %+v", again.Messages)
	}
	moveClock(6_000)
	if again := runPreStep(t, floorRegistry, floorBuilt, 3, 1, nil); len(again.Messages) != 0 {
		// Same state after the floor: still suppressed — the floor only
		// gates changed states.
		t.Fatalf("unchanged state re-injected after floor: %+v", again.Messages)
	}
}

func TestAttachNoOpsWithoutShellOnRejectAndNonFirstSteps(t *testing.T) {
	// An absent executor service is a no-op.
	registry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	detach, err := Attach(registry, nil, cordis.Discard{}, Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	built, _ := newLoopAgent(t, "tmux-3")
	if decision := runPreStep(t, registry, built, 1, 1, nil); len(decision.Messages) != 0 {
		t.Fatalf("nil shell injected: %+v", decision.Messages)
	}

	// State is pulled once per turn, on the first step only.
	shellRegistry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	shell := &fakeShell{stdout: inPaneStdout()}
	detachShell, err := Attach(shellRegistry, shell, cordis.Discard{}, Config{})
	if err != nil {
		t.Fatalf("attach shell: %v", err)
	}
	defer detachShell()
	stepBuilt, _ := newLoopAgent(t, "tmux-7")
	if decision := runPreStep(t, shellRegistry, stepBuilt, 1, 2, nil); len(decision.Messages) != 0 {
		t.Fatalf("step 2 injected: %+v", decision.Messages)
	}
	if len(shell.commands) != 0 {
		t.Fatal("step 2 still queried the shell")
	}

	// An executor rejection is contained: the turn continues unchanged.
	failing := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	detachFail, err := Attach(failing, &fakeShell{err: errors.New("resolve rejected")}, &warnLogger{}, Config{})
	if err != nil {
		t.Fatalf("attach failing: %v", err)
	}
	defer detachFail()
	failBuilt, _ := newLoopAgent(t, "tmux-4")
	if decision := runPreStep(t, failing, failBuilt, 1, 1, nil); len(decision.Messages) != 0 {
		t.Fatalf("failed query injected: %+v", decision.Messages)
	}
}

func TestPrependsOntoDownstreamMessages(t *testing.T) {
	shell := &fakeShell{stdout: inPaneStdout()}
	registry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	detach, err := Attach(registry, shell, cordis.Discard{}, Config{})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	built, _ := newLoopAgent(t, "tmux-5")
	proposed := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}}
	decision := runPreStep(t, registry, built, 2, 1, proposed)
	if len(decision.Messages) != 2 {
		t.Fatalf("messages = %d", len(decision.Messages))
	}
	if decision.Messages[0].Source.Plugin != Name {
		t.Fatalf("reading not prepended: %+v", decision.Messages[0].Source)
	}
	if decision.Messages[1].Content[0].Text != "hello" {
		t.Fatalf("proposal lost: %+v", decision.Messages[1])
	}
}

func TestLatestInjectedStateScansDurableEvents(t *testing.T) {
	_, sess := newLoopAgent(t, "tmux-6")
	if _, _, ok := LatestInjectedState(sess); ok {
		t.Fatal("empty session has a previous state")
	}
	reading := "tmux location (turn 1):\nsession main, window 3 \"build\", pane 1 %7\nwindow active=1, pane active=1, layout L"
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: "text", Text: reading}},
		llm.MessageSource{Kind: llm.SourcePlugin, Plugin: Name, Form: "snapshot", Sections: []llm.ContextSnapshotSection{{Name: Name, Text: reading}}},
	)
	if _, err := sess.Append(session.EventUserMessage, message, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	state, at, ok := LatestInjectedState(sess)
	if !ok || !strings.HasPrefix(state, "session main") || at == 0 {
		t.Fatalf("state = %q %d %v", state, at, ok)
	}
	// Another plugin's user message does not count.
	other := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: "text", Text: "unrelated"}},
		llm.MessageSource{Kind: llm.SourcePlugin, Plugin: "someone-else"},
	)
	if _, err := sess.Append(session.EventUserMessage, other, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if state, _, _ = LatestInjectedState(sess); !strings.HasPrefix(state, "session main") {
		t.Fatalf("foreign injection read: %q", state)
	}
}

type warnLogger struct{}

func (warnLogger) Warn(args ...any)  {}
func (warnLogger) Error(args ...any) {}
func (warnLogger) Info(args ...any)  {}

// moveClock advances the clock seam by millis from its current reading.
func moveClock(millis int64) {
	base := nowMillis()
	nowMillis = func() int64 { return base + millis }
}
