package tokenmeter

import (
	"fmt"

	"dshgo/llm"
	"dshgo/session"
)

// MeterSurfaceNode is one priced surface node with the image occurrences
// route pricing replaces.
type MeterSurfaceNode struct {
	// Seq is the durable sequence number of the surface event.
	Seq int64
	// HeuristicTokens is the fixed-heuristic price of the node's exact
	// message.
	HeuristicTokens int64
	// ImageFreeTokens is the fixed-heuristic price with every image
	// occurrence's structural price removed.
	ImageFreeTokens int64
	// Images are the durable image occurrences in message order; empty for
	// image-free nodes.
	Images []any
}

// SurfaceTokenPlan is one validated surface transition that has not mutated
// the priced surface yet.
type SurfaceTokenPlan struct {
	// Tokens is the heuristic price of the event's own message; 0 when it
	// derives none.
	Tokens int64
	// DeltaTokens is the signed change in the surface total: Tokens minus
	// anything shadowed.
	DeltaTokens int64
	// Node is the priced node the commit inserts for this event.
	Node MeterSurfaceNode
	// Replace is nil for an append, or the inclusive replaced index range.
	Replace *[2]int
}

// collectImages gathers image occurrences recursively and totals their
// structural prices.
func collectImages(blocks []llm.ContentBlock, images *[]any) int64 {
	structuralTokens := int64(0)
	for _, block := range blocks {
		switch block.Type {
		case "image":
			*images = append(*images, block.Attachment)
			structuralTokens += EstimateStructuralBlock(block)
		case "tool-result":
			structuralTokens += collectImages(block.Content, images)
		}
	}
	return structuralTokens
}

// analyzeNode builds one priced node from a surface event's derived message.
func analyzeNode(seq int64, message *llm.Message) MeterSurfaceNode {
	if message == nil {
		return MeterSurfaceNode{Seq: seq}
	}
	heuristicTokens := EstimateMessage(*message)
	var images []any
	imageStructuralTokens := collectImages(message.Content, &images)
	return MeterSurfaceNode{
		Seq:             seq,
		HeuristicTokens: heuristicTokens,
		ImageFreeTokens: heuristicTokens - imageStructuralTokens,
		Images:          images,
	}
}

// PlanSurfaceTokens validates and prices one surface event without mutating
// the surface. A replacement naming a range absent from nodes is log
// corruption — committed logs are surface-validated at append time, so an
// unresolvable range fails loud rather than skipping the event.
func PlanSurfaceTokens(nodes []MeterSurfaceNode, event session.Event) (SurfaceTokenPlan, error) {
	node := analyzeNode(event.Seq, session.DeriveEventMessage(event))
	tokens := node.HeuristicTokens
	op := event.SurfaceOp
	if op == nil || op.Kind == "append" {
		return SurfaceTokenPlan{Tokens: tokens, DeltaTokens: tokens, Node: node}, nil
	}
	startIdx := firstIndexOfSeq(nodes, op.Start)
	endIdx := firstIndexOfSeq(nodes, op.End)
	if startIdx == -1 || endIdx == -1 || startIdx > endIdx {
		return SurfaceTokenPlan{}, fmt.Errorf(
			"token surface: replace at seq %d has invalid current range %d-%d", event.Seq, op.Start, op.End)
	}
	removed := int64(0)
	for _, candidate := range nodes[startIdx : endIdx+1] {
		removed += candidate.HeuristicTokens
	}
	return SurfaceTokenPlan{
		Tokens:      tokens,
		DeltaTokens: tokens - removed,
		Node:        node,
		Replace:     &[2]int{startIdx, endIdx},
	}, nil
}

// firstIndexOfSeq finds the first node carrying the seq, or -1.
func firstIndexOfSeq(nodes []MeterSurfaceNode, seq int64) int {
	for index, candidate := range nodes {
		if candidate.Seq == seq {
			return index
		}
	}
	return -1
}

// CommitSurfaceTokens applies one validated plan to the priced surface in
// place; infallible, so it cannot leave a half-applied surface behind.
func CommitSurfaceTokens(nodes *[]MeterSurfaceNode, plan SurfaceTokenPlan) {
	if plan.Replace == nil {
		*nodes = append(*nodes, plan.Node)
		return
	}
	startIdx, endIdx := plan.Replace[0], plan.Replace[1]
	replacement := []MeterSurfaceNode{plan.Node}
	*nodes = append((*nodes)[:startIdx], append(replacement, (*nodes)[endIdx+1:]...)...)
}
