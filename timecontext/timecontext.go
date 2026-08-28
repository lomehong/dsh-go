package timecontext

import (
	"encoding/json"
	"fmt"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// nowMillis is the clock seam; tests override it.
var nowMillis = func() int64 { return time.Now().UnixMilli() }

// Name is the cordis plugin name used by loader diagnostics and as the
// plugin-source attribution on every durable reading.
const Name = "time-context"

// Config is request-preparation clock formatting and append scheduling.
// Invalid values fail plugin load.
type Config struct {
	// TimeZone is the fallback display zone when the open turn has no
	// unique browser zone. Empty uses the process zone.
	TimeZone string
	// RefreshIntervalMs is the minimum milliseconds between durable
	// injections in one session. Nil or 0 injects at every eligible step.
	RefreshIntervalMs *int64
}

// ValidateRefreshInterval rejects refresh intervals that cannot represent
// an exact elapsed-millisecond threshold.
func ValidateRefreshInterval(refreshIntervalMs *int64) error {
	if refreshIntervalMs != nil && (*refreshIntervalMs < 0 || *refreshIntervalMs > 9007199254740991) {
		return fmt.Errorf("time-context: refreshIntervalMs must be a non-negative safe integer, got %d", *refreshIntervalMs)
	}
	return nil
}

// formatDuration formats a non-negative elapsed millisecond count as
// compact whole-second units, e.g. `2d 3h 4m 5s`.
func formatDuration(elapsedMs int64) string {
	seconds := elapsedMs / 1000
	if seconds < 0 {
		seconds = 0
	}
	days := seconds / 86400
	seconds %= 86400
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60
	parts := ""
	if days > 0 {
		parts += fmt.Sprintf("%dd ", days)
	}
	if days > 0 || hours > 0 {
		parts += fmt.Sprintf("%dh ", hours)
	}
	if days > 0 || hours > 0 || minutes > 0 {
		parts += fmt.Sprintf("%dm ", minutes)
	}
	parts += fmt.Sprintf("%ds", seconds)
	return parts
}

// reversed returns the session events in reverse order without mutating the
// live slice.
func reversed(events []session.Event) []session.Event {
	out := make([]session.Event, len(events))
	for index, event := range events {
		out[len(events)-1-index] = event
	}
	return out
}

// precedingMessageTime finds the latest model-visible event, excluding this
// plugin's pending append.
func precedingMessageTime(sess *session.Session) (int64, bool) {
	for _, event := range reversed(sess.Events()) {
		switch event.Type {
		case session.EventUserMessage, session.EventAssistantMsg, session.EventToolResult:
			return event.Time, true
		}
	}
	return 0, false
}

// precedingStepContextTime finds the preceding time-context event within
// the open turn.
func precedingStepContextTime(sess *session.Session, turn int64) (int64, bool) {
	for _, event := range reversed(sess.Events()) {
		if event.Type == session.EventTurnStart {
			var data session.TurnStartData
			if err := json.Unmarshal(event.Data, &data); err == nil && data.Turn == turn {
				return 0, false
			}
		}
		if event.Type == session.EventUserMessage {
			if message, err := session.DecodeUserMessage(event); err == nil &&
				message.Source.Kind == llm.SourcePlugin && message.Source.Plugin == Name {
				return event.Time, true
			}
		}
	}
	return 0, false
}

// latestInjectionTime finds this plugin's latest durable injection,
// including a shadowed surface event.
func latestInjectionTime(sess *session.Session) (int64, bool) {
	for _, event := range reversed(sess.Events()) {
		if event.Type != session.EventUserMessage {
			continue
		}
		if message, err := session.DecodeUserMessage(event); err == nil &&
			message.Source.Kind == llm.SourcePlugin && message.Source.Plugin == Name {
			return event.Time, true
		}
	}
	return 0, false
}

// requestMessages collects already-entered and proposed user messages
// belonging to one open turn.
func requestMessages(sess *session.Session, turn int64, proposed []llm.Message) []llm.Message {
	events := sess.Events()
	start := -1
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != session.EventTurnStart {
			continue
		}
		var data session.TurnStartData
		if err := json.Unmarshal(event.Data, &data); err == nil && data.Turn == turn {
			start = index
			break
		}
	}
	messages := make([]llm.Message, 0, len(proposed)+4)
	if start >= 0 {
		for _, event := range events[start+1:] {
			if event.Type != session.EventUserMessage {
				continue
			}
			if message, err := session.DecodeUserMessage(event); err == nil {
				messages = append(messages, message)
			}
		}
	}
	return append(messages, proposed...)
}

