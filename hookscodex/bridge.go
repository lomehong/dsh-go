// Bridge ports hooks-codex/src/index.ts: register Codex hook handlers on
// the harness interception points.
//
// Go adaptations, each documented at its site: `agent.inject` and
// `agent.steer` map to Inbox.Append(InboxNextStep) — the Go pending-input
// store the driver claims at the next step and re-checks after
// agent/turn-stopping, so an append at the stopping boundary forces another
// step exactly like the official steer; extension points run synchronously
// (the Go pipeline and driver await their listeners at the same boundaries
// the official awaited); the session persistence service is not a Go
// service, so transcript_path comes from an optional LocateTranscript
// config func (empty when unset, matching the official `?.locate(...)?.path
// ?? ”`); the subagent lifecycle bus is optional (Codex registers no
// subagent points, so no parameter exists at all).
package hookscodex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/hookprotocol"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tools"
)

// Name is the plugin name this bridge stamps on sources and diagnostics.
const Name = "hooks-codex"

// Config is the plugin config: where the Codex hooks.json lives + the model
// name for payloads.
type Config struct {
	// ConfigPath points at a Codex hooks.json. Process-level: read once at
	// load, a relative path resolves against the process launch cwd.
	ConfigPath string
	// Model is the model name stamped on every payload (Codex includes
	// `model` on each event).
	Model string
	// DefaultTimeoutMs is the per-hook timeout when a hook sets none
	// (Codex default: 600000). Zero applies the reference default.
	DefaultTimeoutMs int64
	// StderrSummaryMaxChars caps the hook/result event's persisted stderr
	// summary. Zero applies the reference default; a non-positive value
	// after defaulting fails the apply.
	StderrSummaryMaxChars int
	// LocateTranscript maps a session header to its transcript path for
	// payload building (the official sessionPersistence locate). nil sends
	// "" — the same value the official sends without the service.
	LocateTranscript func(header *session.SessionHeader) string
	// Logger receives skip/config warnings; nil discards.
	Logger cordis.Logger
	// Now is the millisecond clock for run durations; nil uses wall time
	// (test seam).
	Now func() int64
}

// bridge carries the parsed config and shared state one Apply instance
// owns.
type bridge struct {
	config                Config
	parsed                map[string][]hookprotocol.MatcherGroup
	stderrSummaryMaxChars int
	defaultTimeoutMs      int64
	detached              *hookprotocol.DetachedRuns
	logger                cordis.Logger
	now                   func() int64
}

// runHook indirection lets tests stub execution deterministically; the
// production value is the protocol runner.
var runHook = hookprotocol.RunHook

