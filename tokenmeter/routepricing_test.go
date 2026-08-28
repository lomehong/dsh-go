package tokenmeter

import (
	"strings"
	"testing"

	"dshgo/llm"
)

func TestPriceSurfaceWithoutPricingKeepsHeuristic(t *testing.T) {
	nodes := []MeterSurfaceNode{
		{Seq: 0, HeuristicTokens: 9, ImageFreeTokens: 9},
		{Seq: 1, HeuristicTokens: 5, ImageFreeTokens: 5},
	}
	for _, pricing := range []ImageRequestPricing{nil, ImageRequestPricingFunc(func([]any) []ImagePrice {
		t.Fatal("pricing must not be consulted without image occurrences")
		return nil
	})} {
		surface, err := PriceSurface(nodes, pricing)
		if err != nil {
			t.Fatalf("price failed: %v", err)
		}
		if surface.SurfaceTokens != 14 || len(surface.Nodes) != 2 {
			t.Fatalf("heuristic surface wrong: %#v", surface)
		}
		if surface.Nodes[0].Tokens != 9 || surface.Nodes[0].HeuristicTokens != 9 {
			t.Fatalf("node prices wrong: %#v", surface.Nodes)
		}
	}
}

func TestPriceSurfaceReplacesImageOccurrences(t *testing.T) {
	structural := EstimateStructuralBlock(llm.ContentBlock{Type: llm.BlockImage, Attachment: "img"})
	nodes := []MeterSurfaceNode{
		{Seq: 0, HeuristicTokens: 10 + structural, ImageFreeTokens: 10, Images: []any{"img-0"}},
		{Seq: 1, HeuristicTokens: 5, ImageFreeTokens: 5},
		{Seq: 2, HeuristicTokens: 7 + 2*structural, ImageFreeTokens: 7, Images: []any{"img-2a", "img-2b"}},
	}
	pricing := ImageRequestPricingFunc(func(images []any) []ImagePrice {
		if len(images) != 3 {
			t.Fatalf("pricing asked for %d occurrences", len(images))
		}
		return []ImagePrice{
			{VisualTokens: 100, Text: "image zero"},
			{VisualTokens: 200, Text: "image two a"},
			{VisualTokens: 300, Text: "image two b"},
		}
	})
	surface, err := PriceSurface(nodes, pricing)
	if err != nil {
		t.Fatalf("price failed: %v", err)
	}
	// node 0: 10 + 100 + (ceil(10/4)=3 + 4); node 2: 7 + 207 + 307.
	if surface.Nodes[0].Tokens != 117 {
		t.Fatalf("node 0 price wrong: %d", surface.Nodes[0].Tokens)
	}
	if surface.Nodes[1].Tokens != 5 {
		t.Fatalf("image-free node must keep its price: %d", surface.Nodes[1].Tokens)
	}
	if surface.Nodes[2].Tokens != 521 {
		t.Fatalf("node 2 price wrong: %d", surface.Nodes[2].Tokens)
	}
	if surface.SurfaceTokens != 117+5+521 {
		t.Fatalf("surface total wrong: %d", surface.SurfaceTokens)
	}
	if surface.Nodes[0].HeuristicTokens != 10+structural {
		t.Fatalf("heuristic shadow price must survive: %d", surface.Nodes[0].HeuristicTokens)
	}
}

func TestPriceSurfaceCountMismatchFailsLoud(t *testing.T) {
	nodes := []MeterSurfaceNode{
		{Seq: 0, HeuristicTokens: 10, ImageFreeTokens: 5, Images: []any{"img-0", "img-1"}},
	}
	pricing := ImageRequestPricingFunc(func([]any) []ImagePrice {
		return []ImagePrice{{VisualTokens: 1}}
	})
	_, err := PriceSurface(nodes, pricing)
	if err == nil || !strings.Contains(err.Error(), "answered 1 prices for 2 occurrences") {
		t.Fatalf("count mismatch must fail loud, got %v", err)
	}
}
