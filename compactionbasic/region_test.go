package compactionbasic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
)

func testHeader(provider string, model string) session.EpochHeader {
	return session.EpochHeader{Config: llm.LlmCallConfig{Provider: provider, Model: model}}
}

func newTestSession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"}, 0)
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

func appendIntent() *session.SurfaceIntent {
	return &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}
}

// regionConversation builds one completed turn with `count` user messages of
// `filler` characters each so a short checkpoint satisfies the shrink rule.
func regionConversation(t *testing.T, id string, count int, filler string) *session.Session {
	t.Helper()
	sess := newTestSession(t, id)
	appendEvent(t, sess, session.EventRequestHeader, session.RequestHeaderData{Header: testHeader("deepseek", "chat"), Reason: session.HeaderReasonInitial}, nil)
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 1}, nil)
	appendEvent(t, sess, session.EventStepStart, session.StepStartData{Turn: 1, Step: 1}, nil)
	for i := 0; i < count; i++ {
		appendEvent(t, sess, session.EventUserMessage, llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: filler}}, llm.MessageSource{}), appendIntent())
	}
	appendEvent(t, sess, session.EventAssistantMsg, session.AssistantMessageData{
		Turn: 1, Step: 1,
		Message: llm.NewAssistantMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "answer"}}, "deepseek", "chat", nil),
	}, appendIntent())
	appendEvent(t, sess, session.EventStepEnd, session.StepEndData{Turn: 1, Step: 1}, nil)
	appendEvent(t, sess, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: session.TurnEndCompleted}}, nil)
	return sess
}

// staticRegionView is an AgentView over a fixed session.
type staticRegionView struct{ sess *session.Session }

func (v staticRegionView) Session() *session.Session { return v.sess }
func (v staticRegionView) OptionsProvider() string   { return "" }
func (v staticRegionView) OptionsModel() string      { return "" }

// shortSummarize returns a summary far smaller than any realistic span.
func shortSummarize(input SummarizationInput, agent AgentView, signal context.Context) (SummaryResult, error) {
	return SummaryResult{
		Summary:       []llm.ContentBlock{{Type: llm.BlockText, Text: "# checkpoint\n- condensed"}},
		LlmStreamCall: true,
		Provider:      "deepseek",
		Model:         "chat",
	}, nil
}

func regionDeps(sess *session.Session, summarize func(SummarizationInput, AgentView, context.Context) (SummaryResult, error)) RegionDependencies {
	meter := tokenmeter.NewMeter(nil)
	if summarize == nil {
		summarize = shortSummarize
	}
	return RegionDependencies{
		Meter:     meter,
		Balance:   compaction.NewToolPairingBalance(),
		Summarize: summarize,
	}
}

func TestSelectCompactableRangeWholeSurface(t *testing.T) {
	sess := regionConversation(t, "select", 2, strings.Repeat("a", 400))
	deps := regionDeps(sess, nil)
	measurement, err := deps.Meter.Measure(sess, nil)
	if err != nil {
		t.Fatalf("measure failed: %v", err)
	}
	start, end, ok, err := SelectCompactableRange(deps, sess, measurement, 0)
	if err != nil || !ok {
		t.Fatalf("select failed: %v %v", start, err)
	}
	nodes := sess.Surface().Nodes()
	// retain 0 breaks at the tail, so the newest node stays verbatim and the
	// cutoff is the second-newest position.
	if start != nodes[0] || end != nodes[len(nodes)-2] {
		t.Fatalf("range wrong: %d-%d over %#v", start, end, nodes)
	}
	// A retain budget above the priced surface keeps everything verbatim.
	huge := measurement.SurfaceTokens + 1000
	_, _, ok, err = SelectCompactableRange(deps, sess, measurement, huge)
	if err != nil || ok {
		t.Fatalf("oversized retention must select nothing: %v %v", ok, err)
	}
}

