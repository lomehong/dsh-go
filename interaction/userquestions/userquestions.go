// Package userquestions ports packages/interaction/user-questions: the
// `ctx.userQuestions` capability seam — a UI-backed service for pausing an
// agent tool call until the human answers a question. The model-facing tool
// lives in toolaskuser; UI packages compose answerers on the agent-scoped
// event bus waterfall.
//
// Go adaptations: the cordis `ctx.userQuestions` service property is an
// explicit constructor bound to the agent registry (identity checks + scoped
// waterfall); a listener panic is contained at the call boundary and mapped
// through the same error-restoration path as a thrown answerer.
package userquestions

import (
	"context"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/scope"
)

// EventUserQuestionsRequest asks composed answerers for structured user
// input. It is an agent.SubjectEventBus waterfall: return an answer to claim
// the request or call next to delegate.
const EventUserQuestionsRequest = "user-questions/request"

// UserQuestionError is the stable error taxonomy for user-questions failures.
type UserQuestionError struct {
	err *llm.Error
}

func newUserQuestionError(message, code string, cause error) UserQuestionError {
	return UserQuestionError{err: llm.NewError(code, message, cause)}
}

// UserQuestionError restorers must survive error wrapping: the service
// restores this type by code when an answerer's error crosses the boundary.
func (e UserQuestionError) Error() string { return e.err.Error() }

// Code is the stable machine-routable code.
func (e UserQuestionError) Code() string { return e.err.Code() }

// Unwrap reaches the harness error chain.
func (e UserQuestionError) Unwrap() error { return e.err }

// User question error codes.
const (
	CodeAskAborted      = "ASK_ABORTED"
	CodeEmptyQuestions  = "EMPTY_QUESTIONS"
	CodeCallerNotLive   = "CALLER_NOT_LIVE"
	CodeDelegatedCaller = "DELEGATED_CALLER"
	CodeBadIntent       = "BAD_INTENT"
	CodeNoProvider      = "NO_PROVIDER"
)

func abortedQuestion(cause error) UserQuestionError {
	return newUserQuestionError("ask_user_question was aborted before the user answered", CodeAskAborted, cause)
}

// AskUserQuestionOption is one selectable answer offered to the user.
type AskUserQuestionOption struct {
	// Label is the user-facing label.
	Label string `json:"label"`
	// Description is optional extra context rendered by capable UIs.
	Description string `json:"description,omitempty"`
}

// AskUserQuestionIntent is a caller-declared presentation intent: the
// question IS this kind of decision, so a UI that recognises the tag may
// present it as such instead of as a generic option list. An intent changes
// presentation only, never the protocol — the answer encoding is identical
// either way.
type AskUserQuestionIntent struct {
	// Kind tags the presentation family; "plan-review" is the only kind:
	// detail is the plan markdown Ask requires, and the decision approves or
	// declines it.
	Kind string `json:"kind"`
	// Approve is the option label that approves the plan; every other option
	// declines it. Named rather than positional so no UI infers the verdict
	// from option order. A label naming no option of its own question is
	// rejected at Ask.
	Approve string `json:"approve"`
}

// AskUserQuestionItem is one question in a request.
type AskUserQuestionItem struct {
	// ID is the stable caller-provided question id, echoed in the answer.
	ID string `json:"id"`
	// Question is the question to display.
	Question string `json:"question"`
	// Detail is optional supporting detail rendered with the question but
	// kept out of option labels.
	Detail string `json:"detail,omitempty"`
	// Header is an optional short heading/group label.
	Header string `json:"header,omitempty"`
	// Options are the choices the UI can render as a menu.
	Options []AskUserQuestionOption `json:"options,omitempty"`
	// MultiSelect allows more than one option to be selected. Defaults to
	// single-select.
	MultiSelect bool `json:"multiSelect,omitempty"`
	// Intent is the optional presentation intent for capable UIs; absent
	// asks for the generic option list.
	Intent *AskUserQuestionIntent `json:"intent,omitempty"`
}

// AskUserQuestionAnswerItem is the answer to one question.
type AskUserQuestionAnswerItem struct {
	// ID is the answered question id.
	ID string `json:"id"`
	// Selected holds the chosen option labels; it may accompany custom text
	// for a multi-select question.
	Selected []string `json:"selected"`
	// Custom is the optional free-text "Other" answer.
	Custom string `json:"custom,omitempty"`
}

// AskUserQuestionAnswer is the human's answer.
type AskUserQuestionAnswer struct {
	// Answers are the structured answers keyed by question id.
	Answers []AskUserQuestionAnswerItem `json:"answers"`
}

// Request asks for a human answer.
type Request struct {
	// Questions are the questions to display.
	Questions []AskUserQuestionItem
	// Agent routes the request: a UI answerer only answers for agents it
	// owns, and human interaction is valid only for the exact live runtime
	// root — runtime ownership, not durable session lineage, decides this
	// boundary.
	Agent *agent.Agent
	// Signal is the cancellation lifetime of the pending request.
	Signal context.Context
}

// Service validates requests and dispatches the scoped answerer waterfall.
type Service struct {
	registry *agent.AgentRegistry
}

