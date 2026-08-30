// Package commandfeedback ports @deepseek-ai/dsh-command-feedback: the
// session feedback event plus the human-facing /feedback producer. Recording
// appends one authoritative log-only event and does not start model work;
// the append is eager but unflushed, so acknowledgement reports that the
// entry is logged, not that it reached disk.
package commandfeedback

import (
	"fmt"
	"strings"

	"dshgo/commands"
	"dshgo/identity"
	"dshgo/session"
)

// EventFeedbackRecord is one recorded human remark about this session.
// Log-only and independent of its trigger; it never enters model context or
// derived history.
const EventFeedbackRecord = "feedback/record"

// RegisterEvents extends the session vocabulary with this package's event
// type; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(EventFeedbackRecord)
}

// Usage is the usage hint appended to the error result.
const Usage = "Usage: /feedback <text>"

// RecordFeedback records feedback independently of any UI trigger;
// surrounding whitespace is discarded. An empty normalized text errors and
// leaves no event.
func RecordFeedback(sess *session.Session, text string) error {
	normalized := strings.TrimSpace(text)
	if normalized == "" {
		return fmt.Errorf("feedback text must not be empty")
	}
	_, err := sess.Append(EventFeedbackRecord, map[string]any{"text": normalized}, nil)
	return err
}

// Options carries the composition seams. SharingDisclosure supplies the
// mounted telemetry backend's disclosed sharing policy sentence; nil means
// no telemetry service is composed (the "not configured" disclosure).
type Options struct {
	// Getenv resolves environment overrides for the harness home.
	Getenv func(string) string
	// SharingDisclosure returns one sentence describing this session's
	// sharing policy.
	SharingDisclosure func() string
}

// sharingDisclosure renders the disclosure: the mounted backend's policy or
// the "not configured" notice when no backend is mounted.
func sharingDisclosure(options Options) string {
	if options.SharingDisclosure == nil {
		return "Session sharing is not configured."
	}
	return options.SharingDisclosure()
}

// Register registers the global /feedback command for every composed
// command adapter. Returning an error from the handler leaves no
// feedback/record event.
func Register(runtime *commands.CommandRuntime, options Options) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("command-feedback: a command runtime is required")
	}
	recordInput := false
	undo, err := runtime.Register(nil, commands.CommandDefinition{
		Name:        "feedback",
		Description: "record feedback about this session",
		Input:       &commands.CommandInputDescriptor{Hint: "<text>"},
		RecordInput: &recordInput,
		Handler: func(invocation commands.Invocation) (commands.CommandResult, error) {
			if strings.TrimSpace(invocation.RawInput) == "" {
				return commands.CommandResult{Kind: commands.ResultError, Text: fmt.Sprintf("Feedback text is required. %s", Usage)}, nil
			}
			if err := RecordFeedback(invocation.Session, invocation.RawInput); err != nil {
				return commands.CommandResult{}, err
			}
			return commands.CommandResult{
				Kind: commands.ResultSuccess,
				Text: fmt.Sprintf("Feedback recorded for session %s\nAnonymous user: %s. %s",
					invocation.Session.ID(), identity.GetOrCreateAnonymousUserID(identity.Options{Getenv: options.Getenv}), sharingDisclosure(options)),
				HasText: true,
			}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	return undo, nil
}
