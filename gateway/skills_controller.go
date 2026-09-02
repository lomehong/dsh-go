package gateway

import (
	"context"

	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/typert"
)

// skillsQueryUnavailable is the honest diagnostic when the composition does
// not expose the session-query engine the catalog reads through.
const skillsQueryUnavailable = "session query service is absent: this deployment does not compose a session-query engine in its composition"

// SkillsController hosts the skills Remote namespace (official
// session-skill-catalog): user-invocable skills visible through one Session's
// composition, without activating a cold Agent. The Go host has no skill
// registry yet, so a resolved Session answers an empty catalog — the true
// state of the composition — while a missing query engine or missing Session
// answers the official diagnostics.
type SkillsController struct {
	engine *sessionquery.Engine
}

// NewSkillsController builds the namespace host; a nil engine answers the
// absent-query diagnostic until the composition provides one.
func NewSkillsController(engine *sessionquery.Engine) *SkillsController {
	return &SkillsController{engine: engine}
}

// skillsListRequest is the wire request (official SkillListRequest).
type skillsListRequest struct {
	SessionID string `json:"sessionId"`
}

// List answers the user-invocable skills visible to one Session composition.
func (c *SkillsController) List(ctx context.Context, request skillsListRequest) (any, error) {
	if c.engine == nil {
		return nil, wrapGatewayError("gateway/internal", "skills/list", "", nil, "%s", skillsQueryUnavailable)
	}
	snapshot, err := c.engine.ReadSession(ctx, session.SessionID(request.SessionID))
	if err != nil {
		return nil, wrapGatewayError("session/not-found", "skills/list", "", nil, "session %q not found", request.SessionID)
	}
	if snapshot.Session.CWD == "" {
		return nil, wrapGatewayError("gateway/internal", "skills/list", "", nil, "session %q has no project cwd", request.SessionID)
	}
	return map[string]any{"skills": []any{}}, nil
}

// Contribution is the strict typert definition of the skills namespace.
func (c *SkillsController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "skills-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "skills.list",
				Service:               "sessionSkillCatalog",
				Namespace:             "skills",
				Method:                "list",
				Implementation:        "List",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Parameters: []typert.InvocationParameterDescriptor{
					{Name: "request", Wire: "request", Source: typert.SourceJSON, Codec: jsonCodec},
				},
				Result: jsonCodec,
			},
		},
	}
}
