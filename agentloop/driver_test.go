package agentloop

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// registerEchoTool defines a trivial concurrency-safe tool echoing its input.
func (h *harness) registerEchoTool() {
	h.t.Helper()
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        "echo",
		Description: "echo the provided text",
		Parameters: map[string]tools.PropSpec{
			"text": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "string"},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: value.(string)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return args["text"].(string), nil
		},
		IsConcurrencySafe: func(args map[string]any) bool { return true },
	})
	if err != nil {
		h.t.Fatalf("DefineTool: %v", err)
	}
	dispose, err := h.tools.Register(definition)
	if err != nil {
		h.t.Fatalf("Register: %v", err)
	}
	h.t.Cleanup(dispose)
}

func TestTextOnlyTurnCompletes(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("hello there")})
	a := h.startAgent("text-turn")

	h.run(a, "say hi")

	types := eventTypes(a)
	for _, want := range []string{
		session.EventUserMessage, session.EventTurnStart, session.EventStepStart,
		session.EventAssistantMsg, session.EventStepEnd, session.EventTurnEnd,
	} {
		if !contains(types, want) {
			t.Fatalf("session log missing %s: %v", want, types)
		}
	}
	messages := a.Session.DeriveMessages()
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want user + assistant", len(messages))
	}
	if messages[1].Role != llm.RoleAssistant || len(messages[1].Content) != 1 || messages[1].Content[0].Text != "hello there" {
		t.Fatalf("assistant message = %+v", messages[1])
	}
	// The assistant message is a model-source message with provider/model.
	if messages[1].Source.Provider != "stub" || messages[1].Source.Model != "stub-model" {
		t.Fatalf("assistant source = %+v", messages[1].Source)
	}
	// One request-header anchor with the initial reason, plus request-context.
	var headerReason string
	for _, event := range a.Session.Events() {
		if event.Type == session.EventRequestHeader {
			var data session.RequestHeaderData
			if err := decodeEvent(event.Data, &data); err != nil {
				t.Fatalf("decode header: %v", err)
			}
			headerReason = data.Reason
		}
	}
	if headerReason != session.HeaderReasonInitial {
		t.Fatalf("header reason = %q", headerReason)
	}
}

func decodeEvent(data []byte, target any) error {
	return jsonUnmarshal(data, target)
}

func TestToolCallTurnExecutesToolAndContinues(t *testing.T) {
	h := newHarness(t)
	h.registerEchoTool()
	h.adapter.script(scriptCall{chunks: toolCallChunks("call-1", "echo", `{"text":"hi"}`)})
	h.adapter.script(scriptCall{chunks: textChunks("done")})
	a := h.startAgent("tool-turn")

	h.run(a, "use the tool")

	var sawCall, sawResult bool
	var resultIsError bool
	for _, event := range a.Session.Events() {
		if event.Type == session.EventToolCall {
			sawCall = true
		}
		if event.Type == session.EventToolResult {
			sawResult = true
			var data session.ToolResultData
			if err := decodeEvent(event.Data, &data); err != nil {
				t.Fatalf("decode tool result: %v", err)
			}
			resultIsError = data.Error != nil
			if len(data.Message.Content) == 0 {
				t.Fatalf("tool result content missing: %+v", data)
			}
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("tool call/result missing: %v", eventTypes(a))
	}
	if resultIsError {
		t.Fatalf("tool result unexpectedly errored")
	}
	// Second request carries the tool result message (a user-role message
	// holding the tool-result block).
	second := h.adapter.request(1)
	last := second.Messages[len(second.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Content) == 0 || last.Content[0].Type != llm.BlockToolResult {
		t.Fatalf("last request message = role %q, blocks %+v", last.Role, last.Content)
	}
	if last.Content[0].ToolCallID != "call-1" {
		t.Fatalf("tool result correlation = %q", last.Content[0].ToolCallID)
	}
	// The turn ended completed after the follow-up text step.
	var turnEnd session.TurnEndData
	for _, event := range a.Session.Events() {
		if event.Type == session.EventTurnEnd {
			if err := decodeEvent(event.Data, &turnEnd); err != nil {
				t.Fatalf("decode turn end: %v", err)
			}
		}
	}
	if turnEnd.Reason.Kind != session.TurnEndCompleted {
		t.Fatalf("turn ended %q, want completed", turnEnd.Reason.Kind)
	}
}

func TestRequestErrorRetryContinues(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: nil, block: errors.New("provider exploded")})
	h.adapter.script(scriptCall{chunks: textChunks("recovered")})
	a := h.startAgent("retry-turn")

	retried := make(chan struct{}, 1)
	retry := false
	dispose := h.events.RequestError().On(nil, func(payload agent.RequestErrorPayload, next func(agent.RequestErrorPayload) agent.RequestErrorAction) agent.RequestErrorAction {
		action := next(payload)
		if !retry {
			retry = true
			select {
			case retried <- struct{}{}:
			default:
			}
			action.Retry = true
		}
		return action
	})
	t.Cleanup(dispose)

	h.run(a, "try again")

	select {
	case <-retried:
	default:
		t.Fatalf("request-error waterfall never fired")
	}
	if h.adapter.requestCount() != 2 {
		t.Fatalf("requests = %d, want initial + retry", h.adapter.requestCount())
	}
	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndCompleted {
		t.Fatalf("turn ended %q, want completed after retry", turnEnd.Reason.Kind)
	}
}

