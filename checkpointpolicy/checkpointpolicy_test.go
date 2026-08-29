package checkpointpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/tools"
)

// fakeFlusher records checkpoint calls and can fail them.
type fakeFlusher struct {
	flushed []string
	err     error
}

func (f *fakeFlusher) FlushSession(sessionID string) error {
	f.flushed = append(f.flushed, sessionID)
	return f.err
}

// stubAdapter serves a two-chunk stream and records dispatch.
type stubAdapter struct {
	llm.Adapter
	dispatched bool
}

func (s *stubAdapter) Stream(options llm.GenerateOptions) llm.Seq {
	s.dispatched = true
	return llm.FromChunks([]llm.StreamChunk{
		{Type: llm.ChunkTextDelta, Text: "hi"},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishStop}},
	})
}

func (s *stubAdapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: provider}
}

func (s *stubAdapter) ProviderRetryPolicy(provider string) *llm.ResolvedRetryPolicy {
	return nil
}

func (s *stubAdapter) ListModels(provider string) ([]llm.LlmModelInfo, error) {
	return nil, nil
}

func (s *stubAdapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	return llm.LlmResolvedModelInfo{LlmModelInfo: llm.LlmModelInfo{Provider: provider, ID: model, Name: model}}, nil
}

func newLLMRuntime(t *testing.T, adapter *stubAdapter) *llm.Runtime {
	t.Helper()
	runtime := llm.NewRuntime()
	if _, err := runtime.RegisterAdapter([]string{"provider-x"}, adapter); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	return runtime
}

func collect(seq llm.Seq) []llm.StreamChunk {
	var chunks []llm.StreamChunk
	seq(func(chunk llm.StreamChunk) bool {
		chunks = append(chunks, chunk)
		return true
	})
	return chunks
}

func TestStreamArmCheckpointsBeforeFirstChunk(t *testing.T) {
	flusher := &fakeFlusher{}
	adapter := &stubAdapter{}
	runtime := newLLMRuntime(t, adapter)
	detach, err := Attach(runtime, nil, nil, flusher, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	chunks := collect(runtime.Stream(llm.GenerateOptions{Provider: "provider-x", Model: "model-x", SessionID: "s1"}))
	if !adapter.dispatched {
		t.Fatal("adapter never dispatched on a healthy checkpoint")
	}
	if len(flusher.flushed) != 1 || flusher.flushed[0] != "s1" {
		t.Fatalf("flushed = %v", flusher.flushed)
	}
	if len(chunks) != 2 || chunks[0].Text != "hi" {
		t.Fatalf("chunks = %+v", chunks)
	}

	// A request without a session id is not a session boundary: no
	// checkpoint, straight dispatch.
	adapter2 := &stubAdapter{}
	runtime2 := newLLMRuntime(t, adapter2)
	detach2, err := Attach(runtime2, nil, nil, flusher, nil)
	if err != nil {
		t.Fatalf("attach 2: %v", err)
	}
	defer detach2()
	collect(runtime2.Stream(llm.GenerateOptions{Provider: "provider-x", Model: "model-x"}))
	if len(flusher.flushed) != 1 {
		t.Fatalf("session-less request checkpointed: %v", flusher.flushed)
	}
}

func TestStreamArmFailsClosedOnCheckpointError(t *testing.T) {
	flusher := &fakeFlusher{err: errors.New("disk full")}
	adapter := &stubAdapter{}
	runtime := newLLMRuntime(t, adapter)
	detach, err := Attach(runtime, nil, nil, flusher, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	chunks := collect(runtime.Stream(llm.GenerateOptions{Provider: "provider-x", SessionID: "s1"}))
	if adapter.dispatched {
		t.Fatal("adapter dispatched despite checkpoint failure")
	}
	if len(chunks) != 1 || chunks[0].Type != llm.ChunkFinish || chunks[0].Reason == nil ||
		chunks[0].Reason.Kind != llm.FinishError || chunks[0].Reason.Failure == nil ||
		!strings.Contains(chunks[0].Reason.Failure.Message, "disk full") {
		t.Fatalf("chunks = %+v", chunks)
	}
}

func newEchoRuntime(t *testing.T, executed *bool) *tools.ToolRuntime {
	t.Helper()
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	echo, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "bash", Description: "echo",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: "ran"}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			*executed = true
			return map[string]any{"ran": true}, nil
		},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := runtime.Register(echo); err != nil {
		t.Fatalf("register: %v", err)
	}
	return runtime
}