// renderText composes one durable reading: the sampled clock, the
// browser-zone policy line, and the elapsed time since the preceding
// model-visible message (step 1) or step context (later steps).
func renderText(now int64, turn int64, step int64, previous int64, hasPrevious bool, timeZone string, loc *time.Location, browser BrowserTimeZoneContext) string {
	elapsed := "unavailable"
	if hasPrevious {
		elapsed = formatDuration(now - previous)
	}
	baseline := "step context"
	if step == 1 {
		baseline = "model-visible message"
	}
	return fmt.Sprintf("Time sampled while preparing turn %d, step %d: %s\n%s\nElapsed since the preceding %s: %s.",
		turn, step, FormatTimestamp(now, loc, timeZone), RenderBrowserTimeZoneContext(browser), baseline, elapsed)
}

// Register validates the config and registers a prepended agent/pre-step
// waterfall listener for the lifetime of the given context. The registry's
// first-registered waterfall listener is outermost, which is the source's
// `{ prepend: true }` placement: this listener calls next first, then
// appends its reading after every other pre-step transform. Invalid
// refresh intervals or an unresolvable fallback zone fail here (plugin
// load).
func Register(ctx *cordis.Context, agents *agent.AgentRegistry, config Config) (func(), error) {
	if err := ValidateRefreshInterval(config.RefreshIntervalMs); err != nil {
		return nil, err
	}
	fallbackFormatter, err := CreateTimestampFormatter(config.TimeZone)
	if err != nil {
		if config.TimeZone == "" {
			return nil, fmt.Errorf("time-context: failed to resolve the system time zone")
		}
		return nil, fmt.Errorf("time-context: invalid IANA timeZone %q", config.TimeZone)
	}
	fallbackTimeZone := config.TimeZone
	if fallbackTimeZone == "" {
		// Go adaptation: the fallback bracket label is the location's name —
		// the IANA string for LoadLocation zones, "Local" for the process
		// zone when no name is recoverable.
		fallbackTimeZone = fallbackFormatter.String()
	}
	undo := agents.Events().OnWaterfall(agent.EventPreStep, nil, func(payload any, next func(any) any) any {
		preStep, ok := payload.(agent.PreStepPayload)
		if !ok {
			return next(payload)
		}
		decision, ok := next(payload).(agent.PreStepDecision)
		if !ok {
			return decision
		}
		if decision.Kind == "reject" || preStep.Signal.Err() != nil {
			return decision
		}
		now := nowMillis()
		if config.RefreshIntervalMs != nil && *config.RefreshIntervalMs > 0 {
			if lastInjection, ok := latestInjectionTime(preStep.Agent.Session); ok && now >= lastInjection && now-lastInjection < *config.RefreshIntervalMs {
				return decision
			}
		}
		var previous int64
		var hasPrevious bool
		if preStep.Step == 1 {
			previous, hasPrevious = precedingMessageTime(preStep.Agent.Session)
		} else {
			previous, hasPrevious = precedingStepContextTime(preStep.Agent.Session, preStep.Turn)
		}
		messages := requestMessages(preStep.Agent.Session, preStep.Turn, decision.Messages)
		browser, err := DeriveBrowserTimeZoneContext(messages)
		if err != nil {
			return agent.PreStepReject()
		}
		selectedTimeZone := fallbackTimeZone
		loc := fallbackFormatter
		if browser.Kind == "resolved" {
			selectedTimeZone = browser.TimeZone
			created, err := CreateTimestampFormatter(selectedTimeZone)
			if err != nil {
				return agent.PreStepReject()
			}
			loc = created
		}
		text := renderText(now, preStep.Turn, preStep.Step, previous, hasPrevious, selectedTimeZone, loc, browser)
		appended := llm.NewUserMessage(
			[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
			llm.MessageSource{
				Kind:   llm.SourcePlugin,
				Plugin: Name,
				Form:   llm.FormSnapshot,
				Sections: []llm.ContextSnapshotSection{
					{Name: Name, Text: text},
				},
			})
		withReading := make([]llm.Message, 0, len(decision.Messages)+1)
		withReading = append(withReading, decision.Messages...)
		withReading = append(withReading, appended)
		decision.Messages = withReading
		return decision
	})
	return undo, nil
}
