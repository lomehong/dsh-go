// The complete-result spill sink: oversized glob/grep results persist
// through the composed spill store and the render face reports the
// recovery locator instead of the could-not-save explanation (official
// trySaveFormattedResult). Best effort by contract: a save failure renders
// the honest fallback.
package fssearch

import (
	"context"
	"strings"

	"dshgo/spill"
	"dshgo/tools"
)

// SpillSink persists one oversized complete result through the spill store.
// The owner session is best-effort descriptive: the backend groups storage
// by it while the locator stays the model-facing handle.
type SpillSink struct {
	store spill.Store
	// owner resolves the producing session id per call (the executing
	// agent's session when one is composed; empty groups under the shared
	// bucket).
	owner func(exec *tools.ToolRunContext) string
}

// NewSpillSink binds the store and the per-call owner resolver.
func NewSpillSink(store spill.Store, owner func(exec *tools.ToolRunContext) string) *SpillSink {
	return &SpillSink{store: store, owner: owner}
}

// SaveFullResult persists the complete formatted result text. A nil sink or
// a storage failure yields nil — the caller renders the could-not-save
// recovery path, never a fabricated locator.
func (s *SpillSink) SaveFullResult(ctx context.Context, exec *tools.ToolRunContext, tool, suggestedName, content string) *SpillRef {
	if s == nil || s.store == nil || strings.TrimSpace(content) == "" {
		return nil
	}
	owner := ""
	if s.owner != nil && exec != nil {
		owner = s.owner(exec)
	}
	ref, err := s.store.SaveText(ctx, spill.SaveTextSpill{
		Owner:         spill.SpillOwner{SessionID: owner},
		Source:        spill.SpillSource{ToolName: tool, Label: "complete result"},
		SuggestedName: suggestedName,
		Content:       content,
	})
	if err != nil {
		return nil
	}
	return &SpillRef{Locator: ref.Locator, RetrievalHint: ref.RetrievalHint}
}
