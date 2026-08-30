package compactionbasic

import (
	"context"
	"strings"
	"testing"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
	"dshgo/toolresultpruner"
)

// staticModelInfo resolves one fixed capacity for every target.
type staticModelInfo struct {
	window int64
}

// testPruner adapts the concrete pruner's result-bearing method onto the
// engine's error-only face.
type testPruner struct{ pruner *toolresultpruner.Pruner }

func (p testPruner) PruneSession(sess *session.Session) error {
	_, err := p.pruner.PruneSession(sess)
	return err
}

func (m staticModelInfo) ResolveModelInfo(provider string, model string) (llm.LlmResolvedModelInfo, error) {
	return llm.LlmResolvedModelInfo{
		LlmModelInfo: llm.LlmModelInfo{Provider: provider, ID: model, Name: model},
		Context:      &llm.LlmModelContext{ContextWindow: m.window},
	}, nil
}

// TestCompactIfNeededPrunesBeforePressureVerdict drives the pressure
// trigger over a session whose only bulk is one oversized tool result: the
// model-free prune lands first (shadow-price event plus replacement), the
// remeasure drops below the pressure threshold, and the summary is skipped
// entirely.
func TestCompactIfNeededPrunesBeforePressureVerdict(t *testing.T) {
	pruner := toolresultpruner.New(toolresultpruner.ResolvedConfig{ThresholdChars: 100, HeadChars: 40, TailChars: 10})
	engine, err := NewEngine(BasicConfig{}, EngineConfig{
		Meter:     tokenmeter.NewMeter(nil),
		ModelInfo: staticModelInfo{window: 500},
		Pruner:    testPruner{pruner},
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	sess := newTestSession(t, "prune-pressure")
	appendEvent(t, sess, session.EventRequestHeader, session.RequestHeaderData{
		Header: testHeader("deepseek", "chat"), Reason: session.HeaderReasonInitial,
	}, nil)
	appendEvent(t, sess, session.EventUserMessage,
		llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "task"}}, llm.MessageSource{}), appendIntent())
	// 4000 characters price far above the 400-token threshold of a 500-token
	// window (thresholdRatio 0.8).
	appendEvent(t, sess, session.EventToolResult, session.ToolResultData{
		Turn: 1, Step: 1,
		Message: llm.NewToolResultMessage("call-1", []llm.ContentBlock{{Type: llm.BlockText, Text: strings.Repeat("x", 4000)}}, false),
	}, appendIntent())

	result, err := engine.CompactIfNeeded(staticRegionView{sess: sess}, TriggerPressure, context.Background())
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if result != nil {
		t.Fatalf("pruning must clear pressure and skip the summary: %+v", result)
	}

	// The prune pass landed: one shadow-price event with its adjacent
	// replacement, both durable.
	prunes := 0
	var pruneSeq int64
	for _, event := range sess.Events() {
		if event.Type != compaction.EventCompactionPrune {
			continue
		}
		prunes++
		pruneSeq = event.Seq
	}
	if prunes != 1 {
		t.Fatalf("prune events: %d", prunes)
	}
	nodes := sess.Surface().Nodes()
	replacementSeq := nodes[len(nodes)-1]
	if replacementSeq-1 != pruneSeq {
		t.Fatalf("replacement %d not adjacent to the metering event %d", replacementSeq, pruneSeq)
	}

	// The replacement is far smaller: the model-visible surface no longer
	// carries the 4000-character body.
	measurement, err := tokenmeter.NewMeter(nil).Measure(sess, nil)
	if err != nil {
		t.Fatalf("remeasure: %v", err)
	}
	if measurement.TotalTokens >= 400 {
		t.Fatalf("pruning must clear pressure: %+v", measurement)
	}
}
