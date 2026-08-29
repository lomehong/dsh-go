package hookscodex

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

func (l *recordingLogger) Info(args ...any)  {}
func (l *recordingLogger) Error(args ...any) {}
func (l *recordingLogger) Warn(args ...any)  { l.lines = append(l.lines, fmt.Sprint(args...)) }
func (l *recordingLogger) warns() []string   { return l.lines }

// nopNotifications satisfies the inbox's durability callbacks.
type nopNotifications struct{}

func (nopNotifications) Inserted(llm.Message)       {}
func (nopNotifications) Discarded(llm.Message)      {}
func (nopNotifications) Claimed(llm.Message, int64) {}

// observedHook carries what one stubbed hook run received.
type observedHook struct {
	command string
	options hookprotocol.RunHookOptions
}

// fixture isolates one config file, live registry/runtime, and agent. Each
// start() replaces the registry/runtime/agent triple so tests can reload
// the config.
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
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), logger: &recordingLogger{}}
	f.configPath = filepath.Join(f.dir, "hooks.json")
	f.start()
	t.Cleanup(func() {
		if f.dispose != nil {
			f.dispose()
		}
	})
	return f
}

func (f *fixture) start() {
	f.t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		f.t.Fatalf("runtime: %v", err)
	}
	dispose, err := Apply(registry, runtime, Config{
		ConfigPath:       f.configPath,
		Model:            "gpt-5-codex",
		DefaultTimeoutMs: 10_000,
		Logger:           f.logger,
		LocateTranscript: func(header *session.SessionHeader) string { return filepath.Join(f.dir, "rollout.jsonl") },
		Now:              func() int64 { return 0 },
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
	sess, err := session.NewDetached(session.SessionID("codex-agent"), nil, &session.SessionHeader{ID: session.SessionID("codex-agent"), CWD: f.dir})
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

func (f *fixture) writeConfig(body string) {
	f.t.Helper()
	if err := os.WriteFile(f.configPath, []byte(body), 0o644); err != nil {
		f.t.Fatalf("write config: %v", err)
	}
}

// withStubHooks replaces the runHook seam: the handler value is the raw
// process outcome, parsed exactly like the real runner. Every run is
// recorded for payload/env assertions.
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
		return hookprotocol.RunHookResult{Output: output, DurationMs: 3}
	}
	t.Cleanup(func() { runHook = previous })
	return observed
}

func rawOutput(exit int, stdout, stderr string) hookprotocol.HookOutput {
	return hookprotocol.HookOutput{ExitCode: &exit, Stdout: stdout, Stderr: stderr}
}

// denyOutput blocks with a reason (exit 2 + stderr).
func denyOutput() hookprotocol.HookOutput { return rawOutput(2, "", "codex-deny") }

func stubOutput(t *testing.T, structured map[string]any) hookprotocol.HookOutput {
	t.Helper()
	encoded, err := json.Marshal(structured)
	if err != nil {
		t.Fatalf("marshal stub stdout: %v", err)
	}
	return rawOutput(0, string(encoded), "")
}

func intPtr(value int) *int { return &value }

func userTextMessage(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourceUser})
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

func runPreStep(f *fixture, turn int64, claimed ...llm.Message) agent.PreStepDecision {
	f.t.Helper()
	return f.registry.Events().PreStep().Dispatch(f.agent.Scope, agent.PreStepPayload{
		Agent:    f.agent,
		Messages: claimed,
		Turn:     turn,
		Step:     1,
		Signal:   context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(claimed) })
}

// executeTool registers and runs a tool that carries a `command` argument
// (the shape Codex's tool_input derives from).
func (f *fixture) executeTool(name, command string, arguments map[string]any) *tools.ToolExecutionResult {
	f.t.Helper()
	if arguments == nil {
		arguments = map[string]any{}
	}
	arguments["command"] = command
	openSchema := true
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: name,
		Parameters: map[string]tools.PropSpec{
			"command": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "object", AdditionalProperties: &openSchema},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: "ran"}}
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
		Arguments: arguments,
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

func newRuntime(t *testing.T) (*tools.ToolRuntime, error) {
	t.Helper()
	return tools.NewToolRuntime(nil, tools.Config{})
}
