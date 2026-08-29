// Events ports hook-protocol/src/events.ts: append helpers for durable,
// log-only hook events. They carry no surface intent and must remain
// turn-enclosed and invoked/result paired. Mid-turn hook points satisfy
// that boundary; SessionStart records injected context instead and does not
// append hook/* outside a turn.
package hookprotocol

import (
	"encoding/json"
	"strings"

	"dshgo/session"
)

// The shared log-only event types (not SurfaceEventTypes; no surfaceOp).
const (
	// EventHookInvoked records a hook command invoked at a hook point.
	EventHookInvoked = "hook/invoked"
	// EventHookResult is the outcome paired to hook/invoked by HandlerID.
	EventHookResult = "hook/result"
)

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventHookInvoked, EventHookResult)
}

// HookInvocation is what identifies a hook invocation across its
// invoked/result pair.
type HookInvocation struct {
	// Turn is the open turn the invocation lives inside.
	Turn int64
	// Point is the hook point (PreToolUse, Stop, …).
	Point string
	// Dialect is the bridge dialect that ran it.
	Dialect HookDialect
	// HandlerID is a stable id correlating the invoked event with its
	// result.
	HandlerID string
	// Matcher is the matcher-group pattern that selected it; nil for
	// match-all.
	Matcher *string
}

// hookInvokedData is the durable payload of hook/invoked.
type hookInvokedData struct {
	Turn      int64       `json:"turn"`
	Point     string      `json:"point"`
	Dialect   HookDialect `json:"dialect"`
	HandlerID string      `json:"handlerId"`
	Matcher   *string     `json:"matcher,omitempty"`
}

// HookResultRecord is the decided outcome half of the pair.
type HookResultRecord struct {
	Turn int64
	// Point is the hook point.
	Point string
	// HandlerID pairs with the invoked event.
	HandlerID string
	// Output is the decoded outcome the run produced; AppendHookResult
	// derives the durable decision/exitCode/stderrSummary fields from it,
	// so the shared event's semantics live here, in the lib that declares
	// it, not per-bridge.
	Output HookOutput
	// StderrSummaryMaxChars bounds the derived stderrSummary. The bound is
	// the bridge's to own (its StderrSummaryMaxChars config) and is passed
	// in explicitly — DefaultStderrSummaryMaxChars is the reference
	// default.
	StderrSummaryMaxChars int
	// DurationMs is the wall-clock duration of the run (from RunHook) —
	// durable audit timing.
	DurationMs int64
}

// hookResultData is the durable payload of hook/result.
type hookResultData struct {
	Turn          int64   `json:"turn"`
	Point         string  `json:"point"`
	HandlerID     string  `json:"handlerId"`
	Decision      string  `json:"decision"`
	ExitCode      *int    `json:"exitCode,omitempty"`
	StderrSummary *string `json:"stderrSummary,omitempty"`
	DurationMs    int64   `json:"durationMs"`
}

// DefaultStderrSummaryMaxChars is the reference default for
// HookResultRecord.StderrSummaryMaxChars (both bridges' config default). It
// lives here, next to the truncation rule it bounds, so the bridges cannot
// drift apart on the shared event's default cap.
const DefaultStderrSummaryMaxChars = 500

// SummarizeStderr truncates a hook's stderr for
// HookResultRecord: trimmed, empty when blank, cut at maxChars with an
// ellipsis when over. The bound is a parameter — like RunHook's
// defaultTimeoutMs, each bridge owns the config default and passes it in.
func SummarizeStderr(stderr string, maxChars int) string {
	t := strings.TrimSpace(stderr)
	if t == "" {
		return ""
	}
	if len([]rune(t)) > maxChars {
		runes := []rune(t)
		return string(runes[:maxChars]) + "…"
	}
	return t
}

// AppendHookInvoked appends a hook/invoked event naming the handler and
// hook point to sess. A nil Matcher is omitted from the payload.
func AppendHookInvoked(sess *session.Session, invocation HookInvocation) error {
	_, err := sess.Append(EventHookInvoked, hookInvokedData{
		Turn:      invocation.Turn,
		Point:     invocation.Point,
		Dialect:   invocation.Dialect,
		HandlerID: invocation.HandlerID,
		Matcher:   invocation.Matcher,
	}, nil)
	return err
}

// AppendHookResult appends the durable result paired with hook/invoked. The
// recorded decision is the parsed decision, then "stop" for
// continue:false, else "pass"; stderr is trimmed and capped, and an absent
// process exit stays omitted.
func AppendHookResult(sess *session.Session, record HookResultRecord) error {
	decision := string(record.Output.Decision)
	if decision == "" {
		if record.Output.Continue != nil && !*record.Output.Continue {
			decision = "stop"
		} else {
			decision = "pass"
		}
	}
	data := hookResultData{
		Turn:       record.Turn,
		Point:      record.Point,
		HandlerID:  record.HandlerID,
		Decision:   decision,
		ExitCode:   record.Output.ExitCode,
		DurationMs: record.DurationMs,
	}
	if summary := SummarizeStderr(record.Output.Stderr, record.StderrSummaryMaxChars); summary != "" {
		data.StderrSummary = &summary
	}
	_, err := sess.Append(EventHookResult, data, nil)
	return err
}

// decodeTurnStart reads the open turn number from a turn/start event.
func decodeTurnStart(data json.RawMessage) (int64, bool) {
	var decoded struct {
		Turn int64 `json:"turn"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return 0, false
	}
	return decoded.Turn, true
}
