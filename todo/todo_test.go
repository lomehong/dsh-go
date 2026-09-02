package todo

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/tools"
)

func item(content, status string) map[string]any {
	return map[string]any{"content": content, "status": status}
}

func TestToTodoListValidatesAndCanonicalizes(t *testing.T) {
	todos, err := ToTodoList([]any{
		item("  audit deps  ", StatusInProgress),
		item("write report", StatusPending),
	}, true)
	if err != nil {
		t.Fatalf("ToTodoList: %v", err)
	}
	if len(todos) != 2 || todos[0].Content != "audit deps" || todos[0].Status != StatusInProgress {
		t.Fatalf("todos = %+v", todos)
	}
	counts := CountsOf(todos)
	if counts.InProgress != 1 || counts.Pending != 1 || counts.Completed != 0 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestToTodoListRejectsBadValues(t *testing.T) {
	cases := []struct {
		name  string
		raw   []any
		allow bool
		want  string
	}{
		{"empty content", []any{item("   ", StatusPending)}, true,
			"invalid todo: `content` must be a non-empty string"},
		{"duplicate", []any{item("a", StatusPending), item("a", StatusCompleted)}, true,
			`invalid todos: duplicate content "a"`},
		{"parallel disabled", []any{item("a", StatusInProgress), item("b", StatusInProgress)}, false,
			"invalid todos: at most one task may be in_progress (got 2)"},
		{"parallel allowed", []any{item("a", StatusInProgress), item("b", StatusInProgress)}, true, ""},
		{"bad status", []any{item("a", "done")}, true,
			"invalid todo: `status` must be one of pending, in_progress, completed"},
	}
	for _, testCase := range cases {
		_, err := ToTodoList(testCase.raw, testCase.allow)
		if testCase.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", testCase.name, err)
			}
			continue
		}
		if err == nil || err.Error() != testCase.want {
			t.Fatalf("%s: err = %v, want %q", testCase.name, err, testCase.want)
		}
	}
}

func TestDescribeVariesOnlyActiveClause(t *testing.T) {
	head := "Record and update a structured task list for the current work."
	tail := "Statuses: `pending` (not started), `in_progress` (being worked on now), `completed` (finished)."
	parallel := Describe(true)
	single := Describe(false)
	for _, text := range []string{parallel, single} {
		if !strings.HasPrefix(text, head) || !strings.Contains(text, tail) {
			t.Fatalf("description %q missing the fixed head/tail", text)
		}
	}
	if !strings.Contains(parallel, "Mark every todo being actively worked") {
		t.Fatal("parallel description missing its clause")
	}
	if !strings.Contains(single, "Keep AT MOST ONE todo `in_progress` at a time") {
		t.Fatal("single description missing its clause")
	}
}

// newTestAgent mirrors the subagent depth test helper: one live
// registry-registered agent whose session accepts log-only appends between
// turns.
func newTestAgent(t *testing.T, registry *agent.AgentRegistry, id string) *agent.Agent {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id)}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built
}

// newRegistry builds one tool runtime for the tests.
func newRegistry(t *testing.T) *tools.ToolRuntime {
	t.Helper()
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	return runtime
}

