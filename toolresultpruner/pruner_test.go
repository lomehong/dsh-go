package toolresultpruner

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
)

func resolvedConfig(threshold, head, tail int) ResolvedConfig {
	return ResolvedConfig{ThresholdChars: threshold, HeadChars: head, TailChars: tail}
}

func textBlock(text string) llm.ContentBlock {
	return llm.ContentBlock{Type: llm.BlockText, Text: text}
}

func TestResolveConfigDefaultsAndBudgets(t *testing.T) {
	resolved, err := ResolveConfig(Config{})
	if err != nil {
		t.Fatalf("defaults resolve: %v", err)
	}
	if resolved != Defaults {
		t.Fatalf("defaults: %+v", resolved)
	}

	// The emitted replacement must fit the threshold.
	_, err = ResolveConfig(Config{
		ThresholdChars: intPtr(100), HeadChars: intPtr(80), TailChars: intPtr(10),
	})
	if err == nil || !strings.Contains(err.Error(), "must be at most thresholdChars") {
		t.Fatalf("budget violation: %v", err)
	}

	// Negative budgets fail loud.
	if _, err := ResolveConfig(Config{HeadChars: intPtr(-1)}); err == nil {
		t.Fatal("negative headChars must fail")
	}
	if _, err := ResolveConfig(Config{TailChars: intPtr(-1)}); err == nil {
		t.Fatal("negative tailChars must fail")
	}
	if _, err := ResolveConfig(Config{ThresholdChars: intPtr(0)}); err == nil {
		t.Fatal("zero thresholdChars must fail")
	}

	// Unknown configuration keys fail loud at decode.
	if _, err := DecodeConfig(map[string]any{"thresholdChars": 10, "bogus": 1}); err == nil {
		t.Fatal("unknown key must fail decode")
	}
}

func intPtr(value int) *int { return &value }

func TestMeasureContentCountsCodePoints(t *testing.T) {
	pruner := New(resolvedConfig(8192, 4096, 1024))
	// Multibyte runes count once each; non-text blocks cost zero.
	blocks := []llm.ContentBlock{
		textBlock("héllo🎉"), // 6 code points
		{Type: llm.BlockToolResult, Content: []llm.ContentBlock{textBlock("nested")}},
		textBlock("ab"), // 2
	}
	if got := pruner.MeasureContent(blocks); got != 8 {
		t.Fatalf("code point measure: %d", got)
	}
}

func TestPruneContentWithinBudgetReturnsNil(t *testing.T) {
	pruner := New(resolvedConfig(100, 40, 10))
	content, err := pruner.PruneContent([]llm.ContentBlock{textBlock(strings.Repeat("a", 100))})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if content != nil {
		t.Fatalf("within budget must return nil: %+v", content)
	}
}

func TestPruneContentHeadMarkerTail(t *testing.T) {
	pruner := New(resolvedConfig(100, 40, 10))
	head := strings.Repeat("h", 40)
	tail := strings.Repeat("t", 10)
	middle := strings.Repeat("m", 60)
	content, err := pruner.PruneContent([]llm.ContentBlock{textBlock(head + middle + tail)})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(content) != 1 {
		t.Fatalf("blocks: %+v", content)
	}
	want := head + PruneMarker + tail
	if content[0].Text != want {
		t.Fatalf("pruned text mismatch: %q", content[0].Text)
	}
	if got := pruner.MeasureContent(content); got > 100 || got >= 110 {
		t.Fatalf("replacement size: %d", got)
	}
}

