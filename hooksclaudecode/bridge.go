// Bridge ports hooks-claude-code/src/index.ts: register CC hook handlers on
// the harness interception points.
//
// Go adaptations, each documented at its site: `agent.inject` maps to
// Inbox.Append(InboxNextStep) (the Go pending-input store the driver
// claims at the next step); `agent.steer` maps to the same append — the
// driver re-checks the queue after agent/turn-stopping, so an append there
// forces another step exactly like the official steer; extension points run
// synchronously (the Go pipeline and driver await their listeners at the
// same boundaries the official awaited); the session persistence service is
// not a Go service, so transcript_path comes from an optional
// LocateTranscript config func (empty when unset, matching the official
// `ctx.get('sessionPersistence')?.locate(...)?.path ?? ”`).
package hooksclaudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/hookprotocol"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/subagent"
	"dshgo/tools"
)

// Name is the plugin name this bridge stamps on sources and diagnostics.
const Name = "hooks-claude-code"

// subagentType is the agent_type value the bridge reports for
// SubagentStart/Stop. The harness subagent seam carries no per-kind label,
// so the bridge uses Claude Code's own Task-tool default — a hooks.json
// with a default/'*'/empty agent_type matcher fires; a config matching a
// specific kind (e.g. code-reviewer) does not.
const subagentType = "general-purpose"

// Config is the plugin config: where the CC hook config lives +
// substitution roots.
type Config struct {
	// ConfigPath points at a hooks.json or a settings file whose `hooks`
	// key holds the config. Process-level: read once at load, a relative
	// path resolves against the process launch cwd, so one config applies
	// to the whole process.
	ConfigPath string
	// PluginRoot replaces ${CLAUDE_PLUGIN_ROOT} in command strings. nil
	// leaves the token verbatim.
	PluginRoot *string
	// ProjectDir replaces ${CLAUDE_PROJECT_DIR} in command strings AND is
	// exported as the CLAUDE_PROJECT_DIR env var for hook processes. When
	// nil, the env var defaults per-run to the agent's session workspace
	// (session.header.cwd, the same dir the hook runs in) — Claude Code
	// always exports this var.
	ProjectDir *string
	// DefaultTimeoutMs is the per-hook timeout when a hook sets none
	// (CC default: 600000). Zero applies the reference default.
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
	agents                *agent.AgentRegistry

	// subagentChildren retains each local child through its paired end so
	// stop hooks keep the session workspace after the handle unregisters
	// the agent. Every retained entry relies on that paired end; a
	// producer that can omit it must provide another release edge.
	subagentChildrenMu sync.Mutex
	subagentChildren   map[subagent.SubagentRunID]*agent.Agent
}

// runHook indirection lets tests stub execution deterministically; the
// production value is the protocol runner.
var runHook = hookprotocol.RunHook

