package agentloop

import (
	"fmt"
	"iter"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// scriptCall is one scripted model response.
type scriptCall struct {
	chunks []llm.StreamChunk
	// block, when set, fails the stream with this error after delivering the
	// scripted chunks.
	block error
}

// scriptAdapter is a BaseAdapter-backed adapter replaying scripted responses
// in request order. Every observed request is recorded for assertions.
type scriptAdapter struct {
	llm.BaseAdapter

	mu       sync.Mutex
	scripts  []scriptCall
	requests []llm.GenerateOptions
	calls    int
	// hold, when non-nil, blocks the stream until closed (abort tests).
	hold chan struct{}
	// onStream observes each request as its stream starts.
	onStream func(req llm.GenerateOptions)
}

func (a *scriptAdapter) script(call scriptCall) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.scripts = append(a.scripts, call)
}

func (a *scriptAdapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	return func(yield func(llm.StreamChunk) bool) {
		a.mu.Lock()
		index := a.calls
		a.calls++
		var current scriptCall
		if index < len(a.scripts) {
			current = a.scripts[index]
		}
		hold := a.hold
		a.mu.Unlock()
		a.mu.Lock()
		a.requests = append(a.requests, options)
		a.mu.Unlock()
		if a.onStream != nil {
			a.onStream(options)
		}
		for index, chunk := range current.chunks {
			if options.Context != nil && options.Context.Err() != nil {
				return
			}
			if !yield(chunk) {
				return
			}
			// A hold after the first delta blocks the stream mid-response, so
			// a cancel can arrive while the model response is in flight.
			if hold != nil && index == 1 {
				<-hold
			}
		}
		if current.block != nil {
			yield(llm.StreamChunk{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishError, Failure: &llm.LlmFailure{Message: current.block.Error(), Code: "PROVIDER_ERROR"}}})
		}
	}
}

func (a *scriptAdapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: provider}
}

func (a *scriptAdapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	window := int64(128000)
	return llm.LlmResolvedModelInfo{
		LlmModelInfo: llm.LlmModelInfo{Provider: provider, ID: model, Name: model},
		Context:      &llm.LlmModelContext{ContextWindow: window},
	}, nil
}

func (a *scriptAdapter) requestCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.requests)
}

func (a *scriptAdapter) request(index int) llm.GenerateOptions {
	a.mu.Lock()
	defer a.mu.Unlock()
	if index >= len(a.requests) {
		panic(fmt.Sprintf("request %d not observed (%d total)", index, len(a.requests)))
	}
	return a.requests[index]
}

// textChunks scripts one visible-text response.
func textChunks(text string) []llm.StreamChunk {
	return []llm.StreamChunk{
		{Type: llm.ChunkBlockStart, Index: 0, BlockType: llm.BlockText},
		{Type: llm.ChunkTextDelta, Index: 0, Text: text},
		{Type: llm.ChunkBlockEnd, Index: 0, Block: &llm.ContentBlock{Type: llm.BlockText, Text: text}},
		{Type: llm.ChunkUsage, Usage: &llm.TokenUsage{}},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishStop}},
	}
}

// toolCallChunks scripts one tool-call response.
func toolCallChunks(id, name, arguments string) []llm.StreamChunk {
	return []llm.StreamChunk{
		{Type: llm.ChunkBlockStart, Index: 0, BlockType: llm.BlockToolCall},
		{Type: llm.ChunkToolCallDelta, Index: 0, ID: llm.ToolCallID(id), Name: name, ArgumentsDelta: arguments},
		{Type: llm.ChunkBlockEnd, Index: 0, Block: &llm.ContentBlock{Type: llm.BlockToolCall, ID: id, Name: name, Arguments: arguments}},
		{Type: llm.ChunkUsage, Usage: &llm.TokenUsage{}},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishToolCalls}},
	}
}

// maxTokensChunks scripts a response truncated by the output ceiling.
func maxTokensChunks(text string) []llm.StreamChunk {
	return []llm.StreamChunk{
		{Type: llm.ChunkBlockStart, Index: 0, BlockType: llm.BlockText},
		{Type: llm.ChunkTextDelta, Index: 0, Text: text},
		{Type: llm.ChunkBlockEnd, Index: 0, Block: &llm.ContentBlock{Type: llm.BlockText, Text: text}},
		{Type: llm.ChunkUsage, Usage: &llm.TokenUsage{}},
		{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishMaxTokens}},
	}
}

// harness wires one full agent loop over a script adapter with a quiet
// tool runtime.
type harness struct {
	t        *testing.T
	adapter  *scriptAdapter
	loop     *AgentLoop
	registry *agent.AgentRegistry
	tools    *tools.ToolRuntime
	events   *agent.SubjectEventBus
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	adapter := &scriptAdapter{}
	llmRuntime := llm.NewRuntime()
	if _, err := llmRuntime.RegisterAdapter([]string{"stub"}, adapter); err != nil {
		t.Fatalf("RegisterAdapter: %v", err)
	}
	toolRuntime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("NewToolRuntime: %v", err)
	}
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("NewSystemPrompt: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	loop, err := NewAgentLoop(cordis.NewRoot(cordis.Discard{}), registry, cordis.Discard{}, llmRuntime, toolRuntime, prompt, projection.NewRegistry(), AgentLoopConfig{})
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	h := &harness{t: t, adapter: adapter, loop: loop, registry: registry, tools: toolRuntime, events: registry.Events()}
	t.Cleanup(func() { _ = loop.ownership.dispose() })
	return h
}

// startAgent creates one agent with the stub route and waits for publication.
func (h *harness) startAgent(id string) *agent.Agent {
	h.t.Helper()
	handle, err := h.loop.CreateAgent(nil, agent.CreateAgentOptions{
		SessionID:    session.SessionID(id),
		AgentOptions: agent.AgentOptions{Provider: "stub", Model: "stub-model"},
	})
	if err != nil {
		h.t.Fatalf("CreateAgent: %v", err)
	}
	return handle.Agent
}

// run sends one user turn and waits for the driver to quiesce.
func (h *harness) run(a *agent.Agent, text string) {
	h.t.Helper()
	h.send(a, text, true)
	h.awaitIdle(a)
}

// send queues one user turn without waiting for quiescence.
func (h *harness) send(a *agent.Agent, text string, wakeup bool) {
	h.t.Helper()
	h.sendTarget(a, text, agent.InboxNextTurn, wakeup)
}

// sendTarget queues one user message at an explicit inbox boundary.
func (h *harness) sendTarget(a *agent.Agent, text string, target agent.InboxTarget, wakeup bool) {
	h.t.Helper()
	driver, ok := a.Driver().(*ReactLoopAgent)
	if !ok {
		h.t.Fatalf("agent %q has no ReactLoopAgent driver", a.ID)
	}
	driver.Send(llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourceUser}), target, wakeup)
}

func (h *harness) awaitIdle(a *agent.Agent) {
	h.t.Helper()
	driver, ok := a.Driver().(*ReactLoopAgent)
	if !ok {
		h.t.Fatalf("agent %q has no ReactLoopAgent driver", a.ID)
	}
	select {
	case <-driver.WhenIdle():
	case <-time.After(10 * time.Second):
		h.t.Fatalf("agent %q did not quiesce in time", a.ID)
	}
}

// eventTypes flattens the session log to event type names.
func eventTypes(a *agent.Agent) []string {
	names := make([]string, 0, len(a.Session.Events()))
	for _, event := range a.Session.Events() {
		names = append(names, event.Type)
	}
	return names
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