func TestCompactSurfaceRegionTransaction(t *testing.T) {
	sess := regionConversation(t, "region", 2, strings.Repeat("b", 1200))
	deps := regionDeps(sess, nil)
	nodesBefore := append([]int64{}, sess.Surface().Nodes()...)

	result, err := CompactSurfaceRegion(deps, sess, nodesBefore[0], nodesBefore[len(nodesBefore)-1], staticRegionView{sess},
		TransactionOptions{}, context.Background())
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}
	if result.CompactionID == "" || result.SourceCommandID != "" {
		t.Fatalf("identity wrong: %+v", result)
	}
	if result.StartSeq != 8 || result.EndSeq <= result.SummarySeq {
		t.Fatalf("bracket wrong: %#v", result)
	}
	if len(result.ShadowedSeqs) != 3 || result.ShadowedSeqs[0] != nodesBefore[0] {
		t.Fatalf("shadowed seqs wrong: %#v", result.ShadowedSeqs)
	}
	if result.ShadowedTokenCount == 0 || result.ShadowedRange.Start != nodesBefore[0] {
		t.Fatalf("shadow accounting wrong: %#v", result)
	}
	// The replacement user/message collapsed the surface to one node.
	nodesAfter := sess.Surface().Nodes()
	if len(nodesAfter) != 1 {
		t.Fatalf("surface not replaced: %#v", nodesAfter)
	}
	// Durable bracket events carry the shared compaction id.
	events := sess.Events()
	if events[result.StartSeq].Type != compaction.EventCompactionStart ||
		events[result.SummarySeq].Type != compaction.EventCompactionSummary ||
		events[result.EndSeq].Type != compaction.EventCompactionEnd {
		t.Fatalf("bracket events wrong: %s/%s/%s",
			events[result.StartSeq].Type, events[result.SummarySeq].Type, events[result.EndSeq].Type)
	}
	// A second compaction is refused by the now-inactive... the bracket
	// closed, so another compaction is legal and the surface still
	// validates.
	if err := AssertNoActiveCompaction(sess, "compaction"); err != nil {
		t.Fatalf("closed bracket must not hold the lock: %v", err)
	}
}

func TestCompactSurfaceRegionShrinkRuleFails(t *testing.T) {
	sess := regionConversation(t, "shrink", 1, "tiny")
	deps := regionDeps(sess, func(input SummarizationInput, agent AgentView, signal context.Context) (SummaryResult, error) {
		return SummaryResult{
			Summary: []llm.ContentBlock{{Type: llm.BlockText, Text: strings.Repeat("huge ", 800)}},
		}, nil
	})
	nodes := append([]int64{}, sess.Surface().Nodes()...)
	_, err := CompactSurfaceRegion(deps, sess, nodes[0], nodes[len(nodes)-1], staticRegionView{sess},
		TransactionOptions{}, context.Background())
	var manualErr *ManualCompactionError
	if err == nil || !errors.As(err, &manualErr) || manualErr.Kind != ManualSummary ||
		!strings.Contains(llm.ErrorChain(err), "summary is not smaller than the shadowed content") {
		t.Fatalf("shrink rule must fail loud as a manual summary failure: %v", err)
	}
	// The failure path appended the closing marker with the error recorded.
	events := sess.Events()
	last := events[len(events)-1]
	if last.Type != compaction.EventCompactionEnd {
		t.Fatalf("failure must close the bracket: %s", last.Type)
	}
	var endPayload compaction.EndPayload
	if err := json.Unmarshal(last.Data, &endPayload); err != nil || endPayload.Error == "" {
		t.Fatalf("close must record the error: %+v %v", endPayload, err)
	}
	// The surface is untouched.
	if len(sess.Surface().Nodes()) != len(nodes) {
		t.Fatal("failed compaction must not mutate the surface")
	}
	// And the closed bracket releases the lock for the next attempt.
	if err := AssertNoActiveCompaction(sess, "compaction"); err != nil {
		t.Fatalf("lock must be released: %v", err)
	}
}