func TestPruneContentMarkerOnceAcrossBlocksKeepsRichBlocks(t *testing.T) {
	pruner := New(resolvedConfig(100, 10, 10))
	rich := llm.ContentBlock{Type: llm.BlockToolResult, ToolCallID: "call-1"}
	blocks := []llm.ContentBlock{
		textBlock(strings.Repeat("a", 8)),
		textBlock(strings.Repeat("b", 50)),
		rich,
		textBlock(strings.Repeat("c", 50)),
		textBlock(strings.Repeat("d", 8)),
	}
	content, err := pruner.PruneContent(blocks)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	markers := strings.Count(printBlocks(content), PruneMarker)
	if markers != 1 {
		t.Fatalf("marker count: %d", markers)
	}
	// The rich block survives in order.
	var richSeen bool
	for index, block := range content {
		if block.Type == llm.BlockToolResult {
			richSeen = true
			if index < 2 {
				t.Fatalf("rich block moved early: %+v", content)
			}
		}
	}
	if !richSeen {
		t.Fatal("rich block must be retained")
	}
	// Fully-consumed head blocks produce no empty text blocks.
	for _, block := range content {
		if block.Type == llm.BlockText && block.Text == "" {
			t.Fatalf("empty text block retained: %+v", content)
		}
	}
}

func printBlocks(blocks []llm.ContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		builder.WriteString(block.Text)
	}
	return builder.String()
}

func TestPruneContentSplitsOnCodePointsNotBytes(t *testing.T) {
	// marker is 39 code points; 3 + 39 + 2 = 44 <= 50.
	pruner := New(resolvedConfig(50, 3, 2))
	// 60 multibyte runes; head 3 + tail 2 keeps 5 intact runes plus marker.
	text := strings.Repeat("🎉", 60)
	content, err := pruner.PruneContent([]llm.ContentBlock{textBlock(text)})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	want := strings.Repeat("🎉", 3) + PruneMarker + strings.Repeat("🎉", 2)
	if content[0].Text != want {
		t.Fatalf("code point slicing: %q", content[0].Text)
	}
}

// newSession builds a detached probe session.
func newSession(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewDetached("prune-probe", nil, &session.SessionHeader{ID: "prune-probe", CWD: "D:\\tmp"}, 0)
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	return sess
}

func appendEvent(t *testing.T, sess *session.Session, eventType string, data any, intent *session.SurfaceIntent) session.Event {
	t.Helper()
	event, err := sess.Append(eventType, data, intent)
	if err != nil {
		t.Fatalf("append %s failed: %v", eventType, err)
	}
	return event
}

func appendSurface(t *testing.T, sess *session.Session, eventType string, data any) session.Event {
	return appendEvent(t, sess, eventType, data, &session.SurfaceIntent{
		SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend},
	})
}

func toolResultEvent(t *testing.T, sess *session.Session, callID string, text string, isError bool) session.Event {
	message := llm.NewToolResultMessage(callID, []llm.ContentBlock{textBlock(text)}, isError)
	return appendSurface(t, sess, session.EventToolResult, session.ToolResultData{
		Turn: 1, Step: 1, Message: message,
	})
}

func smallPruner() *Pruner {
	// marker is 39 code points; 40 + 39 + 10 = 89 <= 100.
	return New(resolvedConfig(100, 40, 10))
}

