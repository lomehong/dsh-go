package tokenmeter

import (
	"testing"

	"dshgo/llm"
)

func TestPriceSurfacePricesFileHandleText(t *testing.T) {
	nodes := []MeterSurfaceNode{{
		Seq:             1,
		HeuristicTokens: 10,
		Files:           []any{map[string]any{"attachmentId": "sha256:abcdef1234567890", "name": "report.txt", "bytes": 2048}},
	}}
	text := "[File \"report.txt\" (2048 bytes, sha256:abcdef12): verbatim read-only copy saved at \"C:/x/report.txt\".]"
	surface, err := PriceSurface(nodes, nil, func(ref any) string { return text })
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	want := int64(10) + EstimateContent([]llm.ContentBlock{{Type: "text", Text: text}})
	if surface.SurfaceTokens != want {
		t.Fatalf("surface tokens: got %d want %d", surface.SurfaceTokens, want)
	}
}

func TestPriceSurfaceWithoutResolverKeepsHeuristic(t *testing.T) {
	nodes := []MeterSurfaceNode{{
		Seq:             1,
		HeuristicTokens: 10,
		Files:           []any{map[string]any{"attachmentId": "sha256:abcdef1234567890", "name": "report.txt", "bytes": 2048}},
	}}
	surface, err := PriceSurface(nodes, nil, nil)
	if err != nil {
		t.Fatalf("price: %v", err)
	}
	if surface.SurfaceTokens != 10 {
		t.Fatalf("heuristic tokens: %d", surface.SurfaceTokens)
	}
}
