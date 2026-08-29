// Package hooksclaudecode ports @deepseek-ai/dsh-hooks-claude-code: the
// bridge for unmodified Claude Code command hooks on harness interception
// extension points. It supports SessionStart, prompt/tool pre/post, Stop,
// and subagent start/stop. It owns Claude payloads, environment,
// substitution, and decision mapping; shared execution and parsing live in
// dshgo/hookprotocol. updatedInput is logged and warned but not honored.
package hooksclaudecode

import (
	"fmt"
	"strings"

	"dshgo/hookprotocol"
)

// claudeEvents lists the CC hook events this bridge recognizes; every
// other event name in a config is ignored before its groups are parsed.
var claudeEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"Stop",
	"SubagentStart",
	"SubagentStop",
}

// ParsedClaudeConfig is the outcome of parsing one config file: the
// runnable groups (event name → its matcher groups, command hooks only)
// plus what was skipped.
type ParsedClaudeConfig struct {
	// Config maps each supported event to its runnable matcher groups.
	Config map[string][]hookprotocol.MatcherGroup
	// Skipped lists the non-command hooks, surfaced so the bridge can warn
	// about them.
	Skipped []SkippedHook
}

// SkippedHook is a skipped non-command hook.
type SkippedHook struct {
	// Event is the hook event the skipped hook was configured under.
	Event string
	// Type is the unsupported hook type string.
	Type string
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

// SubstitutionVars are the substitution variables applied to each command
// string at parse time.
type SubstitutionVars struct {
	// PluginRoot replaces ${CLAUDE_PLUGIN_ROOT} — the plugin's root dir.
	// nil leaves the token verbatim.
	PluginRoot *string
	// ProjectDir replaces ${CLAUDE_PROJECT_DIR} — the project root. nil
	// leaves the token verbatim.
	ProjectDir *string
}

// SubstituteCommand applies ${CLAUDE_PLUGIN_ROOT} /
// ${CLAUDE_PROJECT_DIR} substitution to a command string. A token whose
// variable is unset stays verbatim.
func SubstituteCommand(command string, vars SubstitutionVars) string {
	out := command
	if vars.PluginRoot != nil {
		out = strings.ReplaceAll(out, "${CLAUDE_PLUGIN_ROOT}", *vars.PluginRoot)
	}
	if vars.ProjectDir != nil {
		out = strings.ReplaceAll(out, "${CLAUDE_PROJECT_DIR}", *vars.ProjectDir)
	}
	return out
}

// ParseClaudeCodeConfig parses either a settings `hooks` value or a bare
// hooks.json event map. Malformed entries are ignored rather than failing
// boot; unsupported events are ignored before their groups are parsed,
// non-command hooks are returned in Skipped, and substitutions are applied
// to every surviving command. Matcher fields on UserPromptSubmit and Stop
// are discarded because those events have no matcher subject. A
// matcher-bearing supported runnable group with an invalid regex fails the
// parse (a Go error standing in for the reference SyntaxError), allowing
// the bridge to reject the complete config before listener registration.
func ParseClaudeCodeConfig(raw any, vars SubstitutionVars) (ParsedClaudeConfig, error) {
	parsed := ParsedClaudeConfig{Config: map[string][]hookprotocol.MatcherGroup{}}
	// Accept either { hooks: { … } } (a settings file) or the bare event
	// map.
	root, _ := raw.(map[string]any)
	hooksMap, _ := root["hooks"].(map[string]any)
	if hooksMap == nil && root != nil {
		hooksMap = root
	}
	if hooksMap == nil {
		return parsed, nil
	}

	for _, event := range claudeEvents {
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
					parsed.Skipped = append(parsed.Skipped, SkippedHook{Event: event, Type: hookType})
					continue
				}
				command, ok := hook["command"].(string)
				if !ok {
					continue
				}
				entry := hookprotocol.CommandHook{Command: SubstituteCommand(command, vars)}
				if timeout, ok := hook["timeout"].(float64); ok {
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
			if diagnostic := hookprotocol.MatcherDiagnostic(matcher, hookprotocol.MatcherModeClaudeCode); diagnostic != "" {
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