// Apply validates the config, parses the hook config file, and registers
// the bridge's listeners. It returns the disposer that unregisters every
// listener, aborts still-running detached hooks, and drains their
// continuations. A config file that cannot be read or parsed logs a warning
// and registers nothing (no error) — the reference behavior.
func Apply(agents *agent.AgentRegistry, runtime *tools.ToolRuntime, config Config) (func(), error) {
	if agents == nil {
		return nil, fmt.Errorf("hooks-codex: agents registry is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("hooks-codex: tool runtime is required")
	}
	// Validate before config parsing so a bad value cannot be hidden by its
	// early return.
	stderrSummaryMaxChars := config.StderrSummaryMaxChars
	if stderrSummaryMaxChars == 0 {
		stderrSummaryMaxChars = hookprotocol.DefaultStderrSummaryMaxChars
	}
	if stderrSummaryMaxChars < 1 {
		return nil, fmt.Errorf("hooks-codex: stderrSummaryMaxChars must be a positive integer")
	}
	defaultTimeoutMs := config.DefaultTimeoutMs
	if defaultTimeoutMs == 0 {
		defaultTimeoutMs = hookprotocol.DefaultHookTimeoutMs
	}
	logger := config.Logger
	if logger == nil {
		logger = cordis.Discard{}
	}
	now := config.Now
	if now == nil {
		now = func() int64 { return timeNowMs() }
	}

	// Parse once at load. A read or parse failure logs and registers
	// nothing.
	b := &bridge{
		config:                config,
		parsed:                map[string][]hookprotocol.MatcherGroup{},
		stderrSummaryMaxChars: stderrSummaryMaxChars,
		defaultTimeoutMs:      defaultTimeoutMs,
		detached:              hookprotocol.NewDetachedRuns(),
		logger:                logger,
		now:                   now,
	}
	raw, err := os.ReadFile(config.ConfigPath)
	if err == nil {
		var decoded any
		if err = json.Unmarshal(raw, &decoded); err != nil {
			err = fmt.Errorf("hooks-codex: %w", err)
		} else {
			var parsed ParsedCodexConfig
			parsed, err = ParseCodexConfig(decoded)
			if err == nil {
				b.parsed = parsed.Config
				for _, skipped := range parsed.Skipped {
					logger.Warn(fmt.Sprintf("hooks-codex: skipping %s on %s (only sync command hooks run)", skipped.Reason, skipped.Event))
				}
			}
		}
	}
	if err != nil {
		logger.Warn(fmt.Sprintf("hooks-codex: could not load hook config %q: %v — no hooks registered", config.ConfigPath, err))
		b.detached.Drain()
		return func() {}, nil
	}

	disposers := []func(){}

	// SessionStart is the one emit-shaped (detached) point Codex has:
	// track its run chains so disposal aborts a still-running hook process
	// and drains the continuation.
	disposers = append(disposers, func() { b.detached.Drain() })

	// SessionStart injects plain stdout when its detached hook resolves; a
	// slow hook may miss the first request.
	disposers = append(disposers, agents.Events().OnEmit(agent.EventAgentSessionStart, nil, func(payload any) error {
		start, ok := payload.(agent.AgentSessionStartPayload)
		if !ok || start.Agent == nil {
			return nil
		}
		agentRef := start.Agent
		source := string(start.Source)
		sessionPayload := b.base(pointSessionStart, agentRef)
		sessionPayload["source"] = source
		b.detached.Track(func(ctx context.Context) {
			merged, err := b.runPoint(pointSessionStart, source, sessionPayload, runPointOptions{agent: agentRef, signal: ctx, plainStdoutAsContext: true})
			if err != nil {
				logger.Warn(fmt.Sprintf("hooks-codex: SessionStart hook failed: %v", err))
				return
			}
			if context := contextFrom(b, merged); context != nil {
				// agent.inject maps to the next-step pending-input store.
				_ = agentRef.Inbox.Append(agent.InboxNextStep, *context)
			}
		})
		return nil
	}))

	// UserPromptSubmit → PreStepDecision. Codex supports reject, not
	// rewrite or ask.
	disposers = append(disposers, agents.Events().OnWaterfall(agent.EventPreStep, nil, func(payload any, next func(any) any) any {
		step, ok := payload.(agent.PreStepPayload)
		if !ok {
			return next(payload)
		}
		if len(step.Messages) == 0 {
			return next(payload)
		}
		var blocks []llm.ContentBlock
		for _, message := range step.Messages {
			blocks = append(blocks, message.Content...)
		}
		promptPayload := b.turnBase(pointUserPromptSubmit, step.Agent, step.Turn)
		promptPayload["prompt"] = blocksToText(blocks)
		merged, runErr := b.runPoint(pointUserPromptSubmit, "", promptPayload, runPointOptions{agent: step.Agent, turn: step.Turn, hasTurn: true, signal: step.Signal, plainStdoutAsContext: true})
		if runErr != nil {
			b.logger.Warn(fmt.Sprintf("hooks-codex: UserPromptSubmit hook failed: %v", runErr))
			return next(payload)
		}
		if merged.Decision == hookprotocol.MergedDeny {
			return agent.PreStepReject()
		}
		// Context alone is not a veto: DELEGATE so a later pre-step
		// listener can still reject/rewrite, then fold our context onto its
		// decision.
		downstream := next(payload)
		decision, ok := downstream.(agent.PreStepDecision)
		if !ok {
			return downstream
		}
		ours := contextFrom(b, merged)
		if ours == nil || decision.Kind != "enter" {
			return decision
		}
		messages := make([]llm.Message, 0, len(decision.Messages)+1)
		messages = append(messages, decision.Messages...)
		messages = append(messages, *ours)
		decision.Messages = messages
		return decision
	}))

	// PreToolUse → PreToolDecision. Codex blocks only (no allow/ask
	// honored). The Go ToolExecution routes the agent as a scope key, so
	// the listener resolves the live agent from the registry first.
	disposers = append(disposers, runtime.OnPreExecute(nil, func(exec *tools.ToolExecution, next func(*tools.ToolExecution) *tools.PreToolDecision) *tools.PreToolDecision {
		execAgent := resolveByScope(agents, exec.Agent)
		turn := lastTurn(execAgent)
		outcome, runErr := b.runPoint(pointPreToolUse, exec.Name, b.preToolPayload(exec, execAgent, turn), runPointOptions{agent: execAgent, turn: turn, hasTurn: execAgent != nil, signal: context.Background()})
		merged := b.mergeOrWarn(pointPreToolUse, outcome, runErr)
		if merged.Decision == hookprotocol.MergedDeny {
			return &tools.PreToolDecision{Kind: tools.PreDeny, Reason: blockReason(merged, "blocked by PreToolUse hook"), HasReason: true}
		}
		return next(exec)
	}))

	// PostToolUse → PostToolDecision (block with feedback, or attach
	// context).
	disposers = append(disposers, runtime.OnPostExecute(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult, next func(*tools.ToolExecutionResult) *tools.PostToolDecision) *tools.PostToolDecision {
		execAgent := resolveByScope(agents, exec.Agent)
		turn := lastTurn(execAgent)
		outcome, runErr := b.runPoint(pointPostToolUse, exec.Name, b.postToolPayload(exec, result, execAgent, turn), runPointOptions{agent: execAgent, turn: turn, hasTurn: execAgent != nil, signal: context.Background()})
		merged := b.mergeOrWarn(pointPostToolUse, outcome, runErr)
		contextMsg := contextFrom(b, merged)
		if merged.Decision == hookprotocol.MergedDeny {
			decision := &tools.PostToolDecision{
				Kind:     tools.PostBlock,
				Feedback: []llm.ContentBlock{{Type: llm.BlockText, Text: blockReason(merged, "blocked by PostToolUse hook")}},
			}
			if contextMsg != nil {
				decision.AdditionalContexts = []llm.Message{*contextMsg}
			}
			return decision
		}
		// Context alone is not a veto: DELEGATE, then fold our context onto
		// the downstream decision (a downstream block carries it too).
		downstream := next(result)
		if contextMsg == nil {
			return downstream
		}
		contexts := make([]llm.Message, 0, len(downstream.AdditionalContexts)+1)
		contexts = append(contexts, *contextMsg)
		contexts = append(contexts, downstream.AdditionalContexts...)
		downstream.AdditionalContexts = contexts
		return downstream
	}))

	// A blocking Stop hook steers at the stopping boundary, which makes the
	// machine observe pending input and run another step.
	disposers = append(disposers, agents.Events().OnSerial(agent.EventTurnStopping, nil, func(payload any) (any, bool) {
		stopping, ok := payload.(agent.TurnStoppingPayload)
		if !ok || stopping.Agent == nil {
			return nil, false
		}
		stopPayload := b.turnBase(pointStop, stopping.Agent, stopping.Turn)
		stopPayload["stop_hook_active"] = false
		stopPayload["last_assistant_message"] = nil
		merged, runErr := b.runPoint(pointStop, "", stopPayload, runPointOptions{agent: stopping.Agent, turn: stopping.Turn, hasTurn: true, signal: stopping.Signal, plainStdoutAsContext: true})
		if runErr != nil {
			b.logger.Warn(fmt.Sprintf("hooks-codex: Stop hook failed: %v", runErr))
			return nil, false
		}
		if merged.Decision == hookprotocol.MergedDeny {
			// A blocking Stop hook forces continuation; a block with no
			// reason (exit 2, empty stderr) still forces it — fall back to
			// a generic steering line rather than letting the turn stop.
			text := blockReason(merged, "continue: blocked by Stop hook")
			// agent.steer maps to the same pending-input store; the driver
			// re-checks it after this boundary and runs another step.
			_ = stopping.Agent.Inbox.Append(agent.InboxNextStep, userMessage(b, text))
		}
		return nil, false
	}))

	return func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}, nil
}

