package userquestions

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/session"
)

// newTestService builds one registry-bound service with one live agent.
func newTestService(t *testing.T) (*Service, *agent.AgentRegistry, *agent.Agent) {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	sess, err := session.NewDetached(session.SessionID("asker"), nil, &session.SessionHeader{ID: session.SessionID("asker")}, 0)
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
	return NewService(registry), registry, built
}

func oneQuestion() []AskUserQuestionItem {
	return []AskUserQuestionItem{{ID: "q1", Question: "Proceed?"}}
}

func TestEmptyQuestionsRejected(t *testing.T) {
	service, _, _ := newTestService(t)
	_, err := service.Ask(Request{})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeEmptyQuestions {
		t.Fatalf("err = %v, want EMPTY_QUESTIONS", err)
	}
	if questionErr.Error() != "ask_user_question requires at least one question" {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestPreAbortedSignalFailsClosed(t *testing.T) {
	service, _, _ := newTestService(t)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.Ask(Request{Questions: oneQuestion(), Signal: signal})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeAskAborted {
		t.Fatalf("err = %v, want ASK_ABORTED", err)
	}
}

func TestNoAnswererFailsWithNoProvider(t *testing.T) {
	service, _, _ := newTestService(t)
	_, err := service.Ask(Request{Questions: oneQuestion()})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeNoProvider {
		t.Fatalf("err = %v, want NO_PROVIDER", err)
	}
	if questionErr.Error() != "no user-questions answerer accepted the request" {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestAnswererClaimsRequest(t *testing.T) {
	service, registry, a := newTestService(t)
	unsubscribe := Requests(registry.Events()).On(a.Scope, func(request Request, next func(Request) QuestionDecision) QuestionDecision {
		if len(request.Questions) != 1 || request.Questions[0].ID != "q1" {
			t.Fatalf("questions = %+v", request.Questions)
		}
		if request.Agent != a {
			t.Fatal("agent identity missing in transit")
		}
		return QuestionDecision{Answer: AskUserQuestionAnswer{Answers: []AskUserQuestionAnswerItem{{ID: "q1", Selected: []string{"Yes"}}}}}
	})
	defer unsubscribe()
	answer, err := service.Ask(Request{Questions: oneQuestion(), Agent: a})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(answer.Answers) != 1 || answer.Answers[0].ID != "q1" || len(answer.Answers[0].Selected) != 1 || answer.Answers[0].Selected[0] != "Yes" {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestLiveCallerCheck(t *testing.T) {
	service, registry, a := newTestService(t)
	_ = a
	// A same-id agent instance that never entered the registry is not the
	// live one: Get(id) resolves to a different instance.
	ghostSession, err := session.NewDetached(session.SessionID("ghost"), nil, &session.SessionHeader{ID: session.SessionID("ghost")}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	ghostInbox, err := agent.NewInbox(ghostSession, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	ghost := agent.NewAgent(agent.AgentConfig{ID: ghostSession.ID(), Session: ghostSession, Inbox: ghostInbox}, registry.Events())
	_, err = service.Ask(Request{Questions: oneQuestion(), Agent: ghost})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeCallerNotLive {
		t.Fatalf("err = %v, want CALLER_NOT_LIVE", err)
	}
	if !strings.Contains(questionErr.Error(), "exact live calling agent") {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestOwnedCallerCannotAsk(t *testing.T) {
	service, registry, parent := newTestService(t)
	// Enter an owned child (owner = parent): it is live but not a root.
	childSession, err := session.NewDetached(session.SessionID("child"), nil, &session.SessionHeader{ID: session.SessionID("child")}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	childInbox, err := agent.NewInbox(childSession, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	child := agent.NewAgent(agent.AgentConfig{ID: childSession.ID(), Session: childSession, Inbox: childInbox}, registry.Events())
	detach, err := registry.Enter(child, parent)
	if err != nil {
		t.Fatalf("Enter child: %v", err)
	}
	defer detach()

	_, err = service.Ask(Request{Questions: oneQuestion(), Agent: child})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeDelegatedCaller {
		t.Fatalf("err = %v, want DELEGATED_CALLER", err)
	}
	if !strings.Contains(questionErr.Error(), "owned by another live agent") {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestBadIntentApproveLabelRejected(t *testing.T) {
	service, _, _ := newTestService(t)
	_, err := service.Ask(Request{Questions: []AskUserQuestionItem{{
		ID: "plan", Question: "Approve the plan?", Detail: "1. Do it.",
		Intent: &AskUserQuestionIntent{Kind: "plan-review", Approve: "ship it"},
	}}})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeBadIntent {
		t.Fatalf("err = %v, want BAD_INTENT", err)
	}
	if !strings.Contains(questionErr.Error(), `approve label "ship it" names none of its options`) {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestBadIntentWithoutDetailRejected(t *testing.T) {
	service, _, _ := newTestService(t)
	_, err := service.Ask(Request{Questions: []AskUserQuestionItem{{
		ID:      "plan",
		Options: []AskUserQuestionOption{{Label: "Approve"}, {Label: "Decline"}},
		Intent:  &AskUserQuestionIntent{Kind: "plan-review", Approve: "Approve"},
	}}})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeBadIntent {
		t.Fatalf("err = %v, want BAD_INTENT", err)
	}
	if !strings.Contains(questionErr.Error(), "without the detail it reviews") {
		t.Fatalf("message = %q", questionErr.Error())
	}
}

func TestValidPlanReviewIntentAsks(t *testing.T) {
	service, registry, a := newTestService(t)
	unsubscribe := Requests(registry.Events()).On(a.Scope, func(Request, func(Request) QuestionDecision) QuestionDecision {
		return QuestionDecision{Answer: AskUserQuestionAnswer{Answers: []AskUserQuestionAnswerItem{{ID: "plan", Selected: []string{"Approve"}}}}}
	})
	defer unsubscribe()
	answer, err := service.Ask(Request{
		Questions: []AskUserQuestionItem{{
			ID: "plan", Question: "Approve?", Detail: "1. Do it.",
			Options: []AskUserQuestionOption{{Label: "Approve"}, {Label: "Decline"}},
			Intent:  &AskUserQuestionIntent{Kind: "plan-review", Approve: "Approve"},
		}},
		Agent: a,
	})
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if answer.Answers[0].Selected[0] != "Approve" {
		t.Fatalf("answer = %+v", answer)
	}
}

func TestThrowingAnswererRestoresThroughAbortion(t *testing.T) {
	service, registry, a := newTestService(t)
	unsubscribe := Requests(registry.Events()).On(a.Scope, func(Request, func(Request) QuestionDecision) QuestionDecision {
		panic("ui exploded")
	})
	defer unsubscribe()
	// Without an abort the panic surfaces as an error (not a panic).
	_, err := service.Ask(Request{Questions: oneQuestion(), Agent: a})
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("err = %v, want the listener panic contained", err)
	}
	// With an abort racing the failure, the ask reports ASK_ABORTED.
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.Ask(Request{Questions: oneQuestion(), Agent: a, Signal: signal})
	var questionErr UserQuestionError
	if !asUserQuestionError(err, &questionErr) || questionErr.Code() != CodeAskAborted {
		t.Fatalf("err = %v, want ASK_ABORTED", err)
	}
}

func TestForeignAnswererErrorPropagates(t *testing.T) {
	service, registry, a := newTestService(t)
	unsubscribe := Requests(registry.Events()).On(a.Scope, func(Request, func(Request) QuestionDecision) QuestionDecision {
		return QuestionDecision{Err: context.DeadlineExceeded}
	})
	defer unsubscribe()
	if _, err := service.Ask(Request{Questions: oneQuestion(), Agent: a}); err != context.DeadlineExceeded {
		t.Fatalf("err = %v, want the foreign error preserved", err)
	}
}

func TestAgentByScopeResolution(t *testing.T) {
	service, _, a := newTestService(t)
	if got := service.AgentByScope(a.Scope); got != a {
		t.Fatalf("AgentByScope = %v", got)
	}
	if got := service.AgentByScope(nil); got != nil {
		t.Fatalf("nil scope resolved to %v", got)
	}
}