func TestRequestErrorWithoutRetryFailsTurn(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: nil, block: errors.New("provider exploded")})
	a := h.startAgent("fail-turn")

	h.run(a, "this will fail")

	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndError {
		t.Fatalf("turn ended %q, want error", turnEnd.Reason.Kind)
	}
	if turnEnd.Reason.Error == nil || turnEnd.Reason.Error.Code != "PROVIDER_ERROR" {
		t.Fatalf("structured failure = %+v", turnEnd.Reason.Error)
	}
}

func TestCancelMidStreamMarksInterruptedPrefix(t *testing.T) {
	h := newHarness(t)
	// The adapter blocks after the first text delta, so the cancel lands
	// mid-stream with delivered content to keep.
	h.adapter.mu.Lock()
	h.adapter.scripts = append(h.adapter.scripts, scriptCall{chunks: textChunks("partial answer")})
	hold := make(chan struct{})
	h.adapter.hold = hold
	h.adapter.mu.Unlock()
	a := h.startAgent("abort-turn")

	driver := a.Driver().(*ReactLoopAgent)
	driver.Send(llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "go"}}, llm.MessageSource{Kind: llm.SourceUser}), agent.InboxNextTurn, true)

	// Wait until the request is in flight, then cancel the turn.
	deadline := time.Now().Add(5 * time.Second)
	for h.adapter.requestCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	driver.Cancel(session.TurnEndCancelCause{Kind: "user", Reason: "stop"}, agent.CancelOptions{})
	close(hold)
	h.awaitIdle(a)

	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndAborted {
		t.Fatalf("turn ended %q, want aborted", turnEnd.Reason.Kind)
	}
	if turnEnd.Reason.Reason == nil || turnEnd.Reason.Reason.Kind != "user" {
		t.Fatalf("abort cause = %+v", turnEnd.Reason.Reason)
	}
	// The delivered text prefix is finalized as an interrupted message.
	var interrupted *session.AssistantMessageData
	for index := range a.Session.Events() {
		event := a.Session.Events()[index]
		if event.Type != session.EventAssistantMsg {
			continue
		}
		var data session.AssistantMessageData
		if err := decodeEvent(event.Data, &data); err != nil {
			t.Fatalf("decode assistant: %v", err)
		}
		if data.Interrupted {
			interrupted = &data
		}
	}
	if interrupted == nil {
		t.Fatalf("no interrupted assistant message in %v", eventTypes(a))
	}
	if len(interrupted.Message.Content) != 1 || interrupted.Message.Content[0].Text != "partial answer" {
		t.Fatalf("interrupted content = %+v", interrupted.Message.Content)
	}
}

