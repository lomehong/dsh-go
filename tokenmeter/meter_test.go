package tokenmeter

import (
	"strings"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

// liveConversation builds one detached session carrying a canonical request
// header, one completed step (user + assistant with usage), and returns the
// matching request header for measure().
func liveConversation(t *testing.T, usage *llm.TokenUsage, chunkSeqs []int64) (*session.Session, *session.EpochHeader) {
	t.Helper()
	sess := newDetached(t)
	header := session.EpochHeader{
		Config: llm.LlmCallConfig{Provider: "deepseek", Model: "chat"},
		System: "abcd",
	}
	appendEvent(t, sess, session.EventRequestHeader, session.RequestHeaderData{Header: header, Reason: session.HeaderReasonInitial}, nil)
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 1}, nil)
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, llm.MessageSource{}), appendIntent())
	if chunkSeqs != nil {
		for _, seq := range chunkSeqs {
			appendEvent(t, sess, session.EventAssistantChunk, map[string]any{
				"turn": 1, "step": 1,
				"chunk": map[string]any{"type": "text-delta", "text": "hi"},
			}, nil)
			_ = seq
		}
	}
	intent := &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
	if chunkSeqs != nil {
		intent.SourceEventSeqs = chunkSeqs
		intent.SourceSeqsPresent = true
	}
	appendEvent(t, sess, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, "deepseek", "chat", nil),
		Usage:   usage,
	}, intent)
	appendEvent(t, sess, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}, nil)
	return sess, &header
}

func TestMeterUsageAnchoredMeasurement(t *testing.T) {
	usage := llm.TokenUsage{InputTokens: 20, CacheReadTokens: intPtr(5), OutputTokens: 5}
	sess, header := liveConversation(t, &usage, nil)
	meter := NewMeter(nil)
	measurement, err := meter.Measure(sess, header)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	// Anchor: step-start snapshot is empty, assistant durable price 9 →
	// anchor surface 9; header estimate 5+4=9; estimated anchor 14 < usage
	// total 30 → usage baseline.
	if measurement.Baseline.Kind != BaselineUsage || measurement.Baseline.Tokens != 30 || measurement.Baseline.Usage == nil {
		t.Fatalf("usage baseline wrong: %#v", measurement.Baseline)
	}
	// Surface = user 9 + assistant 9; delta vs anchored 9 = 9.
	if measurement.SurfaceTokens != 18 || measurement.SurfaceDeltaTokens != 9 {
		t.Fatalf("surface accounting wrong: %#v", measurement)
	}
	if measurement.TotalTokens != 39 {
		t.Fatalf("total wrong: %d", measurement.TotalTokens)
	}
	if measurement.LogRevision != int64(len(sess.Events())) {
		t.Fatalf("revision wrong: %d vs %d events", measurement.LogRevision, len(sess.Events()))
	}
	if len(measurement.Nodes) != 2 {
		t.Fatalf("nodes wrong: %#v", measurement.Nodes)
	}
	// The anchored measurement reuses the fold without re-reading.
	again, err := meter.Measure(sess, header)
	if err != nil || again.TotalTokens != measurement.TotalTokens {
		t.Fatalf("repeat measure must be stable: %#v %v", again, err)
	}
}

func TestMeterEstimatedBaselineWhenUsageBelowEstimate(t *testing.T) {
	usage := llm.TokenUsage{InputTokens: 1, OutputTokens: 1}
	sess, header := liveConversation(t, &usage, nil)
	meter := NewMeter(nil)
	measurement, err := meter.Measure(sess, header)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	// 2 < estimated anchor 14 → estimated baseline; delta 9 keeps total at
	// estimate + delta.
	if measurement.Baseline.Kind != BaselineEstimated || measurement.Baseline.Tokens != 14 {
		t.Fatalf("estimated baseline wrong: %#v", measurement.Baseline)
	}
	if measurement.TotalTokens != 23 {
		t.Fatalf("total wrong: %d", measurement.TotalTokens)
	}
}

func TestMeterEstimatedBaselineWithoutHeader(t *testing.T) {
	sess := newDetached(t)
	appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, llm.MessageSource{}), appendIntent())
	meter := NewMeter(nil)
	measurement, err := meter.Measure(sess, nil)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	if measurement.Baseline.Kind != BaselineEstimated || measurement.Baseline.Tokens != 9 {
		t.Fatalf("headerless baseline wrong: %#v", measurement.Baseline)
	}
	if measurement.TotalTokens != 9 || measurement.SurfaceTokens != 9 {
		t.Fatalf("headerless totals wrong: %#v", measurement)
	}
}

func TestMeterNoneBaselineOnEmptyLog(t *testing.T) {
	sess := newDetached(t)
	meter := NewMeter(nil)
	measurement, err := meter.Measure(sess, nil)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	if measurement.Baseline.Kind != BaselineNone || measurement.TotalTokens != 0 || len(measurement.Nodes) != 0 {
		t.Fatalf("empty log measurement wrong: %#v", measurement)
	}
}

func TestMeterHeaderlessEstimateTracksSurfaceDeltas(t *testing.T) {
	// No logged header and none supplied: the anchored branch is skipped
	// because the anchor carries no header, so the full estimate reprices.
	sess := newDetached(t)
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 1}, nil)
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, llm.MessageSource{}), appendIntent())
	usage := llm.TokenUsage{InputTokens: 50, OutputTokens: 5}
	appendEvent(t, sess, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcd"}}, "deepseek", "chat", nil),
		Usage:   &usage,
	}, appendIntent())
	appendEvent(t, sess, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}, nil)
	meter := NewMeter(nil)
	measurement, err := meter.Measure(sess, nil)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	// Without a logged header the usage is not anchored (the usage branch
	// requires a known header), so the anchor prices the durable assistant
	// output only: anchor 0+9, both-nil headers match → estimated baseline.
	if measurement.Baseline.Kind != BaselineEstimated || measurement.Baseline.Tokens != 9 {
		t.Fatalf("headerless anchor wrong: %#v", measurement.Baseline)
	}
	if measurement.SurfaceTokens != 18 || measurement.SurfaceDeltaTokens != 9 || measurement.TotalTokens != 18 {
		t.Fatalf("measurement wrong: %#v", measurement)
	}
}

