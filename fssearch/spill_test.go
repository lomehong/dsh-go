// The complete-result spill sink: persistence through the composed store
// and the render contract for the recovery locator.
package fssearch

import (
	"context"
	"strings"
	"testing"

	"dshgo/spill"
	"dshgo/tools"
)

type recordingSpillStore struct {
	saved []spill.SaveTextSpill
}

func (s *recordingSpillStore) SaveText(ctx context.Context, input spill.SaveTextSpill) (spill.SpillRef, error) {
	s.saved = append(s.saved, input)
	return spill.SpillRef{Locator: "spill://test/" + input.Source.ToolName, Bytes: len(input.Content), RetrievalHint: "Use read with offset/limit"}, nil
}

// The sink persists the complete result with its tool identity; empty
// content and a nil sink render the fallback instead of a fabricated ref.
func TestSpillSinkSavesCompleteResult(t *testing.T) {
	store := &recordingSpillStore{}
	sink := NewSpillSink(store, nil)
	ref := sink.SaveFullResult(context.Background(), nil, "glob", "glob-result.txt", strings.Repeat("p\n", 300))
	if ref == nil || ref.Locator != "spill://test/glob" || ref.RetrievalHint == "" {
		t.Fatalf("ref = %#v", ref)
	}
	if len(store.saved) != 1 || store.saved[0].Source.ToolName != "glob" || store.saved[0].SuggestedName != "glob-result.txt" {
		t.Fatalf("saved = %+v", store.saved)
	}

	if sink.SaveFullResult(context.Background(), nil, "glob", "glob-result.txt", "") != nil {
		t.Fatal("empty content must not persist")
	}
	var nilSink *SpillSink
	if nilSink.SaveFullResult(context.Background(), nil, "glob", "glob-result.txt", "x") != nil {
		t.Fatal("a nil sink must render the fallback")
	}
}

// The canonical savedTo value feeds the render footer: the recovery locator
// replaces the could-not-save explanation.
func TestSavedToDrivesRecoveryFooter(t *testing.T) {
	outcome := map[string]any{
		"paths":   []any{"a", "b"},
		"savedTo": map[string]any{"locator": "spill://x", "retrievalHint": "Use read"},
	}
	ref := savedToFromOutcome(outcome)
	if ref == nil || ref.Locator != "spill://x" {
		t.Fatalf("ref = %#v", ref)
	}
	formatted := formatGlobPage([]string{"a", "b"}, 2, ref, ".")
	if !strings.Contains(formatted, "Full sorted result stored at: spill://x") {
		t.Fatalf("formatted = %q", formatted)
	}
	if !strings.Contains(formatted, "Use read") {
		t.Fatalf("formatted = %q, want the retrieval hint", formatted)
	}

	// Without a savedTo the honest fallback renders.
	if strings.Contains(formatGlobPage([]string{"a", "b"}, 2, nil, "."), "spill://") {
		t.Fatal("no locator must keep the could-not-save wording")
	}
}

// The exec-carrying owner resolution passes the run context through.
func TestSpillSinkOwnerResolution(t *testing.T) {
	store := &recordingSpillStore{}
	seen := ""
	sink := NewSpillSink(store, func(exec *tools.ToolRunContext) string { seen = "session-1"; return seen })
	if sink.SaveFullResult(context.Background(), &tools.ToolRunContext{}, "grep", "grep-result.txt", "body") == nil {
		t.Fatal("save failed")
	}
	if seen != "session-1" {
		t.Fatalf("owner resolver never ran (seen=%q)", seen)
	}
}