func TestMaxTokensEndsTurnSticky(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: maxTokensChunks("truncated")})
	a := h.startAgent("max-tokens-turn")

	h.run(a, "fill the window")

	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndMaxTokens {
		t.Fatalf("turn ended %q, want max-tokens", turnEnd.Reason.Kind)
	}
}

func TestFollowupTurnStartsSecondTurn(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("first")})
	h.adapter.script(scriptCall{chunks: textChunks("second")})
	a := h.startAgent("followup")

	h.run(a, "one")
	driver := a.Driver().(*ReactLoopAgent)
	driver.Followup(llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "two"}}, llm.MessageSource{Kind: llm.SourceUser}))
	h.awaitIdle(a)

	turns := 0
	var lastTurn session.TurnEndData
	for _, event := range a.Session.Events() {
		if event.Type == session.EventTurnStart {
			turns++
		}
		if event.Type == session.EventTurnEnd {
			if err := decodeEvent(event.Data, &lastTurn); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
	}
	if turns != 2 {
		t.Fatalf("turns = %d, want 2", turns)
	}
	if lastTurn.Turn != 2 || lastTurn.Reason.Kind != session.TurnEndCompleted {
		t.Fatalf("final turn end = %+v", lastTurn)
	}
}

func TestSteerJoinsNearestStepBoundary(t *testing.T) {
	h := newHarness(t)
	h.registerEchoTool()
	h.adapter.script(scriptCall{chunks: toolCallChunks("call-1", "echo", `{"text":"hi"}`)})
	h.adapter.script(scriptCall{chunks: textChunks("tool result summary")})
	// The steered message keeps the turn alive at the next step boundary.
	h.adapter.script(scriptCall{chunks: textChunks("after steering")})
	a := h.startAgent("steer")

	steered := make(chan struct{})
	// After the first request completes, inject steering for the next step.
	var fired atomic.Bool
	dispose := h.events.Request().On(nil, func(payload agent.RequestPayload, next func(agent.RequestPayload) *llm.LlmCallConfig) *llm.LlmCallConfig {
		if fired.CompareAndSwap(false, true) {
			h.sendTarget(a, "also consider this", agent.InboxNextStep, false)
			close(steered)
		}
		return next(payload)
	})
	t.Cleanup(dispose)

	h.run(a, "start")

	<-steered
	// Tool rounds continue within the step, so the steered message joins at
	// the next step claim: the turn's step 2 request.
	third := h.adapter.request(2)
	found := false
	var seen []string
	for _, message := range third.Messages {
		text := ""
		if len(message.Content) > 0 && message.Content[0].Type == llm.BlockText {
			text = message.Content[0].Text
		}
		seen = append(seen, fmt.Sprintf("%s:%q", message.Role, text))
		if message.Role == llm.RoleUser && len(message.Content) > 0 && message.Content[0].Text == "also consider this" {
			found = true
		}
	}
	if !found {
		var log []string
		for _, event := range a.Session.Events() {
			entry := fmt.Sprintf("%s#%d", event.Type, event.Seq)
			if event.Type == "agent/inbox/spliced" {
				splice, err := agent.DecodeInboxSpliced(event)
				if err == nil {
					entry = fmt.Sprintf("%s target=%s start=%d removed=%v inserted=%d", entry, splice.Target, splice.Start, splice.RemovedCount, len(splice.Inserted))
				}
			}
			log = append(log, entry)
		}
		t.Fatalf("steered message missing from request 2; messages = %v; session = %v", seen, log)
	}
	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndCompleted {
		t.Fatalf("turn ended %q, want completed", turnEnd.Reason.Kind)
	}
}

