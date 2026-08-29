package hooksclaudecode

import (
	"strings"
	"testing"

	"dshgo/hookprotocol"
)

func mustParse(t *testing.T, raw any, vars SubstitutionVars) ParsedClaudeConfig {
	t.Helper()
	parsed, err := ParseClaudeCodeConfig(raw, vars)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

func TestParseAcceptsSettingsWrapperAndBareEventMap(t *testing.T) {
	pluginRoot := "/plugins/demo"
	wrapper := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks":   []any{map[string]any{"command": "${CLAUDE_PLUGIN_ROOT}/guard.sh"}},
				},
			},
		},
	}
	parsed := mustParse(t, wrapper, SubstitutionVars{PluginRoot: &pluginRoot})
	groups := parsed.Config["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if got := groups[0].Hooks[0].Command; got != pluginRoot+"/guard.sh" {
		t.Fatalf("command = %q, want the substituted path", got)
	}

	bare := map[string]any{
		"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "stop.sh"}}}},
	}
	parsed = mustParse(t, bare, SubstitutionVars{})
	if len(parsed.Config["Stop"]) != 1 {
		t.Fatalf("bare map groups = %+v", parsed.Config)
	}
}

func TestParseUnsetTokenStaysVerbatim(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "${CLAUDE_PROJECT_DIR}/x.sh"}}}},
	}, SubstitutionVars{})
	if got := parsed.Config["Stop"][0].Hooks[0].Command; got != "${CLAUDE_PROJECT_DIR}/x.sh" {
		t.Fatalf("command = %q, want the verbatim token", got)
	}
}

func TestParseSkipsNonCommandHooksAndUnknownEvents(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"PreToolUse": []any{
			map[string]any{"hooks": []any{
				map[string]any{"type": "prompt", "prompt": "review it"},
				map[string]any{"command": "ok.sh"},
			}},
		},
		"Notification": []any{map[string]any{"hooks": []any{map[string]any{"command": "notify.sh"}}}},
	}, SubstitutionVars{})
	if len(parsed.Skipped) != 1 || parsed.Skipped[0] != (SkippedHook{Event: "PreToolUse", Type: "prompt"}) {
		t.Fatalf("skipped = %+v", parsed.Skipped)
	}
	groups := parsed.Config["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != "ok.sh" {
		t.Fatalf("groups = %+v", groups)
	}
	if _, ok := parsed.Config["Notification"]; ok {
		t.Fatal("an unsupported event must be ignored before its groups parse")
	}
}

func TestParseReadsTimeoutAndDropsPromptStopMatchers(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "Bash",
			"hooks":   []any{map[string]any{"command": "a.sh", "timeout": 2.5}},
		}},
		"UserPromptSubmit": []any{map[string]any{
			"matcher": "should-be-dropped",
			"hooks":   []any{map[string]any{"command": "b.sh"}},
		}},
		"Stop": []any{map[string]any{
			"matcher": "also-dropped",
			"hooks":   []any{map[string]any{"command": "c.sh"}},
		}},
	}, SubstitutionVars{})
	hook := parsed.Config["PreToolUse"][0].Hooks[0]
	if hook.TimeoutSec == nil || *hook.TimeoutSec != 2.5 {
		t.Fatalf("timeout = %+v", hook.TimeoutSec)
	}
	if parsed.Config["UserPromptSubmit"][0].Matcher != nil {
		t.Fatal("UserPromptSubmit has no matcher subject")
	}
	if parsed.Config["Stop"][0].Matcher != nil {
		t.Fatal("Stop has no matcher subject")
	}
}

func TestParseInvalidMatcherFailsWithEventQualifiedError(t *testing.T) {
	_, err := ParseClaudeCodeConfig(map[string]any{
		"PreToolUse": []any{map[string]any{
			"matcher": "([bad",
			"hooks":   []any{map[string]any{"command": "a.sh"}},
		}},
	}, SubstitutionVars{})
	if err == nil {
		t.Fatal("an invalid regex matcher must fail the parse")
	}
	if !strings.Contains(err.Error(), `on event "PreToolUse"`) {
		t.Fatalf("error = %v, want the event-qualified diagnostic", err)
	}
}

func TestParseEmptyAndMalformedEntriesAreIgnored(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"Stop": []any{
			"not-a-group",
			map[string]any{"hooks": "not-an-array"},
			map[string]any{"hooks": []any{"not-a-hook", map[string]any{"command": 42}}},
			map[string]any{"hooks": []any{map[string]any{"command": "good.sh"}}},
		},
	}, SubstitutionVars{})
	groups := parsed.Config["Stop"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != "good.sh" {
		t.Fatalf("groups = %+v", groups)
	}

	empty := mustParse(t, "just a string", SubstitutionVars{})
	if len(empty.Config) != 0 {
		t.Fatalf("a non-object payload parses to nothing, got %+v", empty.Config)
	}
}

func TestMatcherDiagnosticsUseClaudeCodeSemantics(t *testing.T) {
	// Match-all forms.
	for _, matcher := range []*string{nil, strPtr(""), strPtr("*")} {
		if diagnostic := hookprotocol.MatcherDiagnostic(matcher, hookprotocol.MatcherModeClaudeCode); diagnostic != "" {
			t.Fatalf("matcher %v = %q, want match-all", matcher, diagnostic)
		}
	}
	// A CC literal pattern.
	if diagnostic := hookprotocol.MatcherDiagnostic(strPtr("Bash|Read"), hookprotocol.MatcherModeClaudeCode); diagnostic != "" {
		t.Fatalf("literal pattern diagnostic = %q", diagnostic)
	}
	// A regex pattern compiles under the documented RE2 divergence.
	if diagnostic := hookprotocol.MatcherDiagnostic(strPtr("^(Write|Edit)$"), hookprotocol.MatcherModeClaudeCode); diagnostic != "" {
		t.Fatalf("regex pattern diagnostic = %q", diagnostic)
	}
	// A broken regex fails loudly.
	if diagnostic := hookprotocol.MatcherDiagnostic(strPtr("(["), hookprotocol.MatcherModeClaudeCode); diagnostic == "" {
		t.Fatal("a broken regex must carry a diagnostic")
	}
}

func strPtr(value string) *string { return &value }
