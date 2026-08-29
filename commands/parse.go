package commands

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// commandNamePattern is the command-name format: lowercase without the
// leading slash.
var commandNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// ParsedCommand is a syntactically valid slash command before registry
// resolution.
type ParsedCommand struct {
	// Name is the lowercase command name without the leading slash.
	Name string
	// RawInput is the exact text following the command name.
	RawInput string
}

// ParseCommand parses an exact slash command without normalizing its
// trailing input. It reports false when the line is not a command. The
// source's lookahead form (`name` followed by end or horizontal/vertical
// whitespace) is expressed as a boundary check on the character after the
// name run.
func ParseCommand(line string) (ParsedCommand, bool) {
	if len(line) < 2 || line[0] != '/' {
		return ParsedCommand{}, false
	}
	index := 1
	first := line[index]
	if first < 'a' || first > 'z' {
		return ParsedCommand{}, false
	}
	index++
	for index < len(line) {
		c := line[index]
		isNameChar := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !isNameChar {
			break
		}
		index++
	}
	// The lookahead: the name run must end at the line end or at a
	// whitespace separator.
	if index < len(line) {
		switch line[index] {
		case '\t', '\n', '\r', ' ':
		default:
			return ParsedCommand{}, false
		}
	}
	return ParsedCommand{Name: line[1:index], RawInput: line[index:]}, true
}

// CommandDefinition is a plugin-owned command registration.
type CommandDefinition struct {
	// Name is the lowercase command name without the leading slash.
	Name string
	// Description is the human-readable summary used in discovery UI.
	Description string
	// Input is the optional free-form input hint advertised to capable
	// clients.
	Input *CommandInputDescriptor
	// RecordInput controls whether `command/run` records RawInput. Defaults
	// to true. A command whose domain event owns the payload sets this
	// false to avoid duplicating that payload in the session log.
	RecordInput *bool
	// Handler executes against the receiving agent without sending the
	// command to the model.
	Handler func(Invocation) (CommandResult, error)
}

// Invocation is passed to one registered command handler.
type Invocation struct {
	// CommandID is the pairing id already written to this invocation's
	// `command/run` event.
	CommandID CommandID
	// Session is the exact receiving agent's session (the source's
	// invocation carries the Agent; Go handlers take the session the
	// lifecycle records belong to).
	Session *session.Session
	// Agent is the exact receiving agent, when the dispatching surface
	// resolved one. Handlers that steer or switch agent state use it.
	Agent *agent.Agent
	// RawInput is the exact text following the registered command name,
	// including separator whitespace.
	RawInput string
	// Attachments are the durably admitted image blocks accompanying this
	// invocation, in submission order; empty unless the definition declares
	// input.images. The handler owns their model-visible use — the registry
	// never schedules them itself — and a handler whose grammar cannot use
	// them in this invocation returns an error so the dispatching composer
	// retains the originals.
	Attachments []ImageAttachment
	// Context carries the dispatching UI request's cancellation (the
	// source invocation's AbortSignal). Long-running handlers pass it into
	// their work.
	Context context.Context
}

// ImageAttachment is one durably admitted image block handed to a handler.
type ImageAttachment struct {
	// Reference is the store-assigned attachment reference (opaque to the
	// registry).
	Reference any
	// Block is the optional admitted image content block the composition's
	// admission adapter retained for handlers that re-inject the image into
	// model-visible messages (steering); nil when not retained.
	Block *llm.ContentBlock
}

// normalizeDefinition rejects invalid command metadata before it can reach
// a UI protocol.
func normalizeDefinition(definition CommandDefinition) (CommandDefinition, CommandDescriptor, error) {
	if !commandNamePattern.MatchString(definition.Name) {
		return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
			"command name %q must match %s", definition.Name, commandNamePattern)
	}
	if definition.Description == "" {
		return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
			"command %q description must be a string", definition.Name)
	}
	if strings.TrimSpace(definition.Description) == "" {
		return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
			"command %q description must not be empty", definition.Name)
	}
	if definition.Handler == nil {
		return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
			"command %q handler must be a function", definition.Name)
	}
	var input *CommandInputDescriptor
	if definition.Input != nil {
		if definition.Input.Hint == "" {
			return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
				"command %q input hint must be a string", definition.Name)
		}
		if strings.TrimSpace(definition.Input.Hint) == "" {
			return CommandDefinition{}, CommandDescriptor{}, fmt.Errorf(
				"command %q input hint must not be empty", definition.Name)
		}
		input = &CommandInputDescriptor{Hint: definition.Input.Hint, Images: definition.Input.Images}
	}
	descriptor := CommandDescriptor{Name: definition.Name, Description: definition.Description, Input: input}
	return definition, descriptor, nil
}

// normalizeResult validates an untrusted handler result at the registry
// boundary.
func normalizeResult(command string, result CommandResult, err error) (CommandResult, error) {
	if err != nil {
		return CommandResult{}, err
	}
	switch result.Kind {
	case ResultSuccess:
		// The source distinguishes an absent text from a non-string; Go's
		// string type cannot carry the non-string case, so only the
		// safe-integer check below remains for sourceEventSeq.
		if result.SourceEventSeq != nil && *result.SourceEventSeq < 0 {
			return CommandResult{}, fmt.Errorf(
				"command %q success sourceEventSeq must be a non-negative safe integer when supplied", command)
		}
		return result, nil
	case ResultError:
		if strings.TrimSpace(result.Text) == "" {
			return CommandResult{}, fmt.Errorf("command %q error text must be a non-empty string", command)
		}
		return result, nil
	default:
		return CommandResult{}, fmt.Errorf("command %q returned unknown result kind %q", command, result.Kind)
	}
}
