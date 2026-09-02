package gateway

import (
	"context"
	"path/filepath"

	"dshgo/session"
	"dshgo/typert"
	"dshgo/workspace"
)

// WorkspaceController hosts the workspace Remote namespace (official
// workspace-controller): durable Web Workspace registry operations over the
// composed workspaceRegistry service — adopt-or-create by path, title
// mutation, registration deletion, DOM-insertBefore-like ordering for
// workspaces and session membership, and session archival.
type WorkspaceController struct {
	registry *workspace.Registry
}

// NewWorkspaceController builds the namespace host over the composed
// workspace registry.
func NewWorkspaceController(registry *workspace.Registry) *WorkspaceController {
	return &WorkspaceController{registry: registry}
}

// workspaceView projects one Entity onto the wire view (official
// WorkspaceView): the exact declared fields, nothing else.
func workspaceView(entity *workspace.Entity) map[string]any {
	ids := entity.SessionIDs()
	sessionIDs := make([]string, len(ids))
	for i, id := range ids {
		sessionIDs[i] = string(id)
	}
	return map[string]any{
		"workspaceId": string(entity.ID()),
		"path":        entity.Path(),
		"title":       entity.Title(),
		"sessionIds":  sessionIDs,
		"createdAt":   entity.CreatedAt(),
		"updatedAt":   entity.UpdatedAt(),
	}
}

// Create adopts an existing directory into the registry: the previously
// registered Workspace answers with created:false, a fresh adoption with
// created:true.
func (c *WorkspaceController) Create(ctx context.Context, request map[string]any) (any, error) {
	path, _ := request["path"].(string)
	existing, err := c.registry.ResolveByPath(ctx, path)
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/create", "", err, "workspace adoption failed")
	}
	if existing != nil {
		return map[string]any{"workspace": workspaceView(existing), "created": false}, nil
	}
	entity, err := c.registry.Create(ctx, path, filepath.Base(path))
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/create", "", err, "workspace adoption failed")
	}
	return map[string]any{"workspace": workspaceView(entity), "created": true}, nil
}

// Rename mutates one Workspace's title and answers the complete row.
func (c *WorkspaceController) Rename(ctx context.Context, request map[string]any) (any, error) {
	workspaceID, _ := request["workspaceId"].(string)
	title, _ := request["title"].(string)
	entity := c.registry.Get(workspace.WorkspaceID(workspaceID))
	if entity == nil {
		return nil, wrapGatewayError("workspace/not-found", "workspace/rename", "", nil, "workspace %q not found", workspaceID)
	}
	if err := entity.SetTitle(title); err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/rename", "", err, "workspace rename failed")
	}
	return map[string]any{"workspace": workspaceView(entity)}, nil
}

// Delete removes one Workspace registration.
func (c *WorkspaceController) Delete(ctx context.Context, request map[string]any) (any, error) {
	workspaceID, _ := request["workspaceId"].(string)
	if _, err := c.registry.Delete(ctx, workspace.WorkspaceID(workspaceID)); err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/delete", "", err, "workspace delete failed")
	}
	return map[string]any{"deleted": true}, nil
}

// InsertBefore applies the DOM-insertBefore-like Workspace order mutation and
// answers the complete registry order.
func (c *WorkspaceController) InsertBefore(ctx context.Context, request map[string]any) (any, error) {
	workspaceID, _ := request["workspaceId"].(string)
	var before workspace.WorkspaceID
	if s, ok := request["beforeWorkspaceId"].(string); ok {
		before = workspace.WorkspaceID(s)
	}
	order, err := c.registry.InsertBefore(ctx, workspace.WorkspaceID(workspaceID), before)
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/insertBefore", "", err, "workspace reorder failed")
	}
	ids := make([]string, len(order))
	for i, id := range order {
		ids[i] = string(id)
	}
	return map[string]any{"workspaceIds": ids}, nil
}

// InsertSessionBefore applies the session membership order mutation inside
// one Workspace and answers the complete row.
func (c *WorkspaceController) InsertSessionBefore(ctx context.Context, request map[string]any) (any, error) {
	workspaceID, _ := request["workspaceId"].(string)
	sessionID, _ := request["sessionId"].(string)
	entity := c.registry.Get(workspace.WorkspaceID(workspaceID))
	if entity == nil {
		return nil, wrapGatewayError("workspace/not-found", "workspace/insertSessionBefore", "", nil, "workspace %q not found", workspaceID)
	}
	var before session.SessionID
	if s, ok := request["beforeSessionId"].(string); ok {
		before = session.SessionID(s)
	}
	if err := entity.InsertSessionBefore(session.SessionID(sessionID), before); err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/insertSessionBefore", "", err, "session reorder failed")
	}
	return map[string]any{"workspace": workspaceView(entity)}, nil
}

// ArchiveSession archives one Session from Workspace grouping surfaces and
// answers the complete archived set.
func (c *WorkspaceController) ArchiveSession(ctx context.Context, request map[string]any) (any, error) {
	sessionID, _ := request["sessionId"].(string)
	if err := c.registry.ArchiveSession(ctx, session.SessionID(sessionID)); err != nil {
		return nil, wrapGatewayError("gateway/internal", "workspace/archiveSession", "", err, "session archive failed")
	}
	archived := c.registry.ArchivedSessionIDs()
	ids := make([]string, len(archived))
	for i, id := range archived {
		ids[i] = string(id)
	}
	return map[string]any{"archivedSessionIds": ids}, nil
}

// Contribution is the strict typert definition of the workspace namespace.
func (c *WorkspaceController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	inv := typert.InvocationReceiver{Kind: typert.ReceiverDirect}
	descriptor := func(id, method, implementation string, params ...typert.InvocationParameterDescriptor) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID:                    id,
			Service:               "workspaceController",
			Namespace:             "workspace",
			Method:                method,
			Implementation:        implementation,
			Invocation:            inv,
			CancellationParameter: "signal",
			Parameters:            params,
			Result:                jsonCodec,
		}
	}
	requestParam := func() typert.InvocationParameterDescriptor {
		return typert.InvocationParameterDescriptor{Name: "request", Wire: "request", Source: typert.SourceJSON, Codec: jsonCodec}
	}
	return typert.Contribution{
		Package: "workspace-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			descriptor("workspace.create", "create", "Create", requestParam()),
			descriptor("workspace.rename", "rename", "Rename", requestParam()),
			descriptor("workspace.delete", "delete", "Delete", requestParam()),
			descriptor("workspace.insertBefore", "insertBefore", "InsertBefore", requestParam()),
			descriptor("workspace.insertSessionBefore", "insertSessionBefore", "InsertSessionBefore", requestParam()),
			descriptor("workspace.archiveSession", "archiveSession", "ArchiveSession", requestParam()),
		},
	}
}
