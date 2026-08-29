// Contract tests for consumed-work accounting and model selection.
package agent

import (
	"testing"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/systemprompt"
)

func TestFoldConsumedWorkSteppedTurnAccounts(t *testing.T) {
	sess := newEmptySession(t, "fold-1")
	mustTurnStart(t, sess, 1)
	mustStepStart(t, sess, 1, 1)
	mustTurnEnd(t, sess, 1, session.TurnEndCompleted)

	work := FoldConsumedWork(sess.Events())
	if work.End == nil {
		t.Fatal("a stepped turn accounts for its work even when completed")
	}
	if work.DroppedUnrun {
		t.Fatal("nothing was dropped")
	}
}

func TestFoldConsumedWorkClaimedWithoutStep(t *testing.T) {
	sess := newEmptySession(t, "fold-2")
	inbox, _ := NewInbox(sess, &recordingNotifications{})
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	mustTurnStart(t, sess, 1)
	if _, err := inbox.Claim(InboxNextTurn, 1); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A completed end over a claim does NOT account: the claim was rewritten
	// away and there was nothing left to run.
	mustTurnEnd(t, sess, 1, session.TurnEndCompleted)
	work := FoldConsumedWork(sess.Events())
	if work.End != nil {
		t.Fatalf("completed no-step turn must not account: %+v", work)
	}

	// A blocked end is that input's ending too (the pre-step rejection
	// discarded the claimed messages).
	if err := inbox.Append(InboxNextTurn, inboxMessage("m2")); err != nil {
		t.Fatalf("Append m2: %v", err)
	}
	mustTurnStart(t, sess, 2)
	if _, err := inbox.Claim(InboxNextTurn, 2); err != nil {
		t.Fatalf("Claim 2: %v", err)
	}
	if err := inbox.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	mustTurnEnd(t, sess, 2, session.TurnEndBlocked)
	work = FoldConsumedWork(sess.Events())
	if work.End == nil {
		t.Fatal("blocked claim accounts for its input")
	}
	if work.DroppedUnrun {
		t.Fatal("the blocked turn's own ending accounts for the drop")
	}
}

func TestFoldConsumedWorkDroppedUnrun(t *testing.T) {
	sess := newEmptySession(t, "fold-3")
	inbox, _ := NewInbox(sess, &recordingNotifications{})
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Cancelled before any turn opened over it: no turn/end describes it.
	if err := inbox.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	work := FoldConsumedWork(sess.Events())
	if work.End != nil || !work.DroppedUnrun {
		t.Fatalf("work = %+v", work)
	}

	// A replacement keeps the work pending: it is not a drop. (Fresh log:
	// droppedUnrun is sticky until an accounting turn, so the earlier drop
	// must not mask the check.)
	replacement := newEmptySession(t, "fold-3b")
	replacementInbox, _ := NewInbox(replacement, &recordingNotifications{})
	if err := replacementInbox.Append(InboxNextTurn, inboxMessage("m2")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if replaced, err := replacementInbox.Replace("m2", inboxMessage("m3")); err != nil || !replaced {
		t.Fatalf("Replace = %v, %v", replaced, err)
	}
	work = FoldConsumedWork(replacement.Events())
	if work.DroppedUnrun {
		t.Fatal("a replacement is not a drop")
	}

	// A drop after an accounted turn is still unaccounted for.
	mustTurnStart(t, sess, 1)
	mustStepStart(t, sess, 1, 1)
	mustTurnEnd(t, sess, 1, session.TurnEndCompleted)
	work = FoldConsumedWork(sess.Events())
	if work.End == nil || work.DroppedUnrun {
		t.Fatalf("after turn = %+v", work)
	}
	if err := inbox.Append(InboxNextTurn, inboxMessage("m4")); err != nil {
		t.Fatalf("Append m4: %v", err)
	}
	if err := inbox.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	work = FoldConsumedWork(sess.Events())
	if !work.DroppedUnrun {
		t.Fatal("a later drop is unaccounted for")
	}
}

func TestFoldEmptyLog(t *testing.T) {
	sess := newEmptySession(t, "fold-4")
	work := FoldConsumedWork(sess.Events())
	if work.End != nil || work.DroppedUnrun {
		t.Fatalf("work = %+v", work)
	}
}

func TestModelSelectionCouplesAssemblyAndRequest(t *testing.T) {
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{IncludeHarnessIdentity: new(bool)})
	if err != nil {
		t.Fatalf("NewSystemPrompt: %v", err)
	}
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	selection := &ModelSelectionRef{}
	dispose := InstallModelSelection(prompt, registry.Events(), agent.Scope, selection)
	defer dispose()

	selection.Current = &ModelSelection{Provider: "deepseek", Model: "dsh-1", ReasoningEffort: "high", HasReasoningEffort: true}
	assembly, err := prompt.Assemble(agent.AssembleContextFor(nil))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if value, _ := assembly.Variables.Get("provider"); value == nil || *value != "deepseek" {
		t.Fatalf("provider variable = %v", value)
	}
	if value, _ := assembly.Variables.Get("model"); value == nil || *value != "dsh-1" {
		t.Fatalf("model variable = %v", value)
	}
	if selection.Assembled != selection.Current {
		t.Fatal("assembly must snapshot the current selection")
	}

	// The request waterfall applies the assembled selection.
	resolved := registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "other", Model: "other-1", ReasoningEffort: "low"}
	})
	if resolved.Provider != "deepseek" || resolved.Model != "dsh-1" || resolved.ReasoningEffort != "high" {
		t.Fatalf("resolved = %+v", resolved)
	}

	// An absent selected effort clears the inherited one.
	selection.Current = &ModelSelection{Provider: "deepseek", Model: "dsh-2"}
	if _, err := prompt.Assemble(agent.AssembleContextFor(nil)); err != nil {
		t.Fatalf("Assemble 2: %v", err)
	}
	resolved = registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "other", Model: "other-1", ReasoningEffort: "low"}
	})
	if resolved.ReasoningEffort != "" {
		t.Fatalf("absent effort must clear inherited: %+v", resolved)
	}
	if resolved.Provider != "deepseek" || resolved.Model != "dsh-2" {
		t.Fatalf("resolved = %+v", resolved)
	}
}

