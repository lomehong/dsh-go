// Package hookprotocol ports @deepseek-ai/dsh-hook-protocol: the
// dialect-neutral vocabulary, output codec, matcher, outcome merge, durable
// log-only events, command execution, and detached-run quiescence shared by
// the Claude Code and Codex hook bridges. Payload construction, matching
// differences, environment, and extension-point-specific decision mapping
// remain owned by each bridge.
package hookprotocol

// HookDialect is the bridge that ran a hook — the CC bridge stamps
// "claude-code", the Codex bridge "codex". A native plugin at the
// interception points is not a bridge and writes no hook/* records.
type HookDialect string

// The two bridge dialects.
const (
	DialectClaudeCode HookDialect = "claude-code"
	DialectCodex      HookDialect = "codex"
)

// CommandHook is one configured command hook (the `{ type: 'command',
// command, timeout? }` shape shared by both dialects). Non-command hook
// types (CC's prompt/agent/http) are parsed-and-skipped by a bridge, so only
// this shape reaches the runner.
type CommandHook struct {
	// Command is the shell command line to run.
	Command string `json:"command"`
	// TimeoutSec is the per-hook timeout in SECONDS (the wire unit); the
	// runner converts to milliseconds. nil means "use the bridge default".
	TimeoutSec *float64 `json:"timeoutSec,omitempty"`
}

// MatcherGroup is one matcher group: a matcher pattern (absent / ” / '*' =
// match-all) plus the command hooks that run when it matches. Both dialects
// share this shape (CC's hooks.json and Codex's hooks.json).
type MatcherGroup struct {
	// Matcher is the pattern; nil for match-all.
	Matcher *string `json:"matcher,omitempty"`
	// Hooks are the commands that run when the matcher selects the query.
	Hooks []CommandHook `json:"hooks"`
}

// MatcherMode is how a matcher pattern is interpreted. Claude Code uses
// literal mode when the pattern is purely [A-Za-z0-9_|]+ (pipe = exact-match
// alternation) and regex mode otherwise; Codex is always regex.
type MatcherMode string

// The two matcher interpretation modes (the values name the dialect that
// owns the interpretation).
const (
	MatcherModeClaudeCode MatcherMode = "claude-code"
	MatcherModeCodex      MatcherMode = "codex"
)

// HookDecision is the neutral blocking decision a hook expressed, folded
// from the two channels the reference protocols keep DISTINCT: the legacy
// top-level decision (approve/block only) and
// hookSpecificOutput.permissionDecision (allow/deny/ask). "allow"/"deny"/
// "ask" arise ONLY from a permissionDecision, never from a top-level
// decision.
type HookDecision string

// The recognized decision values.
const (
	DecisionApprove HookDecision = "approve"
	DecisionAllow   HookDecision = "allow"
	DecisionBlock   HookDecision = "block"
	DecisionDeny    HookDecision = "deny"
	DecisionAsk     HookDecision = "ask"
)

// HookOutput is the dialect-neutral OUTCOME a hook produced, parsed from its
// exit code + stdout JSON + stderr by ParseHookOutput. A bridge maps this
// onto an extension-point-specific typed Decision. Every optional field may
// be exercised in any subset; the bridge decides which fields are meaningful
// for its hook point and which it ignores (faithful-but-degraded — e.g.
// Codex ignores allow/ask).
type HookOutput struct {
	// ExitCode is the raw process exit code; nil when the hook could not be
	// run (spawn failure, signal/timeout death).
	ExitCode *int `json:"exitCode,omitempty"`
	// Stderr is the trimmed stderr — the block-reason source on a blocking
	// (exit 2) hook.
	Stderr string `json:"stderr"`
	// Stdout is the trimmed stdout, verbatim. On a clean exit a hook may
	// emit PLAIN (non-JSON) stdout that the protocol renders as output (CC)
	// or treats as additionalContext (Codex SessionStart/UserPromptSubmit)
	// — so the bridge needs the raw text, not just the parsed structured
	// fields. Empty when the hook produced no stdout.
	Stdout string `json:"stdout"`
	// Continue is false when the hook asked to halt (CC/Codex
	// `continue:false`); true/nil proceeds.
	Continue *bool `json:"continue,omitempty"`
	// StopReason is the human-readable reason shown when Continue is false.
	StopReason *string `json:"stopReason,omitempty"`
	// Decision is the normalized blocking decision (see HookDecision).
	// Absent ⇒ no explicit decision (the exit code governs).
	Decision HookDecision `json:"decision,omitempty"`
	// Reason is the reason/explanation accompanying Decision.
	Reason *string `json:"reason,omitempty"`
	// HookEventName is the event discriminator claimed by
	// hookSpecificOutput. On a mismatch, ParseHookOutput preserves this
	// value but discards event-scoped fields.
	HookEventName string `json:"hookEventName,omitempty"`
	// AdditionalContext is extra context to inject for the next model
	// request (CC additionalContext).
	AdditionalContext string `json:"additionalContext,omitempty"`
	// SystemMessage is a warning surfaced to the user (CC systemMessage).
	SystemMessage string `json:"systemMessage,omitempty"`
	// UpdatedInput is a tool-input rewrite a hook requested (CC
	// updatedInput). PARSED but NOT honored — input rewrite is deferred; a
	// bridge logs + warns when this is present.
	UpdatedInput map[string]any `json:"updatedInput,omitempty"`
}
