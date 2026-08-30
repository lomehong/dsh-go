// Package subagentcontrol registers the globally named control tools over the
// subagent runtime (official `tool-subagent-control`): send_message and
// interrupt_agent are thin model-facing adapters over the runtime's
// followup/interrupt, and list_agents rides the continuable projection of
// ListChildren/ListDescendants. They stay separately loadable from the
// provider-bound delegation tools so a deployment can register continuation
// delivery without exposing discovery.
package subagentcontrol

import (
	"context"
	"encoding/json"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/projection"
	"dshgo/subagent"
	"dshgo/tools"
)

// ListingDeps carries the listing service seams: the live session registry,
// the shared projection registry, and the durable cold-read coordinator.
type ListingDeps struct {
	Store       *session.Store
	Projections *projection.Registry
	Coordinator *persistence.Coordinator
}

// sessionStore adapts the live store to the listing's SubagentSessionStore
// face (Get without the ok flag: absence is nil).
type sessionStore struct{ inner *persistence.StoreSessions }

func (s sessionStore) Get(id session.SessionID) *session.Session {
	live, _ := s.inner.Get(id)
	return live
}

// projectionListing adapts the shared projection registry to the listing's
// SubagentProjectionRegistry face: the erased cut decodes into the typed
// identity values the listing reads.
type projectionListing struct{ inner *projection.Registry }

func (p projectionListing) Snapshot(sess *session.Session, units []string) (subagent.SubagentProjectionValues, error) {
	cut := p.inner.Snapshot(sess, units...)
	values := subagent.SubagentProjectionValues{}
	if raw, ok := cut.Values[subagent.ProjectionKeySubagent]; ok && raw != nil {
		identity, ok := raw.(*subagent.SubagentIdentityProjection)
		if !ok {
			return values, fmt.Errorf("subagent-control: unexpected identity record %T", raw)
		}
		values.Subagent = identity
	}
	return values, nil
}

// queryEngine is the cold read seam (official `sessionQuery`): the corpus
// list rides the coordinator's durable header list, and one cold observation
// restores the stored prefix and folds the identity cut over it.
type queryEngine struct {
	coordinator *persistence.Coordinator
	projections *projection.Registry
}

func (q queryEngine) ListSessions() ([]session.SessionHeader, error) {
	return q.coordinator.List()
}

func (q queryEngine) ObserveSession(id session.SessionID) (*subagent.SubagentObservedSession, error) {
	inspection, err := q.coordinator.Load(id)
	if err != nil {
		return nil, err
	}
	values := subagent.SubagentProjectionValues{}
	sess, err := session.NewRestored(id, inspection.Events, inspection.Meta)
	if err != nil {
		return nil, err
	}
	cut := q.projections.Snapshot(sess, subagent.ProjectionKeySubagent)
	if raw, ok := cut.Values[subagent.ProjectionKeySubagent]; ok && raw != nil {
		identity, ok := raw.(*subagent.SubagentIdentityProjection)
		if !ok {
			return nil, fmt.Errorf("subagent-control: unexpected identity record %T", raw)
		}
		values.Subagent = identity
	}
	return &subagent.SubagentObservedSession{Header: inspection.Meta, Projections: &values}, nil
}

// listingServices binds the durable seams into one ListChildrenServices.
func listingServices(deps ListingDeps) subagent.ListChildrenServices {
	return subagent.ListChildrenServices{
		Projections: projectionListing{inner: deps.Projections},
		Sessions:    sessionStore{inner: persistence.NewSessionsAdapter(deps.Store)},
		Query:       queryEngine{coordinator: deps.Coordinator, projections: deps.Projections},
	}
}

// resolveCaller finds the exact live agent behind one execution's scope key
// (the established resolveByScope pattern); a transport sub-dispatch has no
// agent of its own and resolves nothing.
func resolveCaller(agents *agent.AgentRegistry, key tools.ScopeKey) *agent.Agent {
	if key == nil {
		return nil
	}
	for _, candidate := range agents.List() {
		if candidate.Scope == key {
			return candidate
		}
	}
	return nil
}

// statusOf refines one candidate's status through the live registry and its
// active-turn timing cut: running for an open turn, idle for a resident
// agent between turns, ready when no live agent remains (cold but
// resumable — never presented as a terminal result to collect).
func statusOf(deps ListingDeps, id session.SessionID) string {
	live := deps.Store.Get(id)
	if live == nil {
		return "ready"
	}
	cut := deps.Projections.Snapshot(live, subagent.ProjectionKeySubagentTiming)
	if timing, ok := cut.Values[subagent.ProjectionKeySubagentTiming].(subagent.SubagentTimingProjection); ok && timing.Active != nil {
		return "running"
	}
	return "idle"
}