// NewService binds the service to the registry whose live identity and
// scoped bus it consults.
func NewService(registry *agent.AgentRegistry) *Service {
	return &Service{registry: registry}
}

// Ask the scoped answerer waterfall and wait for the user's answer.
//
// When a caller supplies an agent, human interaction is valid only for the
// exact live runtime root. An owned child has no human answerer and would
// block forever, while a lineage-bearing session resumed as a new runtime
// root may ask normally.
func (s *Service) Ask(request Request) (answer AskUserQuestionAnswer, err error) {
	if request.Signal != nil && request.Signal.Err() != nil {
		return AskUserQuestionAnswer{}, abortedQuestion(nil)
	}
	if len(request.Questions) == 0 {
		return AskUserQuestionAnswer{}, newUserQuestionError(
			"ask_user_question requires at least one question", CodeEmptyQuestions, nil)
	}
	if request.Agent != nil {
		if s.registry.Get(request.Agent.ID) != request.Agent {
			return AskUserQuestionAnswer{}, newUserQuestionError(
				"human interaction requires the exact live calling agent when an agent is supplied",
				CodeCallerNotLive, nil)
		}
		rooted := false
		for _, root := range s.registry.Roots() {
			if root == request.Agent {
				rooted = true
				break
			}
		}
		if !rooted {
			return AskUserQuestionAnswer{}, newUserQuestionError(
				"human interaction is unavailable while the calling agent is owned by another live agent; "+
					"include the unresolved question or decision in the child agent's final result",
				CodeDelegatedCaller, nil)
		}
	}
	// A presentation intent asserts two things the types cannot: that the
	// named approve label is one of this question's own options, and that a
	// plan-review carries the plan it is a review of. A UI honouring the
	// intent answers with that label, and shows that detail as the plan, so
	// either gap would put a choice the asker never offered — or an approval
	// of something invisible — in front of the user. Caught at the asker,
	// where the mistake is, rather than in each UI.
	for _, question := range request.Questions {
		if question.Intent == nil {
			continue
		}
		approved := false
		for _, option := range question.Options {
			if option.Label == question.Intent.Approve {
				approved = true
				break
			}
		}
		if !approved {
			return AskUserQuestionAnswer{}, newUserQuestionError(fmt.Sprintf(
				"question %s declares intent %s whose approve label %q names none of its options",
				question.ID, question.Intent.Kind, question.Intent.Approve), CodeBadIntent, nil)
		}
		if question.Detail == "" {
			return AskUserQuestionAnswer{}, newUserQuestionError(fmt.Sprintf(
				"question %s declares intent %s without the detail it reviews",
				question.ID, question.Intent.Kind), CodeBadIntent, nil)
		}
	}
	answer, err = s.dispatch(request)
	if err != nil {
		// A thrown answerer error crossing the boundary keeps its identity;
		// a foreign failure during an aborted ask reports the abort.
		var questionErr UserQuestionError
		if asUserQuestionError(err, &questionErr) {
			return AskUserQuestionAnswer{}, questionErr
		}
		if request.Signal != nil && request.Signal.Err() != nil {
			return AskUserQuestionAnswer{}, abortedQuestion(err)
		}
		return AskUserQuestionAnswer{}, err
	}
	return answer, nil
}

// dispatch runs the contained waterfall. A listener panic restores through
// the same error path as a thrown one; a delegation to the base means no
// answerer accepted the request.
func (s *Service) dispatch(request Request) (answer AskUserQuestionAnswer, err error) {
	fallback := func(any) any {
		return newUserQuestionError("no user-questions answerer accepted the request", CodeNoProvider, nil)
	}
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("user-questions answerer panicked: %v", rec)
		}
	}()
	var agentScope scope.ScopeKey
	if request.Agent != nil {
		agentScope = request.Agent.Scope
	}
	result := s.registry.Events().Waterfall(EventUserQuestionsRequest, agentScope, request, fallback)
	switch typed := result.(type) {
	case AskUserQuestionAnswer:
		return typed, nil
	case *AskUserQuestionAnswer:
		if typed != nil {
			return *typed, nil
		}
	case UserQuestionError:
		return AskUserQuestionAnswer{}, typed
	case *UserQuestionError:
		if typed != nil {
			return AskUserQuestionAnswer{}, *typed
		}
	case error:
		return AskUserQuestionAnswer{}, typed
	}
	// A claim must be a recognizable answer; a foreign return value fails
	// closed instead of feeding the model a fabricated structure.
	return AskUserQuestionAnswer{}, newUserQuestionError(
		"user-questions answerer returned an unrecognized answer shape", CodeNoProvider, nil)
}

// AgentByScope resolves one live agent by its scope key, for consumers that
// only carry a tools ScopeKey (the model-facing tool).
func (s *Service) AgentByScope(target scope.ScopeKey) *agent.Agent {
	for _, candidate := range s.registry.List() {
		if candidate.Scope == target {
			return candidate
		}
	}
	return nil
}

func asUserQuestionError(err error, target *UserQuestionError) bool {
	for err != nil {
		if questionErr, ok := err.(UserQuestionError); ok {
			*target = questionErr
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
