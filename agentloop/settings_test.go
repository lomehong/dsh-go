// The settings read-through: a committed agent-loop section change re-caps
// the scheduler for the next tool group (SetMaxParallelToolCalls), and the
// config-start failure emitter fires on contained declarative-start
// failures.
package agentloop

import (
	"errors"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/systemprompt"
	"dshgo/tools"
)

func newLiveLoop(t *testing.T, config AgentLoopConfig) *AgentLoop {
	t.Helper()
	prompt, err := systemprompt.NewSystemPrompt(systemprompt.Config{})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	toolRuntime, toolErr := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if toolErr != nil {
		t.Fatalf("tools: %v", toolErr)
	}
	loop, err := NewAgentLoop(cordis.NewRoot(cordis.Discard{}), registry, cordis.Discard{}, llm.NewRuntime(), toolRuntime, prompt, projection.NewRegistry(), config)
	if err != nil {
		t.Fatalf("NewAgentLoop: %v", err)
	}
	return loop
}

// The setter re-caps the live scheduler: valid values land, invalid values
// keep the last good cap (the official validate-keeps-last-good rule).
func TestSetMaxParallelToolCallsLive(t *testing.T) {
	loop := newLiveLoop(t, AgentLoopConfig{MaxParallelToolCalls: intPtr(3)})
	if got := loop.maxParallelToolCalls.Load(); got != 3 {
		t.Fatalf("initial cap = %d, want 3", got)
	}
	if err := loop.SetMaxParallelToolCalls(7); err != nil {
		t.Fatalf("set 7: %v", err)
	}
	if got := loop.maxParallelToolCalls.Load(); got != 7 {
		t.Fatalf("cap after set = %d, want 7", got)
	}
	if err := loop.SetMaxParallelToolCalls(0); err == nil || !strings.Contains(err.Error(), "positive integer") {
		t.Fatalf("err = %v, want the positive-integer refusal", err)
	}
	if got := loop.maxParallelToolCalls.Load(); got != 7 {
		t.Fatalf("cap after rejected set = %d, want the last good 7", got)
	}
}

// The config-start failure emitter fires with the configured agent's
// session id and the error (official agent-loop/config-start-failed).
func TestConfigStartFailedEmitterFires(t *testing.T) {
	type failure struct {
		sessionID session.SessionID
		err       error
	}
	hits := make(chan failure, 1)
	loop := newLiveLoop(t, AgentLoopConfig{
		ConfigStartFailed: func(sessionID session.SessionID, err error) {
			hits <- failure{sessionID: sessionID, err: err}
		},
	})
	loop.reportConfiguredStartupFailure("cfg-1", "restore", "session-csf", errors.New("boom"))
	select {
	case hit := <-hits:
		if hit.sessionID != "session-csf" || !strings.Contains(hit.err.Error(), "boom") {
			t.Fatalf("hit = %#v", hit)
		}
	default:
		t.Fatal("the config-start-failed emitter never fired")
	}
}
