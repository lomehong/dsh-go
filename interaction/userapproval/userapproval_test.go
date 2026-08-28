package userapproval

import (
	"context"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/session"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// newTestService builds one registry-bound service with one live agent
// carrying a fresh detached session.
func newTestService(t *testing.T, config Config) (*Service, *agent.AgentRegistry, *agent.Agent) {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, nil)
	sess, err := session.NewDetached(session.SessionID("agent-1"), nil, &session.SessionHeader{ID: session.SessionID("agent-1")})
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
	service, err := NewService(registry, config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return service, registry, built
}

// openTurn appends the turn boundary the audit pair requires.
func openTurn(t *testing.T, a *agent.Agent) {
	t.Helper()
	if _, err := a.Session.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
}

// lastEvents returns the log tail as (type, decoded payload) pairs.
func eventOf(t *testing.T, a *agent.Agent, eventType string) session.Event {
	t.Helper()
	for index := len(a.Session.Events()) - 1; index >= 0; index -= 1 {
		if a.Session.Events()[index].Type == eventType {
			return a.Session.Events()[index]
		}
	}
	t.Fatalf("no %q event in the log", eventType)
	return session.Event{}
}

func TestRequestFailsClosedWithoutAnswerers(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	openTurn(t, a)
	outcome, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable (fail closed)", outcome)
	}
	asked := eventOf(t, a, EventApprovalAsked)
	var askedData AskedData
	if err := unmarshalForTest(asked.Data, &askedData); err != nil {
		t.Fatalf("decode asked: %v", err)
	}
	if askedData.ToolName != "shell" || askedData.ID == "" {
		t.Fatalf("asked payload = %+v", askedData)
	}
	decided := eventOf(t, a, EventApprovalDecided)
	var decidedData DecidedData
	if err := unmarshalForTest(decided.Data, &decidedData); err != nil {
		t.Fatalf("decode decided: %v", err)
	}
	if decidedData.ID != askedData.ID || decidedData.Outcome != OutcomeUnavailable {
		t.Fatalf("decided payload = %+v, want pairing with %q", decidedData, askedData.ID)
	}
}

func TestRequestOutsideOpenTurnRefusesBeforeAppending(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	before := len(a.Session.Events())
	_, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"})
	if err == nil {
		t.Fatal("expected the open-turn precondition to refuse")
	}
	const needle = "must be turn-enclosed"
	if !strings.Contains(err.Error(), needle) {
		t.Fatalf("error = %v, want it to mention %q", err, needle)
	}
	if len(a.Session.Events()) != before {
		t.Fatalf("audit events leaked outside a turn: %d appended", len(a.Session.Events())-before)
	}
}

func TestNeverPolicyRejectsDeterministically(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	called := false
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(payload any, next func(any) any) any {
		called = true
		return OutcomeAllowedOnce
	})
	defer unsubscribe()
	if err := SetApprovalPolicy(a.Session, PolicyNever); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	outcome, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// The never policy is decided before any dispatch, regardless of
	// listener registration order.
	if called {
		t.Fatal("answerer was consulted under the never policy")
	}
	if outcome != OutcomeRejected {
		t.Fatalf("outcome = %q, want rejected", outcome)
	}
}

func TestAnswererWaterfallClaimsAndCarriesContext(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	var seen map[string]any
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(payload any, next func(any) any) any {
		request := payload.(ApprovalRequest)
		seen = map[string]any{"toolName": request.ToolName, "callId": request.CallID, "reason": request.Reason}
		return OutcomeAllowedOnce
	})
	defer unsubscribe()
	outcome, err := service.Request(ApprovalRequest{
		Agent: a, ToolName: "shell", CallID: "call-9", Reason: "runs rm -rf",
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeAllowedOnce {
		t.Fatalf("outcome = %q, want allowed-once", outcome)
	}
	if seen["toolName"] != "shell" || seen["callId"] != "call-9" || seen["reason"] != "runs rm -rf" {
		t.Fatalf("answerer saw %+v", seen)
	}
}

func TestRogueAnswererNormalizesToUnavailable(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(any, func(any) any) any {
		return "banana"
	})
	defer unsubscribe()
	outcome, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want unavailable", outcome)
	}
}

func TestThrowingAnswererFailsQuestionClosed(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(any, func(any) any) any {
		panic("answerer exploded")
	})
	defer unsubscribe()
	outcome, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want the panic contained to unavailable", outcome)
	}
}

