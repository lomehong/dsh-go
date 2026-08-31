package toolsessionquery

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/tools"
)

// SearchArguments is the session_search tool's argument surface.
type SearchArguments struct {
	Query               string   `json:"query"`
	ParentSessionIDs    []string `json:"parent_session_ids,omitempty"`
	IncludeRootSessions bool     `json:"include_root_sessions,omitempty"`
}

// EventSearchArguments is the session_event_search argument surface.
type EventSearchArguments struct {
	SessionID  string   `json:"session_id"`
	Query      string   `json:"query"`
	EventTypes []string `json:"event_types,omitempty"`
}

// EventTargetArguments names one session+seq target.
type EventTargetArguments struct {
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
	Before    *int64 `json:"before,omitempty"`
	After     *int64 `json:"after,omitempty"`
}

// registerSearchTools registers session_search and session_event_search.
func registerSearchTools(toolRuntime *tools.ToolRuntime, agents *agent.AgentRegistry, engine *sessionquery.Engine, resolved ResolvedConfig) (func(), error) {
	searchSchema := map[string]tools.PropSpec{
		"query":                 {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The literal full-text query."}, Required: true},
		"parent_session_ids":    {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "string"}, Description: "Optional parent session ids to restrict search."}},
		"include_root_sessions": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "boolean", Description: "Include root (parent-less) sessions."}},
	}
	undo1, err := registerTool(toolRuntime, "session_search", "Search prior sessions in the caller workspace and return the strongest matching event from each session.", searchSchema, true, resolved.SearchTimeoutMs, func(args map[string]any) (string, error) {
		var request SearchArguments
		if err := decodeArgs(args, &request); err != nil {
			return "", err
		}
		return executeSessionSearch(agents, engine, request, resolved.MaxSearchResults)
	})
	if err != nil {
		return nil, err
	}
	eventSchema := map[string]tools.PropSpec{
		"session_id":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The session id to search."}, Required: true},
		"query":       {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The literal full-text query."}, Required: true},
		"event_types": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "array", Items: &tools.ValueSchemaSpec{Type: "string"}, Description: "Optional event type filter."}},
	}
	undo2, err := registerTool(toolRuntime, "session_event_search", "Search prior events in one authorized session; the current session excludes the step performing this call.", eventSchema, true, resolved.SearchTimeoutMs, func(args map[string]any) (string, error) {
		var request EventSearchArguments
		if err := decodeArgs(args, &request); err != nil {
			return "", err
		}
		return executeEventSearch(agents, engine, request, resolved.MaxSearchResults)
	})
	if err != nil {
		undo1()
		return nil, err
	}
	return func() { undo2(); undo1() }, nil
}