func TestModelSelectionNilPassthroughAndDisposal(t *testing.T) {
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{IncludeHarnessIdentity: new(bool)})
	if err != nil {
		t.Fatalf("NewSystemPrompt: %v", err)
	}
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	selection := &ModelSelectionRef{}
	dispose := InstallModelSelection(prompt, registry.Events(), agent.Scope, selection)

	assembly, err := prompt.Assemble(agent.AssembleContextFor(nil))
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if _, ok := assembly.Variables.Get("provider"); ok {
		t.Fatal("no selection must inject no variables")
	}
	resolved := registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "other", Model: "other-1"}
	})
	if resolved.Provider != "other" {
		t.Fatalf("passthrough = %+v", resolved)
	}

	dispose()
	selection.Current = &ModelSelection{Provider: "deepseek", Model: "dsh-1"}
	assembly, err = prompt.Assemble(agent.AssembleContextFor(nil))
	if err != nil {
		t.Fatalf("post-dispose Assemble: %v", err)
	}
	if _, ok := assembly.Variables.Get("provider"); ok {
		t.Fatal("disposal must remove the assembly listener")
	}
	resolved = registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{Provider: "other", Model: "other-1"}
	})
	if resolved.Provider != "other" {
		t.Fatal("disposal must remove the request listener")
	}
}

func TestModelSelectionConcurrentSwitchTakesEffectNextStep(t *testing.T) {
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{IncludeHarnessIdentity: new(bool)})
	if err != nil {
		t.Fatalf("NewSystemPrompt: %v", err)
	}
	registry := NewAgentRegistry(nil, nil)
	agent := newTestAgent(t, registry, "agent-1", nil)
	selection := &ModelSelectionRef{Current: &ModelSelection{Provider: "a", Model: "a-1"}}
	dispose := InstallModelSelection(prompt, registry.Events(), agent.Scope, selection)
	defer dispose()

	// Step 1 enters assembly under selection A...
	if _, err := prompt.Assemble(agent.AssembleContextFor(nil)); err != nil {
		t.Fatalf("Assemble 1: %v", err)
	}
	// ...and the request is routed with A even though the switch landed
	// before the request dispatched.
	selection.Current = &ModelSelection{Provider: "b", Model: "b-1"}
	resolved := registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{}
	})
	if resolved.Provider != "a" || resolved.Model != "a-1" {
		t.Fatalf("step 1 resolved = %+v", resolved)
	}
	// Step 2 assembly captures the new selection.
	if _, err := prompt.Assemble(agent.AssembleContextFor(nil)); err != nil {
		t.Fatalf("Assemble 2: %v", err)
	}
	resolved = registry.Events().Request().Dispatch(agent.Scope, RequestPayload{}, func(RequestPayload) *llm.LlmCallConfig {
		return &llm.LlmCallConfig{}
	})
	if resolved.Provider != "b" || resolved.Model != "b-1" {
		t.Fatalf("step 2 resolved = %+v", resolved)
	}
}
