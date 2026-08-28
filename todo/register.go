package todo

import (
	"encoding/json"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/session/projection"
	"dshgo/tools"
)

// Name is the tool's wire name.
const Name = "todo_write"

// Config is the model-facing todo tool configuration.
type Config struct {
	// AllowParallelInProgress is a required deployment choice for whether
	// several todos may be in_progress at once. True suits agents that run
	// work concurrently — subagents, background commands, workflow fan-out —
	// and the description then instructs the model to mark every actively
	// worked task. False restores the single-active discipline: the
	// description asks for exactly one, and a call marking more is rejected.
	AllowParallelInProgress bool
}

// ProjectionKey is the session-projection key this package declares; the
// unit carries the latest whole todo/write list, or nil before the first
// write.
const ProjectionKey = "todos"

// Register defines the todo_write tool on the runtime and, when a
// projection registry is given, the todos unit (headless assemblies without
// the seam stay unaffected). It returns the tool registration disposer.
func Register(runtime *tools.ToolRuntime, agents *agent.AgentRegistry, projections *projection.Registry, config Config) (func(), error) {
	if runtime == nil {
		return nil, fmt.Errorf("todo: a tool runtime is required")
	}
	closedObject := func() *bool { value := false; return &value }
	itemSpec := tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: closedObject(),
		Properties: map[string]tools.PropSpec{
			"content": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "What the task is — a short imperative line.",
			}, Required: true},
			"status": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Enum:        []any{StatusPending, StatusInProgress, StatusCompleted},
				Description: "pending (not started) | in_progress (now) | completed (done).",
			}, Required: true},
		},
	}
	tool, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        Name,
		Description: Describe(config.AllowParallelInProgress),
		Parameters: map[string]tools.PropSpec{
			"todos": {
				ValueSchemaSpec: tools.ValueSchemaSpec{
					Type:        "array",
					Items:       &itemSpec,
					Description: "The COMPLETE task list, replacing any previous list.",
				},
				Required: true,
			},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: closedObject(),
				Properties: map[string]tools.PropSpec{
					"todos": {
						ValueSchemaSpec: tools.ValueSchemaSpec{
							Type:  "array",
							Items: &itemSpec,
						},
						Required: true,
					},
					"counts": {
						ValueSchemaSpec: tools.ValueSchemaSpec{
							Type:                 "object",
							AdditionalProperties: closedObject(),
							Properties: map[string]tools.PropSpec{
								"pending":    {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
								"inProgress": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
								"completed":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
							},
						},
						Required: true,
					},
				},
			},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				counts := Counts{}
				if structured, ok := value.(map[string]any); ok {
					if raw, ok := structured["counts"].(Counts); ok {
						counts = raw
					}
				}
				return []llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf(
					"Updated todo list: %d pending, %d in progress, %d completed.",
					counts.Pending, counts.InProgress, counts.Completed)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			raw, ok := args["todos"].([]any)
			if !ok {
				return nil, fmt.Errorf("todo_write requires a todos array")
			}
			todos, err := ToTodoList(raw, config.AllowParallelInProgress)
			if err != nil {
				return nil, err
			}
			owner := resolveAgent(agents, exec.Agent)
			if owner == nil {
				// The list is per-agent-session state; a non-agent caller (no
				// owning session) has nowhere to write it. Reject rather than
				// silently no-op.
				return nil, fmt.Errorf("todo_write requires an owning agent session")
			}
			if _, err := owner.Session.Append(EventTodoWrite, map[string]any{"todos": todos}, nil); err != nil {
				return nil, err
			}
			return map[string]any{"todos": todos, "counts": CountsOf(todos)}, nil
		},
		IsConcurrencySafe: func(map[string]any) bool { return true },
	})
	if err != nil {
		return nil, err
	}
	undo, err := runtime.Register(tool)
	if err != nil {
		return nil, err
	}
	if projections != nil {
		if _, err := projections.Register(todosProjectionDefinition()); err != nil {
			undo()
			return nil, err
		}
	}
	return undo, nil
}

// resolveAgent resolves one live agent by its tools scope key; the todo
// list is per-agent-session state, so the owning instance must be live.
func resolveAgent(agents *agent.AgentRegistry, target scope.ScopeKey) *agent.Agent {
	if agents == nil || target == nil {
		return nil
	}
	for _, candidate := range agents.List() {
		if candidate.Scope == target {
			return candidate
		}
	}
	return nil
}

// todosProjectionDefinition builds the todos unit: latest whole todo/write
// list, cleared by the next turn/start (turn/end keeps the finished
// checklist visible); nil before the first write or after a later turn
// begins; every other event returns the same state reference.
func todosProjectionDefinition() projection.Definition {
	return projection.Definition{
		Key:          ProjectionKey,
		StateVersion: 2,
		Init:         func(session.SessionHeader) any { return nil },
		Apply: func(state any, event session.Event) any {
			if event.Type == EventTodoWrite {
				var payload struct {
					Todos []TodoItem `json:"todos"`
				}
				if err := json.Unmarshal(event.Data, &payload); err != nil {
					return state
				}
				return payload.Todos
			}
			if event.Type == session.EventTurnStart {
				return nil
			}
			return state
		},
		Wire: &projection.WireView{View: func(state any) any { return state }},
		DecodeState: func(raw json.RawMessage) (any, error) {
			var todos []TodoItem
			if err := json.Unmarshal(raw, &todos); err != nil {
				return nil, err
			}
			return todos, nil
		},
	}
}
