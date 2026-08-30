package toolresultpruner

import (
	"errors"
	"fmt"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
)

// PrunedEntry cites the source event and size accounting for one landed
// surface replacement.
type PrunedEntry struct {
	// OriginalSeq is the full-fidelity tool-result event shadowed by the
	// replacement.
	OriginalSeq int64 `json:"originalSeq"`
	// ReplacementSeq is the newly appended pruned tool-result event.
	ReplacementSeq int64 `json:"replacementSeq"`
	// CallID is the tool call shared by the original and replacement.
	CallID string `json:"callId"`
	// CharsBefore is the original nested-content size in Unicode code
	// points.
	CharsBefore int `json:"charsBefore"`
	// CharsAfter is the replacement nested-content size in Unicode code
	// points.
	CharsAfter int `json:"charsAfter"`
}

// PruneResult is the aggregate outcome of one stable-surface pruning pass.
type PruneResult struct {
	// Pruned lists the replacements in the snapshotted surface order.
	Pruned []PrunedEntry `json:"pruned"`
	// CharsRemoved is the total Unicode code points removed across
	// replacements.
	CharsRemoved int `json:"charsRemoved"`
}

// Pruner is the deterministic head/middle/tail pruning service for current
// tool-result surface nodes. Pricing rides the same package-level fixed
// estimator the token meter's fold uses, so no service dependency is
// declared.
type Pruner struct {
	config ResolvedConfig
}

// New builds a pruner over an already resolved configuration.
func New(config ResolvedConfig) *Pruner {
	return &Pruner{config: config}
}

// Config exposes the resolved, immutable character budgets.
func (p *Pruner) Config() ResolvedConfig {
	return p.config
}

// MeasureContent measures text content in Unicode code points; non-text
// blocks cost zero.
func (p *Pruner) MeasureContent(blocks []llm.ContentBlock) int {
	chars := 0
	for _, block := range blocks {
		if block.Type == llm.BlockText {
			chars += CodePointLength(block.Text)
		}
	}
	return chars
}

// PruneContent replaces an over-budget text middle while retaining
// rich-block order. Text slicing is by Unicode code point, not UTF-16 code
// unit, so a retained boundary cannot split a surrogate pair; grapheme
// clusters may still split. Returns nil when the text is within budget.
func (p *Pruner) PruneContent(blocks []llm.ContentBlock) ([]llm.ContentBlock, error) {
	totalChars := p.MeasureContent(blocks)
	if totalChars <= p.config.ThresholdChars {
		return nil, nil
	}

	removedStart := p.config.HeadChars
	removedEnd := totalChars - p.config.TailChars
	pruned := make([]llm.ContentBlock, 0, len(blocks))
	consumed := 0
	markerInserted := false

	for _, block := range blocks {
		if block.Type != llm.BlockText {
			pruned = append(pruned, block)
			continue
		}

		points := []rune(block.Text)
		blockStart := consumed
		blockEnd := blockStart + len(points)
		headEnd := min(len(points), max(0, removedStart-blockStart))
		tailStart := min(len(points), max(0, removedEnd-blockStart))
		intersectsRemoved := blockStart < removedEnd && blockEnd > removedStart
		marker := ""
		if intersectsRemoved && !markerInserted {
			marker = PruneMarker
			markerInserted = true
		}
		text := string(points[:headEnd]) + marker + string(points[tailStart:])
		if len(text) > 0 {
			retained := block
			retained.Text = text
			pruned = append(pruned, retained)
		}
		consumed = blockEnd
	}

	if !markerInserted {
		return nil, errors.New("tool-result prune: failed to locate the removed text span")
	}
	charsAfter := p.MeasureContent(pruned)
	if charsAfter > p.config.ThresholdChars || charsAfter >= totalChars {
		return nil, errors.New("tool-result prune: replacement must be smaller and within threshold")
	}
	return pruned, nil
}

// PruneSession prunes every over-budget tool result from one stable
// current-surface snapshot. Each replacement preserves the complete event
// data except for the nested content, cites the shadowed node, and is
// immediately preceded by a compaction/prune shadow-price event pricing
// the shadowed node, so pure consumers can subtract it without per-node
// state. Earlier replacements stay durable when the session rejects a
// later one; the error names the failure.
func (p *Pruner) PruneSession(sess *session.Session) (PruneResult, error) {
	// Snapshot the surface once: replacements land as new tool-result
	// nodes and must not requalify inside this pass.
	bySeq := make(map[int64]session.Event)
	for _, event := range sess.Events() {
		bySeq[event.Seq] = event
	}
	var candidates []session.Event
	for _, seq := range sess.Surface().Nodes() {
		if event, ok := bySeq[seq]; ok && event.Type == session.EventToolResult {
			candidates = append(candidates, event)
		}
	}

	result := PruneResult{}
	for _, event := range candidates {
		data, err := session.DecodeToolResult(event)
		if err != nil {
			return result, fmt.Errorf("tool-result prune: decode shadowed event at seq %d: %w", event.Seq, err)
		}
		if len(data.Message.Content) == 0 {
			continue
		}
		block := data.Message.Content[0]
		content, err := p.PruneContent(block.Content)
		if err != nil {
			return result, fmt.Errorf("tool-result prune: event at seq %d: %w", event.Seq, err)
		}
		if content == nil {
			continue
		}
		charsBefore := p.MeasureContent(block.Content)
		charsAfter := p.MeasureContent(content)

		// Shadow-price protocol: the metering event and its replacement
		// are appended synchronously adjacent, so pure consumers subtract
		// the shadowed node's heuristic price without retaining per-node
		// state.
		if _, err := sess.Append(compaction.EventCompactionPrune, compaction.PrunePayload{
			ShadowedRange:      compaction.SeqRange{Start: event.Seq, End: event.Seq},
			ShadowedSeqs:       []int64{event.Seq},
			ShadowedTokenCount: tokenmeter.EstimateMessage(data.Message),
		}, nil); err != nil {
			return result, fmt.Errorf("tool-result prune: shadow price at seq %d: %w", event.Seq, err)
		}

		block.Content = content
		data.Message.Content = []llm.ContentBlock{block}
		replacement, err := sess.Append(session.EventToolResult, data, &session.SurfaceIntent{
			SurfaceOp: session.SurfaceOp{
				Kind:  session.SurfaceReplace,
				Start: event.Seq,
				End:   event.Seq,
			},
			SourceEventSeqs:   []int64{event.Seq},
			SourceSeqsPresent: true,
		})
		if err != nil {
			return result, fmt.Errorf("tool-result prune: replacement for seq %d: %w", event.Seq, err)
		}
		result.Pruned = append(result.Pruned, PrunedEntry{
			OriginalSeq:    event.Seq,
			ReplacementSeq: replacement.Seq,
			CallID:         data.Message.Source.CallID,
			CharsBefore:    charsBefore,
			CharsAfter:     charsAfter,
		})
		result.CharsRemoved += charsBefore - charsAfter
	}
	return result, nil
}