// Apply validates the config, parses the hook config file, and registers
// the bridge's listeners. It returns the disposer that unregisters every
// listener, aborts still-running detached hooks, and drains their
// continuations. A config file that cannot be read or parsed logs a warning
// and registers nothing (no error) — the reference behavior.
//
// Subagent lifecycle edges dispatch on the same registry subject bus the
// composition hands the subagent runtime, so the bridge subscribes there.
func Apply(agents *agent.AgentRegistry, runtime *tools.ToolRuntime, config Config) (func(), error) {
	if agents == nil {
		return nil, fmt.Errorf("hooks-claude-code: agents registry is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("hooks-claude-code: tool runtime is required")
	}
	// Validate before config parsing so a bad value cannot be hidden by its
	// early return.
	stderrSummaryMaxChars := config.StderrSummaryMaxChars
	if stderrSummaryMaxChars == 0 {
		stderrSummaryMaxChars = hookprotocol.DefaultStderrSummaryMaxChars
	}
	if stderrSummaryMaxChars < 1 {
		return nil, fmt.Errorf("hooks-claude-code: stderrSummaryMaxChars must be a positive integer")
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
		now = func() int64 { return time.Now().UnixMilli() }
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
		subagentChildren:      map[subagent.SubagentRunID]*agent.Agent{},
	}
	raw, err := os.ReadFile(config.ConfigPath)
	if err == nil {
		var decoded any
		if err = json.Unmarshal(raw, &decoded); err != nil {
			err = fmt.Errorf("hooks-claude-code: %w", err)
		} else {
			var parsed ParsedClaudeConfig
			parsed, err = ParseClaudeCodeConfig(decoded, SubstitutionVars{PluginRoot: config.PluginRoot, ProjectDir: config.ProjectDir})
			if err == nil {
				b.parsed = parsed.Config
				for _, skipped := range parsed.Skipped {
					logger.Warn(fmt.Sprintf("hooks-claude-code: skipping unsupported %q hook on %s (only command hooks run)", skipped.Type, skipped.Event))
				}
			}
		}
	}
	if err != nil {
		logger.Warn(fmt.Sprintf("hooks-claude-code: could not load hook config %q: %v — no hooks registered", config.ConfigPath, err))
		b.detached.Drain()
		return func() {}, nil
	}

	disposers := []func(){}

	// Emit-shaped points run detached, so track their chains; disposal
	// aborts active hooks and drains continuations before resolving.
	disposers = append(disposers, func() { b.detached.Drain() })

	// SessionStart injects context when its detached hook resolves; a slow
	// hook may miss the first request.
	disposers = append(disposers, agents.Events().OnEmit(agent.EventAgentSessionStart, nil, func(payload any) error {
		start, ok := payload.(agent.AgentSessionStartPayload)
		if !ok || start.Agent == nil {
			return nil
		}
		agentRef := start.Agent
		source := string(start.Source)
		b.detached.Track(func(ctx context.Context) {
			merged, err := b.runPoint(hookprotocolPointSessionStart, source, b.sessionStartPayload(agentRef, source), runPointOptions{agent: agentRef, signal: ctx})
			if err != nil {
				logger.Warn(fmt.Sprintf("hooks-claude-code: SessionStart hook failed: %v", err))
				return
			}
			if context := contextFrom(b, merged); context != nil {
				// agent.inject maps to the next-step pending-input store.
				_ = agentRef.Inbox.Append(agent.InboxNextStep, *context)
			}
		})
		return nil
	}))

	// UserPromptSubmit → PreStepDecision. The prompt text is the payload;
	// no matcher subject (CC ignores matchers for this event).
	disposers = append(disposers, agents.Events().PreStep().On(nil, func(step agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		if len(step.Messages) == 0 {
			return next(step)
		}
		var blocks []llm.ContentBlock
		for _, message := range step.Messages {
			blocks = append(blocks, message.Content...)
		}
		merged, runErr := b.runPoint(pointUserPromptSubmit, "", b.promptPayload(step.Agent, blocks), runPointOptions{agent: step.Agent, turn: step.Turn, hasTurn: true, signal: step.Signal})
		if runErr != nil {
			b.logger.Warn(fmt.Sprintf("hooks-claude-code: UserPromptSubmit hook failed: %v", runErr))
			return next(step)
		}
		if merged.Decision == hookprotocol.MergedDeny {
			return agent.PreStepReject()
		}
		// Delegate so later listeners may still rewrite or reject, then
		// prepend our context only to a downstream enter decision.
		decision := next(step)
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

	// PreToolUse → PreToolDecision. Matcher subject is the tool name. The
	// Go ToolExecution routes the agent as a scope key, so the listener
	// resolves the live agent from the registry first.
	disposers = append(disposers, runtime.OnPreExecute(nil, func(exec *tools.ToolExecution, next func(*tools.ToolExecution) *tools.PreToolDecision) *tools.PreToolDecision {
		execAgent := resolveByScope(agents, exec.Agent)
		turn := lastTurn(execAgent)
		outcome, runErr := b.runPoint(pointPreToolUse, exec.Name, b.preToolPayload(exec, execAgent), runPointOptions{agent: execAgent, turn: turn, hasTurn: execAgent != nil, signal: context.Background()})
		merged := b.mergeOrWarn(pointPreToolUse, outcome, runErr)
		if merged.Decision == hookprotocol.MergedDeny {
			return &tools.PreToolDecision{Kind: tools.PreDeny, Reason: blockReason(merged, "blocked by PreToolUse hook"), HasReason: true}
		}
		if merged.Decision == hookprotocol.MergedAsk {
			decision := &tools.PreToolDecision{Kind: tools.PreAsk}
			if merged.Reason != "" {
				decision.Reason = merged.Reason
				decision.HasReason = true
			}
			return decision
		}
		return next(exec)
	}))

	// PostToolUse → PostToolDecision. Matcher subject is the tool name.
	disposers = append(disposers, runtime.OnPostExecute(nil, func(exec *tools.ToolExecution, result *tools.ToolExecutionResult, next func(*tools.ToolExecutionResult) *tools.PostToolDecision) *tools.PostToolDecision {
		execAgent := resolveByScope(agents, exec.Agent)
		turn := lastTurn(execAgent)
		outcome, runErr := b.runPoint(pointPostToolUse, exec.Name, b.postToolPayload(exec, result, execAgent), runPointOptions{agent: execAgent, turn: turn, hasTurn: execAgent != nil, signal: context.Background()})
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
		// Our hooks did not block. DELEGATE so a later listener can still
		// block/replace, then fold our context onto its decision (a
		// downstream block carries it too).
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
		merged, runErr := b.runPoint(pointStop, "", b.stopPayload(stopping.Agent), runPointOptions{agent: stopping.Agent, turn: stopping.Turn, hasTurn: true, signal: stopping.Signal})
		if runErr != nil {
			b.logger.Warn(fmt.Sprintf("hooks-claude-code: Stop hook failed: %v", runErr))
			return nil, false
		}
		if merged.Decision == hookprotocol.MergedDeny {
			// A blocking Stop hook forces continuation.
			text := blockReason(merged, "continue: blocked by Stop hook")
			// agent.steer maps to the same pending-input store; the driver
			// re-checks it after this boundary and runs another step.
			_ = stopping.Agent.Inbox.Append(agent.InboxNextStep, userMessage(b, text))
		}
		return nil, false
	}))

	// SubagentStart may inject child context; SubagentStop only observes.
	// Both use the live child's workspace and the generic agent-type
	// matcher subject.
	{
		disposers = append(disposers, agents.Events().OnEmit(subagent.EventSubagentStart, nil, func(payload any) error {
			info, ok := payload.(subagent.SubagentRunInfo)
			if !ok {
				return nil
			}
			child := resolveChild(agents, info.ID)
			if child != nil {
				b.subagentChildrenMu.Lock()
				b.subagentChildren[info.RunID] = child
				b.subagentChildrenMu.Unlock()
			}
			b.detached.Track(func(ctx context.Context) {
				merged, err := b.runPoint(pointSubagentStart, subagentType, b.subagentPayload(pointSubagentStart, info, child), runPointOptions{agent: child, signal: ctx})
				if err != nil {
					logger.Warn(fmt.Sprintf("hooks-claude-code: SubagentStart hook failed: %v", err))
					return
				}
				if context := contextFrom(b, merged); context != nil && child != nil {
					_ = child.Inbox.Append(agent.InboxNextStep, *context)
				}
			})
			return nil
		}))
		disposers = append(disposers, agents.Events().OnEmit(subagent.EventSubagentEnd, nil, func(payload any) error {
			info, ok := payload.(subagent.SubagentRunEndInfo)
			if !ok {
				return nil
			}
			b.subagentChildrenMu.Lock()
			child := b.subagentChildren[info.RunID]
			delete(b.subagentChildren, info.RunID)
			b.subagentChildrenMu.Unlock()
			if child == nil {
				child = resolveChild(agents, info.ID)
			}
			b.detached.Track(func(ctx context.Context) {
				_, _ = b.runPoint(pointSubagentStop, subagentType, b.subagentPayload(pointSubagentStop, info.SubagentRunInfo, child), runPointOptions{agent: child, signal: ctx})
			})
			return nil
		}))
	}

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

// resolveChild looks a child agent up by its session id.
func resolveChild(agents *agent.AgentRegistry, id session.SessionID) *agent.Agent {
	return agents.Get(id)
}

// runPointOptions carry the per-invocation agent/turn/signal context.
type runPointOptions struct {
	agent   *agent.Agent
	turn    int64
	hasTurn bool
	signal  context.Context
}

// The hook points this bridge fires (the CC event names).
const (
	hookprotocolPointSessionStart = "SessionStart"
	pointUserPromptSubmit         = "UserPromptSubmit"
	pointPreToolUse               = "PreToolUse"
	pointPostToolUse              = "PostToolUse"
	pointStop                     = "Stop"
	pointSubagentStart            = "SubagentStart"
	pointSubagentStop             = "SubagentStop"
)

// runPoint runs every command hook configured for point whose matcher
// selects matchQuery, with the per-event payload on stdin, and folds the
// results. Writes a hook/invoked + hook/result pair per hook when opts
// names an open turn. Detached lifecycle points omit the pair. Returns the
// merged outcome (a neutral, already-most-restrictive view) for the caller
// to map onto its extension point decision. matchQuery is the event's
// matcher subject (tool name, session source, …); "" for events that
// ignore matchers.
func (b *bridge) runPoint(point string, matchQuery string, payload map[string]any, opts runPointOptions) (hookprotocol.MergedHookOutcome, error) {
	groups := b.parsed[point]
	outputs := []hookprotocol.HookOutput{}
	// Run hooks in the agent's session workspace (the session/new cwd on
	// the session header), not the entry-point process's launch dir.
	workdir := ""
	if opts.agent != nil {
		workdir = opts.agent.Session.Header().CWD
	}
	// CLAUDE_PROJECT_DIR: an explicit config value wins; otherwise default
	// it to the session workspace (the same dir the hook runs in).
	projectDir := b.config.ProjectDir
	if projectDir == nil && workdir != "" {
		dir := workdir
		projectDir = &dir
	}
	for _, group := range groups {
		if !hookprotocol.MatchesMatcher(group.Matcher, matchQuery, hookprotocol.MatcherModeClaudeCode) {
			continue
		}
		for _, hook := range group.Hooks {
			handlerID := fmt.Sprintf("claude-code:%s:%d", point, nextHandlerID())
			sess := (*session.Session)(nil)
			if opts.agent != nil {
				sess = opts.agent.Session
			}
			if sess != nil && opts.hasTurn {
				if err := hookprotocol.AppendHookInvoked(sess, hookprotocol.HookInvocation{
					Turn:      opts.turn,
					Point:     point,
					Dialect:   hookprotocol.DialectClaudeCode,
					HandlerID: handlerID,
					Matcher:   group.Matcher,
				}); err != nil {
					return hookprotocol.MergedHookOutcome{}, err
				}
			}
			env := map[string]string{}
			if projectDir != nil {
				env["CLAUDE_PROJECT_DIR"] = *projectDir
			}
			run := runHook(hook, hookprotocol.RunHookOptions{
				Payload:           payload,
				Env:               env,
				CWD:               workdir,
				Signal:            opts.signal,
				TrailingNewline:   true,
				DefaultTimeoutMs:  b.defaultTimeoutMs,
				ExpectedEventName: point,
			}, b.now)
			output := run.Output
			outputs = append(outputs, output)
			if output.UpdatedInput != nil {
				b.logger.Warn(fmt.Sprintf("hooks-claude-code: %s hook requested updatedInput, which is not yet honored (ignored)", point))
			}
			if output.SystemMessage != "" {
				b.logger.Warn(fmt.Sprintf("hooks-claude-code: %s hook emitted a systemMessage, which is not yet surfaced (ignored)", point))
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

// blockReason prefers the merged reason, falling back to the caller's
// generic line (a block with no reason still blocks).
func blockReason(merged hookprotocol.MergedHookOutcome, fallback string) string {
	if merged.Reason != "" {
		return merged.Reason
	}
	return fallback
}

// mergeOrWarn contains a runPoint failure at the synchronous tool
// extension points: the run is logged and treated as neutral so a durable
// log write failure degrades one call instead of failing the pipeline.
func (b *bridge) mergeOrWarn(point string, outcome hookprotocol.MergedHookOutcome, err error) hookprotocol.MergedHookOutcome {
	if err == nil {
		return outcome
	}
	b.logger.Warn(fmt.Sprintf("hooks-claude-code: %s hook failed: %v", point, err))
	return hookprotocol.MergedHookOutcome{Decision: hookprotocol.MergedNone}
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

// base holds the fields every CC payload carries.
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
	}
}

func (b *bridge) sessionStartPayload(a *agent.Agent, source string) map[string]any {
	payload := b.base("SessionStart", a)
	payload["source"] = source
	return payload
}

func (b *bridge) promptPayload(a *agent.Agent, blocks []llm.ContentBlock) map[string]any {
	payload := b.base("UserPromptSubmit", a)
	payload["prompt"] = blocksToText(blocks)
	return payload
}

func (b *bridge) preToolPayload(exec *tools.ToolExecution, execAgent *agent.Agent) map[string]any {
	payload := b.base("PreToolUse", execAgent)
	payload["tool_name"] = exec.Name
	payload["tool_input"] = exec.Arguments
	payload["tool_use_id"] = exec.CallID
	return payload
}

func (b *bridge) postToolPayload(exec *tools.ToolExecution, result *tools.ToolExecutionResult, execAgent *agent.Agent) map[string]any {
	payload := b.base("PostToolUse", execAgent)
	payload["tool_name"] = exec.Name
	payload["tool_input"] = exec.Arguments
	payload["tool_use_id"] = exec.CallID
	payload["tool_response"] = blocksToText(result.Content)
	return payload
}

func (b *bridge) stopPayload(a *agent.Agent) map[string]any {
	payload := b.base("Stop", a)
	payload["stop_hook_active"] = false
	return payload
}

// subagentPayload builds a SubagentStart/SubagentStop payload from the CC
// base (the child's session_id/cwd when the child agent is available) plus
// the subagent-hook fields. stop_hook_active is present on SubagentStop
// only (the loop-guard flag, always false).
func (b *bridge) subagentPayload(event string, info subagent.SubagentRunInfo, child *agent.Agent) map[string]any {
	payload := b.base(event, child)
	payload["agent_id"] = string(info.ID)
	payload["agent_type"] = subagentType
	if event == pointSubagentStop {
		payload["stop_hook_active"] = false
	}
	return payload
}
