package hookscodex

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw any) ParsedCodexConfig {
	t.Helper()
	parsed, err := ParseCodexConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

func TestParseAcceptsWrapperAndBareMaps(t *testing.T) {
	wrapper := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{
				"matcher": "shell",
				"hooks":   []any{map[string]any{"command": "guard.sh", "timeout": 1.5}},
			}},
		},
	}
	parsed := mustParse(t, wrapper)
	groups := parsed.Config["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Hooks[0].TimeoutSec == nil || *groups[0].Hooks[0].TimeoutSec != 1.5 {
		t.Fatalf("timeout = %+v", groups[0].Hooks[0].TimeoutSec)
	}

	bare := map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "s.sh"}}}}}
	parsed = mustParse(t, bare)
	if len(parsed.Config["Stop"]) != 1 {
		t.Fatalf("bare groups = %+v", parsed.Config)
	}
}

func TestParseTimeoutSecAlias(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"Stop": []any{map[string]any{"hooks": []any{map[string]any{"command": "s.sh", "timeoutSec": 7.5}}}},
	})
	hook := parsed.Config["Stop"][0].Hooks[0]
	if hook.TimeoutSec == nil || *hook.TimeoutSec != 7.5 {
		t.Fatalf("timeoutSec alias = %+v", hook.TimeoutSec)
	}
}

func TestParseSkipsAsyncAndUnsupported(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"PreToolUse": []any{map[string]any{"hooks": []any{
			map[string]any{"command": "a.sh", "async": true},
			map[string]any{"type": "prompt", "prompt": "x"},
			map[string]any{"command": "sync.sh"},
		}}},
	})
	if len(parsed.Skipped) != 2 {
		t.Fatalf("skipped = %+v", parsed.Skipped)
	}
	if parsed.Skipped[0].Reason != "async hook" {
		t.Fatalf("first skip = %+v, want the async hook", parsed.Skipped[0])
	}
	if !strings.Contains(parsed.Skipped[1].Reason, `unsupported "prompt" hook`) {
		t.Fatalf("second skip = %+v", parsed.Skipped[1])
	}
	groups := parsed.Config["PreToolUse"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != "sync.sh" {
		t.Fatalf("groups = %+v", groups)
	}
}

func TestParseUnknownEventsAndMalformedEntriesIgnored(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"Notification": []any{map[string]any{"hooks": []any{map[string]any{"command": "n.sh"}}}},
		"Stop": []any{
			42,
			map[string]any{"hooks": nil},
			map[string]any{"hooks": []any{map[string]any{}}},
			map[string]any{"hooks": []any{map[string]any{"command": "good.sh"}}},
		},
	})
	if _, ok := parsed.Config["Notification"]; ok {
		t.Fatal("unknown events are ignored")
	}
	if len(parsed.Config["Stop"]) != 1 || parsed.Config["Stop"][0].Hooks[0].Command != "good.sh" {
		t.Fatalf("groups = %+v", parsed.Config)
	}
}

func TestParseInvalidMatcherFailsWithEventQualifiedError(t *testing.T) {
	_, err := ParseCodexConfig(map[string]any{
		"PostToolUse": []any{map[string]any{
			"matcher": "*[",
			"hooks":   []any{map[string]any{"command": "x.sh"}},
		}},
	})
	if err == nil {
		t.Fatal("an invalid regex matcher must fail the parse")
	}
	if !strings.Contains(err.Error(), `on event "PostToolUse"`) {
		t.Fatalf("error = %v, want the event-qualified diagnostic", err)
	}
}

func TestParseDropsPromptAndStopMatchers(t *testing.T) {
	parsed := mustParse(t, map[string]any{
		"UserPromptSubmit": []any{map[string]any{"matcher": "gone", "hooks": []any{map[string]any{"command": "u.sh"}}}},
		"Stop":             []any{map[string]any{"matcher": "also-gone", "hooks": []any{map[string]any{"command": "s.sh"}}}},
	})
	if parsed.Config["UserPromptSubmit"][0].Matcher != nil || parsed.Config["Stop"][0].Matcher != nil {
		t.Fatalf("matchers = %+v/%+v, want nil (no matcher subject)", parsed.Config["UserPromptSubmit"][0].Matcher, parsed.Config["Stop"][0].Matcher)
	}
}
