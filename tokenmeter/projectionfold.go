package tokenmeter

import (
	"fmt"

	"dshgo/compaction"
	"dshgo/session"
)

// This file ports @deepseek-ai/dsh-token-meter/surface-projection: the O(1)
// surface-token fold shared by the token-meter projection units.
//
// A projection state must stay bounded — the persisted projection cache
// checkpoints every unit's whole state, so carrying the priced surface (one
// node per model-visible message) would grow a checkpoint without bound
// over the session's life. Instead, replacements ride the compact seam's
// shadow-price protocol: the metering event immediately before a surface
// replace (`compaction/summary` or `compaction/prune`) states the heuristic
// price of the exact replaced range, so the fold keeps a running total plus
// at most one pending claim and never retains per-node prices. The counts
// are exact by construction: producers derive them from the same fixed
// estimator this fold prices appends with. A replacement without an armed
// claim folds with zero delta because bounded state cannot reconstruct the
// replaced range; this preserves replay at the cost of possible drift.

// ShadowPriceClaim is one armed shadow price: the heuristic tokens of the
// surface range the IMMEDIATELY following event replaces. Plain JSON — it
// is part of the persisted unit state while armed.
type ShadowPriceClaim struct {
	// Start is the declared inclusive first surface-node seq of the priced
	// range.
	Start int64 `json:"start"`
	// End is the declared inclusive last surface-node seq of the priced
	// range.
	End int64 `json:"end"`
	// Tokens is the heuristic price of the priced range under the fixed
	// estimator.
	Tokens int64 `json:"tokens"`
}

// SurfaceTokensFold is one event's effect on a running surface-token total.
type SurfaceTokensFold struct {
	// DeltaTokens is the signed change in the surface total; 0 for events
	// off the surface.
	DeltaTokens int64
	// Claim is the claim to carry into the next event; nil when none
	// survives.
	Claim *ShadowPriceClaim
}

// FoldSurfaceTokens folds one committed event onto a running surface-token
// total.
//
// A shadow-price event arms a claim; any other event expires it, and a
// surface replace consumes the claim naming its exact range — the producers
// append the metering event and the replacement synchronously adjacent, so
// a surviving claim always prices the very next event. A replace with no
// claim folds with zero delta because the bounded state cannot reconstruct
// the replaced range.
//
// A replacement carrying an armed claim for a different range is a live
// producer's shadow-price contract violation, not historical data: the
// returned error fails loud (projection units surface it by panicking,
// their only failure channel) rather than letting the total drift.
func FoldSurfaceTokens(claim *ShadowPriceClaim, event session.Event) (SurfaceTokensFold, error) {
	switch event.Type {
	case compaction.EventCompactionSummary:
		var data compaction.SummaryPayload
		if err := decodeEventPayload(event, &data); err != nil {
			return SurfaceTokensFold{}, err
		}
		return SurfaceTokensFold{Claim: &ShadowPriceClaim{
			Start:  data.ShadowedRange.Start,
			End:    data.ShadowedRange.End,
			Tokens: data.ShadowedTokenCount,
		}}, nil
	case compaction.EventCompactionPrune:
		var data compaction.PrunePayload
		if err := decodeEventPayload(event, &data); err != nil {
			return SurfaceTokensFold{}, err
		}
		return SurfaceTokensFold{Claim: &ShadowPriceClaim{
			Start:  data.ShadowedRange.Start,
			End:    data.ShadowedRange.End,
			Tokens: data.ShadowedTokenCount,
		}}, nil
	}
	if !session.IsSurfaceEventType(event.Type) {
		return SurfaceTokensFold{}, nil
	}
	tokens := int64(0)
	if message := session.DeriveEventMessage(event); message != nil {
		tokens = EstimateMessage(*message)
	}
	if event.SurfaceOp == nil || event.SurfaceOp.Kind == session.SurfaceAppend {
		return SurfaceTokensFold{DeltaTokens: tokens}, nil
	}
	// Sessions recorded before the shadow-price protocol log replacements
	// with no adjacent metering event; the bounded state cannot
	// reconstruct the replaced range's price, so fold those neutrally —
	// historical replay degrades to drift instead of failing.
	if claim == nil {
		return SurfaceTokensFold{}, nil
	}
	if claim.Start != event.SurfaceOp.Start || claim.End != event.SurfaceOp.End {
		return SurfaceTokensFold{}, fmt.Errorf(
			"token surface: replace at seq %d over range %d-%d has no adjacent shadow price (armed claim covers %d-%d)",
			event.Seq, event.SurfaceOp.Start, event.SurfaceOp.End, claim.Start, claim.End)
	}
	return SurfaceTokensFold{DeltaTokens: tokens - claim.Tokens}, nil
}

// decodeEventPayload decodes one event's raw payload.
func decodeEventPayload(event session.Event, target any) error {
	if err := jsonUnmarshal(event.Data, target); err != nil {
		return fmt.Errorf("token meter: %s at seq %d payload: %w", event.Type, event.Seq, err)
	}
	return nil
}
