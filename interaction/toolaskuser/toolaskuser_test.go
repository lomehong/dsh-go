package toolaskuser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/interaction/userquestions"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// answerRecorder is a controllable answerer waterfall listener.
type answerRecorder struct {
	mu      sync.Mutex
	claim   func(request userquestions.Request) userquestions.AskUserQuestionAnswer
	seen    []userquestions.Request
	panics  bool
	failure error
}

func (r *answerRecorder) listener(request userquestions.Request, next func(userquestions.Request) userquestions.QuestionDecision) userquestions.QuestionDecision {
	r.mu.Lock()
	r.seen = append(r.seen, request)
	panics := r.panics
	failure := r.failure
	claim := r.claim
	r.mu.Unlock()
	if panics {
		panic("ui exploded")
	}
	if failure != nil {
		return userquestions.QuestionDecision{Err: failure}
	}
	if claim == nil {
		// No claim and no delegation to offer: pass through to the base,
		// which fails the request with NO_PROVIDER.
		return next(request)
	}
	return userquestions.QuestionDecision{Answer: claim(request)}
}

func (r *answerRecorder) requests() []userquestions.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]userquestions.Request(nil), r.seen...)
}

// testWorld wires the registry, service, tool runtime, and the registered
// tool.
type testWorld struct {
	registry *agent.AgentRegistry
	service  *userquestions.Service
	runtime  *tools.ToolRuntime
	recorder *answerRecorder
	tool     *tools.ToolDefinition
	dispose  func()
	agent    *agent.Agent
}

func newTestWorld(t *testing.T) *testWorld {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	service := userquestions.NewService(registry)
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	disposeTool, err := Register(runtime, service)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(disposeTool)
	tool, ok := runtime.Get(Name, nil)
	if !ok {
		t.Fatalf("tool %q not registered", Name)
	}
	sess, err := session.NewDetached(session.SessionID("root"), nil, &session.SessionHeader{ID: session.SessionID("root")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return &testWorld{
		registry: registry, service: service, runtime: runtime,
		recorder: &answerRecorder{}, tool: tool, dispose: disposeTool, agent: built,
	}
}

func questionsArgs(t *testing.T, payload string) map[string]any {
	t.Helper()
	var parsed any
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return parsed.(map[string]any)
}

func TestToolSchemaAndRegistration(t *testing.T) {
	world := newTestWorld(t)
	if world.tool.Name != Name || world.tool.Description == "" {
		t.Fatalf("tool = %q", world.tool.Name)
	}
	// Missing questions fail schema validation before the body.
	_, err := world.tool.Execute(map[string]any{}, &tools.ToolRunContext{ToolExecution: tools.ToolExecution{CallID: "call-1"}})
	var argsErr *tools.ToolArgsError
	if !errors.As(err, &argsErr) {
		t.Fatalf("err = %v, want INVALID_ARGS", err)
	}
	// A duplicate registration fails loud.
	if _, err := Register(world.runtime, world.service); err == nil {
		t.Fatal("expected the duplicate registration to fail")
	}
}

func TestToolAsksAndReturnsAnswers(t *testing.T) {
	world := newTestWorld(t)
	userquestions.Requests(world.registry.Events()).On(world.agent.Scope, world.recorder.listener)
	world.recorder.claim = func(request userquestions.Request) userquestions.AskUserQuestionAnswer {
		return userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{
			{ID: "mode", Selected: []string{"Fast", "Safe"}, Custom: "also cheap"},
		}}
	}
	value, err := world.tool.Execute(questionsArgs(t, `{"questions":[
		{"id":"mode","question":"Pick a mode","header":"Mode","multi_select":true,
		 "options":[{"label":"Fast","description":"quick"},{"label":"Safe"}]}
	]}`), &tools.ToolRunContext{ToolExecution: tools.ToolExecution{
		CallID: "call-1", Agent: world.agent.Scope,
	}, Signal: context.Background()})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"selected":["Fast","Safe"]`) || !strings.Contains(string(encoded), `"custom":"also cheap"`) {
		t.Fatalf("result = %s", encoded)
	}
	seen := world.recorder.requests()
	if len(seen) != 1 {
		t.Fatalf("requests = %d", len(seen))
	}
	item := seen[0].Questions[0]
	if item.ID != "mode" || !item.MultiSelect || item.Header != "Mode" || len(item.Options) != 2 || item.Options[0].Description != "quick" {
		t.Fatalf("mapped question = %+v", item)
	}
	if seen[0].Agent != world.agent {
		t.Fatal("the live caller identity did not travel")
	}
}

func TestToolWithoutLiveAgentAsksUnscoped(t *testing.T) {
	world := newTestWorld(t)
	// A global-scope listener (no agent routing) still answers.
	unsubscribe := userquestions.Requests(world.registry.Events()).On(nil, func(userquestions.Request, func(userquestions.Request) userquestions.QuestionDecision) userquestions.QuestionDecision {
		return userquestions.QuestionDecision{Answer: userquestions.AskUserQuestionAnswer{Answers: []userquestions.AskUserQuestionAnswerItem{{ID: "go", Selected: []string{"Yes"}}}}}
	})
	defer unsubscribe()
	value, err := world.tool.Execute(questionsArgs(t, `{"questions":[{"id":"go","question":"Go?"}]}`),
		&tools.ToolRunContext{ToolExecution: tools.ToolExecution{CallID: "call-1"}})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), `"id":"go"`) {
		t.Fatalf("result = %s", encoded)
	}
}

func TestToolSurfacesAskAborted(t *testing.T) {
	world := newTestWorld(t)
	userquestions.Requests(world.registry.Events()).On(world.agent.Scope, world.recorder.listener)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := world.tool.Execute(questionsArgs(t, `{"questions":[{"id":"go","question":"Go?"}]}`),
		&tools.ToolRunContext{ToolExecution: tools.ToolExecution{CallID: "call-1"}, Signal: signal})
	if err == nil || !strings.Contains(err.Error(), "aborted before the user answered") {
		t.Fatalf("err = %v", err)
	}
}

func TestToolRenderSerializesValue(t *testing.T) {
	world := newTestWorld(t)
	blocks := world.tool.Render(nil, map[string]any{"answers": []any{map[string]any{"id": "a", "selected": []string{"x"}}}})
	if len(blocks) != 1 || blocks[0].Type != llm.BlockText {
		t.Fatalf("blocks = %+v", blocks)
	}
	if !strings.Contains(blocks[0].Text, `"id":"a"`) {
		t.Fatalf("render = %q", blocks[0].Text)
	}
}

func TestToolIsNotConcurrencySafe(t *testing.T) {
	world := newTestWorld(t)
	if world.tool.IsConcurrencySafe(map[string]any{}) {
		t.Fatal("ask_user_question must pause the tool lane")
	}
}