// listAgentsEntry is one model-facing discovery row (official list_agents
// shape): a continuable child or a diagnostic; one-shot children are never
// selectable by send_message and stay omitted.
type listAgentsEntry struct {
	Kind   string            `json:"kind"`
	ID     session.SessionID `json:"id,omitempty"`
	Label  *string           `json:"label,omitempty"`
	Status string            `json:"status,omitempty"`
	Parent session.SessionID `json:"parent,omitempty"`
	Depth  *int              `json:"depth,omitempty"`
	Reason string            `json:"reason,omitempty"`
	DiagID session.SessionID `json:"diagId,omitempty"`
}

// projectChild converts one listing row into its model-facing entry.
func projectChild(deps ListingDeps, entry subagent.SubagentListEntry) *listAgentsEntry {
	if entry.Kind == "diagnostic" {
		return &listAgentsEntry{Kind: "diagnostic", DiagID: entry.ID, Reason: entry.Reason}
	}
	// One-shot children cannot be continued, so the model never selects
	// them; discovery still traversed them for the descendants scope.
	if entry.Mode != subagent.SubagentModeContinual {
		return nil
	}
	return &listAgentsEntry{
		Kind:   "child",
		ID:     entry.ID,
		Label:  entry.Label,
		Status: statusOf(deps, entry.ID),
	}
}

// renderEntries renders rows as one JSON text block; a malformed row never
// blocks the others.
func renderEntries(rows []*listAgentsEntry) []llm.ContentBlock {
	if rows == nil {
		rows = []*listAgentsEntry{}
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return []llm.ContentBlock{{Type: llm.BlockText, Text: "[]"}}
	}
	return []llm.ContentBlock{{Type: llm.BlockText, Text: string(encoded)}}
}

// Register installs send_message, interrupt_agent, and list_agents. The
// returned disposer tears them down in reverse registration order.
func Register(
	runtime *tools.ToolRuntime,
	subagents *subagent.SubagentRuntime,
	agents *agent.AgentRegistry,
	listing ListingDeps,
) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("subagent-control: a tool runtime is required")
	}
	if subagents == nil {
		return nil, fmt.Errorf("subagent-control: a subagent runtime is required")
	}
	closed := true
	sendMessage, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "send_message",
		Description: "Send a message to a background subagent by its subagent id, continuing the same conversation. It " +
			"becomes the subagent's next turn: if it is still working, the message waits until its current turn " +
			"finishes, so it cannot redirect work already underway. This call returns no answer from the " +
			"subagent — only confirmation that the message was delivered — so use it to give it more work. A " +
			"failure means the message was NOT delivered.",
		Parameters: map[string]tools.PropSpec{
			"subagent_id": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "The subagent id returned when the background subagent was started.",
				},
				Required: true,
			},
			"message": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "The message to deliver to the subagent.",
				},
				Required: true,
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: &closed,
				Properties: map[string]tools.PropSpec{
					"messageId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			Render: func(args map[string]any, _ any) []llm.ContentBlock {
				id, _ := args["subagent_id"].(string)
				return []llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("message queued as the next turn for subagent %s", id)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			id, _ := args["subagent_id"].(string)
			message, _ := args["message"].(string)
			if id == "" {
				return nil, fmt.Errorf("subagent_id is required")
			}
			if message == "" {
				return nil, fmt.Errorf("message is required")
			}
			var caller *agent.Agent
			if exec != nil {
				caller = resolveCaller(agents, exec.Agent)
			}
			if caller == nil {
				return nil, fmt.Errorf("send_message requires a receiving agent")
			}
			content := []llm.ContentBlock{{Type: llm.BlockText, Text: message}}
			options := subagent.SubagentFollowupOptions{
				Source: llm.MessageSource{Kind: llm.SourceUser},
			}
			if exec != nil && exec.Signal != nil {
				options.Signal = exec.Signal
			}
			messageID, err := subagents.Followup(caller, session.SessionID(id), content, options)
			if err != nil {
				return nil, err
			}
			return map[string]any{"messageId": string(messageID)}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	sendUndo, err := runtime.Register(sendMessage)
	if err != nil {
		return nil, err
	}

	interruptAgent, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "interrupt_agent",
		Description: "Interrupt a background subagent by its subagent id. The subagent will stop its current " +
			"work but its conversation stays available for later follow-ups.",
		Parameters: map[string]tools.PropSpec{
			"subagent_id": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Description: "The subagent id of the running subagent to interrupt.",
				},
				Required: true,
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: &closed,
				Properties: map[string]tools.PropSpec{
					"interrupted": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
				},
			},
			Render: func(args map[string]any, _ any) []llm.ContentBlock {
				id, _ := args["subagent_id"].(string)
				return []llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("stop requested for subagent %s", id)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			id, _ := args["subagent_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("subagent_id is required")
			}
			var caller *agent.Agent
			if exec != nil {
				caller = resolveCaller(agents, exec.Agent)
			}
			if caller == nil {
				return nil, fmt.Errorf("interrupt_agent requires a receiving agent")
			}
			if err := subagents.Interrupt(session.SessionID(id), subagent.SubagentInterruptAuthority{
				Kind:  subagent.InterruptAuthorityAncestor,
				Agent: caller,
			}); err != nil {
				return nil, err
			}
			return map[string]any{"interrupted": id}, nil
		},
	})
	if err != nil {
		sendUndo()
		return nil, err
	}
	interruptUndo, err := runtime.Register(interruptAgent)
	if err != nil {
		sendUndo()
		return nil, err
	}

	listUndo, err := RegisterListAgents(runtime, subagents, agents, listing)
	if err != nil {
		interruptUndo()
		sendUndo()
		return nil, err
	}
	return func() {
		listUndo()
		interruptUndo()
		sendUndo()
	}, nil
}

