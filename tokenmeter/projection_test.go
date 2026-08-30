package tokenmeter

import (
	"encoding/json"
	"testing"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
)

// rawEvent builds a pure fold fixture (no live session needed).
func rawEvent(seq int64, eventType string, data any) session.Event {
	encoded, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return session.Event{
		Type: eventType,
		Seq:  seq,
		Time: seq,
		Data: encoded,
	}
}

// rawSurfaceEvent builds a pure surface-event fixture.
func rawSurfaceEvent(seq int64, eventType string, data any, op *session.SurfaceOp) session.Event {
	event := rawEvent(seq, eventType, data)
	if op != nil {
		event.SurfaceOp = op
	}
	return event
}

// usageEvent is an assistant/chunk usage sample.
func usageEvent(seq int64, turn, step int64, usage llm.TokenUsage) session.Event {
	return rawEvent(seq, session.EventAssistantChunk, struct {
		Turn  int64           `json:"turn"`
		Step  int64           `json:"step"`
		Chunk llm.StreamChunk `json:"chunk"`
	}{turn, step, llm.StreamChunk{Type: llm.ChunkUsage, Usage: &usage}})
}

// assistantMessageEvent is a finalized assistant/message.
func assistantMessageEvent(seq int64, turn, step int64, usage *llm.TokenUsage, provider, model string) session.Event {
	message := llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "done"}}, provider, model, nil)
	return rawEvent(seq, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: turn, Step: step, Message: message, Usage: usage,
	})
}

// userMessageData builds the user/message payload.
func userMessageData(text string) llm.Message {
	return llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
}

func TestFoldSurfaceTokensArmsAndConsumesClaims(t *testing.T) {
	sess := newDetached(t)
	first := userMessageEvent(t, sess, "abcd")

	// An append with no claim prices the message.
	fold, err := FoldSurfaceTokens(nil, first)
	if err != nil {
		t.Fatalf("append fold: %v", err)
	}
	if fold.DeltaTokens <= 0 || fold.Claim != nil {
		t.Fatalf("append fold: %+v", fold)
	}

	// The metering event arms the claim.
	summary := appendEvent(t, sess, compaction.EventCompactionSummary, compaction.SummaryPayload{
		CompactionID:       "c1",
		ShadowedRange:      compaction.SeqRange{Start: 0, End: 0},
		ShadowedTokenCount: 55,
	}, nil)
	armed, err := FoldSurfaceTokens(nil, summary)
	if err != nil {
		t.Fatalf("summary fold: %v", err)
	}
	if armed.Claim == nil || armed.Claim.Start != 0 || armed.Claim.End != 0 || armed.Claim.Tokens != 55 {
		t.Fatalf("armed claim: %+v", armed)
	}
	if armed.DeltaTokens != 0 {
		t.Fatalf("metering event must not move the total: %+v", armed)
	}

	// The adjacent replacement consumes the claim.
	replacement := rawSurfaceEvent(sess.Seq(), session.EventUserMessage, userMessageData("summary"), &session.SurfaceOp{
		Kind: session.SurfaceReplace, Start: 0, End: 0,
	})
	// The replacement's own price derives from the same estimator.
	replacementTokens := EstimateMessage(userMessageData("summary"))
	consumed, err := FoldSurfaceTokens(armed.Claim, replacement)
	if err != nil {
		t.Fatalf("replace fold: %v", err)
	}
	if consumed.Claim != nil {
		t.Fatalf("claim must expire: %+v", consumed)
	}
	if consumed.DeltaTokens != replacementTokens-55 {
		t.Fatalf("replace delta: %+v (replacement price %d)", consumed, replacementTokens)
	}

	// A non-surface event expires a claim without moving the total.
	stray := appendEvent(t, newDetached(t), session.EventToolCall, session.ToolCallData{
		Turn: 1, Step: 1, Name: "x", Arguments: "{}",
	}, nil)
	expired, err := FoldSurfaceTokens(&ShadowPriceClaim{Start: 9, End: 9, Tokens: 5}, stray)
	if err != nil {
		t.Fatalf("stray fold: %v", err)
	}
	if expired.DeltaTokens != 0 || expired.Claim != nil {
		t.Fatalf("stray fold: %+v", expired)
	}
}