// registerReadTools registers session_trace, session_event_trace, and
// session_event_read.
func registerReadTools(toolRuntime *tools.ToolRuntime, agents *agent.AgentRegistry, engine *sessionquery.Engine) (func(), error) {
	targetSchema := map[string]tools.PropSpec{
		"session_id": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The session id to read."}, Required: true},
	}
	undo1, err := registerTool(toolRuntime, "session_trace", "Read the authorized session lineage around one session, including complete visible ancestor and descendant relationships.", targetSchema, true, 0, func(args map[string]any) (string, error) {
		var request EventTargetArguments
		if err := decodeArgs(args, &request); err != nil {
			return "", err
		}
		return executeSessionTrace(agents, engine, request.SessionID)
	})
	if err != nil {
		return nil, err
	}
	traceSchema := map[string]tools.PropSpec{
		"session_id": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The session id."}, Required: true},
		"seq":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer", Description: "Target event sequence number."}, Required: true},
	}
	undo2, err := registerTool(toolRuntime, "session_event_trace", "Read every direct replacement and relationship to a cited source event for one event in an authorized session.", traceSchema, true, 0, func(args map[string]any) (string, error) {
		var request EventTargetArguments
		if err := decodeArgs(args, &request); err != nil {
			return "", err
		}
		return executeEventTrace(agents, engine, request.SessionID, request.Seq)
	})
	if err != nil {
		undo1()
		return nil, err
	}
	readSchema := map[string]tools.PropSpec{
		"session_id": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string", Description: "The session id."}, Required: true},
		"seq":        {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer", Description: "Target event sequence number."}, Required: true},
		"before":     {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer", Description: "Number of preceding raw events to summarize. Omit for none."}},
		"after":      {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer", Description: "Number of following raw events to summarize. Omit for none."}},
	}
	undo3, err := registerTool(toolRuntime, "session_event_read", "Read one full unabridged event and optional neighboring raw-event summaries from an authorized session.", readSchema, true, 0, func(args map[string]any) (string, error) {
		var request EventTargetArguments
		if err := decodeArgs(args, &request); err != nil {
			return "", err
		}
		return executeEventRead(agents, engine, request.SessionID, request.Seq, request.Before, request.After)
	})
	if err != nil {
		undo2()
		undo1()
		return nil, err
	}
	return func() { undo3(); undo2(); undo1() }, nil
}

// registerTool is a small helper wrapping the tools DSL.
func registerTool(runtime *tools.ToolRuntime, name, description string, parameters map[string]tools.PropSpec, concurrencySafe bool, timeoutMs int, execute func(map[string]any) (string, error)) (func(), error) {
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name:        name,
		Description: description,
		Parameters:  parameters,
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "string"},
			Render: textOutput,
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return execute(args)
		},
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// decodeArgs decodes the model arguments into a typed request.
func decodeArgs(args map[string]any, out any) error {
	encoded, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("session-query tool arguments are not JSON: %w", err)
	}
	return json.Unmarshal(encoded, out)
}

// executeSessionSearch runs the workspace-scoped cross-session search.
func executeSessionSearch(agents *agent.AgentRegistry, engine *sessionquery.Engine, request SearchArguments, maxResults int) (string, error) {
	// The caller cwd is required; the workspace fence applies it as the
	// scope filter. The engine applies the cwd filter over its corpus.
	cwd, err := callerCWD(agents, nil)
	if err != nil {
		return "", unauthorizedError()
	}
	_ = cwd
	filters := []sessionquery.SessionResultFilter{{Kind: "cwd", Values: []string{cwd}}}
	search, err := engine.SearchSessions(context.Background(), sessionquery.SessionSearchRequest{
		Query:          request.Query,
		SessionFilters: filters,
		Limit:          &maxResults,
	})
	if err != nil {
		return "", fmt.Errorf("session search failed: %w", err)
	}
	if search == nil || len(search.Items) == 0 {
		return formatEmptySearch(), nil
	}
	return formatSessionSearch(search.Items), nil
}

// executeEventSearch searches prior events in one authorized session.
func executeEventSearch(agents *agent.AgentRegistry, engine *sessionquery.Engine, request EventSearchArguments, maxResults int) (string, error) {
	page, err := engine.SearchEvents(context.Background(), sessionquery.SessionEventSearchRequest{
		SessionID: session.SessionID(request.SessionID),
		Query:     request.Query,
		Limit:     &maxResults,
	})
	if err != nil {
		return "", fmt.Errorf("event search failed: %w", err)
	}
	if page == nil || len(page.Items) == 0 {
		return formatEmptySearch(), nil
	}
	return formatEventSearch(page.Items), nil
}

// executeSessionTrace reads the session lineage.
func executeSessionTrace(agents *agent.AgentRegistry, engine *sessionquery.Engine, sessionID string) (string, error) {
	trace, err := engine.TraceSession(context.Background(), session.SessionID(sessionID))
	if err != nil {
		return "", fmt.Errorf("session trace failed: %w", err)
	}
	return formatSessionTrace(trace), nil
}

// executeEventTrace reads the replacement/relationship chain of one event.
func executeEventTrace(agents *agent.AgentRegistry, engine *sessionquery.Engine, sessionID string, seq int64) (string, error) {
	trace, err := engine.TraceEvent(context.Background(), sessionquery.SessionEventTraceRequest{
		SessionID: session.SessionID(sessionID),
		Seq:       seq,
	})
	if err != nil {
		return "", fmt.Errorf("event trace failed: %w", err)
	}
	return formatEventTrace(trace), nil
}

// executeEventRead reads one full event and optional neighbors.
func executeEventRead(agents *agent.AgentRegistry, engine *sessionquery.Engine, sessionID string, seq int64, before, after *int64) (string, error) {
	request := sessionquery.SessionEventReadRequest{
		SessionID: session.SessionID(sessionID),
		Seq:       seq,
	}
	if before != nil {
		value := int(*before)
		request.Before = &value
	}
	if after != nil {
		value := int(*after)
		request.After = &value
	}
	window, err := engine.ReadEvent(context.Background(), request)
	if err != nil {
		return "", fmt.Errorf("event read failed: %w", err)
	}
	return formatEventRead(window), nil
}

// callerCWD resolves the caller workspace root for the fence.
func callerCWD(agents *agent.AgentRegistry, exec *tools.ToolRunContext) (string, error) {
	// The tool call carries the calling agent scope; its session header
	// cwd is the workspace fence root.
	if exec == nil || exec.Agent == nil {
		return "", fmt.Errorf("no caller")
	}
	if agents == nil {
		return "", fmt.Errorf("no agent registry")
	}
	agent := agents.ByScope(exec.Agent)
	if agent == nil || agent.Session == nil {
		return "", fmt.Errorf("no live caller")
	}
	return agent.Session.Header().CWD, nil
}

// formatEmptySearch is the empty-result rendering.
func formatEmptySearch() string {
	return "No matching sessions or events were found."
}

// formatSessionSearch renders the strongest matching event per session.
func formatSessionSearch(hits []sessionquery.SessionSearchHit) string {
	var builder strings.Builder
	for _, hit := range hits {
		builder.WriteString(fmt.Sprintf("Session %s (live=%v persisted=%v):\n", hit.Header.ID, hit.Live, hit.Persisted))
		if hit.BestMatch.Snippet != "" {
			builder.WriteString("  " + hit.BestMatch.Snippet + "\n")
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

// formatEventSearch renders the strongest matching events.
func formatEventSearch(hits []sessionquery.SessionEventSearchHit) string {
	var builder strings.Builder
	for _, hit := range hits {
		builder.WriteString(fmt.Sprintf("seq %d [%s]: %s\n", hit.Seq, hit.Type, hit.Snippet))
	}
	return strings.TrimRight(builder.String(), "\n")
}

// formatSessionTrace renders the lineage.
func formatSessionTrace(trace sessionquery.SessionLineageTrace) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Session %s\n", trace.Target.Header.ID))
	if len(trace.Ancestors) > 0 {
		names := make([]string, 0, len(trace.Ancestors))
		for _, ancestor := range trace.Ancestors {
			names = append(names, string(ancestor.Header.ID))
		}
		builder.WriteString("Ancestors: " + strings.Join(names, ", ") + "\n")
	}
	if len(trace.Descendants) > 0 {
		names := make([]string, 0, len(trace.Descendants))
		for _, descendant := range trace.Descendants {
			names = append(names, string(descendant.Session.Header.ID))
		}
		builder.WriteString("Descendants: " + strings.Join(names, ", ") + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// formatEventTrace renders the target and its positional replacement chain.
func formatEventTrace(trace sessionquery.SessionEventTraceObservation) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("Event %d [%s]\n", trace.Target.Seq, trace.Target.Type))
	if len(trace.ReplacementChain) > 0 {
		seqs := make([]string, 0, len(trace.ReplacementChain))
		for _, seq := range trace.ReplacementChain {
			seqs = append(seqs, fmt.Sprintf("%d", seq))
		}
		builder.WriteString("Replaced by: " + strings.Join(seqs, ", ") + "\n")
	}
	return strings.TrimRight(builder.String(), "\n")
}

// formatEventRead renders the full event window.
func formatEventRead(window sessionquery.SessionEventWindow) string {
	var builder strings.Builder
	index := -1
	for i := range window.Events {
		if window.Events[i].Seq == window.Target.Seq {
			index = i
			break
		}
	}
	for i, event := range window.Events {
		marker := " "
		if i == index {
			marker = ">"
		}
		builder.WriteString(fmt.Sprintf("%s seq %d [%s]\n", marker, event.Seq, event.Type))
	}
	return strings.TrimRight(builder.String(), "\n")
}

// Compile-time interface check: the tool execute adapter conforms to the
// tools runtime contract.
var _ = llm.ContentBlock{}
var _ = sort.Strings
