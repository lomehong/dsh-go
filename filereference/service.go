package filereference

import (
	"context"
	"sync"
)

// Service is the local-filesystem owner of file-reference discovery: one
// WorkspaceFileSearch per agent workspace, invalidated on tool results and
// disposed with the agent. Host wiring (the system-prompt section and the
// tool-result invalidation listener) subscribes through these methods; the
// section text itself is FileReferencePrompt, shown when the session's read
// tool is mounted.
type Service struct {
	config SearchConfig

	mu       sync.Mutex
	searches map[string]*WorkspaceFileSearch
}

// NewService validates the limits once at construction — misconfiguration
// fails loud at load.
func NewService(config SearchConfig) (*Service, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Service{config: config, searches: map[string]*WorkspaceFileSearch{}}, nil
}

// DefaultServiceConfig is the shipped configuration.
func DefaultServiceConfig() SearchConfig {
	return SearchConfig{
		MaxResults:          DefaultMaxResults,
		MaxEntries:          DefaultMaxEntries,
		ExcludedDirectories: append([]string{}, DefaultExcludedDirectories...),
	}
}

// List returns candidates for one agent's workspace. The first call for an
// agent roots its search at cwd; later calls reuse that root. agentID keys
// the per-agent index (Go adaptation of the official Agent-keyed map).
func (s *Service) List(ctx context.Context, agentID, cwd, query string) ([]Candidate, error) {
	search := s.searchFor(agentID, cwd)
	return search.List(ctx, query)
}

// InvalidateAgent marks one agent's index stale (the tool/result listener).
func (s *Service) InvalidateAgent(agentID string) {
	s.mu.Lock()
	search := s.searches[agentID]
	s.mu.Unlock()
	if search != nil {
		search.Invalidate()
	}
}

// DisposeAgent releases one agent's index (the agent/disposed listener).
func (s *Service) DisposeAgent(agentID string) {
	s.mu.Lock()
	search := s.searches[agentID]
	delete(s.searches, agentID)
	s.mu.Unlock()
	if search != nil {
		search.Dispose()
	}
}

// Dispose releases every agent's index (the service effect teardown).
func (s *Service) Dispose() {
	s.mu.Lock()
	searches := s.searches
	s.searches = map[string]*WorkspaceFileSearch{}
	s.mu.Unlock()
	for _, search := range searches {
		search.Dispose()
	}
}

func (s *Service) searchFor(agentID, cwd string) *WorkspaceFileSearch {
	s.mu.Lock()
	defer s.mu.Unlock()
	if search := s.searches[agentID]; search != nil {
		return search
	}
	// The config was validated at NewService, so construction cannot fail.
	search := newSearch(cwd, s.config)
	s.searches[agentID] = search
	return search
}