func TestFoldSurfaceTokensReplaceWithoutClaimIsNeutral(t *testing.T) {
	replacement := rawSurfaceEvent(7, session.EventUserMessage, userMessageData("summary"), &session.SurfaceOp{
		Kind: session.SurfaceReplace, Start: 0, End: 3,
	})
	fold, err := FoldSurfaceTokens(nil, replacement)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if fold.DeltaTokens != 0 || fold.Claim != nil {
		t.Fatalf("unclaimed replace must fold neutrally: %+v", fold)
	}
}

func TestFoldSurfaceTokensMismatchedClaimFailsLoud(t *testing.T) {
	replacement := rawSurfaceEvent(7, session.EventUserMessage, userMessageData("summary"), &session.SurfaceOp{
		Kind: session.SurfaceReplace, Start: 0, End: 3,
	})
	_, err := FoldSurfaceTokens(&ShadowPriceClaim{Start: 0, End: 4, Tokens: 55}, replacement)
	if err == nil {
		t.Fatal("mismatched claim must fail loud")
	}
	const want = "has no adjacent shadow price"
	if err.Error()[:len(want)] != want && !contains(err.Error(), want) {
		t.Fatalf("error text: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func TestTokenUsageUnitFoldsAndReplaces(t *testing.T) {
	unit := TokenUsageUnit()
	state := unit.Init(session.SessionHeader{})

	// A usage chunk samples the attempt.
	state = unit.Apply(state, usageEvent(1, 1, 1, llm.TokenUsage{
		InputTokens: 100, OutputTokens: 20, CacheReadTokens: int64Ptr(50),
	}))
	totals := state.(tokenUsageState).Totals
	if totals.UncachedInputTokens != 100 || totals.OutputTokens != 20 || totals.CacheReadTokens != 50 {
		t.Fatalf("chunk totals: %+v", totals)
	}

	// The final message replaces the sample for the same attempt.
	state = unit.Apply(state, assistantMessageEvent(2, 1, 1, &llm.TokenUsage{
		InputTokens: 30, OutputTokens: 5, CacheReadTokens: int64Ptr(10),
	}, "deepseek", "deepseek-chat"))
	totals = state.(tokenUsageState).Totals
	if totals.UncachedInputTokens != 30 || totals.OutputTokens != 5 || totals.CacheReadTokens != 10 {
		t.Fatalf("replaced totals: %+v", totals)
	}

	// A repeated identical sample changes nothing.
	before := state
	state = unit.Apply(state, assistantMessageEvent(3, 1, 1, &llm.TokenUsage{
		InputTokens: 30, OutputTokens: 5, CacheReadTokens: int64Ptr(10),
	}, "deepseek", "deepseek-chat"))
	if !jsonEqual(t, before, state) {
		t.Fatalf("identical sample must not double count")
	}

	// retry-started closes the slot so the retried attempt adds.
	state = unit.Apply(state, rawEvent(4, EventLlmRetryStarted, struct {
		RetryID string `json:"retryId"`
		Turn    int64  `json:"turn"`
		Step    int64  `json:"step"`
		Retry   int64  `json:"retry"`
	}{"r1", 1, 1, 1}))
	state = unit.Apply(state, assistantMessageEvent(5, 1, 1, &llm.TokenUsage{
		InputTokens: 40, OutputTokens: 10,
	}, "deepseek", "deepseek-chat"))
	totals = state.(tokenUsageState).Totals
	if totals.UncachedInputTokens != 70 || totals.OutputTokens != 15 || totals.CacheReadTokens != 10 {
		t.Fatalf("post-retry totals: %+v", totals)
	}
}

func jsonEqual(t *testing.T, left, right any) bool {
	t.Helper()
	encodedLeft, err := json.Marshal(left)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	encodedRight, err := json.Marshal(right)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encodedLeft) == string(encodedRight)
}

func int64Ptr(value int64) *int64 { return &value }

func TestContextPressureUnitAnchorsAndProjects(t *testing.T) {
	unit := ContextPressureUnit()
	state := unit.Init(session.SessionHeader{})

	// Route capacity lands via request/context.
	state = unit.Apply(state, rawEvent(1, session.EventRequestCtx, session.RequestContext{
		Provider: "deepseek", Model: "deepseek-chat", ContextWindow: int64Ptr(128000),
	}))
	pressure := state.(contextPressureState)
	if pressure.ContextWindow == nil || *pressure.ContextWindow != 128000 {
		t.Fatalf("context window: %+v", pressure)
	}

	// The user message joins the surface first.
	messageEvent := rawSurfaceEvent(2, session.EventUserMessage, userMessageData("hi"), nil)
	state = unit.Apply(state, messageEvent)
	surfaceAfterUser := state.(contextPressureState).SurfaceTokens
	if surfaceAfterUser <= 0 {
		t.Fatalf("surface must grow: %+v", state)
	}

	// The usage sample stamps BEFORE its own message joins the surface.
	state = unit.Apply(state, assistantMessageEvent(3, 1, 1, &llm.TokenUsage{
		InputTokens: 100, CacheReadTokens: int64Ptr(50),
	}, "deepseek", "deepseek-chat"))
	pressureState := state.(contextPressureState)
	if pressureState.PressureTokens == nil || *pressureState.PressureTokens != 150 {
		t.Fatalf("pressure: %+v", pressureState)
	}
	if pressureState.SampledSurfaceTokens == nil || *pressureState.SampledSurfaceTokens != surfaceAfterUser {
		t.Fatalf("sampled surface: %+v", pressureState)
	}

	// View projects the anchored sample plus the signed surface movement.
	view := unit.Wire.View(pressureState).(ContextPressureProjection)
	if view.ProjectedTokens == nil || *view.ProjectedTokens != 150+pressureState.SurfaceTokens-surfaceAfterUser {
		t.Fatalf("projected: %+v", view)
	}
	if view.ContextWindow == nil || *view.ContextWindow != 128000 {
		t.Fatalf("window view: %+v", view)
	}
}

func TestContextPressureUnitCompactionShrinksProjection(t *testing.T) {
	unit := ContextPressureUnit()
	state := unit.Init(session.SessionHeader{})
	state = unit.Apply(state, rawEvent(1, session.EventRequestCtx, session.RequestContext{
		Provider: "deepseek", Model: "deepseek-chat",
	}))
	state = unit.Apply(state, rawSurfaceEvent(2, session.EventUserMessage, userMessageData("long conversation body"), nil))
	state = unit.Apply(state, assistantMessageEvent(3, 1, 1, &llm.TokenUsage{InputTokens: 200}, "p", "m"))
	base := unit.Wire.View(state.(contextPressureState)).(ContextPressureProjection)
	if base.ProjectedTokens == nil || *base.ProjectedTokens <= 0 {
		t.Fatalf("baseline projection: %+v", base)
	}

	// Compaction: metering event arms the shadow price, the adjacent
	// replacement consumes it, the projection reacts immediately.
	state = unit.Apply(state, rawEvent(4, compaction.EventCompactionSummary, compaction.SummaryPayload{
		CompactionID:       "c1",
		ShadowedRange:      compaction.SeqRange{Start: 0, End: 0},
		ShadowedTokenCount: 40,
	}))
	state = unit.Apply(state, rawSurfaceEvent(5, session.EventUserMessage, userMessageData("summary"), &session.SurfaceOp{
		Kind: session.SurfaceReplace, Start: 0, End: 0,
	}))
	compacted := unit.Wire.View(state.(contextPressureState)).(ContextPressureProjection)
	if compacted.ProjectedTokens == nil || *compacted.ProjectedTokens >= *base.ProjectedTokens {
		t.Fatalf("compaction must shrink the projection: %+v vs %+v", compacted, base)
	}
}

func TestContextBreakdownUnitFoldsEnvelopeAndSurface(t *testing.T) {
	unit := ContextBreakdownUnit()
	state := unit.Init(session.SessionHeader{})

	header := session.EpochHeader{
		Config: llm.LlmCallConfig{Provider: "deepseek", Model: "deepseek-chat"},
		System: "You are a helpful assistant.",
		Tools:  []llm.ToolSchema{{Name: "bash", Description: "run commands"}},
	}
	state = unit.Apply(state, rawEvent(1, session.EventRequestHeader, session.RequestHeaderData{Header: header}))
	breakdown := state.(contextBreakdownState)
	if breakdown.SystemTokens <= 0 || breakdown.ToolsTokens <= 0 {
		t.Fatalf("envelope figures: %+v", breakdown)
	}

	// Surface messages accumulate under the message figure.
	state = unit.Apply(state, rawSurfaceEvent(2, session.EventUserMessage, userMessageData("hello there"), nil))
	grown := state.(contextBreakdownState)
	if grown.MessageTokens <= 0 {
		t.Fatalf("message tokens: %+v", grown)
	}

	// Compaction shrinks the message figure by its shadow price.
	state = unit.Apply(state, rawEvent(3, compaction.EventCompactionSummary, compaction.SummaryPayload{
		CompactionID:       "c1",
		ShadowedRange:      compaction.SeqRange{Start: 0, End: 0},
		ShadowedTokenCount: grown.MessageTokens,
	}))
	state = unit.Apply(state, rawSurfaceEvent(4, session.EventUserMessage, userMessageData("summary"), &session.SurfaceOp{
		Kind: session.SurfaceReplace, Start: 0, End: 0,
	}))
	compacted := state.(contextBreakdownState)
	if compacted.MessageTokens >= grown.MessageTokens {
		t.Fatalf("compaction must shrink the message figure: %+v vs %+v", compacted, grown)
	}

	view := unit.Wire.View(compacted).(ContextBreakdownProjection)
	if view.SystemTokens != compacted.SystemTokens || view.MessageTokens != compacted.MessageTokens {
		t.Fatalf("view: %+v", view)
	}
}

func TestContextBreakdownUnitUnchangedEventKeepsState(t *testing.T) {
	unit := ContextBreakdownUnit()
	state := unit.Init(session.SessionHeader{})
	// A tool/call is neither envelope nor surface: unchanged.
	next := unit.Apply(state, rawEvent(1, session.EventToolCall, session.ToolCallData{
		Turn: 1, Step: 1, Name: "x", Arguments: "{}",
	}))
	if !jsonEqual(t, state, next) {
		t.Fatalf("uninteresting event must not change state")
	}
}

func turnFixture(seq int64, eventType string, data any) session.Event {
	return rawEvent(seq, eventType, data)
}

func TestDeriveTurnTokenUsageAggregatesAndAttribution(t *testing.T) {
	usage := func(input, output int64, total int64, cacheRead *int64, extra func(*llm.TokenUsage)) llm.TokenUsage {
		value := llm.TokenUsage{InputTokens: input, OutputTokens: output, TotalTokens: &total, CacheReadTokens: cacheRead}
		if extra != nil {
			extra(&value)
		}
		return value
	}
	events := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		assistantMessageEvent(3, 1, 1, ptrUsage(usage(100, 20, 170, int64Ptr(50), func(u *llm.TokenUsage) {
			zero := int64(0)
			eight := int64(8)
			u.CacheWriteTokens = &zero
			u.ReasoningTokens = &eight
		})), "deepseek", "deepseek-chat"),
		turnFixture(4, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(5, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	result := DeriveTurnTokenUsage(events)
	if result == nil {
		t.Fatal("expected an aggregate")
	}
	if result.UncachedInputTokens != 100 || result.OutputTokens != 20 || result.TotalTokens != 170 {
		t.Fatalf("totals: %+v", result)
	}
	if result.CacheReadTokens == nil || *result.CacheReadTokens != 50 ||
		result.CacheWriteTokens == nil || *result.CacheWriteTokens != 0 ||
		result.ReasoningTokens == nil || *result.ReasoningTokens != 8 {
		t.Fatalf("buckets: %+v", result)
	}
	if len(result.Routes) != 1 || result.Routes[0].Provider != "deepseek" || result.Routes[0].Model != "deepseek-chat" {
		t.Fatalf("routes: %+v", result.Routes)
	}
}

func ptrUsage(value llm.TokenUsage) *llm.TokenUsage { return &value }

func TestDeriveTurnTokenUsageReplacesStreamingSample(t *testing.T) {
	events := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		usageEvent(3, 1, 1, llm.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: int64Ptr(170), CacheReadTokens: int64Ptr(50)}),
		assistantMessageEvent(4, 1, 1, ptrUsage(llm.TokenUsage{InputTokens: 30, OutputTokens: 5, TotalTokens: int64Ptr(45), CacheReadTokens: int64Ptr(10)}), "p", "m"),
		turnFixture(5, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(6, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	result := DeriveTurnTokenUsage(events)
	if result == nil || result.UncachedInputTokens != 30 || result.TotalTokens != 45 {
		t.Fatalf("message usage must replace the sample: %+v", result)
	}
}

func TestDeriveTurnTokenUsageKeepsSampleWhenMessageOmitsUsage(t *testing.T) {
	events := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		usageEvent(3, 1, 1, llm.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: int64Ptr(170), CacheReadTokens: int64Ptr(50)}),
		assistantMessageEvent(4, 1, 1, nil, "p", "m"),
		turnFixture(5, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(6, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	result := DeriveTurnTokenUsage(events)
	if result == nil || result.UncachedInputTokens != 100 || result.TotalTokens != 170 {
		t.Fatalf("streaming sample must survive: %+v", result)
	}
	// The attempt closed through the message carries its route.
	if result.Routes == nil || len(result.Routes) != 1 {
		t.Fatalf("routes: %+v", result.Routes)
	}
}

func TestDeriveTurnTokenUsageCountsErrorAttemptOnceAcrossRetry(t *testing.T) {
	finishError := func(seq int64) session.Event {
		return rawEvent(seq, session.EventAssistantChunk, struct {
			Turn  int64           `json:"turn"`
			Step  int64           `json:"step"`
			Chunk llm.StreamChunk `json:"chunk"`
		}{1, 1, llm.StreamChunk{Type: llm.ChunkFinish, Reason: &llm.FinishReason{Kind: llm.FinishError}}})
	}
	events := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		usageEvent(3, 1, 1, llm.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: int64Ptr(170), CacheReadTokens: int64Ptr(50)}),
		finishError(4),
		turnFixture(5, EventLlmRetry, struct {
			Turn int64 `json:"turn"`
			Step int64 `json:"step"`
		}{1, 1}),
		turnFixture(6, EventLlmRetryStarted, struct {
			RetryID string `json:"retryId"`
			Turn    int64  `json:"turn"`
			Step    int64  `json:"step"`
			Retry   int64  `json:"retry"`
		}{"r1", 1, 1, 1}),
		assistantMessageEvent(7, 1, 1, ptrUsage(llm.TokenUsage{InputTokens: 40, OutputTokens: 10, TotalTokens: int64Ptr(70), CacheReadTokens: int64Ptr(20)}), "deepseek", "deepseek-chat"),
		turnFixture(8, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(9, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	result := DeriveTurnTokenUsage(events)
	if result == nil {
		t.Fatal("expected an aggregate")
	}
	if result.UncachedInputTokens != 140 || result.OutputTokens != 30 || result.TotalTokens != 240 {
		t.Fatalf("totals: %+v", result)
	}
	if result.CacheReadTokens == nil || *result.CacheReadTokens != 70 {
		t.Fatalf("cache read: %+v", result)
	}
	// The error-finished attempt has no attribution, so routes stay absent.
	if result.Routes != nil {
		t.Fatalf("routes: %+v", result.Routes)
	}
}

func TestDeriveTurnTokenUsageScheduledRetryNeverStarted(t *testing.T) {
	events := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		usageEvent(3, 1, 1, llm.TokenUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: int64Ptr(170), CacheReadTokens: int64Ptr(50)}),
		turnFixture(4, EventLlmRetry, struct {
			Turn int64 `json:"turn"`
			Step int64 `json:"step"`
		}{1, 1}),
		turnFixture(5, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(6, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	result := DeriveTurnTokenUsage(events)
	if result == nil || result.TotalTokens != 170 {
		t.Fatalf("a scheduled but never started retry must not invent an attempt: %+v", result)
	}
}

func TestDeriveTurnTokenUsageFailsClosed(t *testing.T) {
	// Missing turn/end.
	missing := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		assistantMessageEvent(3, 1, 1, ptrUsage(llm.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: int64Ptr(2), CacheReadTokens: int64Ptr(0), CacheWriteTokens: int64Ptr(0)}), "p", "m"),
		turnFixture(4, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
	}
	if DeriveTurnTokenUsage(missing) != nil {
		t.Fatal("missing turn/end must fail closed")
	}
	// A usage report without its lifecycle bracket.
	orphan := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		usageEvent(2, 1, 1, llm.TokenUsage{InputTokens: 1, OutputTokens: 1}),
		turnFixture(3, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	if DeriveTurnTokenUsage(orphan) != nil {
		t.Fatal("an orphaned usage report must fail closed")
	}
	// retry-started after the final message.
	badRetryStart := []session.Event{
		turnFixture(1, session.EventTurnStart, session.TurnStartData{Turn: 1}),
		turnFixture(2, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}),
		assistantMessageEvent(3, 1, 1, ptrUsage(llm.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: int64Ptr(2), CacheReadTokens: int64Ptr(0), CacheWriteTokens: int64Ptr(0)}), "p", "m"),
		turnFixture(4, EventLlmRetryStarted, struct {
			RetryID string `json:"retryId"`
			Turn    int64  `json:"turn"`
			Step    int64  `json:"step"`
			Retry   int64  `json:"retry"`
		}{"r1", 1, 1, 1}),
		turnFixture(5, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}),
		turnFixture(6, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}),
	}
	if DeriveTurnTokenUsage(badRetryStart) != nil {
		t.Fatal("retry-started after the final message must fail closed")
	}
}
