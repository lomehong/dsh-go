// The commands Remote namespace (official api-session commands surface the
// ui-commands client consumes): the session-scoped slash-command catalog
// (commands.list) and the admission+execution endpoint (commands.execute).
// Result fields project to the official camelCase wire keys.
package gateway

import (
	"context"
	"strings"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/session"
	"dshgo/typert"
)

// CommandsController hosts the commands Remote namespace over the composed
// commands runtime.
type CommandsController struct {
	lookup func() any
	agents func() *agent.AgentRegistry
}

// NewCommandsController builds the namespace host. Lookups resolve per call;
// absent services answer the loud not-composed error.
func NewCommandsController(lookup func() any, agents func() *agent.AgentRegistry) *CommandsController {
	return &CommandsController{lookup: lookup, agents: agents}
}

func (c *CommandsController) runtime() *commands.CommandRuntime {
	if runtime, ok := c.lookup().(*commands.CommandRuntime); ok && runtime != nil {
		return runtime
	}
	return nil
}

// List answers the session-scoped command catalog (official commands.list):
// the descriptors the slash menu renders, name-ordered, shadowed by the
// agent's scoped registrations.
func (c *CommandsController) List(ctx context.Context, request map[string]any) (any, error) {
	runtime := c.runtime()
	if runtime == nil {
		return nil, wrapGatewayError("gateway/not-composed", "commands/list", "", nil, "commands are not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	registry := c.agents()
	if registry == nil || registry.Get(sessionID) == nil {
		return nil, wrapGatewayError("session/not-found", "commands/list", "sessionId", nil,
			"session %q is not live", sessionID)
	}
	live := registry.Get(sessionID)
	descriptors := runtime.List(live.Scope)
	rows := make([]any, 0, len(descriptors))
	for _, descriptor := range descriptors {
		row := map[string]any{"name": descriptor.Name, "description": descriptor.Description}
		if descriptor.Input != nil {
			input := map[string]any{}
			if descriptor.Input.Hint != "" {
				input["hint"] = descriptor.Input.Hint
			}
			if descriptor.Input.Images {
				input["images"] = true
			}
			row["input"] = input
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Execute admits and runs one slash invocation in the receiving session
// (official commands.execute): the attachments ride as raw wire payloads the
// composed image admission seam decodes; the settled lifecycle pairing and
// the handler result ride back for the flow node.
func (c *CommandsController) Execute(ctx context.Context, request map[string]any) (any, error) {
	runtime := c.runtime()
	if runtime == nil {
		return nil, wrapGatewayError("gateway/not-composed", "commands/execute", "", nil, "commands are not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	line := requestString(request, "line")
	registry := c.agents()
	if registry == nil || registry.Get(sessionID) == nil {
		return nil, wrapGatewayError("session/not-found", "commands/execute", "sessionId", nil,
			"session %q is not live", sessionID)
	}
	if strings.TrimSpace(line) == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "commands/execute", "line", nil, "command execute requires a line")
	}
	live := registry.Get(sessionID)
	var images []any
	if raw, ok := request["attachments"].([]any); ok {
		images = raw
	}
	execution, err := runtime.ExecuteForAgent(ctx, live, live.Scope, live.Session, line, images)
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "commands/execute", "", err, "%v", err)
	}
	if execution == nil {
		// Admission miss (syntax / unknown name): nothing was logged. The
		// official RPC answers value-undefined and the client synthesizes
		// the refusal; the gateway states it as an arguments failure so the
		// browser composer gets the same outcome without a null deref.
		return nil, wrapGatewayError("gateway/arguments-invalid", "commands/execute", "", nil,
			"unknown or malformed command: %s", line)
	}
	return map[string]any{
		"commandId": string(execution.CommandID),
		"result":    projectCommandResult(execution.Result),
	}, nil
}

// projectCommandResult renders the result in the official camelCase wire
// keys (the browser reads kind/text and correlates sourceEventSeq).
func projectCommandResult(result commands.CommandResult) map[string]any {
	projected := map[string]any{"kind": result.Kind}
	if result.Text != "" {
		projected["text"] = result.Text
	}
	if result.SourceEventSeq != nil {
		projected["sourceEventSeq"] = *result.SourceEventSeq
	}
	return projected
}

// Contribution registers the namespace.
func (c *CommandsController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	requestParam := typert.InvocationParameterDescriptor{
		Name: "_request", Wire: "_request", Source: typert.SourceJSON, Codec: jsonCodec,
	}
	descriptor := func(id, method, implementation string) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID: id, Service: "commandsController", Namespace: "commands", Method: method,
			Implementation:        implementation,
			Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
			CancellationParameter: "signal",
			Parameters:            []typert.InvocationParameterDescriptor{requestParam},
			Result:                jsonCodec,
		}
	}
	return typert.Contribution{
		Package: "commands-controller",
		Face:    typert.FaceHost,
		Invocations: func() []typert.InvocationDescriptor {
			return []typert.InvocationDescriptor{
				descriptor("commands.list", "list", "List"),
				descriptor("commands.execute", "execute", "Execute"),
			}
		}(),
	}
}
