// Tests for the commands Remote namespace: the session-scoped catalog and
// the execute endpoint with the camelCase result projection.
package gateway

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/session"
)

// newCommandsController builds the namespace over a real runtime with one
// live agent and two registered commands.
func newCommandsController(t *testing.T) (*CommandsController, *commands.CommandRuntime, *agent.AgentRegistry, session.SessionID) {
	t.Helper()
	root := cordis.NewRoot(cordis.Discard{})
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, commands.CommandDefinition{
		Name: "greet", Description: "says hello",
		Handler: func(commands.Invocation) (commands.CommandResult, error) {
			return commands.CommandResult{Kind: commands.ResultSuccess, Text: "hello"}, nil
		},
	}); err != nil {
		t.Fatalf("register greet: %v", err)
	}
	if _, err := runtime.Register(nil, commands.CommandDefinition{
		Name: "aardvark", Description: "alphabetical",
		Input: &commands.CommandInputDescriptor{Hint: "name", Images: true},
		Handler: func(commands.Invocation) (commands.CommandResult, error) {
			return commands.CommandResult{Kind: commands.ResultError, HasText: true, Text: "failed"}, nil
		},
	}); err != nil {
		t.Fatalf("register aardvark: %v", err)
	}
	sess, err := session.NewDetached(session.SessionID("session-cmds"), nil,
		&session.SessionHeader{Version: session.SESSION_FORMAT_VERSION, ID: session.SessionID("session-cmds")}, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	live := agent.NewAgent(agent.AgentConfig{
		ID: "session-cmds", Session: sess, Ctx: root.Child(),
	}, registry.Events())
	if _, err := registry.Register(live); err != nil {
		t.Fatalf("register agent: %v", err)
	}
	controller := NewCommandsController(
		func() any { return runtime },
		func() *agent.AgentRegistry { return registry },
	)
	return controller, runtime, registry, "session-cmds"
}

// The catalog renders name-ordered descriptors with the input descriptor
// projected (hint + images flag).
func TestCommandsListRendersCatalog(t *testing.T) {
	controller, _, _, sessionID := newCommandsController(t)
	value, err := controller.List(context.Background(), map[string]any{"sessionId": string(sessionID)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	rows, ok := value.([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("value = %#v", value)
	}
	first := rows[0].(map[string]any)
	if first["name"] != "aardvark" {
		t.Fatalf("rows[0] = %#v, want name-ordered", first)
	}
	input := first["input"].(map[string]any)
	if input["hint"] != "name" || input["images"] != true {
		t.Fatalf("input = %#v", input)
	}
}

// A cold session answers session/not-found.
func TestCommandsListAnswersNotFoundForColdSession(t *testing.T) {
	controller, _, _, _ := newCommandsController(t)
	_, err := controller.List(context.Background(), map[string]any{"sessionId": "session-missing"})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/not-found" {
		t.Fatalf("err = %v, want session/not-found", err)
	}
}

// Execute returns the lifecycle pairing id and the camelCase result.
func TestCommandsExecuteProjectsResult(t *testing.T) {
	controller, _, _, sessionID := newCommandsController(t)
	value, err := controller.Execute(context.Background(), map[string]any{
		"sessionId": string(sessionID), "line": "/greet",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	execution := value.(map[string]any)
	if execution["commandId"] == "" || !strings.HasPrefix(execution["commandId"].(string), "cmd-") {
		t.Fatalf("execution = %#v", execution)
	}
	result := execution["result"].(map[string]any)
	if result["kind"] != "success" || result["text"] != "hello" {
		t.Fatalf("result = %#v", result)
	}
}

// A handler ERROR RESULT settles as an admitted execution carrying the
// error outcome (the composer keeps the draft; the host logged the
// lifecycle).
func TestCommandsExecuteCarriesHandlerErrorResult(t *testing.T) {
	controller, _, _, sessionID := newCommandsController(t)
	value, err := controller.Execute(context.Background(), map[string]any{
		"sessionId": string(sessionID), "line": "/aardvark",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	result := value.(map[string]any)["result"].(map[string]any)
	if result["kind"] != "error" || result["text"] != "failed" {
		t.Fatalf("result = %#v", result)
	}
}

// An unknown command is an admission miss: no lifecycle logged, loud error.
func TestCommandsExecuteRejectsUnknownCommand(t *testing.T) {
	controller, _, _, sessionID := newCommandsController(t)
	_, err := controller.Execute(context.Background(), map[string]any{
		"sessionId": string(sessionID), "line": "/nope",
	})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v, want the admission miss", err)
	}
}