func TestPruneSessionRewritesSurfaceAndLogsShadowPrice(t *testing.T) {
	pruner := smallPruner()
	sess := newSession(t)
	appendSurface(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{textBlock("task")}, llm.MessageSource{}))
	big := toolResultEvent(t, sess, "call-big", strings.Repeat("x", 120), false)
	small := toolResultEvent(t, sess, "call-small", "tiny", false)

	result, err := pruner.PruneSession(sess)
	if err != nil {
		t.Fatalf("prune session: %v", err)
	}
	if len(result.Pruned) != 1 {
		t.Fatalf("pruned entries: %+v", result.Pruned)
	}
	entry := result.Pruned[0]
	if entry.OriginalSeq != big.Seq || entry.CallID != "call-big" {
		t.Fatalf("entry: %+v", entry)
	}
	if entry.CharsBefore != 120 || entry.CharsAfter >= entry.CharsBefore {
		t.Fatalf("size accounting: %+v", entry)
	}
	if result.CharsRemoved != entry.CharsBefore-entry.CharsAfter {
		t.Fatalf("aggregate: %+v", result)
	}

	// The replacement replaces the original node on the surface.
	nodes := sess.Surface().Nodes()
	if len(nodes) != 3 {
		t.Fatalf("surface nodes: %+v", nodes)
	}
	if nodes[1] != entry.ReplacementSeq {
		t.Fatalf("replacement not in place: %+v", nodes)
	}

	// The durable pair: compaction/prune immediately before the cited
	// replacement.
	events := sess.Events()
	if got := events[entry.ReplacementSeq-1]; got.Type != compaction.EventCompactionPrune {
		t.Fatalf("adjacent metering event: %+v", got)
	}
	var prunePayload compaction.PrunePayload
	if err := jsonUnmarshalInto(events[entry.ReplacementSeq-1].Data, &prunePayload); err != nil {
		t.Fatalf("decode prune payload: %v", err)
	}
	if prunePayload.ShadowedRange.Start != big.Seq || prunePayload.ShadowedRange.End != big.Seq ||
		len(prunePayload.ShadowedSeqs) != 1 || prunePayload.ShadowedSeqs[0] != big.Seq {
		t.Fatalf("shadow price: %+v", prunePayload)
	}

	// The logged shadow price is the original message's heuristic estimate.
	var originalData session.ToolResultData
	if err := jsonUnmarshalInto(big.Data, &originalData); err != nil {
		t.Fatalf("decode original: %v", err)
	}
	if prunePayload.ShadowedTokenCount != tokenmeter.EstimateMessage(originalData.Message) {
		t.Fatalf("shadow price tokens: %+v", prunePayload)
	}

	// The replacement keeps the message identity and error flag, swapping
	// only the nested content.
	var replacementData session.ToolResultData
	if err := jsonUnmarshalInto(events[entry.ReplacementSeq].Data, &replacementData); err != nil {
		t.Fatalf("decode replacement: %v", err)
	}
	if replacementData.Message.ID != originalData.Message.ID {
		t.Fatalf("message id must be preserved: %s vs %s", replacementData.Message.ID, originalData.Message.ID)
	}
	if len(replacementData.Message.Content) != 1 || len(replacementData.Message.Content[0].Content) != 1 {
		t.Fatalf("replacement content: %+v", replacementData.Message.Content)
	}

	// The small result is untouched.
	var smallData session.ToolResultData
	if err := jsonUnmarshalInto(events[small.Seq].Data, &smallData); err != nil {
		t.Fatalf("decode small: %v", err)
	}
	if smallData.Message.Content[0].Content[0].Text != "tiny" {
		t.Fatalf("small result mutated: %+v", smallData)
	}
}

func TestPrunedPairFoldsThroughTokenMeterShadowPrice(t *testing.T) {
	pruner := smallPruner()
	sess := newSession(t)
	appendSurface(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{textBlock("task")}, llm.MessageSource{}))
	toolResultEvent(t, sess, "call-big", strings.Repeat("x", 120), false)
	if _, err := pruner.PruneSession(sess); err != nil {
		t.Fatalf("prune session: %v", err)
	}

	// Replay the metering event then its replacement through the pure
	// shadow-price fold: the prune arms the claim, the adjacent tool/result
	// replace consumes it and prices the replacement text.
	events := sess.Events()
	claimState, err := tokenmeter.FoldSurfaceTokens(nil, events[2])
	if err != nil {
		t.Fatalf("prune event fold: %v", err)
	}
	if claimState.Claim == nil {
		t.Fatalf("prune event must arm a claim: %+v", claimState)
	}
	consumed, err := tokenmeter.FoldSurfaceTokens(claimState.Claim, events[3])
	if err != nil {
		t.Fatalf("replacement fold: %v", err)
	}
	if consumed.Claim != nil {
		t.Fatalf("claim must be consumed: %+v", consumed)
	}
	// The delta is the replacement's own price minus the logged shadow
	// price; the replacement text is far smaller than the shadowed 120.
	if consumed.DeltaTokens >= 0 {
		t.Fatalf("pruned replacement must shrink the surface: %+v", consumed)
	}
}

func jsonUnmarshalInto(raw []byte, target any) error {
	return json.Unmarshal(raw, target)
}
