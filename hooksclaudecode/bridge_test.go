package hooksclaudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/hookprotocol"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// recordingLogger captures warnings for assertions.
type recordingLogger struct {
	lines []string
}

func (l *recordingLogger) Info(args ...any) {}
func (l *recordingLogger) Warn(args ...any) {
	l.lines = append(l.lines, fmt.Sprint(args...))
}
func (l *recordingLogger) Error(args ...any) {
	l.lines = append(l.lines, fmt.Sprint(args...))
}

func (l *recordingLogger) warns() []string { return l.lines }

// nopNotifications satisfies the inbox's durability callbacks.
type nopNotifications struct{}

func (nopNotifications) Inserted(llm.Message)       {}
func (nopNotifications) Discarded(llm.Message)      {}
func (nopNotifications) Claimed(llm.Message, int64) {}

// fixture isolates one config file, live registry/runtime, and agent.
type fixture struct {
	t          *testing.T
	dir        string
	configPath string
	logger     *recordingLogger
	registry   *agent.AgentRegistry
	runtime    *tools.ToolRuntime
	agent      *agent.Agent
	sess       *session.Session
	dispose    func()
	clock      int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		t:      t,
		dir:    t.TempDir(),
		logger: &recordingLogger{},
	}
	f.configPath = filepath.Join(f.dir, "hooks.json")
	f.start()
	t.Cleanup(func() {
		if f.dispose != nil {
			f.dispose()
		}
	})
	return f
}

// start mounts the bridge with the current config file contents and a
// fresh agent bound to the new registry (each start replaces the previous
// registry/runtime/agent triple, so tests can reload the config).
func (f *fixture) start() {
	f.t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		f.t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, Config{
		ConfigPath:       f.configPath,
		DefaultTimeoutMs: 10_000,
		Logger:           f.logger,
		LocateTranscript: func(header *session.SessionHeader) string { return filepath.Join(f.dir, "transcript.jsonl") },
		Now:              func() int64 { f.clock += 5; return f.clock },
	})
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	f.registry = registry
	f.runtime = runtime
	f.dispose = dispose
	f.startAgent()
}

func (f *fixture) startAgent() {
	f.t.Helper()
	sess, err := session.NewDetached(session.SessionID("hook-agent"), nil, &session.SessionHeader{ID: session.SessionID("hook-agent"), CWD: f.dir})
	if err != nil {
		f.t.Fatalf("session: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nopNotifications{})
	if err != nil {
		f.t.Fatalf("inbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.registry.Events())
	if _, err := f.registry.Enter(built, nil); err != nil {
		f.t.Fatalf("enter: %v", err)
	}
	f.agent = built
	f.sess = sess
}

// writeConfig persists a hooks.json body.
func (f *fixture) writeConfig(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatalf("write config: %v", err)
	}
}

// observedHook carries what one stubbed hook run received and what it
// returned.
type observedHook struct {
	command string
	options hookprotocol.RunHookOptions
}

// withStubHooks replaces the runHook seam with a deterministic decoder:
// each configured command is looked up in `handlers`, whose value is the
// RAW process outcome (exit code, stdout, stderr) — the stub parses it
// through hookprotocol.ParseHookOutput exactly like the real runner,
// including the expected-event-name guard. Every run is recorded in
// `observed` (command + options) for substitution/env assertions.
func withStubHooks(t *testing.T, handlers map[string]hookprotocol.HookOutput) *[]observedHook {
	t.Helper()
	observed := &[]observedHook{}
	previous := runHook
	runHook = func(hook hookprotocol.CommandHook, options hookprotocol.RunHookOptions, now func() int64) hookprotocol.RunHookResult {
		*observed = append(*observed, observedHook{command: hook.Command, options: options})
		raw, ok := handlers[hook.Command]
		if !ok {
			raw = hookprotocol.HookOutput{}
		}
		output := hookprotocol.ParseHookOutput(raw.ExitCode, raw.Stdout, raw.Stderr, options.ExpectedEventName)
		return hookprotocol.RunHookResult{Output: output, DurationMs: 7}
	}
	t.Cleanup(func() { runHook = previous })
	return observed
}

// stubOutput builds a clean-exit structured stdout output.
func stubOutput(t *testing.T, structured map[string]any) hookprotocol.HookOutput {
	t.Helper()
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("marshal stub stdout: %v", err)
	}
	return hookprotocol.HookOutput{ExitCode: intPtr(0), Stdout: string(encoded)}
}

func intPtr(value int) *int { return &value }

// userTextMessage builds a claimed user turn message for pre-step runs.
func userTextMessage(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourceUser})
}

func runPreStep(f *fixture, step int64, claimed ...llm.Message) agent.PreStepDecision {
	f.t.Helper()
	return f.registry.Events().PreStep().Dispatch(f.agent.Scope, agent.PreStepPayload{
		Agent:    f.agent,
		Messages: claimed,
		Turn:     1,
		Step:     step,
		Signal:   context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(claimed) })
}

func blockText(block llm.ContentBlock) string {
	if block.Type != llm.BlockText {
		return ""
	}
	return block.Text
}

func messageTexts(messages []llm.Message) []string {
	texts := []string{}
	for _, message := range messages {
		for _, block := range message.Content {
			if text := blockText(block); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

// executeTool routes a tool call through the runtime as an agent-scoped
// call.
func (f *fixture) executeTool(name string) *tools.ToolExecutionResult {
	f.t.Helper()
	openSchema := true
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: name,
		Parameters: map[string]tools.PropSpec{
			"file_path": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "object", AdditionalProperties: &openSchema},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: "done"}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"ok": true}, nil
		},
	})
	if err != nil {
		f.t.Fatalf("define %s: %v", name, err)
	}
	if _, err := f.runtime.Register(definition); err != nil {
		f.t.Fatalf("register %s: %v", name, err)
	}
	return f.runtime.Execute(&tools.ToolExecutionInput{
		CallID:    "call-" + name,
		Name:      name,
		Arguments: map[string]any{"file_path": "x.txt"},
		Agent:     f.agent.Scope,
		Signal:    context.Background(),
	})
}

func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never became ready", what)
}

// hookEvents decodes the session's logged hook/invoked + hook/result pair.
func hookEvents(t *testing.T, sess *session.Session) []map[string]any {
	t.Helper()
	decoded := []map[string]any{}
	for _, event := range sess.Events() {
		if event.Type != hookprotocol.EventHookInvoked && event.Type != hookprotocol.EventHookResult {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatalf("decode %s: %v", event.Type, err)
		}
		data["type"] = event.Type
		decoded = append(decoded, data)
	}
	return decoded
}

// newRuntime wraps the tool runtime constructor with the test failure
// check.
func newRuntime(t *testing.T) (*tools.ToolRuntime, error) {
	t.Helper()
	return tools.NewToolRuntime(nil, tools.Config{})
}

func mustRuntime(t *testing.T) *tools.ToolRuntime {
	t.Helper()
	runtime, err := newRuntime(t)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return runtime
}
