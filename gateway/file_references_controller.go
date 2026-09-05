// The fileReferences Remote namespace (official
// session-controller/src/file-references.ts): Agent-scoped file and
// directory candidates for the browser's `@` completion.
package gateway

import (
	"context"

	"dshgo/agent"
	"dshgo/filereference"
	"dshgo/session"
	"dshgo/typert"
)

// SessionFileReferences hosts the fileReferences Remote namespace over the
// composed file-reference provider.
type SessionFileReferences struct {
	lookup func() any
	agents func() *agent.AgentRegistry
}

// NewSessionFileReferences builds the namespace host. Lookups resolve per
// call; absent services answer the loud not-composed error.
func NewSessionFileReferences(lookup func() any, agents func() *agent.AgentRegistry) *SessionFileReferences {
	return &SessionFileReferences{lookup: lookup, agents: agents}
}

// List answers one agent's completion candidates (official
// fileReferences.list): the live session's cwd roots the per-agent index
// and the query is the path text after `@`.
func (c *SessionFileReferences) List(ctx context.Context, request map[string]any) (any, error) {
	service, _ := c.lookup().(*filereference.Service)
	if service == nil {
		return nil, wrapGatewayError("gateway/not-composed", "fileReferences/list", "", nil, "file references are not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	registry := c.agents()
	if registry == nil || registry.Get(sessionID) == nil {
		return nil, wrapGatewayError("session/not-found", "fileReferences/list", "sessionId", nil,
			"session %q is not live", sessionID)
	}
	live := registry.Get(sessionID)
	cwd := live.Session.Header().CWD
	candidates, err := service.List(ctx, string(live.ID), cwd, requestString(request, "query"))
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "fileReferences/list", "", err, "%v", err)
	}
	rows := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		rows = append(rows, map[string]any{"path": candidate.Path, "kind": candidate.Kind})
	}
	return map[string]any{"files": rows}, nil
}

// Contribution registers the namespace.
func (c *SessionFileReferences) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	requestParam := typert.InvocationParameterDescriptor{
		Name: "_request", Wire: "_request", Source: typert.SourceJSON, Codec: jsonCodec,
	}
	return typert.Contribution{
		Package: "session-file-references",
		Face:    typert.FaceHost,
		Invocations: func() []typert.InvocationDescriptor {
			return []typert.InvocationDescriptor{{
				ID: "fileReferences.list", Service: "sessionFileReferences", Namespace: "fileReferences", Method: "list",
				Implementation:        "List",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Parameters:            []typert.InvocationParameterDescriptor{requestParam},
				Result:                jsonCodec,
			}}
		}(),
	}
}