func TestScopeFiltering(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	// One agent-scoped answerer for agent-1 only.
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(any, func(any) any) any {
		return OutcomeAllowedOnce
	})
	defer unsubscribe()

	sess2, err := session.NewDetached(session.SessionID("agent-2"), nil, &session.SessionHeader{ID: session.SessionID("agent-2")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox2, err := agent.NewInbox(sess2, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	other := agent.NewAgent(agent.AgentConfig{ID: sess2.ID(), Session: sess2, Inbox: inbox2}, registry.Events())
	detach, err := registry.Enter(other, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	defer detach()
	openTurn(t, other)

	outcome, err := service.Request(ApprovalRequest{Agent: other, ToolName: "shell"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeUnavailable {
		t.Fatalf("outcome = %q, want agent-scoped answers to miss the other agent", outcome)
	}
}

func TestAbortedSignalYieldsCancelled(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	openTurn(t, a)
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell", Signal: signal})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if outcome != OutcomeCancelled {
		t.Fatalf("outcome = %q, want cancelled", outcome)
	}
	decided := eventOf(t, a, EventApprovalDecided)
	var decidedData DecidedData
	if err := unmarshalForTest(decided.Data, &decidedData); err != nil {
		t.Fatalf("decode decided: %v", err)
	}
	if decidedData.Outcome != OutcomeCancelled {
		t.Fatalf("audited outcome = %q", decidedData.Outcome)
	}
}

func TestEffectiveApprovalPolicyFoldsLastWins(t *testing.T) {
	sess, err := session.NewDetached(session.SessionID("fold"), nil, &session.SessionHeader{ID: session.SessionID("fold")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	if _, ok := EffectiveApprovalPolicy(sess.Events()); ok {
		t.Fatal("fresh session reported an override")
	}
	if err := SetApprovalPolicy(sess, PolicyNever); err != nil {
		t.Fatalf("set never: %v", err)
	}
	if err := SetApprovalPolicy(sess, PolicyAsk); err != nil {
		t.Fatalf("set ask: %v", err)
	}
	policy, ok := EffectiveApprovalPolicy(sess.Events())
	if !ok || policy != PolicyAsk {
		t.Fatalf("policy = %q ok=%v, want the last override to win", policy, ok)
	}
}

func TestSetApprovalPolicyRejectsInvalidValues(t *testing.T) {
	sess, err := session.NewDetached(session.SessionID("invalid"), nil, &session.SessionHeader{ID: session.SessionID("invalid")})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	err = SetApprovalPolicy(sess, ApprovalPolicy("banana"))
	if err == nil || err.Error() != `approval policy must be one of "ask" or "never"` {
		t.Fatalf("err = %v", err)
	}
	if len(sess.Events()) != 0 {
		t.Fatal("the invalid value reached the log")
	}
}

func TestNewConfigValidatesAndDefaults(t *testing.T) {
	config, err := NewConfig("")
	if err != nil || config.Policy != PolicyAsk {
		t.Fatalf("default config = %+v, %v", config, err)
	}
	if _, err := NewConfig(ApprovalPolicy("banana")); err == nil {
		t.Fatal("expected untrusted policy input to be rejected")
	}
}

func TestServiceSetPolicyAppendsOverride(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	if service.EffectivePolicy(a.Session) != PolicyAsk {
		t.Fatal("default effective policy should be ask")
	}
	if err := service.SetPolicy(a, PolicyNever); err != nil {
		t.Fatalf("SetPolicy: %v", err)
	}
	if service.EffectivePolicy(a.Session) != PolicyNever {
		t.Fatal("override did not flip the effective policy")
	}
	// Idempotent switch: no second event.
	if err := service.SetPolicy(a, PolicyNever); err != nil {
		t.Fatalf("second SetPolicy: %v", err)
	}
	policies := 0
	for _, event := range a.Session.Events() {
		if event.Type == EventApprovalPolicy {
			policies += 1
		}
	}
	if policies != 1 {
		t.Fatalf("policy events = %d, want 1", policies)
	}
}

func TestToolsSeamMapsVocabulary(t *testing.T) {
	service, registry, a := newTestService(t, Config{})
	openTurn(t, a)
	unsubscribe := registry.Events().OnWaterfall(EventApprovalRequest, a.Scope, func(any, func(any) any) any {
		return OutcomeAllowedOnce
	})
	defer unsubscribe()
	seam := service.ToolsSeam()
	outcome := seam.Request(tools.ApprovalRequest{Agent: a.Scope, ToolName: "shell", CallID: "call-1"})
	if outcome != tools.ApprovalAllowedOnce {
		t.Fatalf("seam outcome = %q", outcome)
	}
	// An unresolvable caller fails closed rather than answering globally.
	stray := seam.Request(tools.ApprovalRequest{ToolName: "shell"})
	if stray != tools.ApprovalUnavailable {
		t.Fatalf("stray outcome = %q, want unavailable", stray)
	}
}

func TestPolicyContextStatesTheEffectiveSentence(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	never := PolicyContext(a.Session, service.config, service.EffectivePolicy)
	if never.Name != "approval:policy" || never.Order != 115 {
		t.Fatalf("context = %+v", never)
	}
	if text := never.TextProvider(systemprompt.AssembleContext{}); text != AskSentence {
		t.Fatalf("ask text = %q", text)
	}
	if err := SetApprovalPolicy(a.Session, PolicyNever); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	if text := never.TextProvider(systemprompt.AssembleContext{}); text != NeverSentence {
		t.Fatalf("never text = %q", text)
	}
}

func TestRequestIDsAreUnique(t *testing.T) {
	service, _, a := newTestService(t, Config{})
	openTurn(t, a)
	seen := map[string]bool{}
	for index := 0; index < 3; index += 1 {
		if _, err := service.Request(ApprovalRequest{Agent: a, ToolName: "shell"}); err != nil {
			t.Fatalf("request %d: %v", index, err)
		}
	}
	for _, event := range a.Session.Events() {
		if event.Type != EventApprovalAsked {
			continue
		}
		var data AskedData
		if err := unmarshalForTest(event.Data, &data); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if seen[data.ID] {
			t.Fatalf("request id %q reused", data.ID)
		}
		seen[data.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("ids = %d, want 3", len(seen))
	}
}

func TestNewServiceRequiresRegistry(t *testing.T) {
	if _, err := NewService(nil, Config{}); err == nil {
		t.Fatal("expected a nil registry to be rejected")
	}
}
