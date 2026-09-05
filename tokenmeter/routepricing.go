package tokenmeter

import (
	"fmt"

	"dshgo/llm"
)

// ImagePrice is one image occurrence's route-declared request price: the
// visual tokens plus the model-visible text the routed adapter actually
// sends.
type ImagePrice struct {
	// VisualTokens is the route's declared price for the image itself.
	VisualTokens int64
	// Text is the model-visible textual stand-in the adapter sends.
	Text string
}

// ImageRequestPricing is the routed adapter's request-image pricing seam.
// PriceImages must answer one price per occurrence it is given; any other
// count misprices nodes and fails loud at the caller.
type ImageRequestPricing interface {
	PriceImages(images []any) []ImagePrice
}

// ImageRequestPricingFunc adapts a function to the pricing seam.
type ImageRequestPricingFunc func(images []any) []ImagePrice

// PriceImages implements ImageRequestPricing.
func (fn ImageRequestPricingFunc) PriceImages(images []any) []ImagePrice {
	return fn(images)
}

// PricedSurface is one surface priced for a request route: public nodes plus
// their total.
type PricedSurface struct {
	// Nodes are positional nodes carrying both the route price and the
	// fixed-heuristic price.
	Nodes []TokenSurfaceNode
	// SurfaceTokens is the sum of the route prices across the surface.
	SurfaceTokens int64
}

// PriceSurface prices one ordered surface under a route's request-image
// pricing. Without declared pricing every node keeps its fixed heuristic
// price, so provider-neutral behavior is unchanged. A pricing that answers a
// different occurrence count than it was asked fails loud — misalignment
// would silently misprice nodes.
func PriceSurface(nodes []MeterSurfaceNode, pricing ImageRequestPricing, fileText func(ref any) string) (PricedSurface, error) {
	if pricing == nil && fileText == nil {
		return heuristicSurface(nodes), nil
	}
	hasFiles := fileText != nil && nodesSomeFiles(nodes)
	var images []any
	if pricing != nil {
		for _, node := range nodes {
			images = append(images, node.Images...)
		}
		if len(images) == 0 {
			return fileAwareSurface(nodes, fileText, hasFiles), nil
		}
		prices := pricing.PriceImages(images)
		if len(prices) != len(images) {
			return PricedSurface{}, fmt.Errorf(
				"token meter: route image pricing answered %d prices for %d occurrences", len(prices), len(images))
		}
		cursor := 0
		surfaceTokens := int64(0)
		publicNodes := make([]TokenSurfaceNode, 0, len(nodes))
		for _, node := range nodes {
			tokens := node.HeuristicTokens
			if len(node.Images) > 0 {
				tokens = node.ImageFreeTokens
				for range node.Images {
					price := prices[cursor]
					cursor++
					tokens += price.VisualTokens + EstimateContent([]llm.ContentBlock{{Type: "text", Text: price.Text}})
				}
			}
			if hasFiles && len(node.Files) > 0 {
				for _, file := range node.Files {
					tokens += EstimateContent([]llm.ContentBlock{{Type: "text", Text: fileText(file)}})
				}
			}
			surfaceTokens += tokens
			publicNodes = append(publicNodes, TokenSurfaceNode{Seq: node.Seq, Tokens: tokens, HeuristicTokens: node.HeuristicTokens})
		}
		return PricedSurface{Nodes: publicNodes, SurfaceTokens: surfaceTokens}, nil
	}
	return fileAwareSurface(nodes, fileText, hasFiles), nil
}

func nodesSomeFiles(nodes []MeterSurfaceNode) bool {
	for _, node := range nodes {
		if len(node.Files) > 0 {
			return true
		}
	}
	return false
}

// fileAwareSurface prices file occurrences over the fixed heuristics when
// only the file projection is mounted.
func fileAwareSurface(nodes []MeterSurfaceNode, fileText func(ref any) string, hasFiles bool) PricedSurface {
	surfaceTokens := int64(0)
	publicNodes := make([]TokenSurfaceNode, 0, len(nodes))
	for _, node := range nodes {
		tokens := node.HeuristicTokens
		if hasFiles && len(node.Files) > 0 {
			for _, file := range node.Files {
				tokens += EstimateContent([]llm.ContentBlock{{Type: "text", Text: fileText(file)}})
			}
		}
		surfaceTokens += tokens
		publicNodes = append(publicNodes, TokenSurfaceNode{Seq: node.Seq, Tokens: tokens, HeuristicTokens: node.HeuristicTokens})
	}
	return PricedSurface{Nodes: publicNodes, SurfaceTokens: surfaceTokens}
}

// heuristicSurface is the no-pricing fast path: every node at its fixed
// heuristic price.
func heuristicSurface(nodes []MeterSurfaceNode) PricedSurface {
	surfaceTokens := int64(0)
	publicNodes := make([]TokenSurfaceNode, 0, len(nodes))
	for _, node := range nodes {
		surfaceTokens += node.HeuristicTokens
		publicNodes = append(publicNodes, TokenSurfaceNode{Seq: node.Seq, Tokens: node.HeuristicTokens, HeuristicTokens: node.HeuristicTokens})
	}
	return PricedSurface{Nodes: publicNodes, SurfaceTokens: surfaceTokens}
}
