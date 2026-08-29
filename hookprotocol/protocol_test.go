package hookprotocol

import (
	"encoding/json"
	"reflect"
	"testing"

	"dshgo/session"
)

// newEventSession builds a detached session for durable-event assertions.
func newEventSession(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewDetached("hook-events", nil, &session.SessionHeader{ID: "hook-events", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	return sess
}

func exitCode(code int) *int { return &code }

func boolPtr(value bool) *bool { return &value }

func stringPtr(value string) *string { return &value }

func TestParseHookOutputBlockingExit(t *testing.T) {
	output := ParseHookOutput(exitCode(2), "", "denied: secrets", "")
	if output.Decision != DecisionBlock {
		t.Fatalf("decision = %q, want block", output.Decision)
	}
	if output.Reason == nil || *output.Reason != "denied: secrets" {
		t.Fatalf("reason = %+v, want stderr", output.Reason)
	}
	if output.ExitCode == nil || *output.ExitCode != 2 {
		t.Fatalf("exit code = %+v", output.ExitCode)
	}
	// An empty stderr still blocks; it just carries no reason.
	quiet := ParseHookOutput(exitCode(2), "  \n", "   ", "")
	if quiet.Decision != DecisionBlock || quiet.Reason != nil {
		t.Fatalf("quiet block = %+v", quiet)
	}
}

func TestParseHookOutputStructuredTopLevel(t *testing.T) {
	output := ParseHookOutput(exitCode(0), `{"continue":false,"stopReason":"halt now","decision":"approve","reason":"fine","systemMessage":"beware"}`, "", "")
	if output.Continue == nil || *output.Continue {
		t.Fatalf("continue = %+v", output.Continue)
	}
	if output.StopReason == nil || *output.StopReason != "halt now" {
		t.Fatalf("stopReason = %+v", output.StopReason)
	}
	if output.Decision != DecisionApprove {
		t.Fatalf("decision = %q, want approve", output.Decision)
	}
	if output.Reason == nil || *output.Reason != "fine" {
		t.Fatalf("reason = %+v", output.Reason)
	}
	if output.SystemMessage != "beware" {
		t.Fatalf("systemMessage = %q", output.SystemMessage)
	}
}

func TestParseHookOutputPermissionDecisionOverrides(t *testing.T) {
	stdin := `{"decision":"approve","reason":"legacy","hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no writes","additionalContext":"ctx","updatedInput":{"path":"x"}}}`
	output := ParseHookOutput(exitCode(0), stdin, "", "PreToolUse")
	if output.Decision != DecisionDeny {
		t.Fatalf("decision = %q, want deny (permissionDecision overrides)", output.Decision)
	}
	if output.Reason == nil || *output.Reason != "no writes" {
		t.Fatalf("reason = %+v", output.Reason)
	}
	if output.HookEventName != "PreToolUse" {
		t.Fatalf("hookEventName = %q", output.HookEventName)
	}
	if output.AdditionalContext != "ctx" {
		t.Fatalf("additionalContext = %q", output.AdditionalContext)
	}
	if !reflect.DeepEqual(output.UpdatedInput, map[string]any{"path": "x"}) {
		t.Fatalf("updatedInput = %+v", output.UpdatedInput)
	}
}

func TestParseHookOutputTopLevelDenyIsInvalid(t *testing.T) {
	// allow/deny/ask are reserved for permissionDecision; an out-of-band
	// top-level deny is ignored per both schemas.
	output := ParseHookOutput(exitCode(0), `{"decision":"deny"}`, "", "")
	if output.Decision != "" {
		t.Fatalf("decision = %q, want none", output.Decision)
	}
}

func TestParseHookOutputEventMismatchDiscardsEventScopedFields(t *testing.T) {
	stdin := `{"decision":"approve","hookSpecificOutput":{"hookEventName":"PostToolUse","permissionDecision":"deny","additionalContext":"ctx"}}`
	output := ParseHookOutput(exitCode(0), stdin, "", "PreToolUse")
	// The claimed discriminator is still recorded for the record.
	if output.HookEventName != "PostToolUse" {
		t.Fatalf("hookEventName = %q", output.HookEventName)
	}
	// Event-scoped fields are discarded; top-level fields remain.
	if output.Decision != DecisionApprove {
		t.Fatalf("decision = %q, want approve", output.Decision)
	}
	if output.AdditionalContext != "" {
		t.Fatalf("additionalContext = %q, want discarded", output.AdditionalContext)
	}
}

func TestParseHookOutputMissingDiscriminatorDiscardsEventScopedFields(t *testing.T) {
	stdin := `{"hookSpecificOutput":{"permissionDecision":"allow"}}`
	output := ParseHookOutput(exitCode(0), stdin, "", "PreToolUse")
	if output.Decision != "" {
		t.Fatalf("decision = %q, want none", output.Decision)
	}
}

func TestParseHookOutputMalformedJSONStaysPlain(t *testing.T) {
	output := ParseHookOutput(exitCode(0), "{nope", "", "")
	if output.Stdout != "{nope" || output.Decision != "" {
		t.Fatalf("output = %+v", output)
	}
	if output.ExitCode == nil || *output.ExitCode != 0 {
		t.Fatalf("exit code = %+v", output.ExitCode)
	}
}

func TestParseHookOutputNonObjectStdoutStaysPlain(t *testing.T) {
	output := ParseHookOutput(exitCode(0), "[1,2]", "", "")
	if output.Stdout != "[1,2]" || output.Decision != "" {
		t.Fatalf("output = %+v", output)
	}
}

func TestParseHookOutputSpawnFailureIsNonBlocking(t *testing.T) {
	output := ParseHookOutput(nil, "", "spawn failure", "")
	if output.ExitCode != nil {
		t.Fatalf("exit code = %+v, want nil", output.ExitCode)
	}
	if output.Decision != "" {
		t.Fatalf("decision = %q, want none", output.Decision)
	}
}

func TestMatchesMatcherClaudeLiteralAlternatives(t *testing.T) {
	literal := stringPtr("Bash|Read")
	if !MatchesMatcher(literal, "Read", MatcherModeClaudeCode) {
		t.Fatal("literal alternative should match exactly")
	}
	if MatchesMatcher(literal, "Reading", MatcherModeClaudeCode) {
		t.Fatal("literal pattern must not match as a substring")
	}
	if MatchesMatcher(literal, "Grep", MatcherModeClaudeCode) {
		t.Fatal("non-listed tool must not match")
	}
}

func TestMatchesMatcherClaudeRegexFallback(t *testing.T) {
	// A pattern with a non-word char is an unanchored regex in CC mode.
	regex := stringPtr("^(web|shell)_")
	if !MatchesMatcher(regex, "shell_fetch", MatcherModeClaudeCode) {
		t.Fatal("regex should match")
	}
	if MatchesMatcher(regex, "file_fetch", MatcherModeClaudeCode) {
		t.Fatal("regex should not match")
	}
}

func TestMatchesMatcherCodexAlwaysRegex(t *testing.T) {
	pattern := stringPtr("Bash")
	if !MatchesMatcher(pattern, "xBashy", MatcherModeCodex) {
		t.Fatal("codex matchers are unanchored regexes")
	}
	if MatchesMatcher(pattern, "Reading", MatcherModeClaudeCode) {
		// In CC mode "Bash" is a literal alternative list, exact-match only.
		t.Fatal("claude literal must exact-match")
	}
}

func TestMatchesMatcherMatchAllSentinels(t *testing.T) {
	for _, matcher := range []*string{nil, stringPtr(""), stringPtr("*")} {
		if !MatchesMatcher(matcher, "anything", MatcherModeCodex) {
			t.Fatalf("matcher %v should match all", matcher)
		}
	}
}

func TestMatchesMatcherInvalidRegexIsNonMatch(t *testing.T) {
	invalid := stringPtr("([unclosed")
	if MatchesMatcher(invalid, "x", MatcherModeCodex) {
		t.Fatal("invalid regex must contain as a non-match")
	}
	if diagnostic := MatcherDiagnostic(invalid, MatcherModeCodex); diagnostic == "" {
		t.Fatal("config parser must surface the invalid regex")
	}
}

func TestMatcherDiagnosticValidPatterns(t *testing.T) {
	if MatcherDiagnostic(nil, MatcherModeClaudeCode) != "" {
		t.Fatal("match-all is valid")
	}
	if MatcherDiagnostic(stringPtr("Bash|Read"), MatcherModeClaudeCode) != "" {
		t.Fatal("claude literal is valid")
	}
	if got := MatcherDiagnostic(stringPtr("a[b"), MatcherModeCodex); got == "" {
		t.Fatal("invalid codex regex must diagnose")
	}
}

func TestMergeHookOutputsPrecedence(t *testing.T) {
	merged := MergeHookOutputs([]HookOutput{
		{Decision: DecisionAllow},
		{Decision: DecisionDeny, Reason: stringPtr("no")},
		{Decision: DecisionAsk, Reason: stringPtr("hmm")},
	})
	if merged.Decision != MergedDeny {
		t.Fatalf("decision = %q, want deny", merged.Decision)
	}
	// Only reasons explaining the winning rank surface.
	if merged.Reason != "no" {
		t.Fatalf("reason = %q, want the deny reason", merged.Reason)
	}

	asked := MergeHookOutputs([]HookOutput{{Decision: DecisionAllow}, {Decision: DecisionAsk, Reason: stringPtr("hmm")}})
	if asked.Decision != MergedAsk || asked.Reason != "hmm" {
		t.Fatalf("ask merge = %+v", asked)
	}
}

func TestMergeHookOutputsBlockFoldsToDenyAndApproveFoldsToAllow(t *testing.T) {
	if merged := MergeHookOutputs([]HookOutput{{Decision: DecisionBlock}}); merged.Decision != MergedDeny {
		t.Fatalf("block folded to %q", merged.Decision)
	}
	if merged := MergeHookOutputs([]HookOutput{{Decision: DecisionApprove}}); merged.Decision != MergedAllow {
		t.Fatalf("approve folded to %q", merged.Decision)
	}
}

func TestMergeHookOutputsStickyStopAndAccumulation(t *testing.T) {
	merged := MergeHookOutputs([]HookOutput{
		{Continue: boolPtr(false), StopReason: stringPtr("first halt")},
		{Continue: boolPtr(false), StopReason: stringPtr("second")},
		{AdditionalContext: "one", SystemMessage: "sys1"},
		{AdditionalContext: "two"},
	})
	if !merged.Stop {
		t.Fatal("stop should be sticky")
	}
	if merged.StopReason != "first halt" {
		t.Fatalf("stopReason = %q, want the FIRST halting reason", merged.StopReason)
	}
	if !reflect.DeepEqual(merged.AdditionalContext, []string{"one", "two"}) {
		t.Fatalf("additionalContext = %+v", merged.AdditionalContext)
	}
	if !reflect.DeepEqual(merged.SystemMessages, []string{"sys1"}) {
		t.Fatalf("systemMessages = %+v", merged.SystemMessages)
	}
}

func TestMergeHookOutputsEmptyIsNeutral(t *testing.T) {
	merged := MergeHookOutputs(nil)
	if merged.Decision != MergedNone || merged.Stop || len(merged.AdditionalContext) != 0 {
		t.Fatalf("empty merge = %+v", merged)
	}
}

func TestSummarizeStderr(t *testing.T) {
	if SummarizeStderr("   \n ", 10) != "" {
		t.Fatal("blank stderr summarizes to empty")
	}
	if got := SummarizeStderr("  short  ", 10); got != "short" {
		t.Fatalf("summary = %q", got)
	}
	multibyte := "ab中文字符"
	got := SummarizeStderr(multibyte, 3)
	if got != "ab中…" {
		t.Fatalf("rune-safe summary = %q", got)
	}
	if DefaultStderrSummaryMaxChars != 500 {
		t.Fatalf("default summary cap = %d", DefaultStderrSummaryMaxChars)
	}
}

type loggedHookEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func TestAppendHookInvokedAndResultPair(t *testing.T) {
	sess := newEventSession(t)
	matcher := "*"
	if err := AppendHookInvoked(sess, HookInvocation{Turn: 3, Point: "PreToolUse", Dialect: DialectClaudeCode, HandlerID: "claude-code:PreToolUse:1", Matcher: &matcher}); err != nil {
		t.Fatalf("append invoked: %v", err)
	}
	if err := AppendHookResult(sess, HookResultRecord{
		Turn:                  3,
		Point:                 "PreToolUse",
		HandlerID:             "claude-code:PreToolUse:1",
		Output:                HookOutput{ExitCode: exitCode(2), Decision: DecisionBlock, Stderr: "denied"},
		StderrSummaryMaxChars: 500,
		DurationMs:            42,
	}); err != nil {
		t.Fatalf("append result: %v", err)
	}

	events := sess.Events()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if events[0].Type != EventHookInvoked || events[1].Type != EventHookResult {
		t.Fatalf("event types = %q, %q", events[0].Type, events[1].Type)
	}
	var invoked struct {
		Turn      int64  `json:"turn"`
		Point     string `json:"point"`
		Dialect   string `json:"dialect"`
		HandlerID string `json:"handlerId"`
		Matcher   string `json:"matcher"`
	}
	if err := json.Unmarshal(events[0].Data, &invoked); err != nil {
		t.Fatalf("decode invoked: %v", err)
	}
	if invoked.Turn != 3 || invoked.Point != "PreToolUse" || invoked.Dialect != "claude-code" || invoked.Matcher != "*" {
		t.Fatalf("invoked = %+v", invoked)
	}
	var result struct {
		Turn          int64  `json:"turn"`
		HandlerID     string `json:"handlerId"`
		Decision      string `json:"decision"`
		ExitCode      *int   `json:"exitCode"`
		StderrSummary string `json:"stderrSummary"`
		DurationMs    int64  `json:"durationMs"`
	}
	if err := json.Unmarshal(events[1].Data, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Decision != "block" || result.ExitCode == nil || *result.ExitCode != 2 || result.StderrSummary != "denied" || result.DurationMs != 42 {
		t.Fatalf("result = %+v", result)
	}
	if result.HandlerID != invoked.HandlerID {
		t.Fatalf("handler ids must pair: %q vs %q", result.HandlerID, invoked.HandlerID)
	}
}

func TestAppendHookResultDecisionFallbacks(t *testing.T) {
	sess := newEventSession(t)
	record := func(output HookOutput) loggedHookEvent {
		before := len(sess.Events())
		if err := AppendHookResult(sess, HookResultRecord{Turn: 1, Point: "Stop", HandlerID: "h", Output: output, StderrSummaryMaxChars: 500, DurationMs: 1}); err != nil {
			t.Fatalf("append: %v", err)
		}
		last := sess.Events()[before]
		var decoded struct {
			Decision      string `json:"decision"`
			ExitCode      *int   `json:"exitCode"`
			StderrSummary string `json:"stderrSummary"`
		}
		if err := json.Unmarshal(last.Data, &decoded); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return loggedHookEvent{Type: last.Type, Data: last.Data}
	}

	// No explicit decision + continue:false → stop.
	stop := record(HookOutput{Continue: boolPtr(false)})
	var decoded struct {
		Decision string `json:"decision"`
	}
	if err := json.Unmarshal(stop.Data, &decoded); err != nil || decoded.Decision != "stop" {
		t.Fatalf("stop fallback = %+v (%v)", decoded, err)
	}
	// No explicit decision, clean exit → pass.
	pass := record(HookOutput{ExitCode: exitCode(0)})
	if err := json.Unmarshal(pass.Data, &decoded); err != nil || decoded.Decision != "pass" {
		t.Fatalf("pass fallback = %+v (%v)", decoded, err)
	}
	// Absent process exit stays omitted.
	quiet := record(HookOutput{})
	var exitless struct {
		Decision      string `json:"decision"`
		ExitCode      *int   `json:"exitCode"`
		StderrSummary string `json:"stderrSummary"`
	}
	if err := json.Unmarshal(quiet.Data, &exitless); err != nil || exitless.ExitCode != nil || exitless.StderrSummary != "" {
		t.Fatalf("quiet record = %+v (%v)", exitless, err)
	}
}
