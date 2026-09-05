package headless

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
)

// The runner's plugin faces, registered in the boot catalog:
//
//   - `@deepseek-ai/dsh-headless/startup` provides the one-shot task parsed
//     from the launcher's cmdlineArgs.
//   - `@deepseek-ai/dsh-headless` (runner) creates one Agent through the
//     registry, drives the task to quiescence, streams provider reasoning to
//     stderr, flushes the Session, prints the final assistant text, and
//     requests process exit.

// ProvideStartup parses cmdlineArgs and provides the startup service. A
// missing or whitespace-only task is a usage error; --help prints usage and
// exits 0 through the recorded exit code.
func ProvideStartup(ctx *cordis.Context, config any) error {
	args, _ := ctx.Get("cmdlineArgs").([]string)
	task, help := ParseTask(args)
	if help {
		fmt.Fprintln(Stderr, "Usage: dsh --profile headless \"task\"\n\nAnswer one task, stream reasoning to stderr, print the final assistant message, and exit.\n\nExample:\n  dsh --profile headless \"run the tests\"")
		RequestExit(0)
		return cordisVeto("headless-startup: --help")
	}
	if strings.TrimSpace(task) == "" {
		RequestExit(1)
		return cordisVeto(`error: a task is required, for example: dsh --profile headless "run the tests"`)
	}
	ctx.Provide(StartupService, map[string]any{"task": task})
	return nil
}

// cordisVeto fails one plugin apply with a user-facing diagnostic (the
// cordis apply wraps it into the loud import failure).
type cordisVetoError struct{ message string }

func (e *cordisVetoError) Error() string { return e.message }

func cordisVeto(message string) error { return &cordisVetoError{message: message} }

// RunnerConfig is the runner row's validated config: the task read from the
// startup service through the yml `!!js ctx.headlessStartup.task`.
type RunnerConfig struct {
	Task string `json:"task"`
}

// DecodeRunnerConfig reads the runner row's task member.
func DecodeRunnerConfig(config any) (RunnerConfig, error) {
	decoded, err := json.Marshal(config)
	if err != nil {
		return RunnerConfig{}, err
	}
	var parsed RunnerConfig
	if err := json.Unmarshal(decoded, &parsed); err != nil {
		return RunnerConfig{}, err
	}
	return parsed, nil
}

