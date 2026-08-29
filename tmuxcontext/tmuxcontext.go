// Package tmuxcontext ports @deepseek-ai/dsh-tmux-context: the opt-in
// request-preparation tmux-location context. Eligible step attempts append
// durable, source-attributed context naming the tmux session, window, and
// pane this agent process runs in, plus the window's pane-tree layout.
//
// The plugin pulls state once per turn, for the first step of a turn, by
// running one tmux/ps query through the shell executor seam. It confirms the
// process genuinely runs inside the pane $TMUX_PANE names by matching the
// pane's #{pane_tty} against the process's controlling terminal, so a
// terminal that merely inherited $TMUX/$TMUX_PANE from a tmux ancestor reads
// as "not in tmux". It re-injects only when the rendered state changes since
// the last injection, with an optional refresh-interval floor. Absent tmux
// environment or a failed query is a no-op, never an error: an executor
// rejection is contained and logged so the turn continues.
//
// Go adaptation: the shell executor is a package-local seam (the shell
// capability service has no Go consumer wiring yet) and the pre-step listener
// registers in ordinary order, prepending its message onto whatever decision
// the downstream produced.
package tmuxcontext

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// Name is the cordis plugin name and the source label on every injection.
const Name = "tmux-context"

// Config configures per-turn tmux-location scheduling. Invalid values fail
// plugin load.
type Config struct {
	// RefreshIntervalMs is the minimum milliseconds between durable
	// injections in one session. Zero injects on every eligible change.
	RefreshIntervalMs int
}

// ValidateConfig rejects refresh intervals that cannot represent an exact
// elapsed-millisecond threshold.
func ValidateConfig(config Config) error {
	if config.RefreshIntervalMs < 0 {
		return fmt.Errorf("tmux-context: refreshIntervalMs must be a non-negative safe integer, got %d", config.RefreshIntervalMs)
	}
	return nil
}

// ShellRunResult is one completed executor run.
type ShellRunResult struct {
	ExitCode int
	Stdout   string
}

// ShellExecutor is the bash seam the plugin runs its read-only tmux/ps query
// through. Run rejects the command on policy grounds (a failed query, never
// a turn failure) and resolves for nonzero exits, timeouts, and aborts.
type ShellExecutor interface {
	Run(command string, signal context.Context) (ShellRunResult, error)
}

// tmuxFields are the tab-separated tmux format fields, in query order.
// Layout (window_layout) is the pane-tree description; pane/window pixel
// sizes are intentionally excluded (own location and layout only).
var tmuxFields = []string{
	"#{session_name}",
	"#{window_index}",
	"#{window_name}",
	"#{pane_index}",
	"#{pane_id}",
	"#{window_active}",
	"#{pane_active}",
	"#{window_layout}",
}

// fieldSep is the separator between tmux format fields. tmux does not
// interpret C escapes in a format, so the literal two-character sequence
// `\t` is emitted verbatim and split back out here; this avoids embedding
// raw whitespace in the command.
const fieldSep = "\\t"

// readingPrefix marks the volatile turn/step preamble line of a rendered
// reading.
const readingPrefix = "tmux location (turn "

// TmuxLocation is the structured tmux location parsed from one
// display-message reading.
type TmuxLocation struct {
	SessionName  string
	WindowIndex  string
	WindowName   string
	PaneIndex    string
	PaneID       string
	WindowActive string
	PaneActive   string
	WindowLayout string
}

// QueryTmuxLocation reads the process's tmux location through the bash seam,
// or returns nil when the process is not genuinely running inside a tmux
// pane or the query fails.
//
// $TMUX_PANE alone is insufficient: a terminal launched from a tmux shell
// inherits the variables from that ancestor. The command therefore also
// compares the pane's #{pane_tty} against the process's own controlling
// terminal (`ps -o tty=` for processID); a genuine pane owns this process's
// tty. Fields are emitted only on a match, so an inherited environment reads
// as "not in tmux" and injects nothing.
func QueryTmuxLocation(bash ShellExecutor, logger cordis.Logger, processID int, signal context.Context) *TmuxLocation {
	format := strings.Join(tmuxFields, fieldSep)
	command := strings.Join([]string{
		`[ -n "$TMUX_PANE" ] || exit 1`,
		fmt.Sprintf(`self_tty=$(ps -o tty= -p %d | tr -d ' ')`, processID),
		`[ -n "$self_tty" ] || exit 1`,
		`pane_tty=$(tmux display-message -t "$TMUX_PANE" -p '#{pane_tty}') || exit 1`,
		`[ "$pane_tty" = "/dev/$self_tty" ] || exit 1`,
		fmt.Sprintf(`exec tmux display-message -t "$TMUX_PANE" -p '%s'`, format),
	}, "\n")
	result, err := bash.Run(command, signal)
	if err != nil {
		// The location is optional context, so an executor rejection is a
		// failed query, not a turn failure.
		if logger != nil {
			logger.Warn(fmt.Sprintf("tmux location query failed: %v; injecting no location this turn", err))
		}
		return nil
	}
	if result.ExitCode != 0 {
		return nil
	}
	line := result.Stdout
	if index := strings.IndexByte(line, '\n'); index >= 0 {
		line = line[:index]
	}
	parts := strings.Split(line, fieldSep)
	if len(parts) != len(tmuxFields) {
		return nil
	}
	location := &TmuxLocation{
		SessionName:  parts[0],
		WindowIndex:  parts[1],
		WindowName:   parts[2],
		PaneIndex:    parts[3],
		PaneID:       parts[4],
		WindowActive: parts[5],
		PaneActive:   parts[6],
		WindowLayout: parts[7],
	}
	if location.PaneID == "" {
		return nil
	}
	return location
}

