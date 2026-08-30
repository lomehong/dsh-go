// Package userapproval ports packages/interaction/user-approval: the Service
// Definition of the approval capability seam — requests, cancellation, audit,
// and per-session policy. Missing answerers fail closed; grants apply only to
// the requested action.
//
// Go adaptations: the cordis `ctx.approval` service property is an explicit
// constructor bound to the agent registry (which owns both the scoped event
// bus and agent identity); the AbortSignal/answer race becomes pre- and
// post-dispatch signal checks on the synchronous waterfall (an answerer runs
// on the calling goroutine, so an abort observed after dispatch wins exactly
// like the source's raced promise); a throwing answerer is contained by a
// recover at the service instead of promise rejection; the system-prompt
// section install becomes PolicyContext because the Go AssembleContext carries
// no agent subject — the composition point registers it at the agent's scope.
package userapproval

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Event names owned by this package (registered into the session vocabulary
// at init, fail-closed for builds without the import).
const (
	// EventApprovalPolicy logs one durable session policy override.
	EventApprovalPolicy = "approval/policy"
	// EventApprovalAsked logs one question put to the answerer chain.
	EventApprovalAsked = "approval/asked"
	// EventApprovalDecided logs the outcome of a prior ask.
	EventApprovalDecided = "approval/decided"
)

// EventApprovalRequest asks composed answerers for one decision. It is an
// agent.SubjectEventBus waterfall: return an ApprovalOutcome to claim the
// request or call next to delegate.
const EventApprovalRequest = "approval/request"

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventApprovalPolicy, EventApprovalAsked, EventApprovalDecided)
}

// ApprovalOutcome is the closed approval outcome vocabulary: a one-shot
// grant, explicit rejection, withdrawn request, or unavailable answerer.
// Callers fail closed on unavailable.
type ApprovalOutcome string

// Approval outcomes.
const (
	OutcomeAllowedOnce ApprovalOutcome = "allowed-once"
	OutcomeRejected    ApprovalOutcome = "rejected"
	OutcomeCancelled   ApprovalOutcome = "cancelled"
	OutcomeUnavailable ApprovalOutcome = "unavailable"
)

// outcomes is every ApprovalOutcome, for runtime normalization of answerer
// returns.
var outcomes = map[ApprovalOutcome]bool{
	OutcomeAllowedOnce: true,
	OutcomeRejected:    true,
	OutcomeCancelled:   true,
	OutcomeUnavailable: true,
}

// ApprovalPolicy is a session's approval policy — what happens to an ask
// BEFORE any interactive answerer sees it: ask delegates to the composed
// answerers (fail-closed with none); never auto-rejects every ask without
// prompting (the deterministic CI/unattended stance).
type ApprovalPolicy string

// Approval policies.
const (
	PolicyAsk   ApprovalPolicy = "ask"
	PolicyNever ApprovalPolicy = "never"
)

// ApprovalPolicies is every ApprovalPolicy, for option advertisement and
// runtime validation of untrusted policy strings.
var ApprovalPolicies = []ApprovalPolicy{PolicyAsk, PolicyNever}

// Model-facing statement for the deterministic never policy.
const NeverSentence = "Approval prompts are disabled in this session: actions that require approval are rejected automatically — do not request sandbox escalation (do not set `sandbox_permissions`)."

// Model-facing statement for an interactive policy that may still fail closed.
const AskSentence = "Approval policy: ask. Operations that require approval may ask through the configured answerers; without an available answerer, the request fails closed."

// PolicyData is the approval/policy payload.
type PolicyData struct {
	Policy ApprovalPolicy `json:"policy"`
	// Source marks an override seeded into a child at delegation; an absent
	// source is a runtime switch.
	Source string `json:"source,omitempty"`
}

// AskedData is the approval/asked payload. ID pairs it with the
// approval/decided that always follows.
type AskedData struct {
	ID       string `json:"id"`
	ToolName string `json:"toolName"`
	// CallID is the exact tool call when the asker had one.
	CallID string `json:"callId,omitempty"`
	// Reason is the asker's human-readable explanation.
	Reason string `json:"reason,omitempty"`
}

// DecidedData is the approval/decided payload — exactly one per ask.
type DecidedData struct {
	ID      string          `json:"id"`
	Outcome ApprovalOutcome `json:"outcome"`
}

// effectiveApprovalPolicy folds the session log to the last approval/policy
// override; ok is false when the session never switched (callers apply the
// plugin's configured default). Replaying the log IS the state.
func effectiveApprovalPolicy(events []session.Event) (ApprovalPolicy, bool) {
	for index := len(events) - 1; index >= 0; index -= 1 {
		event := events[index]
		if event.Type != EventApprovalPolicy {
			continue
		}
		var data PolicyData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return "", false
		}
		return data.Policy, true
	}
	return "", false
}

// hasOpenTurn reports whether the log currently sits inside an open turn (a
// turn/start not yet closed by a turn/end) — the Request precondition. The
// audit pair must be turn-enclosed: a bare event appended between turns is
// indistinguishable from a crash tail and silently dropped on reload.
func hasOpenTurn(events []session.Event) bool {
	for index := len(events) - 1; index >= 0; index -= 1 {
		switch events[index].Type {
		case session.EventTurnStart:
			return true
		case session.EventTurnEnd:
			return false
		}
	}
	return false
}

