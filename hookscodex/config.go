// Package hookscodex ports @deepseek-ai/dsh-hooks-codex: the bridge for
// unmodified Codex command hooks on harness interception points. It
// supports five points (SessionStart, prompt/tool pre/post, Stop),
// regex-only matchers, snake_case payloads without a trailing newline, no
// hook environment or command substitution, and no pre-tool approval or
// rewrite path; only blocking decisions are honored. Shared execution and
// parsing live in dshgo/hookprotocol.
package hookscodex

import (
	"fmt"

	"dshgo/hookprotocol"
)

// codexEvents lists the five Codex hook points this bridge supports.
var codexEvents = []string{
	"PreToolUse",
	"PostToolUse",
	"SessionStart",
	"UserPromptSubmit",
	"Stop",
}

// ParsedCodexConfig is the outcome of parsing one Codex config file.
type ParsedCodexConfig struct {
	// Config maps each supported event to its runnable matcher groups
	// (command hooks only).
	Config map[string][]hookprotocol.MatcherGroup
	// Skipped lists the non-command (or async) hooks, surfaced so the
	// bridge can warn.
	Skipped []SkippedHook
}

// SkippedHook is a skipped non-command (or async) hook.
type SkippedHook struct {
	// Event is the hook event the skipped hook was configured under.
	Event string
	// Reason explains why the hook was skipped.
	Reason string
}

// matcherError rejects a complete config whose runnable group carries an
// invalid matcher (the Go stand-in for the reference SyntaxError).
type matcherError struct {
	diagnostic string
	event      string
}

func (e *matcherError) Error() string {
	return fmt.Sprintf("%s on event %q", e.diagnostic, e.event)
}

// ParseCodexConfig parses a wrapped or bare Codex event map. Unknown events
// and malformed entries are ignored rather than failing boot; unsupported
// or asynchronous hooks are returned in Skipped. Matcher fields on
// UserPromptSubmit and Stop are discarded because those events have no
// matcher subject. A matcher-bearing runnable group with an invalid regex
// fails the parse (a Go error standing in for the reference SyntaxError),
// allowing the bridge to reject the complete config before listener
// registration.
func ParseCodexConfig(raw any) (ParsedCodexConfig, error) {
	parsed := ParsedCodexConfig{Config: map[string][]hookprotocol.MatcherGroup{}}
	root, _ := raw.(map[string]any)
	hooksMap, _ := root["hooks"].(map[string]any)
	if hooksMap == nil && root != nil {
		hooksMap = root
	}
	if hooksMap == nil {
		return parsed, nil
	}

	for _, event := range codexEvents {
		rawGroups, ok := hooksMap[event].([]any)
		if !ok {
			continue
		}
		groups := []hookprotocol.MatcherGroup{}
		for _, rawGroup := range rawGroups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				continue
			}
			rawHooks, ok := group["hooks"].([]any)
			if !ok {
				continue
			}
			commands := []hookprotocol.CommandHook{}
			for _, rawHook := range rawHooks {
				hook, ok := rawHook.(map[string]any)
				if !ok {
					continue
				}
				hookType, ok := hook["type"].(string)
				if !ok {
					hookType = "command"
				}
				if hookType != "command" {
					parsed.Skipped = append(parsed.Skipped, SkippedHook{Event: event, Reason: fmt.Sprintf("unsupported %q hook", hookType)})
					continue
				}
				if async, ok := hook["async"].(bool); ok && async {
					parsed.Skipped = append(parsed.Skipped, SkippedHook{Event: event, Reason: "async hook"})
					continue
				}
				command, ok := hook["command"].(string)
				if !ok {
					continue
				}
				entry := hookprotocol.CommandHook{Command: command}
				// Codex accepts `timeout` or the `timeoutSec` alias.
				if timeout, ok := hook["timeout"].(float64); ok {
					timeout := timeout
					entry.TimeoutSec = &timeout
				} else if timeout, ok := hook["timeoutSec"].(float64); ok {
					timeout := timeout
					entry.TimeoutSec = &timeout
				}
				commands = append(commands, entry)
			}
			if len(commands) == 0 {
				continue
			}
			var matcher *string
			if event != "UserPromptSubmit" && event != "Stop" {
				if rawMatcher, ok := group["matcher"].(string); ok {
					matcher = &rawMatcher
				}
			}
			if diagnostic := hookprotocol.MatcherDiagnostic(matcher, hookprotocol.MatcherModeCodex); diagnostic != "" {
				return parsed, &matcherError{diagnostic: diagnostic, event: event}
			}
			groups = append(groups, hookprotocol.MatcherGroup{Matcher: matcher, Hooks: commands})
		}
		if len(groups) > 0 {
			parsed.Config[event] = groups
		}
	}

	return parsed, nil
}