// RenderState renders the stable tmux state block: the part of a reading
// compared for change suppression. It excludes the turn preamble so
// re-injection is driven only by tmux state, not by loop position.
func RenderState(location *TmuxLocation) string {
	windowName, _ := json.Marshal(location.WindowName)
	return fmt.Sprintf("session %s, window %s %s, pane %s %s\n"+
		"window active=%s, pane active=%s, layout %s",
		location.SessionName, location.WindowIndex, string(windowName),
		location.PaneIndex, location.PaneID,
		location.WindowActive, location.PaneActive, location.WindowLayout)
}

// RenderReading renders the full durable reading, including the volatile
// turn preamble.
func RenderReading(location *TmuxLocation, turn int64) string {
	return fmt.Sprintf("%s%d):\n%s", readingPrefix, turn, RenderState(location))
}

// LatestInjectedState returns the stable state block of the plugin's latest
// durable injection. It scans raw durable events so the schedule survives
// compaction and resumed processes without process-local cache state.
func LatestInjectedState(sess *session.Session) (state string, at int64, ok bool) {
	events := sess.Events()
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != session.EventUserMessage {
			continue
		}
		message, err := session.DecodeUserMessage(event)
		if err != nil || message.Source.Kind != llm.SourcePlugin || message.Source.Plugin != Name {
			continue
		}
		if len(message.Content) == 0 || message.Content[0].Type != "text" {
			return "", 0, false
		}
		text := message.Content[0].Text
		newline := strings.IndexByte(text, '\n')
		if newline == -1 {
			return "", event.Time, true
		}
		return text[newline+1:], event.Time, true
	}
	return "", 0, false
}

// Attach registers the pre-step listener. The bash seam may be nil, which
// makes every attempt a no-op like an absent executor service. The returned
// disposer detaches the listener.
func Attach(agents *agent.AgentRegistry, bash ShellExecutor, logger cordis.Logger, config Config) (func(), error) {
	if err := ValidateConfig(config); err != nil {
		return nil, err
	}
	processID := os.Getpid()
	detach := agents.Events().PreStep().On(nil, func(preStep agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		decision := next(preStep)
		if decision.Kind == "reject" || (preStep.Signal != nil && preStep.Signal.Err() != nil) || preStep.Step != 1 {
			return decision
		}
		if bash == nil {
			return decision
		}
		previousState, previousTime, hasPrevious := LatestInjectedState(preStep.Agent.Session)
		if config.RefreshIntervalMs > 0 && hasPrevious {
			now := nowMillis()
			if now >= previousTime && now-previousTime < int64(config.RefreshIntervalMs) {
				return decision
			}
		}
		location := QueryTmuxLocation(bash, logger, processID, preStep.Signal)
		if location == nil {
			return decision
		}
		state := RenderState(location)
		if hasPrevious && previousState == state {
			return decision
		}
		text := RenderReading(location, preStep.Turn)
		message := llm.NewUserMessage(
			[]llm.ContentBlock{{Type: "text", Text: text}},
			llm.MessageSource{
				Kind:     llm.SourcePlugin,
				Plugin:   Name,
				Form:     "snapshot",
				Sections: []llm.ContextSnapshotSection{{Name: Name, Text: text}},
			},
		)
		decision.Messages = append([]llm.Message{message}, decision.Messages...)
		return decision
	})
	return detach, nil
}

// nowMillis is the clock seam; tests override it.
var nowMillis = func() int64 { return time.Now().UnixMilli() }