// SetApprovalPolicy appends the sole durable representation of a session
// policy override. Invalid values fail before the log changes; consumers fold
// the new value on each read.
func SetApprovalPolicy(sess *session.Session, policy ApprovalPolicy) error {
	if !policyValid(policy) {
		return fmt.Errorf(`approval policy must be one of "ask" or "never"`)
	}
	_, err := sess.Append(EventApprovalPolicy, PolicyData{Policy: policy}, nil)
	return err
}

// EffectiveApprovalPolicy is the exported fold used by consumers that need
// the override without a service instance; the second return is false without
// an override.
func EffectiveApprovalPolicy(events []session.Event) (ApprovalPolicy, bool) {
	return effectiveApprovalPolicy(events)
}

func policyValid(policy ApprovalPolicy) bool {
	return policy == PolicyAsk || policy == PolicyNever
}

// ApprovalRequest is one readonly same-process permission question. CallID
// links to an already presented tool call, so arguments are not duplicated
// here.
type ApprovalRequest struct {
	// Agent is asked on whose behalf the question runs. It routes the
	// question (a UI answerer only answers for agents it owns) and receives
	// the audit events on its session log.
	Agent *agent.Agent
	// ToolName is the tool the question is about (presentation and audit).
	ToolName string
	// CallID is the exact tool call being decided, when the asker has one.
	CallID string
	// Reason is the asker's human-readable explanation of WHY it is asking.
	Reason string
	// Signal aborting withdraws the question: the request settles cancelled
	// and a late answer is discarded.
	Signal context.Context
}

// Config is the plugin config. The zero value means the default policy.
type Config struct {
	// Policy is the deployment default for sessions without an
	// approval/policy override.
	Policy ApprovalPolicy
}

// NewConfig validates untrusted policy input at the owning boundary.
func NewConfig(policy ApprovalPolicy) (Config, error) {
	if policy != "" && !policyValid(policy) {
		return Config{}, fmt.Errorf(`approval policy must be one of "ask" or "never"`)
	}
	if policy == "" {
		policy = PolicyAsk
	}
	return Config{Policy: policy}, nil
}

// Service applies session policy before answerers and logs every
// ask/outcome pair to the requesting session.
type Service struct {
	registry *agent.AgentRegistry
	config   Config
}

// NewService validates the config and binds the service to the registry
// whose scoped bus carries the answerer waterfall.
func NewService(registry *agent.AgentRegistry, config Config) (*Service, error) {
	if registry == nil {
		return nil, fmt.Errorf("userapproval: an agent registry is required")
	}
	if config.Policy == "" {
		config.Policy = PolicyAsk
	}
	if !policyValid(config.Policy) {
		return nil, fmt.Errorf(`approval policy must be one of "ask" or "never"`)
	}
	return &Service{registry: registry, config: config}, nil
}

// Policy is the configured deployment default.
func (s *Service) Policy() ApprovalPolicy { return s.config.Policy }

// OverrideOf reads the session override without applying the configured
// default; ok is false without one.
func (s *Service) OverrideOf(sess *session.Session) (ApprovalPolicy, bool) {
	return effectiveApprovalPolicy(sess.Events())
}

// EffectivePolicy is the session's effective policy: its own
// approval/policy fold, else the configured default.
func (s *Service) EffectivePolicy(sess *session.Session) ApprovalPolicy {
	if override, ok := s.OverrideOf(sess); ok {
		return override
	}
	return s.config.Policy
}

// SetPolicy switches one live agent's policy and queues the transition for
// its next model step. Session initialization uses SetApprovalPolicy
// directly because there is no previously visible policy to change.
func (s *Service) SetPolicy(a *agent.Agent, policy ApprovalPolicy) error {
	previous := s.EffectivePolicy(a.Session)
	if previous == policy {
		return nil
	}
	if err := SetApprovalPolicy(a.Session, policy); err != nil {
		return err
	}
	if driver := a.Driver(); driver != nil {
		driver.Inject(llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf(
			"The approval policy changed from %q to %q (changed by the user).", previous, policy,
		)}}, llm.MessageSource{Kind: llm.SourcePlugin, Plugin: "user-approval"}))
	}
	return nil
}