// Run drives one task through a freshly created Agent and requests the
// process exit. Port of the official run(): await the complete application
// (Go composition is synchronous — a no-op), create the Agent with the
// default model selection, submit the task as a followup, wait for idle,
// flush the Session, print the final assistant text, and report the outcome
// code.
func Run(ctx *cordis.Context, config RunnerConfig) error {
	agentsAny := ctx.Get("agents")
	registry, ok := agentsAny.(*agent.AgentRegistry)
	if !ok || registry == nil {
		return fmt.Errorf("headless-runner: no agent registry is composed")
	}
	defaultModelAny := ctx.Get("agentDefaultModel")
	defaultModel, ok := defaultModelAny.(*agentdefaultmodel.Config)
	if !ok || defaultModel == nil {
		return fmt.Errorf("headless-runner: no agent-default-model service is composed")
	}
	sessionsAny := ctx.Get("sessions")
	store, ok := sessionsAny.(*session.Store)
	if !ok || store == nil {
		return fmt.Errorf("headless-runner: no session store is composed")
	}
	var flusher interface {
		FlushSession(sess *session.Session) error
	}
	if persistAny := ctx.Get("sessionPersistence"); persistAny != nil {
		if coordinator, ok := persistAny.(*persistence.Coordinator); ok {
			flusher = coordinator
		}
	}
	selection := defaultModel.CurrentSelection()

	handle, err := registry.Create(context.Background(), agent.CreateAgentOptions{
		SessionID: session.SessionID(generateSessionID()),
		Meta: agent.CreateAgentMeta{
			CWD: cwdOrEmpty(),
		},
		AgentOptions: agent.AgentOptions{
			Provider: selection.Provider,
			Model:    selection.Model,
		},
	})
	if err != nil {
		return err
	}
	created := handle.Agent
	defer func() { _ = handle.Dispose() }()

	// The reasoning relay: provider-reported reasoning streams to stderr
	// while the durable log stays the outcome authority (official
	// streamReasoning over the agent/assistant-stream feed).
	stopRelay := relayReasoning(ctx, created, Stderr)
	defer stopRelay()

	baseline := int64(created.Session.Seq()) - 1
	created.Driver().Followup(llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: config.Task}},
		llm.MessageSource{Kind: llm.SourceUser},
	))
	// WhenIdle samples the current activity: a followup race can make the
	// first observation see the pre-kick idle, so quiescence is confirmed
	// only when a turn boundary has landed past the baseline (the official
	// await semantics ride the promise, not a channel sample).
	deadline := time.Now().Add(15 * time.Minute)
	for {
		<-created.Driver().WhenIdle()
		if hasTurnBoundaryPast(created.Session, baseline) {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("headless-runner: the task did not quiesce within 15m")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if flusher != nil {
		if flushErr := flusher.FlushSession(created.Session); flushErr != nil {
			// The run itself succeeded; a flush failure still surfaces
			// (official `await sessions.flush` is load-bearing).
			return flushErr
		}
	}

	outcome := summarize(created.Session)
	if strings.TrimSpace(outcome.text) != "" {
		fmt.Fprintln(Stdout, outcome.text)
	}
	if outcome.errorCode != "" {
		fmt.Fprintf(Stderr, "dsh: %s: %s\n", outcome.errorCode, outcome.errorMessage)
	}
	if outcome.completed {
		RequestExit(0)
	} else {
		RequestExit(1)
	}
	return nil
}

// outcome is the aggregated run result (official RunOutcome).
type outcome struct {
	text         string
	completed    bool
	errorCode    string
	errorMessage string
}

// summarize walks the session log for the last assistant text and the final
// turn outcome (official summarize: last text-bearing assistant message
// wins; the final turn/end names the outcome).
func summarize(sess *session.Session) outcome {
	events := sess.Events()
	result := outcome{}
	for _, event := range events {
		switch event.Type {
		case session.EventAssistantMsg:
			data, err := session.DecodeAssistantMessage(event)
			if err != nil {
				continue
			}
			if text := assistantText(data.Message.Content); strings.TrimSpace(text) != "" {
				result.text = text
			}
		case session.EventTurnEnd:
			data, err := decodeTurnEnd(event)
			if err != nil {
				continue
			}
			result.completed = data.Reason.Kind == session.TurnEndCompleted
			if data.Reason.Kind == session.TurnEndError && data.Reason.Error != nil {
				result.errorCode = data.Reason.Error.Code
				result.errorMessage = data.Reason.Error.Message
			}
		}
	}
	return result
}

func decodeTurnEnd(event session.Event) (session.TurnEndData, error) {
	var data session.TurnEndData
	err := json.Unmarshal(event.Data, &data)
	return data, err
}

// relayReasoning streams provider-reported reasoning deltas to stderr as
// they are published on the agent's scoped feed (official streamReasoning).
// The returned disposer also terminates an unterminated reasoning line.
func relayReasoning(ctx *cordis.Context, target *agent.Agent, stderr io.Writer) func() {
	var open bool
	var endsWithNewline = true
	closeLine := func() {
		if !open {
			return
		}
		if !endsWithNewline {
			fmt.Fprint(stderr, "\n")
		}
		open = false
		endsWithNewline = true
	}
	detach := target.Events().AssistantStream().On(nil, func(payload agent.AssistantStreamFrame) error {
		if payload.Type != "chunk" || payload.Chunk == nil {
			closeLine()
			return nil
		}
		switch payload.Chunk.Type {
		case llm.ChunkReasoningDelta:
			if payload.Chunk.Text == "" {
				return nil
			}
			if !open {
				fmt.Fprint(stderr, "dsh: reasoning:\n")
				open = true
			}
			fmt.Fprint(stderr, payload.Chunk.Text)
			endsWithNewline = strings.HasSuffix(payload.Chunk.Text, "\n")
		default:
			closeLine()
		}
		return nil
	})
	return func() {
		detach()
		closeLine()
	}
}

// hasTurnBoundaryPast reports whether a turn/end landed beyond the baseline
// seq (the submitted task's turn reached an outcome).
func hasTurnBoundaryPast(sess *session.Session, baseline int64) bool {
	events := sess.Events()
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Seq <= baseline {
			return false
		}
		if event.Type == session.EventTurnEnd {
			return true
		}
	}
	return false
}

func cwdOrEmpty() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

var sessionIdCounter atomic.Int64

// generateSessionID names the one-shot session (official
// `session-${randomUUID()}`; Go uses a monotonic counter plus a timestamp
// for uniqueness without an uuid dependency).
func generateSessionID() string {
	return fmt.Sprintf("session-headless-%d-%d", time.Now().UnixMilli(), sessionIdCounter.Add(1))
}