// resolveByScope maps a tool execution scope back to the live agent.
func resolveByScope(agents *agent.AgentRegistry, key tools.ScopeKey) *agent.Agent {
	for _, candidate := range agents.List() {
		if candidate.Scope == key {
			return candidate
		}
	}
	return nil
}

// runPointOptions carry the per-invocation agent/turn/signal context plus
// Codex's plain-stdout context rule.
type runPointOptions struct {
	agent   *agent.Agent
	turn    int64
	hasTurn bool
	signal  context.Context
	// plainStdoutAsContext promotes clean plain stdout to
	// additionalContext (Codex SessionStart/UserPromptSubmit).
	plainStdoutAsContext bool
}

// The hook points this bridge fires (the Codex event names).
const (
	pointSessionStart     = "SessionStart"
	pointUserPromptSubmit = "UserPromptSubmit"
	pointPreToolUse       = "PreToolUse"
	pointPostToolUse      = "PostToolUse"
	pointStop             = "Stop"
)

// runPoint runs and folds one configured Codex hook point. A supplied turn
// records the hook invocation/result pair inside that open turn; detached
// lifecycle points omit it.
func (b *bridge) runPoint(point string, matchQuery string, payload map[string]any, opts runPointOptions) (hookprotocol.MergedHookOutcome, error) {
	groups := b.parsed[point]
	outputs := []hookprotocol.HookOutput{}
	// Run hooks in the agent's session workspace so relative paths address
	// the user's project rather than the server launch directory.
	workdir := ""
	sess := (*session.Session)(nil)
	if opts.agent != nil {
		workdir = opts.agent.Session.Header().CWD
		sess = opts.agent.Session
	}
	for _, group := range groups {
		// Codex always interprets matchers as regexes; it has no literal
		// fast path.
		if !hookprotocol.MatchesMatcher(group.Matcher, matchQuery, hookprotocol.MatcherModeCodex) {
			continue
		}
		for _, hook := range group.Hooks {
			handlerID := fmt.Sprintf("codex:%s:%d", point, nextHandlerID())
			if sess != nil && opts.hasTurn {
				if err := hookprotocol.AppendHookInvoked(sess, hookprotocol.HookInvocation{
					Turn:      opts.turn,
					Point:     point,
					Dialect:   hookprotocol.DialectCodex,
					HandlerID: handlerID,
					Matcher:   group.Matcher,
				}); err != nil {
					return hookprotocol.MergedHookOutcome{}, err
				}
			}
			run := runHook(hook, hookprotocol.RunHookOptions{
				Payload:           payload,
				CWD:               workdir,
				Signal:            opts.signal,
				TrailingNewline:   false, // Codex writes stdin without a trailing newline.
				DefaultTimeoutMs:  b.defaultTimeoutMs,
				ExpectedEventName: point,
			}, b.now)
			output := run.Output
			// Clean plain stdout becomes context only when no structured
			// context exists; nonzero output and raw JSON never leak as
			// prose.
			if opts.plainStdoutAsContext && output.ExitCode != nil && *output.ExitCode == 0 &&
				output.AdditionalContext == "" && output.Stdout != "" && !strings.HasPrefix(output.Stdout, "{") {
				output.AdditionalContext = output.Stdout
			}
			outputs = append(outputs, output)
			if output.SystemMessage != "" {
				b.logger.Warn(fmt.Sprintf("hooks-codex: %s hook emitted a systemMessage, which is not yet surfaced (ignored)", point))
			}
			if sess != nil && opts.hasTurn {
				if err := hookprotocol.AppendHookResult(sess, hookprotocol.HookResultRecord{
					Turn:                  opts.turn,
					Point:                 point,
					HandlerID:             handlerID,
					Output:                output,
					StderrSummaryMaxChars: b.stderrSummaryMaxChars,
					DurationMs:            run.DurationMs,
				}); err != nil {
					return hookprotocol.MergedHookOutcome{}, err
				}
			}
		}
	}
	return hookprotocol.MergeHookOutputs(outputs), nil
}

