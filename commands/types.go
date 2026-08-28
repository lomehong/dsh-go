// Package commands ports packages/interaction/commands: the plugin-owned
// human-command registry shared by interactive UI adapters. Plain-context
// definitions are global; definitions registered through a command-injected
// child of an agent context shadow globals for that agent.
package commands

import (
	"fmt"

	"dshgo/session"
)

// EventCommandRun records that a resolved slash command entered its
// handler: log-only (never model surface), paired with `command/done` by
// CommandID, mirroring the `tool/call`↔`tool/result` pairing. The payload
// is structured — Name and Args are ParseCommand's own split (name and
// verbatim rawInput, separator whitespace included), so a consumer never
// re-parses a line. Args is absent when the definition sets RecordInput
// false because an authoritative domain event owns the input payload.
const EventCommandRun = "command/run"

// EventCommandDone records that the paired command settled. Kind/Text carry
// the handler's verbatim outcome (a thrown or aborted handler settles as
// kind "error" with the rendered failure). A successful command may identify
// the earlier authoritative domain event for a richer client-computed
// presentation.
const EventCommandDone = "command/done"

// CommandID pairs one command execution's `command/run`/`command/done`
// lifecycle records with each other and with the admission response.
// Minted by the executor, monotonic per service instance.
type CommandID string

// CommandSource is who issued a command line. Every executor caller is a
// human-facing UI surface dispatching a human-typed line, so the sole
// variant is "user".
type CommandSource struct {
	Kind string `json:"kind"`
}

// CommandRunData is the `command/run` payload.
type CommandRunData struct {
	CommandID CommandID     `json:"commandId"`
	Name      string        `json:"name"`
	Args      *string       `json:"args,omitempty"`
	Source    CommandSource `json:"source"`
}

// CommandDoneData is the `command/done` payload.
type CommandDoneData struct {
	CommandID      CommandID `json:"commandId"`
	Kind           string    `json:"kind"`
	Text           *string   `json:"text,omitempty"`
	SourceEventSeq *int64    `json:"sourceEventSeq,omitempty"`
}

func init() {
	for _, eventType := range []string{EventCommandRun, EventCommandDone} {
		if err := session.RegisterEventType(eventType); err != nil {
			panic(fmt.Sprintf("commands: register %s: %v", eventType, err))
		}
	}
}

// CommandInputDescriptor is the immutable metadata for a command's optional
// unstructured input.
type CommandInputDescriptor struct {
	// Hint is the placeholder shown before the user supplies free-form
	// input.
	Hint string `json:"hint"`
	// Images declares whether composer image attachments may accompany an
	// invocation. False = the executor rejects an invocation carrying
	// images and capable composers refuse the submission before dispatch.
	Images bool `json:"images,omitempty"`
}

// Result kinds of one command execution.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

// CommandResult is the expected command outcome rendered directly by the
// dispatching UI.
type CommandResult struct {
	// Kind is "success" or "error".
	Kind string
	// Text carries an optional success message or the required error
	// message.
	Text string
	// HasText distinguishes an absent success text from an empty one.
	HasText bool
	// SourceEventSeq identifies an earlier authoritative domain event that
	// owns a richer presentation (success results only).
	SourceEventSeq *int64
}

// CommandExecution is one settled command execution: the handler's
// normalized result plus the lifecycle pairing id minted for its
// `command/run`/`command/done` records, so a dispatching surface can
// correlate the acknowledgment with the flow node those events produce.
type CommandExecution struct {
	// CommandID is carried by this execution's lifecycle events.
	CommandID CommandID
	// Result is the handler's normalized outcome.
	Result CommandResult
}

// CommandDescriptor is the handler-free immutable command view returned to
// UI adapters.
type CommandDescriptor struct {
	// Name is the lowercase command name without the leading slash.
	Name string `json:"name"`
	// Description is the human-readable summary used in discovery UI.
	Description string `json:"description"`
	// Input is the optional free-form input hint advertised to capable
	// clients.
	Input *CommandInputDescriptor `json:"input,omitempty"`
}