func TestRegisterToolWritesSessionSnapshot(t *testing.T) {
	agents := agent.NewAgentRegistry(nil, nil)
	runtime := newRegistry(t)
	a := newTestAgent(t, agents, "todo-owner")
	if _, err := Register(runtime, agents, nil, Config{AllowParallelInProgress: false}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	definition, ok := runtime.Get(Name, a.Scope)
	if !ok {
		t.Fatal("the tool was not registered")
	}
	value, err := definition.Execute(map[string]any{
		"todos": []any{item("step one", StatusCompleted), item("step two", StatusInProgress)},
	}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: a.Scope}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	structured := value.(map[string]any)
	counts := structured["counts"].(Counts)
	if counts.Completed != 1 || counts.InProgress != 1 {
		t.Fatalf("counts = %+v", counts)
	}
	// The session log carries the whole-list snapshot.
	events := a.Session.Events()
	last := events[len(events)-1]
	if last.Type != EventTodoWrite {
		t.Fatalf("event type = %q", last.Type)
	}
	if !strings.Contains(string(last.Data), `"content":"step two"`) {
		t.Fatalf("payload = %s", last.Data)
	}
}

func TestRegisterToolRejectsNonAgentCaller(t *testing.T) {
	agents := agent.NewAgentRegistry(nil, nil)
	runtime := newRegistry(t)
	if _, err := Register(runtime, agents, nil, Config{AllowParallelInProgress: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// A scope with no live agent (or none at all).
	definition, ok := runtime.Get(Name, nil)
	if !ok {
		t.Fatal("the tool was not registered")
	}
	if _, err := definition.Execute(map[string]any{
		"todos": []any{item("step one", StatusPending)},
	}, &tools.ToolRunContext{}); err == nil || err.Error() != "todo_write requires an owning agent session" {
		t.Fatalf("err = %v, want the owning-session rejection", err)
	}
}

func TestRegisterToolEnforcesParallelPolicy(t *testing.T) {
	agents := agent.NewAgentRegistry(nil, nil)
	runtime := newRegistry(t)
	a := newTestAgent(t, agents, "policy-owner")
	if _, err := Register(runtime, agents, nil, Config{AllowParallelInProgress: false}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	definition, _ := runtime.Get(Name, a.Scope)
	_, err := definition.Execute(map[string]any{
		"todos": []any{item("a", StatusInProgress), item("b", StatusInProgress)},
	}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: a.Scope}})
	if err == nil || err.Error() != "invalid todos: at most one task may be in_progress (got 2)" {
		t.Fatalf("err = %v, want the parallel rejection", err)
	}
	// The rejected call must not have written anything.
	if events := a.Session.Events(); len(events) > 0 && events[len(events)-1].Type == EventTodoWrite {
		t.Fatal("the rejected call still appended a snapshot")
	}
}

func TestRenderAnnouncesCounts(t *testing.T) {
	agents := agent.NewAgentRegistry(nil, nil)
	runtime := newRegistry(t)
	a := newTestAgent(t, agents, "render-owner")
	if _, err := Register(runtime, agents, nil, Config{AllowParallelInProgress: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	definition, _ := runtime.Get(Name, a.Scope)
	value, err := definition.Execute(map[string]any{
		"todos": []any{item("done thing", StatusCompleted), item("wip thing", StatusInProgress), item("later", StatusPending)},
	}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: a.Scope}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	rendered := definition.Render(map[string]any{}, value)
	if len(rendered) != 1 || rendered[0].Type != llm.BlockText {
		t.Fatalf("rendered = %+v", rendered)
	}
	if rendered[0].Text != "Updated todo list: 1 pending, 1 in progress, 1 completed." {
		t.Fatalf("text = %q", rendered[0].Text)
	}
}

func TestTodosProjectionFold(t *testing.T) {
	agents := agent.NewAgentRegistry(nil, nil)
	runtime := newRegistry(t)
	projections := projection.NewRegistry()
	a := newTestAgent(t, agents, "projection-owner")
	if _, err := Register(runtime, agents, projections, Config{AllowParallelInProgress: true}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	definition, _ := runtime.Get(Name, a.Scope)
	if _, err := definition.Execute(map[string]any{
		"todos": []any{item("first", StatusCompleted)},
	}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: a.Scope}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := definition.Execute(map[string]any{
		"todos": []any{item("second", StatusPending)},
	}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{Agent: a.Scope}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The unit was registered under the declared key; drive its pure fold
	// over the recorded log (the same input the eager registry fold sees).
	def := todosProjectionDefinition()
	if def.Key != ProjectionKey || def.StateVersion != 2 {
		t.Fatalf("definition = %+v", def)
	}
	var state any
	for _, event := range a.Session.Events() {
		state = def.Apply(state, event)
	}
	todos, ok := state.([]TodoItem)
	if !ok || len(todos) != 1 || todos[0].Content != "second" {
		t.Fatalf("state = %+v, want last-write-wins", state)
	}
	// A later turn/start clears the finished checklist: the state is the
	// zero list (typed-unit contract).
	state = def.Apply(state, session.Event{Type: session.EventTurnStart, Data: []byte(`{"turn":2}`)})
	todos, ok = state.([]TodoItem)
	if !ok || len(todos) != 0 {
		t.Fatalf("state after turn/start = %+v, want the zero list", state)
	}
}

func TestTodosProjectionDecodeState(t *testing.T) {
	def := todosProjectionDefinition()
	state, err := def.DecodeState([]byte(`[{"content":"a","status":"completed"}]`))
	if err != nil {
		t.Fatalf("DecodeState: %v", err)
	}
	todos := state.([]TodoItem)
	if len(todos) != 1 || todos[0].Content != "a" {
		t.Fatalf("state = %+v", todos)
	}
	if _, err := def.DecodeState([]byte(`42`)); err == nil {
		t.Fatal("expected the scalar row to be rejected")
	}
}