func TestToolArmCheckpointsTopLevelOnlyAndHonorsAbort(t *testing.T) {
	flusher := &fakeFlusher{}
	executed := false
	runtime := newEchoRuntime(t, &executed)
	agentKey := scope.NewScopeKey(nil)
	detach, err := Attach(nil, runtime, nil, flusher, func(tools.ScopeKey) (string, bool) { return "session-9", true })
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()

	result := runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c1", Name: "bash", Arguments: map[string]any{},
		Agent: agentKey, Signal: context.Background(),
	})
	if result.IsError {
		t.Fatalf("top-level call failed: %+v", result.Error)
	}
	if !executed {
		t.Fatal("tool body never ran")
	}
	if len(flusher.flushed) != 1 || flusher.flushed[0] != "session-9" {
		t.Fatalf("flushed = %v", flusher.flushed)
	}

	// An agent-less call is not a session side-effect boundary.
	if result := runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c3", Name: "bash", Arguments: map[string]any{}, Signal: context.Background(),
	}); result.IsError {
		t.Fatalf("agent-less call failed: %+v", result.Error)
	}
	if len(flusher.flushed) != 1 {
		t.Fatalf("agent-less call checkpointed: %v", flusher.flushed)
	}
}

func TestToolArmFailsClosedOnCheckpointError(t *testing.T) {
	flusher := &fakeFlusher{err: errors.New("read-only fs")}
	executed := false
	runtime := newEchoRuntime(t, &executed)
	detach, err := Attach(nil, runtime, nil, flusher, func(tools.ScopeKey) (string, bool) { return "session-1", true })
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	result := runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c1", Name: "bash", Arguments: map[string]any{},
		Agent: scope.NewScopeKey(nil), Signal: context.Background(),
	})
	if !result.IsError || !strings.Contains(result.Content[0].Text, "durability checkpoint failed") {
		t.Fatalf("checkpoint failure not fail-closed: %+v", result)
	}
	if executed {
		t.Fatal("tool body ran despite checkpoint failure")
	}
}

func TestToolArmReportsAbortedBeforeDispatch(t *testing.T) {
	flusher := &fakeFlusher{}
	executed := false
	runtime := newEchoRuntime(t, &executed)
	detach, err := Attach(nil, runtime, nil, flusher, func(tools.ScopeKey) (string, bool) { return "session-1", true })
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	signal, cancel := context.WithCancel(context.Background())
	cancel()
	result := runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c1", Name: "bash", Arguments: map[string]any{},
		Agent: scope.NewScopeKey(nil), Signal: signal,
	})
	if !result.IsError || result.Content[0].Text != "Error: tool call aborted before dispatch" {
		t.Fatalf("aborted result = %+v", result)
	}
	if result.Error == nil || result.Error.Info == nil ||
		result.Error.Info.Name != "AbortError" || result.Error.Info.Code != tools.CodeToolAbortedBeforeDispatch {
		t.Fatalf("error = %+v", result.Error)
	}
	if executed {
		t.Fatal("tool body ran after abort")
	}
}

func TestPreStepArmFlushesAgentSession(t *testing.T) {
	flusher := &fakeFlusher{}
	registry := agent.NewAgentRegistry(cordis.NewRoot(cordis.Discard{}), cordis.Discard{})
	detach, err := Attach(nil, nil, registry, flusher, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	sess, err := session.NewDetached(session.SessionID("checkpoint-1"), nil, &session.SessionHeader{ID: session.SessionID("checkpoint-1"), CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess}, registry.Events())
	registry.Events().PreStep().Dispatch(nil, agent.PreStepPayload{
		Agent: built, Turn: 1, Step: 1, Signal: context.Background(),
	}, func(agent.PreStepPayload) agent.PreStepDecision { return agent.PreStepEnter(nil) })
	if len(flusher.flushed) != 1 || flusher.flushed[0] != "checkpoint-1" {
		t.Fatalf("flushed = %v", flusher.flushed)
	}
}

func TestAbortedResultIsCanonical(t *testing.T) {
	result := abortedBeforeDispatchResult()
	if !result.IsError || result.Content[0].Text != "Error: tool call aborted before dispatch" {
		t.Fatalf("content = %+v", result.Content)
	}
	if result.Error == nil || result.Error.Message != "tool call aborted before dispatch" ||
		result.Error.Info == nil || result.Error.Info.Name != "AbortError" ||
		result.Error.Info.Code != tools.CodeToolAbortedBeforeDispatch {
		t.Fatalf("error = %+v", result.Error)
	}
}
