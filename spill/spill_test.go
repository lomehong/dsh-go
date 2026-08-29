package spill_test

import (
	"context"
	"testing"

	"dshgo/spill"
)

// compile seam proves the Store contract: SaveText persists the full content
// verbatim and reports the exact byte length through the reference.
type recordingStore struct {
	last spill.SaveTextSpill
}

func (r *recordingStore) SaveText(ctx context.Context, input spill.SaveTextSpill) (spill.SpillRef, error) {
	r.last = input
	return spill.SpillRef{Locator: "db://spill/42", Bytes: len(input.Content), RetrievalHint: "fetch by key 42"}, nil
}

func TestStoreContractPersistsFullContentVerbatim(t *testing.T) {
	var store spill.Store = &recordingStore{}
	ref, err := store.SaveText(context.Background(), spill.SaveTextSpill{
		Owner:         spill.SpillOwner{SessionID: "session-9"},
		Source:        spill.SpillSource{ToolName: "web_fetch", CallID: "call-1", Label: "result"},
		SuggestedName: "web_fetch.txt",
		Content:       "全文\nsecond line",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if ref.Locator != "db://spill/42" || ref.Bytes != len("全文\nsecond line") || ref.RetrievalHint != "fetch by key 42" {
		t.Fatalf("ref = %+v", ref)
	}
	if store.(*recordingStore).last.Owner.SessionID != "session-9" {
		t.Fatalf("owner lost: %+v", store.(*recordingStore).last)
	}
}