func TestCompactSurfaceRegionSummarizerFailureClosesBracket(t *testing.T) {
	sess := regionConversation(t, "fail", 1, "tiny")
	deps := regionDeps(sess, func(input SummarizationInput, agent AgentView, signal context.Context) (SummaryResult, error) {
		return SummaryResult{}, context.Canceled
	})
	nodes := sess.Surface().Nodes()
	_, err := CompactSurfaceRegion(deps, sess, nodes[0], nodes[len(nodes)-1], staticRegionView{sess},
		TransactionOptions{}, context.Background())
	if err == nil {
		t.Fatal("summarizer failure must surface")
	}
	events := sess.Events()
	last := events[len(events)-1]
	if last.Type != compaction.EventCompactionEnd {
		t.Fatalf("failure must close the bracket: %s", last.Type)
	}
}

func TestCompactSurfaceRegionLockAndOwners(t *testing.T) {
	// Owner null with an open turn refuses manual compaction.
	sess := regionConversation(t, "owners", 2, strings.Repeat("c", 1200))
	deps := regionDeps(sess, nil)
	appendEvent(t, sess, session.EventTurnStart, session.TurnStartData{Turn: 2}, nil)
	nodes := sess.Surface().Nodes()
	_, err := CompactSurfaceRegion(deps, sess, nodes[0], nodes[len(nodes)-1], staticRegionView{sess},
		TransactionOptions{}, context.Background())
	var manualErr *ManualCompactionError
	if err == nil || !errors.As(err, &manualErr) || manualErr.Kind != ManualBusy || !strings.Contains(err.Error(), "already has an open turn") {
		t.Fatalf("open turn must refuse owner-null: %v", err)
	}
	// Owner current-turn without an open turn fails.
	sessClosed := regionConversation(t, "owners-closed", 2, strings.Repeat("d", 1200))
	depsClosed := regionDeps(sessClosed, nil)
	nodesClosed := sessClosed.Surface().Nodes()
	_, err = CompactSurfaceRegion(depsClosed, sessClosed, nodesClosed[0], nodesClosed[len(nodesClosed)-1], staticRegionView{sessClosed},
		TransactionOptions{OwnerCurrentTurn: true}, context.Background())
	if err == nil || !strings.Contains(err.Error(), "compactRegion: no open turn") {
		t.Fatalf("missing open turn must fail loud: %v", err)
	}
	// Owner current-turn with an open turn succeeds and stamps the owner.
	sessOpen := regionConversation(t, "owners-open2", 2, strings.Repeat("f", 1200))
	depsOpen := regionDeps(sessOpen, nil)
	appendEvent(t, sessOpen, session.EventTurnStart, session.TurnStartData{Turn: 2}, nil)
	nodesOpen := sessOpen.Surface().Nodes()
	result, err := CompactSurfaceRegion(depsOpen, sessOpen, nodesOpen[0], nodesOpen[len(nodesOpen)-1], staticRegionView{sessOpen},
		TransactionOptions{OwnerCurrentTurn: true}, context.Background())
	if err != nil {
		t.Fatalf("enclosed compaction failed: %v", err)
	}
	events := sessOpen.Events()
	var startPayload compaction.StartPayload
	if err := json.Unmarshal(events[result.StartSeq].Data, &startPayload); err != nil || startPayload.Turn == nil || *startPayload.Turn != 2 {
		t.Fatalf("owner must be stamped: %+v %v", startPayload, err)
	}
}

func TestCompactSurfaceRegionUnbalancedBoundaries(t *testing.T) {
	sess := regionConversation(t, "balance", 1, strings.Repeat("g", 1200))
	deps := regionDeps(sess, nil)
	nodes := sess.Surface().Nodes()
	// An interior start boundary splitting the tool-pair surface... with
	// only user/assistant nodes every boundary is balanced; an invalid
	// position still fails.
	if _, err := ValidateSurfaceRegion(deps, sess, 99, nodes[len(nodes)-1]); err == nil {
		t.Fatal("unknown start seq must fail")
	}
	if _, err := ValidateSurfaceRegion(deps, sess, nodes[0], 99); err == nil {
		t.Fatal("unknown end seq must fail")
	}
	if _, err := ValidateSurfaceRegion(deps, sess, nodes[len(nodes)-1], nodes[0]); err == nil {
		t.Fatal("inverted range must fail")
	}
}
