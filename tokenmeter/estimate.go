// Package tokenmeter ports packages/llm/token-meter: the single
// replay-aware token-meter service for request and surface pressure. The
// fixed estimator has no settings; per-session folds ride the durable log
// tail, and measure() reprices the surface under the routed model's
// request-image pricing.
//
// This round ports the measurement core — estimator, surface fold, route
// pricing, and the meter service. The O(1) projection units
// (usage/pressure/breakdown) and turn-usage fold land with their round.
package tokenmeter

import (
	"encoding/json"
	"math"

	"dshgo/llm"
	"dshgo/session"
)

// The fixed-density heuristic constants (JS estimate.ts).
const (
	// charsPerToken is the fixed text-density estimate used until exact
	// tokenization is needed.
	charsPerToken = 4
	// blockOverhead is the per-block structural overhead for JSON framing
	// and type tags.
	blockOverhead = 4
	// RoleOverhead is the role-field framing overhead added to every priced
	// message.
	RoleOverhead = 4
)

// ceilDiv is the integer ceil(length / charsPerToken).
func ceilDiv(length int) int64 {
	return (int64(length) + charsPerToken - 1) / charsPerToken
}

// EstimateStructuralBlock is the structural JSON price of one block outside
// the typed pricing arms: the fixed heuristic for merge-extended blocks and
// for image references, whose request price is route-owned rather than
// fixed.
func EstimateStructuralBlock(block llm.ContentBlock) int64 {
	encoded, err := json.Marshal(block)
	if err != nil {
		return blockOverhead
	}
	return blockOverhead + ceilDiv(len(encoded))
}

// EstimateContent prices content blocks recursively under the fixed density
// heuristic, including per-block structural overhead.
func EstimateContent(blocks []llm.ContentBlock) int64 {
	tokens := int64(0)
	for _, block := range blocks {
		switch block.Type {
		case "text", "reasoning":
			tokens += ceilDiv(len(block.Text)) + blockOverhead
		case "tool-call":
			tokens += ceilDiv(len(block.Name)) + ceilDiv(len(block.Arguments)) + blockOverhead
		case "tool-result":
			tokens += EstimateContent(block.Content) + blockOverhead
		default:
			// The block map is merge-extensible; unknown blocks (and image
			// references, whose request price is route-owned) retain a
			// conservative structural JSON price under the fixed heuristic.
			tokens += EstimateStructuralBlock(block)
		}
	}
	return tokens
}

// EstimateMessage heuristically prices one model-visible message: content
// plus role-framing tokens.
func EstimateMessage(message llm.Message) int64 {
	return EstimateContent(message.Content) + RoleOverhead
}

// EstimateSystemTokens prices the system-prompt part of a canonical request
// envelope; 0 when absent.
func EstimateSystemTokens(header *session.EpochHeader) int64 {
	if header == nil || header.System == "" {
		return 0
	}
	return ceilDiv(len(header.System)) + RoleOverhead
}

// EstimateToolsTokens prices the tool-schema part of a canonical request
// envelope; 0 when absent or empty.
func EstimateToolsTokens(header *session.EpochHeader) int64 {
	if header == nil || len(header.Tools) == 0 {
		return 0
	}
	encoded, err := json.Marshal(header.Tools)
	if err != nil {
		return blockOverhead
	}
	return ceilDiv(len(encoded)) + blockOverhead
}

// EstimateHeader prices the complete non-surface request envelope: system
// plus tools.
func EstimateHeader(header *session.EpochHeader) int64 {
	return EstimateSystemTokens(header) + EstimateToolsTokens(header)
}

// BaselineKind discriminates the measurement baseline.
type BaselineKind string

// Baseline kinds: none (empty log), estimated (heuristic anchor), usage
// (provider-verified anchor).
const (
	BaselineNone      BaselineKind = "none"
	BaselineEstimated BaselineKind = "estimated"
	BaselineUsage     BaselineKind = "usage"
)

// Baseline is the anchor from which a signed surface delta produces current
// pressure.
type Baseline struct {
	// Kind discriminates none/estimated/usage.
	Kind BaselineKind `json:"kind"`
	// Tokens is the anchor total (0 for none).
	Tokens int64 `json:"tokens"`
	// Usage is the provider usage behind a usage anchor.
	Usage *llm.TokenUsage `json:"usage,omitempty"`
}

// TokenSurfaceNode is one token-priced node in the current ordered session
// surface.
type TokenSurfaceNode struct {
	// Seq is the durable sequence number of the surface event.
	Seq int64 `json:"seq"`
	// Tokens is the request-pressure price for the exact message projected
	// by this node under the measured route.
	Tokens int64 `json:"tokens"`
	// HeuristicTokens is the fixed-heuristic price for the same message,
	// independent of any route. The shadow-price protocol prices
	// replacements with this value so the O(1) projection fold stays in
	// agreement with its own appends.
	HeuristicTokens int64 `json:"heuristicTokens"`
}

// Measurement is the detached request-pressure and surface snapshot at one
// consumed log revision.
type Measurement struct {
	// LogRevision is the number of durable events consumed; equal to the
	// next unread event seq.
	LogRevision int64 `json:"logRevision"`
	// Baseline is the provider or heuristic anchor for this measurement.
	Baseline Baseline `json:"baseline"`
	// SurfaceDeltaTokens is the signed repricing of current surface content
	// relative to the baseline anchor.
	SurfaceDeltaTokens int64 `json:"surfaceDeltaTokens"`
	// TotalTokens is the non-negative current request-and-response
	// pressure.
	TotalTokens int64 `json:"totalTokens"`
	// SurfaceTokens is the total route-priced request tokens across the
	// current surface; equals the sum of the node prices.
	SurfaceTokens int64 `json:"surfaceTokens"`
	// Nodes are the current surface nodes in positional head-to-tail order.
	Nodes []TokenSurfaceNode `json:"nodes"`
}

// usageTokens sums disjoint provider usage buckets without double-counting
// reasoning output.
func usageTokens(usage llm.TokenUsage) int64 {
	total := usage.InputTokens + usage.OutputTokens
	if usage.CacheReadTokens != nil {
		total += *usage.CacheReadTokens
	}
	if usage.CacheWriteTokens != nil {
		total += *usage.CacheWriteTokens
	}
	return total
}

// maxInt64 clamps the non-negative pressure at the safe integer ceiling.
func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	if value > int64(1)<<53-1 {
		return int64(math.MaxInt64)
	}
	return value
}

// optionalHeaderEquals compares optional envelopes so a headerless estimate
// can track later surface deltas.
func optionalHeaderEquals(left, right *session.EpochHeader) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return session.HeaderEquals(*left, *right)
}
