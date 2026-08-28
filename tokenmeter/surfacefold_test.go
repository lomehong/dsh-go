package tokenmeter

import (
	"encoding/json"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

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

func userMessageEvent(t *testing.T, sess *session.Session, text string) session.Event {
	t.Helper()
	message := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: text}}, llm.MessageSource{})
	return appendEvent(t, sess, session.EventUserMessage, message, appendIntent())
}

func marshal(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture failed: %v", err)
	}
	return encoded
}

func TestPlanAppendThenCommitGrowsSurface(t *testing.T) {
	nodes := []MeterSurfaceNode{}
	plan, err := PlanSurfaceTokens(nodes, userMessageEvent(t, newDetached(t), "abcd"))
	if err != nil {
		t.Fatalf("plan failed: %v", err)
	}
	if plan.Tokens != 1+blockOverhead+RoleOverhead || plan.DeltaTokens != plan.Tokens {
		t.Fatalf("append plan wrong: %+v", plan)
	}
	if plan.Replace != nil {
		t.Fatalf("append must not target a range: %+v", plan.Replace)
	}
	CommitSurfaceTokens(&nodes, plan)
	if len(nodes) != 1 || nodes[0].Seq != 0 || nodes[0].HeuristicTokens != plan.Tokens {
		t.Fatalf("commit wrong: %#v", nodes)
	}
}

func TestPlanReplaceSplicesInclusiveRange(t *testing.T) {
	sess := newDetached(t)
	nodes := []MeterSurfaceNode{}
	for _, text := range []string{"a", "b", "c"} {
		plan, err := PlanSurfaceTokens(nodes, userMessageEvent(t, sess, text))
		if err != nil {
			t.Fatalf("plan failed: %v", err)
		}
		CommitSurfaceTokens(&nodes, plan)
	}
	replacement := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: "summary"}}, llm.MessageSource{})
	intent := &session.SurfaceIntent{
		SurfaceOp:         session.SurfaceOp{Kind: session.SurfaceReplace, Start: 0, End: 1},
		SourceEventSeqs:   []int64{0, 1},
		SourceSeqsPresent: true,
	}
	event := appendEvent(t, sess, session.EventUserMessage, replacement, intent)
	plan, err := PlanSurfaceTokens(nodes, event)
	if err != nil {
		t.Fatalf("replace plan failed: %v", err)
	}
	if plan.Replace == nil || plan.Replace[0] != 0 || plan.Replace[1] != 1 {
		t.Fatalf("replace range wrong: %+v", plan.Replace)
	}
	// "summary" prices 10; the replaced pair prices 18 → delta −8.
	if plan.DeltaTokens != -8 {
		t.Fatalf("replace delta wrong: %d", plan.DeltaTokens)
	}
	CommitSurfaceTokens(&nodes, plan)
	// The replacement carries the replace event's seq (3); the untouched
	// tail keeps seq 2.
	if len(nodes) != 2 || nodes[0].Seq != 3 || nodes[1].Seq != 2 {
		t.Fatalf("splice wrong: %#v", nodes)
	}
}

func TestPlanInvalidRangeFailsLoud(t *testing.T) {
	nodes := []MeterSurfaceNode{
		{Seq: 0, HeuristicTokens: 9, ImageFreeTokens: 9},
	}
	// Invalid ranges cannot pass session append validation, so the plan is
	// exercised with a synthetic event — the same event shape a corrupted
	// log replays.
	cases := []struct {
		name       string
		start, end int64
	}{
		{"missing start", 9, 0},
		{"missing end", 0, 9},
	}
	for _, testCase := range cases {
		event := session.Event{
			Type: session.EventUserMessage,
			Seq:  5,
			SurfaceOp: &session.SurfaceOp{
				Kind:  session.SurfaceReplace,
				Start: testCase.start,
				End:   testCase.end,
			},
		}
		_, err := PlanSurfaceTokens(nodes, event)
		if err == nil {
			t.Fatalf("%s: invalid range must fail", testCase.name)
		} else {
			t.Log(err)
		}
	}
	// An inverted but resolvable range is legal at the fold: start 0 and
	// end 0 name one node.
	event := session.Event{
		Type:      session.EventUserMessage,
		Seq:       5,
		SurfaceOp: &session.SurfaceOp{Kind: session.SurfaceReplace, Start: 0, End: 0},
	}
	plan, err := PlanSurfaceTokens(nodes, event)
	if err != nil {
		t.Fatalf("single-node replace must resolve: %v", err)
	}
	if plan.Replace == nil || plan.Replace[0] != 0 || plan.Replace[1] != 0 || plan.DeltaTokens != plan.Tokens-9 {
		t.Fatalf("single-node replace plan wrong: %+v", plan)
	}
}

func TestAnalyzeNodeCollectsImageOccurrences(t *testing.T) {
	image := llm.ContentBlock{Type: llm.BlockImage, Attachment: map[string]any{"id": "att-1"}}
	message := llm.NewUserMessage([]llm.ContentBlock{
		{Type: llm.BlockText, Text: "abcd"},
		image,
		{Type: llm.BlockToolResult, ToolCallID: "call-1", Content: []llm.ContentBlock{image}},
	}, llm.MessageSource{})
	node := analyzeNode(7, &message)
	structural := EstimateStructuralBlock(image)
	want := EstimateMessage(message)
	if node.HeuristicTokens != want {
		t.Fatalf("heuristic total wrong: %d vs %d", node.HeuristicTokens, want)
	}
	if node.ImageFreeTokens != want-2*structural {
		t.Fatalf("image-free price wrong: %d", node.ImageFreeTokens)
	}
	if len(node.Images) != 2 {
		t.Fatalf("image occurrences wrong: %d", len(node.Images))
	}
}

func TestAnalyzeNodeWithoutMessage(t *testing.T) {
	node := analyzeNode(3, nil)
	if node.HeuristicTokens != 0 || node.ImageFreeTokens != 0 || len(node.Images) != 0 {
		t.Fatalf("message-less node must price 0: %#v", node)
	}
}

func TestCommitSplicePreservesTail(t *testing.T) {
	nodes := []MeterSurfaceNode{
		{Seq: 0, HeuristicTokens: 1},
		{Seq: 1, HeuristicTokens: 2},
		{Seq: 2, HeuristicTokens: 3},
	}
	plan := SurfaceTokenPlan{
		Tokens:      9,
		DeltaTokens: 9 - 5,
		Node:        MeterSurfaceNode{Seq: 3, HeuristicTokens: 9},
		Replace:     &[2]int{1, 2},
	}
	CommitSurfaceTokens(&nodes, plan)
	if len(nodes) != 2 || nodes[0].Seq != 0 || nodes[1].Seq != 3 || nodes[1].HeuristicTokens != 9 {
		t.Fatalf("splice wrong: %#v", nodes)
	}
}

func newDetached(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewDetached("meter-probe", nil, &session.SessionHeader{ID: "meter-probe", CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("construct failed: %v", err)
	}
	return sess
}