// contextFrom builds additional model context from hook output, or nil
// when empty.
func contextFrom(b *bridge, merged hookprotocol.MergedHookOutcome) *llm.Message {
	if len(merged.AdditionalContext) == 0 {
		return nil
	}
	blocks := make([]llm.ContentBlock, 0, len(merged.AdditionalContext))
	for _, text := range merged.AdditionalContext {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockText, Text: text})
	}
	message := llm.NewUserMessage(blocks, llm.MessageSource{Kind: llm.SourcePlugin, Plugin: Name})
	return &message
}

// mergeOrWarn contains a runPoint failure at the synchronous tool
// extension points: the run is logged and treated as neutral so a durable
// log write failure degrades one call instead of failing the pipeline.
func (b *bridge) mergeOrWarn(point string, outcome hookprotocol.MergedHookOutcome, err error) hookprotocol.MergedHookOutcome {
	if err == nil {
		return outcome
	}
	b.logger.Warn(fmt.Sprintf("hooks-codex: %s hook failed: %v", point, err))
	return hookprotocol.MergedHookOutcome{Decision: hookprotocol.MergedNone}
}

// blockReason prefers the merged reason, falling back to the caller's
// generic line (a block with no reason still blocks).
func blockReason(merged hookprotocol.MergedHookOutcome, fallback string) string {
	if merged.Reason != "" {
		return merged.Reason
	}
	return fallback
}

