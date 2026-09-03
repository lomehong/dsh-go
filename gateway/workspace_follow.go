package gateway

import (
	"context"

	"dshgo/workspace"
)

// workspaceRegistryService is the cordis service key of the durable Web
// Workspace registry (boot.ServiceWorkspace, official dsh-workspace).
const workspaceRegistryService = "workspaceRegistry"

// workspaceFollowEndpoint is the official workspace state-stream endpoint the
// browser opens over the mux (workspace-controller client: remote.$stream →
// remote.workspace.follow(signal)).
const workspaceFollowEndpoint = "workspace/follow"

// openWorkspaceFollow answers one workspace state stream: an opening
// baseline frame carrying the durable ordered registry projection and the
// archived session set, then the stream stays open until the caller's signal
// ends or the stream is cancelled. Same-client mutations already echo
// through the unary responses, so the opening baseline is what unblocks the
// browser's workspace selection; live cross-client increments are a later
// stream-generation concern.
func (g *Gateway) openWorkspaceFollow(args map[string]any, signal context.Context) (<-chan any, func(), error) {
	value := g.ctx.Get(workspaceRegistryService)
	reg, ok := value.(*workspace.Registry)
	if !ok || reg == nil {
		return nil, func() {}, wrapGatewayError("gateway/service-unavailable", workspaceFollowEndpoint, "", nil,
			"workspace registry service is unavailable in this composition")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer cancel()
		select {
		case <-signal.Done():
			return
		case <-ctx.Done():
			return
		}
	}()
	frames := make(chan any)
	go func() {
		defer close(frames)
		baseline := workspaceBaselineFrame(reg)
		select {
		case frames <- baseline:
		case <-signal.Done():
			return
		case <-ctx.Done():
			return
		}
		select {
		case <-signal.Done():
		case <-ctx.Done():
		}
	}()
	return frames, cancel, nil
}

// workspaceBaselineFrame builds the official baseline frame from the registry
// projection: ordered workspace items plus the complete archived session set.
func workspaceBaselineFrame(reg *workspace.Registry) map[string]any {
	entities := reg.List()
	items := make([]any, 0, len(entities))
	for _, entity := range entities {
		items = append(items, workspaceView(entity))
	}
	archived := reg.ArchivedSessionIDs()
	ids := make([]string, 0, len(archived))
	for _, id := range archived {
		ids = append(ids, string(id))
	}
	return map[string]any{
		"type": "baseline",
		"value": map[string]any{
			"items":              items,
			"archivedSessionIds": ids,
		},
	}
}