// RegisterListAgents installs the globally named list_agents tool alone
// (official list-agents.ts): a thin model-facing adapter over the
// continuable projection of listChildren and, for the descendants scope,
// listDescendants. It stays separately loadable from the root send_message
// plugin so a deployment can register continuation delivery without
// exposing discovery.
func RegisterListAgents(
	runtime *tools.ToolRuntime,
	subagents *subagent.SubagentRuntime,
	agents *agent.AgentRegistry,
	listing ListingDeps,
) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("subagent-control: a tool runtime is required")
	}
	if subagents == nil {
		return nil, fmt.Errorf("subagent-control: a subagent runtime is required")
	}
	services := listingServices(listing)
	closed := true
	listAgents, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "list_agents",
		Description: "List your continuable background subagents by durable id and label. Use it to recall which ones " +
			"you started, not to poll for completion — you are told when one finishes. Status comes from the " +
			"live registry: running means the agent is working right now, idle means it is loaded but between " +
			"turns (it may be waiting on agents it started), and ready means it exists only in storage — " +
			"resumable, not terminal, and not a result waiting to be collected.",
		Parameters: map[string]tools.PropSpec{
			"scope": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "string",
					Enum:        []any{"children", "descendants"},
					Description: "children lists direct children only; descendants walks the complete tree below you.",
				},
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type: "array",
				Items: &tools.ValueSchemaSpec{
					Type:                 "object",
					AdditionalProperties: &closed,
					Properties: map[string]tools.PropSpec{
						"kind":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
						"id":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
						"label":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
						"status": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Enum: []any{"running", "idle", "ready"}}},
						"parent": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
						"depth":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}},
						"reason": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
					},
				},
			},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				rows, _ := value.([]*listAgentsEntry)
				return renderEntries(rows)
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			scope, _ := args["scope"].(string)
			if scope == "" {
				scope = "children"
			}
			var caller *agent.Agent
			if exec != nil {
				caller = resolveCaller(agents, exec.Agent)
			}
			if caller == nil {
				return nil, fmt.Errorf("list_agents requires a receiving agent")
			}
			signal := context.Background()
			if exec != nil && exec.Signal != nil {
				signal = exec.Signal
			}
			switch scope {
			case "children":
				rows, err := subagent.ListChildren(signal, services, caller.Session.ID())
				if err != nil {
					return nil, err
				}
				entries := make([]*listAgentsEntry, 0, len(rows))
				for _, row := range rows {
					if projected := projectChild(listing, row); projected != nil {
						entries = append(entries, projected)
					}
				}
				return entries, nil
			case "descendants":
				rows, err := subagent.ListDescendants(signal, services, caller.Session.ID())
				if err != nil {
					return nil, err
				}
				entries := make([]*listAgentsEntry, 0, len(rows))
				for _, row := range rows {
					projected := projectChild(listing, row.SubagentListEntry)
					if projected == nil {
						continue
					}
					parent := row.ParentID
					depth := row.Depth
					projected.Parent = parent
					projected.Depth = &depth
					entries = append(entries, projected)
				}
				return entries, nil
			default:
				return nil, fmt.Errorf("unknown list_agents scope %q", scope)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	listUndo, err := runtime.Register(listAgents)
	if err != nil {
		return nil, err
	}
	return listUndo, nil
}

// compile-time interfaces
var (
	_ subagent.SubagentSessionStore       = sessionStore{}
	_ subagent.SubagentProjectionRegistry = projectionListing{}
	_ subagent.SubagentQueryEngine        = queryEngine{}
)