func TestMeterRoutePricingReplacesImages(t *testing.T) {
	sess := newDetached(t)
	header := session.EpochHeader{Config: llm.LlmCallConfig{Provider: "vision", Model: "vl"}}
	appendEvent(t, sess, session.EventRequestHeader, session.RequestHeaderData{Header: header, Reason: session.HeaderReasonInitial}, nil)
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 1}, nil)
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{
		{Type: llm.BlockText, Text: "abcd"},
		{Type: llm.BlockImage, Attachment: "att-1"},
	}, llm.MessageSource{}), appendIntent())
	appendEvent(t, sess, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}, nil)

	structural := EstimateStructuralBlock(llm.ContentBlock{Type: llm.BlockImage, Attachment: "att-1"})
	calls := 0
	meter := NewMeter(func(provider string, model string) ImageRequestPricing {
		if provider != "vision" || model != "vl" {
			t.Fatalf("route wrong: %s/%s", provider, model)
		}
		calls++
		return ImageRequestPricingFunc(func(images []any) []ImagePrice {
			if len(images) != 1 || images[0] != "att-1" {
				t.Fatalf("occurrences wrong: %#v", images)
			}
			return []ImagePrice{{VisualTokens: 500, Text: "pic"}}
		})
	})
	measurement, err := meter.Measure(sess, &header)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	if calls == 0 {
		t.Fatal("route pricing must be consulted")
	}
	// Node: image-free (1+4+4) + 500 + ceil(3/4)+4.
	wantNode := int64(9) + 500 + 5
	if measurement.Nodes[0].Tokens != wantNode || measurement.Nodes[0].HeuristicTokens != 9+structural {
		t.Fatalf("route-priced node wrong: %#v (structural %d)", measurement.Nodes[0], structural)
	}
	if measurement.SurfaceTokens != wantNode {
		t.Fatalf("surface total wrong: %d", measurement.SurfaceTokens)
	}
}

func TestMeterMalformedEventFailsLoudAndStaysUnread(t *testing.T) {
	sess, header := liveConversation(t, nil, nil)
	// An orphan step/end after the completed turn breaks the fold.
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 2, Step: 1}, nil)
	appendEvent(t, sess, session.EventStepEnd, session.StepEndData{Turn: 9, Step: 9}, nil)
	meter := NewMeter(nil)
	for attempt := 0; attempt < 2; attempt++ {
		_, err := meter.Measure(sess, header)
		if err == nil || !strings.Contains(err.Error(), "no matching step/start") {
			t.Fatalf("attempt %d: malformed tail must fail identically, got %v", attempt, err)
		}
	}
	// A healthy meter over the same log prefix still measures.
	prefix := sessionMustPrefix(t, sess, 8)
	healthy := NewMeter(nil)
	measurement, err := healthy.Measure(prefix, header)
	if err != nil {
		t.Fatalf("prefix measure failed: %v", err)
	}
	if measurement.Baseline.Kind != BaselineEstimated || measurement.Baseline.Tokens != 14 {
		t.Fatalf("prefix measurement wrong: %#v", measurement.Baseline)
	}
}

func TestMeterAssistantMessageWithoutStepStartFails(t *testing.T) {
	sess := newDetached(t)
	appendEvent(t, sess, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "x"}}, "deepseek", "chat", nil),
	}, appendIntent())
	meter := NewMeter(nil)
	if _, err := meter.Measure(sess, nil); err == nil || !strings.Contains(err.Error(), "no matching step/start") {
		t.Fatalf("orphan assistant/message must fail loud, got %v", err)
	}
}

func TestMeterRepricesAfterNewTurns(t *testing.T) {
	usage := llm.TokenUsage{InputTokens: 20, OutputTokens: 5}
	sess, header := liveConversation(t, &usage, nil)
	meter := NewMeter(nil)
	first, err := meter.Measure(sess, header)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	// Continue the conversation: a second turn appends another node.
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 2}, nil)
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 2, Step: 1}, nil)
	appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "abcdefgh"}}, llm.MessageSource{}), appendIntent())
	second, err := meter.Measure(sess, header)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	if second.LogRevision != first.LogRevision+3 {
		t.Fatalf("revision must track consumption: %d vs %d", second.LogRevision, first.LogRevision)
	}
	// Baseline stays anchored; the delta grows by the appended node (2+4+4)
	// on top of the standing delta 9.
	if second.Baseline.Kind != BaselineUsage || second.SurfaceDeltaTokens != 19 {
		t.Fatalf("delta accounting wrong: %#v", second)
	}
	if second.TotalTokens != 25+19 {
		t.Fatalf("total wrong: %d", second.TotalTokens)
	}
}

func sessionMustPrefix(t *testing.T, sess *session.Session, count int) *session.Session {
	t.Helper()
	events := sess.Events()[:count]
	prefixed, err := session.NewDetached("prefix", append([]session.Event{}, events...), &session.SessionHeader{ID: "prefix", CWD: "D:\\tmp"}, 0)
	if err != nil {
		t.Fatalf("prefix construct failed: %v", err)
	}
	return prefixed
}

func intPtr(value int64) *int64 {
	return &value
}
