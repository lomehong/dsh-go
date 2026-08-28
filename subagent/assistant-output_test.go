package subagent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// assistantMessageEvent builds one assistant/message event.
func assistantMessageEvent(t *testing.T, blocks []llm.ContentBlock) session.Event {
	t.Helper()
	payload, err := json.Marshal(session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.Message{Role: "assistant", Content: blocks},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return session.Event{Type: session.EventAssistantMsg, Data: payload}
}

// textDeltaEvent builds one assistant/chunk text-delta event.
func textDeltaEvent(t *testing.T, text string) session.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"turn": 1, "step": 1,
		"chunk": map[string]any{"type": string(llm.ChunkTextDelta), "index": 0, "text": text},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return session.Event{Type: session.EventAssistantChunk, Data: payload}
}

func TestFoldSelectsLastNonEmptyAssistantMessage(t *testing.T) {
	var fold AssistantOutputFold
	fold.Push(assistantMessageEvent(t, []llm.ContentBlock{{Type: llm.BlockText, Text: "first"}}))
	fold.Push(assistantMessageEvent(t, []llm.ContentBlock{{Type: llm.BlockText, Text: "second"}}))
	output := fold.Collect()
	if len(output) != 1 || output[0].Text != "second" {
		t.Fatalf("output = %+v", output)
	}
}

func TestFoldSkipsEmptyMessages(t *testing.T) {
	var fold AssistantOutputFold
	fold.Push(assistantMessageEvent(t, []llm.ContentBlock{{Type: llm.BlockText, Text: "kept"}}))
	// A usage-only message records usage but does not replace earlier output.
	fold.Push(assistantMessageEvent(t, nil))
	output := fold.Collect()
	if len(output) != 1 || output[0].Text != "kept" {
		t.Fatalf("output = %+v", output)
	}
}

func TestFoldTextDeltaFallback(t *testing.T) {
	var fold AssistantOutputFold
	fold.Push(textDeltaEvent(t, "hel"))
	fold.Push(textDeltaEvent(t, "lo"))
	fold.Push(textDeltaEvent(t, "")) // empty piece is a no-op
	output := fold.Collect()
	if len(output) != 1 || output[0].Type != llm.BlockText || output[0].Text != "hello" {
		t.Fatalf("output = %+v", output)
	}
}

func TestFoldPushTextOutsideSessionEvents(t *testing.T) {
	var fold AssistantOutputFold
	fold.PushText("acp ")
	fold.PushText("chunk")
	output := fold.Collect()
	if len(output) != 1 || output[0].Text != "acp chunk" {
		t.Fatalf("output = %+v", output)
	}
}

func TestFoldNeitherProducesNil(t *testing.T) {
	var fold AssistantOutputFold
	if output := fold.Collect(); output != nil {
		t.Fatalf("output = %+v, want nil", output)
	}
	// An unrelated event contributes nothing.
	fold.Push(session.Event{Type: session.EventTurnStart, Data: []byte(`{"turn":1}`)})
	if output := fold.Collect(); output != nil {
		t.Fatalf("output = %+v, want nil", output)
	}
}

func TestFinalAssistantOutputRule(t *testing.T) {
	events := []session.Event{
		{Type: session.EventUserMessage, Data: []byte(`{"message":{"role":"user","content":[{"type":"text","text":"go"}]}}`)},
		textDeltaEvent(t, "partial"),
		assistantMessageEvent(t, []llm.ContentBlock{{Type: llm.BlockText, Text: "final"}}),
	}
	output := FinalAssistantOutput(events)
	if len(output) != 1 || output[0].Text != "final" {
		t.Fatalf("output = %+v", output)
	}
}

func TestRunOutcomeMapping(t *testing.T) {
	cases := []struct {
		name   string
		result SubagentResult
		want   JobOutcome
	}{
		{"completed", SubagentResult{StopReason: StopCompleted, Output: []llm.ContentBlock{
			{Type: llm.BlockText, Text: "a"},
			{Type: llm.BlockToolCall, ID: "c1"},
			{Type: llm.BlockText, Text: "b"},
		}}, JobOutcome{Status: JobStatusCompleted, Output: "ab"}},
		{"local cancel", SubagentResult{StopReason: StopAborted}, JobOutcome{Status: JobStatusKilled}},
		{"remote abort", SubagentResult{StopReason: StopAborted, Diagnostic: "host stopped"},
			JobOutcome{Status: JobStatusFailed, Detail: "aborted; diagnostic: host stopped"}},
		{"error", SubagentResult{StopReason: StopError},
			JobOutcome{Status: JobStatusFailed, Detail: "error"}},
		{"max-tokens", SubagentResult{StopReason: StopMaxTokens},
			JobOutcome{Status: JobStatusFailed, Detail: "max-tokens"}},
		{"refusal", SubagentResult{StopReason: StopRefusal, Diagnostic: "declined"},
			JobOutcome{Status: JobStatusFailed, Detail: "refusal; diagnostic: declined"}},
		{"merge-extensible", SubagentResult{StopReason: StopReason("handoff"), Diagnostic: "moved"},
			JobOutcome{Status: JobStatusFailed, Detail: "handoff; diagnostic: moved"}},
	}
	for _, testCase := range cases {
		if got := RunOutcome(testCase.result); got != testCase.want {
			t.Fatalf("%s: got %+v, want %+v", testCase.name, got, testCase.want)
		}
	}
}

// settleRun is a controllable SubagentRun for settlement tests.
type settleRun struct {
	result  SubagentResult
	err     error
	dispose error
}

func (r *settleRun) ID() session.SessionID    { return "run-1" }
func (r *settleRun) LocalAgent() *agent.Agent { return nil }
func (r *settleRun) Result() (SubagentResult, error) {
	return r.result, r.err
}
func (r *settleRun) Dispose() error { return r.dispose }

func TestSettleRunJoinsDisposeFailures(t *testing.T) {
	run := &settleRun{result: SubagentResult{StopReason: StopCompleted, Output: []llm.ContentBlock{{Type: llm.BlockText, Text: "done"}}}}
	outcome := SettleRun(run)
	if outcome.Status != JobStatusCompleted || outcome.Output != "done" {
		t.Fatalf("outcome = %+v", outcome)
	}

	run = &settleRun{result: SubagentResult{StopReason: StopError}, dispose: errors.New("token revoked")}
	outcome = SettleRun(run)
	if outcome.Status != JobStatusFailed || !strings.Contains(outcome.Detail, "dispose failed: token revoked") {
		t.Fatalf("outcome = %+v", outcome)
	}

	// When both fail, both details survive.
	run = &settleRun{err: errors.New("result channel broken"), dispose: errors.New("cleanup exploded")}
	outcome = SettleRun(run)
	if outcome.Status != JobStatusFailed {
		t.Fatalf("status = %q", outcome.Status)
	}
	if !strings.Contains(outcome.Detail, "result channel broken") || !strings.Contains(outcome.Detail, "dispose failed: cleanup exploded") {
		t.Fatalf("detail = %q, want both details", outcome.Detail)
	}
	if strings.Index(outcome.Detail, "result channel broken") > strings.Index(outcome.Detail, "dispose failed") {
		t.Fatalf("detail order wrong: %q", outcome.Detail)
	}
}