// Request asks the composed answerers to decide one request. It borrows the
// agent, session, and live signal directly. The request requires an open turn
// because the audit pair must be enclosed by the durable log's commit/replay
// boundary; an idle ask rejects before appending anything. The answerer phase
// always produces an outcome: an aborted signal yields cancelled, a missing
// or throwing answerer yields unavailable (fail closed), and a rogue
// non-vocabulary return value is normalized to unavailable. A failure that
// prevents either audit append from committing still returns an error,
// because returning an unlogged decision would violate the pair.
func (s *Service) Request(req ApprovalRequest) (ApprovalOutcome, error) {
	sess := req.Agent.Session
	if !hasOpenTurn(sess.Events()) {
		return "", fmt.Errorf(
			"approval.request() outside an open turn: the approval/asked + approval/decided audit pair " +
				"must be turn-enclosed (a bare event between turns is crash-tail garbage on reload). " +
				"Ask from inside the turn that needs the decision.")
	}
	id, err := newRequestID()
	if err != nil {
		return "", err
	}
	asked := AskedData{ID: id, ToolName: req.ToolName, CallID: req.CallID, Reason: req.Reason}
	if _, err := sess.Append(EventApprovalAsked, asked, nil); err != nil {
		return "", err
	}
	outcome := s.decide(req, sess)
	if _, err := sess.Append(EventApprovalDecided, DecidedData{ID: id, Outcome: outcome}, nil); err != nil {
		return "", err
	}
	return outcome, nil
}

// decide dispatches the waterfall, contained and raced against the request
// signal. The never policy is decided HERE, before any dispatch: only the
// service's own request path can keep the documented promise that never
// rejects deterministically regardless of listener registration order.
func (s *Service) decide(req ApprovalRequest, sess *session.Session) ApprovalOutcome {
	if req.Signal != nil && req.Signal.Err() != nil {
		return OutcomeCancelled
	}
	if s.EffectivePolicy(sess) == PolicyNever {
		return OutcomeRejected
	}
	// A throwing answerer must fail the QUESTION closed, not the caller's
	// tool call open — the seam contains its callbacks.
	outcome := containedDispatch(s.registry.Events(), req.Agent.Scope, req)
	// The type of an answer is guaranteed by the seam; its vocabulary value
	// is not. A rogue value normalizes to the fail-closed outcome instead of
	// leaking into callers' closed-union switches.
	decided := outcome
	if !outcomes[decided] {
		decided = OutcomeUnavailable
	}
	// An abort observed after the synchronous dispatch wins over a late
	// answer, exactly like the source's raced promise.
	if req.Signal != nil && req.Signal.Err() != nil {
		return OutcomeCancelled
	}
	return decided
}

// Approvals is the typed accessor for the approval request waterfall:
// registered answerers claim the request by returning their outcome. The
// type is compile-time closed; decide still normalizes the value against
// the outcome vocabulary. Register and dispatch this event name only
// through this accessor.
func Approvals(bus *agent.SubjectEventBus) agent.TypedWaterfall[ApprovalRequest, ApprovalOutcome] {
	return agent.NewTypedWaterfall[ApprovalRequest, ApprovalOutcome](bus, EventApprovalRequest)
}

// containedDispatch runs the typed approval waterfall with panic
// containment: a panicking answerer degrades to the fail-closed base.
func containedDispatch(bus *agent.SubjectEventBus, agentScope scope.ScopeKey, req ApprovalRequest) (result ApprovalOutcome) {
	defer func() {
		if rec := recover(); rec != nil {
			result = OutcomeUnavailable
		}
	}()
	return Approvals(bus).Dispatch(agentScope, req, func(ApprovalRequest) ApprovalOutcome { return OutcomeUnavailable })
}

// PolicyContext builds the "approval:policy" dynamic context section for one
// session. The complete current value travels after retained history, so
// switching policy does not rewrite the stable system-prompt cache prefix.
// Register it at the agent's scope; a bare Assemble (tests, diagnostics) has
// no session to state, which is why the section is per-session.
func PolicyContext(sess *session.Session, config Config, effective func(*session.Session) ApprovalPolicy) systemprompt.PromptContext {
	return systemprompt.PromptContext{
		Name:  "approval:policy",
		Order: 115,
		TextProvider: func(systemprompt.AssembleContext) string {
			policy := effective(sess)
			if policy == PolicyNever {
				return NeverSentence
			}
			return AskSentence
		},
	}
}

// ToolsSeam adapts the service to the tools runtime's synchronous approval
// seam: an unresolvable caller fails closed, and the outcome vocabularies map
// one-to-one.
func (s *Service) ToolsSeam() tools.ApprovalService {
	return approvalSeam{service: s}
}

type approvalSeam struct{ service *Service }

func (seam approvalSeam) Request(request tools.ApprovalRequest) tools.ApprovalOutcome {
	caller := seam.service.agentForScope(request.Agent)
	if caller == nil {
		return tools.ApprovalUnavailable
	}
	req := ApprovalRequest{
		Agent:    caller,
		ToolName: request.ToolName,
		CallID:   request.CallID,
		Reason:   request.Reason,
		Signal:   request.Signal,
	}
	outcome, err := seam.service.Request(req)
	if err != nil {
		// A refused ask (no open turn, failed audit append) fails the
		// question closed, never the tool call open.
		return tools.ApprovalUnavailable
	}
	return tools.ApprovalOutcome(outcome)
}

// agentForScope resolves one live agent by its scope key.
func (s *Service) agentForScope(target scope.ScopeKey) *agent.Agent {
	return s.registry.ByScope(target)
}

// newRequestID mints one fresh request id per Request call.
func newRequestID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("approval request id: %w", err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16]), nil
}