// userMessage builds the plugin-sourced user message hooks inject.
func userMessage(b *bridge, text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{Kind: llm.SourcePlugin, Plugin: Name})
}

// handlerIDCounter mints stable per-handler ids so an invoked/result pair
// correlates in the log.
var handlerIDCounter int64

func nextHandlerID() int64 {
	handlerIDCounter++
	return handlerIDCounter
}

// lastTurn reads the last open turn number in the agent's log, or 0
// without an agent.
func lastTurn(a *agent.Agent) int64 {
	if a == nil {
		return 0
	}
	last := int64(0)
	for _, event := range a.Session.Events() {
		if event.Type != session.EventTurnStart {
			continue
		}
		var data session.TurnStartData
		if json.Unmarshal(event.Data, &data) == nil {
			last = data.Turn
		}
	}
	return last
}

// blocksToText flattens content blocks to the text a hook payload carries
// (the common case).
func blocksToText(content []llm.ContentBlock) string {
	var builder strings.Builder
	for _, block := range content {
		if block.Type == llm.BlockText {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

// base holds the fields on every Codex payload (no turn_id).
func (b *bridge) base(event string, a *agent.Agent) map[string]any {
	sessionID := ""
	transcriptPath := ""
	cwd := ""
	if a != nil {
		header := a.Session.Header()
		sessionID = string(header.ID)
		cwd = header.CWD
		if b.config.LocateTranscript != nil {
			transcriptPath = b.config.LocateTranscript(&header)
		}
	}
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcriptPath,
		"cwd":             cwd,
		"hook_event_name": event,
		"model":           b.config.Model,
		"permission_mode": "default",
	}
}

// turnBase is base + turn_id, for the turn-scoped events
// (PreToolUse/PostToolUse/UserPromptSubmit/Stop).
func (b *bridge) turnBase(event string, a *agent.Agent, turn int64) map[string]any {
	payload := b.base(event, a)
	payload["turn_id"] = strconv.FormatInt(turn, 10)
	return payload
}

// commandOf extracts a `command` string from a tool call's parsed
// arguments, else "".
func commandOf(args any) string {
	if m, ok := args.(map[string]any); ok {
		if command, ok := m["command"].(string); ok {
			return command
		}
	}
	return ""
}

func (b *bridge) preToolPayload(exec *tools.ToolExecution, execAgent *agent.Agent, turn int64) map[string]any {
	// `tool_name` is the REAL tool name (matching the exec.Name matcher
	// subject); a hardcoded constant would disagree with what the matcher
	// tests and make a config's tool matcher never fire. `tool_input`
	// keeps Codex's { command } shape (its shell payload), derived from
	// the call's `command` arg when present.
	payload := b.turnBase("PreToolUse", execAgent, turn)
	payload["tool_name"] = exec.Name
	payload["tool_input"] = map[string]any{"command": commandOf(exec.Arguments)}
	payload["tool_use_id"] = exec.CallID
	return payload
}

func (b *bridge) postToolPayload(exec *tools.ToolExecution, result *tools.ToolExecutionResult, execAgent *agent.Agent, turn int64) map[string]any {
	payload := b.turnBase("PostToolUse", execAgent, turn)
	payload["tool_name"] = exec.Name
	payload["tool_input"] = map[string]any{"command": commandOf(exec.Arguments)}
	payload["tool_use_id"] = exec.CallID
	payload["tool_response"] = blocksToText(result.Content)
	return payload
}

// timeNowMs is the wall-clock millisecond clock.
func timeNowMs() int64 {
	return time.Now().UnixMilli()
}