func TestRunMaintenanceRunsFromIdle(t *testing.T) {
	h := newHarness(t)
	a := h.startAgent("maintenance")

	ran := false
	err := a.Driver().RunMaintenance(func(signal context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if !ran {
		t.Fatalf("maintenance task never ran")
	}

	// While a turn runs, maintenance must refuse.
	h.adapter.script(scriptCall{chunks: textChunks("busy")})
	h.send(a, "go", true)
	driver := a.Driver().(*ReactLoopAgent)
	deadline := time.Now().Add(5 * time.Second)
	for driver.status() != agent.AgentRunning && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := driver.RunMaintenance(func(signal context.Context) error { return nil }); err == nil {
		t.Fatalf("maintenance during a running turn must fail")
	}
	h.awaitIdle(a)
}

func TestWakeLatchReplaysAfterMaintenance(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("after")})
	a := h.startAgent("latch")

	// Queue the message first, then hold maintenance, then send the wake.
	err := a.Driver().RunMaintenance(func(signal context.Context) error {
		h.send(a, "wake", true)
		return nil
	})
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	h.awaitIdle(a)

	turnEnd := lastTurnEnd(t, a)
	if turnEnd.Reason.Kind != session.TurnEndCompleted {
		t.Fatalf("latched wake did not replay: %+v", turnEnd)
	}
	messages := a.Session.DeriveMessages()
	if len(messages) < 2 {
		t.Fatalf("messages = %d", len(messages))
	}
	last := messages[len(messages)-1]
	if last.Role != llm.RoleAssistant || last.Content[0].Text != "after" {
		t.Fatalf("final message = %+v", last)
	}
}

// lastTurnEnd decodes the final turn/end event.
func lastTurnEnd(t *testing.T, a *agent.Agent) session.TurnEndData {
	t.Helper()
	var data session.TurnEndData
	found := false
	for _, event := range a.Session.Events() {
		if event.Type != session.EventTurnEnd {
			continue
		}
		if err := decodeEvent(event.Data, &data); err != nil {
			t.Fatalf("decode turn end: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("no turn/end in %v", eventTypes(a))
	}
	return data
}

// wakeState reads the driver's pending-wake set under its lock.
func wakeState(d *ReactLoopAgent) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pendingWakes)
}

// TestPendingWakeSurvivesACleanExitReplaysLostInput: a follow-up that lost
// the race with a cleanly closing turn (its send saw a live driver, so no
// wake was latched) must start a fresh driver via the pending-wake set —
// the B3 narrowing replaces the source's cleanExit flag.
func TestPendingWakeSurvivesCleanExitReplays(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("first")})
	a := h.startAgent("wake-clean-exit")
	driver := a.Driver().(*ReactLoopAgent)

	h.run(a, "hello")

	// A clean driver exit with inbox content latches no wakeRequested; a
	// follow-up sent while that driver was still closing would otherwise be
	// parked. Send one and let the driver restart on the pending wake.
	h.send(a, "follow-up", true)
	h.awaitIdle(a)
	if wakeState(driver) != 0 {
		t.Fatalf("pending wakes = %d after delivery, want 0", wakeState(driver))
	}
}

// TestCancelClearsPendingWakes: cancellation consumes the wake bookkeeping
// even when inbox input is kept, so retained work parks until the next
// waking send.
func TestCancelClearsPendingWakes(t *testing.T) {
	h := newHarness(t)
	h.adapter.script(scriptCall{chunks: textChunks("first")})
	a := h.startAgent("wake-cancel")
	driver := a.Driver().(*ReactLoopAgent)

	h.run(a, "hello")

	// Send a waking follow-up but cancel with KeepInbox before it is
	// delivered: the wake bookkeeping clears while the input stays parked.
	driver.Send(llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "queued"}}, llm.MessageSource{Kind: llm.SourceUser}), agent.InboxNextTurn, true)
	driver.Cancel(session.TurnEndCancelCause{Kind: "user"}, agent.CancelOptions{KeepInbox: true})
	if wakeState(driver) != 0 {
		t.Fatalf("pending wakes = %d after cancel, want 0", wakeState(driver))
	}
}
