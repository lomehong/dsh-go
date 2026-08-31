// Package toolsessionquery ports packages/session-query/tool-session-query:
// model-facing, workspace-authorized session-history search and read tools
// (session_search, session_event_search, session_trace, session_event_trace,
// session_event_read) over the Go session-query engine.
package toolsessionquery

import (
	"context"
	"fmt"
	"strings"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/systemprompt"
	"dshgo/tools"
)

// Defaults for the deployment-owned search bounds.
const (
	DefaultMaxSearchResults = 100
	DefaultSearchTimeoutMs  = 30000
)

// Config is the deployment-owned search count and timeout bounds.
type Config struct {
	// MaxSearchResults is the maximum authorized hits returned by one search
	// call. Defaults to 100.
	MaxSearchResults *int
	// SearchTimeoutMs is the cooperative full-text search deadline in
	// milliseconds. Defaults to 30000.
	SearchTimeoutMs *int
}

// ResolvedConfig is the validated deployment policy.
type ResolvedConfig struct {
	MaxSearchResults int
	SearchTimeoutMs  int
}

// ResolveConfig applies defaults and validates.
func ResolveConfig(cfg Config) (ResolvedConfig, error) {
	maxResults := DefaultMaxSearchResults
	if cfg.MaxSearchResults != nil {
		maxResults = *cfg.MaxSearchResults
	}
	timeout := DefaultSearchTimeoutMs
	if cfg.SearchTimeoutMs != nil {
		timeout = *cfg.SearchTimeoutMs
	}
	if maxResults < 1 {
		return ResolvedConfig{}, fmt.Errorf("tool-session-query: maxSearchResults must be a positive integer")
	}
	if timeout < 1 {
		return ResolvedConfig{}, fmt.Errorf("tool-session-query: searchTimeoutMs must be a positive integer")
	}
	return ResolvedConfig{MaxSearchResults: maxResults, SearchTimeoutMs: timeout}, nil
}

// PromptText is the shared model guidance (verbatim).
const PromptText = "Use session_search to find relevant work from prior sessions, or session_event_search to search earlier events in one session. Search results are cursor-free and workspace-scoped. Follow a useful hit with session_trace, session_event_trace, or session_event_read when you need lineage, relationships, or exact data."

// Register defines the five tools and their shared model guidance. The
// engine and agent registry come from the composition (the official inject
// ['tools', 'systemPrompt', 'sessionQuery', 'sessionProjections']).
func Register(toolRuntime *tools.ToolRuntime, prompt *systemprompt.SystemPrompt, engine *sessionquery.Engine, agents *agent.AgentRegistry, config Config) (func(), error) {
	resolved, err := ResolveConfig(config)
	if err != nil {
		return nil, err
	}
	undoSection, err := prompt.Section(nil, systemprompt.PromptSection{
		Name:  "tool:session-query",
		Order: systemprompt.OrderToolSessionQuery,
		TextProvider: func(systemprompt.AssembleContext) string {
			return PromptText
		},
	})
	if err != nil {
		return nil, err
	}
	undoSearch, err := registerSearchTools(toolRuntime, agents, engine, resolved)
	if err != nil {
		undoSection()
		return nil, err
	}
	undoRead, err := registerReadTools(toolRuntime, agents, engine)
	if err != nil {
		undoSearch()
		undoSection()
		return nil, err
	}
	return func() {
		undoRead()
		undoSearch()
		undoSection()
	}, nil
}

// caller resolves the calling agent from the exec scope.
func caller(agents *agent.AgentRegistry, exec *tools.ToolRunContext) (*agent.Agent, error) {
	if exec.Agent == nil {
		return nil, fmt.Errorf("session-query tool requires a calling agent")
	}
	if agents == nil {
		return nil, fmt.Errorf("session-query tool requires the agent registry")
	}
	agent := agents.ByScope(exec.Agent)
	if agent == nil {
		return nil, fmt.Errorf("session-query tool could not resolve the calling agent")
	}
	return agent, nil
}

// unauthorizedError carries the stable unauthorized code.
func unauthorizedError() error {
	return fmt.Errorf("SESSION_QUERY_TOOL_UNAUTHORIZED: cross-session search is unavailable because the caller session has no workspace")
}

// textOutput renders a plain string result.
func textOutput(_ map[string]any, value any) []llm.ContentBlock {
	text, _ := value.(string)
	return []llm.ContentBlock{{Type: llm.BlockText, Text: text}}
}

var _ = context.Background
var _ = strings.TrimSpace
var _ = session.Event{}
