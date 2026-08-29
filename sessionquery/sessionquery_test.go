package sessionquery

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"dshgo/llm"
	session "dshgo/session"
)

func sameSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func infValue() float64 { return math.Inf(1) }

// newBareSession builds a detached session with a stable deterministic
// header for pure-helper tests.
func newBareSession(t *testing.T, id string) *session.Session {
	t.Helper()
	s, err := session.NewDetached(id, nil, &session.SessionHeader{ID: id, CreatedAt: 1000})
	if err != nil {
		t.Fatalf("new session %s: %v", id, err)
	}
	return s
}

// --- shared append helpers -------------------------------------------------

func mustAppend(t *testing.T, s *session.Session, eventType string, data any, intent *session.SurfaceIntent) session.Event {
	t.Helper()
	event, err := s.Append(eventType, data, intent)
	if err != nil {
		t.Fatalf("append %s failed: %v", eventType, err)
	}
	return event
}

func appendIntent(op session.SurfaceOp) *session.SurfaceIntent {
	return &session.SurfaceIntent{SurfaceOp: op}
}

func userMessage(id, text string) llm.Message {
	return llm.Message{
		ID:      llm.MessageID(id),
		Role:    llm.RoleUser,
		Source:  llm.MessageSource{Kind: llm.SourceUser},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: text}},
	}
}

func appendUserMessage(t *testing.T, s *session.Session, id, text string) session.Event {
	t.Helper()
	return mustAppend(t, s, session.EventUserMessage, userMessage(id, text), appendIntent(session.SurfaceOp{Kind: session.SurfaceAppend}))
}

// --- config ----------------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	negative := -1
	if _, err := NewEngine(nil, nil, nil, nil, &Config{ReadWindowMax: &negative}); err == nil {
		t.Fatal("negative readWindowMax accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeInvalidConfig {
			t.Fatalf("negative readWindowMax error = %v", err)
		}
	}
	zero := 0
	if _, err := NewEngine(nil, nil, nil, nil, &Config{PersistedInspectConcurrency: &zero}); err == nil {
		t.Fatal("zero concurrency accepted")
	}
	engine, err := NewEngine(nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if engine.ReadWindowMax() != SESSION_QUERY_READ_WINDOW_MAX {
		t.Fatalf("default readWindowMax = %d", engine.ReadWindowMax())
	}
}

// --- sources ---------------------------------------------------------------

func TestAssertSessionHeadersCompatible(t *testing.T) {
	base := session.SessionHeader{Version: 0, ID: "s1", CreatedAt: 100}
	if err := AssertSessionHeadersCompatible(base, base); err != nil {
		t.Fatalf("identical headers rejected: %v", err)
	}
	depth := int64(1)
	conflicting := base
	conflicting.DelegationDepth = &depth
	if err := AssertSessionHeadersCompatible(base, conflicting); err == nil {
		t.Fatal("delegation-depth conflict accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSourceConflict {
			t.Fatalf("conflict error = %v", err)
		}
	}
	absent := base
	absent.ParentSession = "p"
	if err := AssertSessionHeadersCompatible(base, absent); err == nil {
		t.Fatal("parent conflict accepted")
	}
}

// --- session filters -------------------------------------------------------

func TestSessionResultFilters(t *testing.T) {
	records := []SessionRecord{
		{Header: session.SessionHeader{ID: "a", CreatedAt: 30, CWD: "C:\\w\\a"}, Live: true},
		{Header: session.SessionHeader{ID: "b", CreatedAt: 20, ParentSession: "a"}, Persisted: true},
		{Header: session.SessionHeader{ID: "c", CreatedAt: 10}, Live: true, Persisted: true},
	}
	from := 15.0
	to := 25.0
	emptyCwd := (*string)(nil)
	idValues := []string{"a", "b"}
	cases := []struct {
		name    string
		filters []SessionResultFilter
		want    []string
	}{
		{"id OR", []SessionResultFilter{{Kind: "id", Values: idValues}}, []string{"a", "b"}},
		{"cwd empty matches nil", []SessionResultFilter{{Kind: "cwd", NullableValues: []*string{emptyCwd}}}, []string{"b", "c"}},
		{"created-at range", []SessionResultFilter{{Kind: "created-at", From: &from, To: &to}}, []string{"b"}},
		{"parent", []SessionResultFilter{{Kind: "parent", NullableValues: []*string{strPtr("a")}}}, []string{"b"}},
		{"availability OR", []SessionResultFilter{{Kind: "availability", Values: []string{AvailabilityLive, AvailabilityPersisted}}}, []string{"a", "b", "c"}},
		{"AND clauses", []SessionResultFilter{
			{Kind: "availability", Values: []string{AvailabilityLive}},
			{Kind: "created-at", From: &from},
		}, []string{"a"}},
		{"empty values match nothing", []SessionResultFilter{{Kind: "id", Values: []string{}}}, nil},
	}
	for _, testCase := range cases {
		if _, err := MaterializeSessionResultFilters(testCase.filters); err != nil {
			t.Fatalf("%s: materialize failed: %v", testCase.name, err)
		}
		got, err := FilterSessionResults(records, testCase.filters)
		if err != nil {
			t.Fatalf("%s: filter failed: %v", testCase.name, err)
		}
		var ids []string
		for _, record := range got {
			ids = append(ids, record.Header.ID)
		}
		if strings.Join(ids, ",") != strings.Join(testCase.want, ",") {
			t.Fatalf("%s = %v, want %v", testCase.name, ids, testCase.want)
		}
	}
}

func strPtr(v string) *string { return &v }

func floatPtr(v float64) *float64 { return &v }

func TestSessionFilterValidationFailures(t *testing.T) {
	cases := []struct {
		name    string
		filters []SessionResultFilter
	}{
		{"unknown kind", []SessionResultFilter{{Kind: "bogus"}}},
		{"unknown availability", []SessionResultFilter{{Kind: "availability", Values: []string{"archived"}}}},
		{"from after to", []SessionResultFilter{{Kind: "created-at", From: floatPtr(5), To: floatPtr(1)}}},
		{"non-finite from", []SessionResultFilter{{Kind: "created-at", From: floatPtrInf()}}},
	}
	for _, testCase := range cases {
		_, err := MaterializeSessionResultFilters(testCase.filters)
		if err == nil {
			t.Fatalf("%s accepted", testCase.name)
		}
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeInvalidFilter {
			t.Fatalf("%s error = %v", testCase.name, err)
		}
	}
}

func floatPtrInf() *float64 {
	v := infValue()
	return &v
}

// --- event filters ---------------------------------------------------------

func TestEventResultFilters(t *testing.T) {
	documents := []SessionEventSearchDocument{
		{SessionEventRecord: SessionEventRecord{Seq: 1, Time: 100, Type: "user/message", Surface: SurfaceCurrent}, Text: "Find the config file"},
		{SessionEventRecord: SessionEventRecord{Seq: 3, Time: 200, Type: "tool/result", Surface: SurfaceShadowed}, Text: "search results listed"},
		{SessionEventRecord: SessionEventRecord{Seq: 5, Time: 300, Type: "assistant/message", Surface: SurfaceCurrent}, Text: "The CONFIG file is ready"},
	}
	text := "config file"
	surface := []string{SurfaceCurrent}
	cases := []struct {
		name    string
		filters []SessionEventResultFilter
		want    []int64
	}{
		{"type", []SessionEventResultFilter{{Kind: "type", Values: []string{"user/message"}}}, []int64{1}},
		{"surface", []SessionEventResultFilter{{Kind: "surface", Values: surface}}, []int64{1, 5}},
		{"seq range", []SessionEventResultFilter{{Kind: "seq", From: floatPtr(2), To: floatPtr(4)}}, []int64{3}},
		{"time range", []SessionEventResultFilter{{Kind: "time", From: floatPtr(150)}}, []int64{3, 5}},
		{"text case-insensitive", []SessionEventResultFilter{{Kind: "text", Text: text}}, []int64{1, 5}},
		{"text whitespace-flexible", []SessionEventResultFilter{{Kind: "text", Text: "the\tconfig\n file"}}, []int64{1, 5}},
		{"text metachars literal", []SessionEventResultFilter{{Kind: "text", Text: "file."}}, nil},
		{"AND", []SessionEventResultFilter{{Kind: "surface", Values: surface}, {Kind: "text", Text: text}}, []int64{1, 5}},
	}
	for _, testCase := range cases {
		if _, err := MaterializeSessionEventResultFilters(testCase.filters); err != nil {
			t.Fatalf("%s: materialize failed: %v", testCase.name, err)
		}
		got, err := FilterSessionEventDocuments(documents, testCase.filters)
		if err != nil {
			t.Fatalf("%s: filter failed: %v", testCase.name, err)
		}
		var seqs []int64
		for _, document := range got {
			seqs = append(seqs, document.Seq)
		}
		if !sameSeqs(seqs, testCase.want) {
			t.Fatalf("%s = %v, want %v", testCase.name, seqs, testCase.want)
		}
	}
	_, err := MaterializeSessionEventResultFilters([]SessionEventResultFilter{{Kind: "surface", Values: []string{"future"}}})
	if err == nil {
		t.Fatal("unknown surface accepted")
	}
	_, err = MaterializeSessionEventResultFilters([]SessionEventResultFilter{{Kind: "bogus"}})
	if err == nil {
		t.Fatal("unknown event filter kind accepted")
	}
	if _, err := CompileSessionTextFilter("   "); err == nil {
		t.Fatal("blank text filter accepted")
	}
	pattern, err := CompileSessionTextFilter("  config   file ")
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if !pattern.MatchString("CONFIG\nFILE") || pattern.MatchString("configfile") {
		t.Fatalf("compiled pattern semantics wrong: %v", pattern)
	}
}

// --- extraction ------------------------------------------------------------

func TestExtractSessionEventText(t *testing.T) {
	s := newBareSession(t, "extract")
	appendUserMessage(t, s, "u1", "list the todos")
	call := mustAppend(t, s, session.EventToolCall,
		session.ToolCallData{Turn: 1, Step: 1, CallID: llm.ToolCallID("c1"), Name: "todo_read", Arguments: "{\"detail\":true}"},
		nil)
	resultMessage := llm.Message{
		ID:     "r1",
		Role:   llm.RoleUser,
		Source: llm.MessageSource{Kind: llm.SourceTool, CallID: llm.ToolCallID("c1")},
		Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1",
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "todo contents"}}}},
	}
	mustAppend(t, s, session.EventToolResult,
		session.ToolResultData{Turn: 1, Step: 1, Message: resultMessage, Error: &session.ToolResultError{Name: "TimeoutError", Code: "TIMEOUT"}},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceEventSeqs: []int64{call.Seq}, SourceSeqsPresent: true})
	mustAppend(t, s, session.EventAssistantMsg,
		session.AssistantMessageData{Turn: 1, Step: 1, Message: llm.Message{
			ID: "a1", Role: llm.RoleAssistant,
			Source:  llm.MessageSource{Kind: llm.SourceModel, Provider: "deepseek", Model: "m1"},
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "done"}},
		}},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceSeqsPresent: true})
	mustAppend(t, s, "todo/write", map[string]any{"todos": []any{
		map[string]any{"content": "write report", "status": "in_progress"},
	}}, nil)
	mustAppend(t, s, session.EventTurnEnd, session.TurnEndData{Turn: 1, Reason: session.TurnEndReason{Kind: "completed"}}, nil)
	mustAppend(t, s, session.EventTurnEnd, session.TurnEndData{Turn: 2, Reason: session.TurnEndReason{Kind: "aborted"}}, nil)
	mustAppend(t, s, session.EventTurnEnd, session.TurnEndData{Turn: 3, Reason: session.TurnEndReason{Kind: "max-tokens"}}, nil)
	mustAppend(t, s, session.EventTurnEnd, session.TurnEndData{Turn: 4, Reason: session.TurnEndReason{
		Kind: "error", Error: &llm.LlmFailure{Message: "boom", Code: "E"},
	}}, nil)
	mustAppend(t, s, session.EventAssistantChunk, map[string]any{"turn": 1}, nil)
	mustAppend(t, s, "brand/new-thing", map[string]any{"nested": "payload"}, nil)

	events := s.Events()
	cases := map[int64]string{
		events[0].Seq:  "list the todos",
		events[1].Seq:  "todo_read\n{\"detail\":true}",
		events[2].Seq:  "todo contents\nTimeoutError\nTIMEOUT",
		events[3].Seq:  "done",
		events[4].Seq:  "in_progress\nwrite report",
		events[5].Seq:  "",
		events[6].Seq:  "aborted",
		events[7].Seq:  "max-tokens",
		events[8].Seq:  "error\nboom",
		events[9].Seq:  "",
		events[10].Seq: "",
	}
	for seq, want := range cases {
		if got := ExtractSessionEventText(events[seq]); got != want {
			t.Fatalf("event %d (%s) text = %q, want %q", seq, events[seq].Type, got, want)
		}
	}
}

// --- title fold ------------------------------------------------------------

func titleEventData(title string, seqs []int64, source SessionTitleSource) SessionTitleEventData {
	if seqs == nil {
		seqs = []int64{}
	}
	return SessionTitleEventData{Title: title, MessageSeqs: seqs, Source: source}
}

func TestFoldSessionTitle(t *testing.T) {
	s := newBareSession(t, "titles")
	if FoldSessionTitle(s.Events()) != nil {
		t.Fatal("empty log folded a title")
	}
	first := titleEventData("first", []int64{0, 1}, SessionTitleSource{Kind: TitleSourceFallback})
	mustAppend(t, s, "session/title", first, nil)
	second := titleEventData("second", nil, SessionTitleSource{Kind: TitleSourceProvider, Provider: "ai", Model: &SessionTitleModelProvenance{Provider: "deepseek", Model: "m1"}})
	last := mustAppend(t, s, "session/title", second, nil)
	folded := FoldSessionTitle(s.Events())
	if folded == nil || folded.Title != "second" || folded.EventSeq != last.Seq || folded.UpdatedAt != last.Time {
		t.Fatalf("folded = %+v", folded)
	}
	if len(folded.MessageSeqs) != 0 || folded.MessageSeqs == nil {
		t.Fatalf("messageSeqs = %#v", folded.MessageSeqs)
	}
	if folded.Source.Model == nil || folded.Source.Model.Model != "m1" {
		t.Fatalf("source model = %+v", folded.Source)
	}
	// Detached: mutating the folded seqs cannot touch the log or later folds.
	withSeqs := titleEventData("third", []int64{5, 7}, SessionTitleSource{Kind: TitleSourceUser})
	mustAppend(t, s, "session/title", withSeqs, nil)
	foldedSeqs := FoldSessionTitle(s.Events())
	foldedSeqs.MessageSeqs[0] = 99
	refolded := FoldSessionTitle(s.Events())
	if !sameSeqs(refolded.MessageSeqs, []int64{5, 7}) {
		t.Fatalf("fold is not detached: %#v", refolded.MessageSeqs)
	}
}

func TestNormalizeSessionTitle(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"plain", "  hello   world  ", "hello world"},
		{"osc stripped", "\x1b]0;set title\x07hello", "hello"},
		{"osc c1 unterminated consumes rest", "\u009d]0;set title\u009chello", ""},
		{"osc bel-terminated", "\u009d]0;set x\u0007hello", "hello"},
		{"osc unterminated stripped", "\x1b]0;set title stays gone", ""},
		{"csi stripped", "\x1b[31mred\x1b[0m text", "red text"},
		{"csi c1 stripped", "\u009b31mred\u009b0m text", "red text"},
		{"esc sequence stripped", "a\x1bBb", "ab"},
		{"controls dropped", "a\x00b\x07c", "abc"},
		{"directional dropped", "ab\u200el\u200fop", "ablop"},
		{"c1 csi consumes final byte", "a\u009bb", "a"},
		{"unicode kept", "\u6807\u9898 ok", "\u6807\u9898 ok"},
	}
	for _, testCase := range cases {
		if got := NormalizeSessionTitle(testCase.input, maxSafeInteger); got != testCase.want {
			t.Fatalf("%s: normalize(%q) = %q, want %q", testCase.name, testCase.input, got, testCase.want)
		}
	}
	// Byte budget truncates without splitting a code point.
	multi := "a\u6c49b\u5b57c"
	if got := NormalizeSessionTitle(multi, 5); got != "a\u6c49b" {
		t.Fatalf("byte truncate = %q", got)
	}
	if got := NormalizeSessionTitle(multi, 4); got != "a\u6c49" {
		t.Fatalf("byte truncate exact = %q", got)
	}
	if got := NormalizeSessionTitle(multi, 2); got != "a" {
		t.Fatalf("byte truncate no-split = %q", got)
	}
	if got := FallbackSessionTitle("  The   quick \t brown fox", 2, maxSafeInteger); got != "The quick" {
		t.Fatalf("fallback = %q", got)
	}
	if got := FallbackSessionTitle("one two", 10, 5); got != "one t" {
		t.Fatalf("fallback byte cap = %q", got)
	}
}

func TestCollectSessionTitleMessages(t *testing.T) {
	s := newBareSession(t, "collect")
	appendUserMessage(t, s, "u1", "   ")
	plugin := llm.Message{
		ID: "p1", Role: llm.RoleUser, Source: llm.MessageSource{Kind: llm.SourcePlugin},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "plugin injected"}},
	}
	mustAppend(t, s, session.EventUserMessage, plugin, appendIntent(session.SurfaceOp{Kind: session.SurfaceAppend}))
	second := appendUserMessage(t, s, "u2", "real question")
	messages := CollectSessionTitleMessages(s.Events(), nil)
	if len(messages) != 1 || messages[0].Seq != second.Seq || messages[0].Text != "real question" {
		t.Fatalf("messages = %+v", messages)
	}
	boundary := second.Seq - 1
	if got := CollectSessionTitleMessages(s.Events(), &boundary); len(got) != 0 {
		t.Fatalf("throughSeq boundary leaked: %+v", got)
	}
}

// --- documents & tracing ---------------------------------------------------

// fixtureLog appends a replacement sequence: user -> tool/call (log-only) ->
// tool/result -> replacing tool/result (content-only rewrite) -> assistant ->
// log-only chunk.
func fixtureLog(t *testing.T, id string) *session.Session {
	t.Helper()
	s := newBareSession(t, id)
	appendUserMessage(t, s, "u1", "find the config file")
	call := mustAppend(t, s, session.EventToolCall,
		session.ToolCallData{Turn: 1, Step: 1, CallID: llm.ToolCallID("c1"), Name: "grep", Arguments: "{\"q\":\"config\"}"},
		nil)
	original := llm.Message{
		ID: "r1", Role: llm.RoleUser, Source: llm.MessageSource{Kind: llm.SourceTool, CallID: llm.ToolCallID("c1")},
		Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1",
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "raw dump"}}}},
	}
	result := mustAppend(t, s, session.EventToolResult,
		session.ToolResultData{Turn: 1, Step: 1, Message: original},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceEventSeqs: []int64{call.Seq}, SourceSeqsPresent: true})
	replacement := original
	replacement.Content = []llm.ContentBlock{{Type: llm.BlockToolResult, ToolCallID: "c1",
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "trimmed dump"}}}}
	mustAppend(t, s, session.EventToolResult,
		session.ToolResultData{Turn: 1, Step: 1, Message: replacement},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceReplace, Start: result.Seq, End: result.Seq}, SourceEventSeqs: []int64{result.Seq}, SourceSeqsPresent: true})
	mustAppend(t, s, session.EventAssistantMsg,
		session.AssistantMessageData{Turn: 1, Step: 1, Message: llm.Message{
			ID: "a1", Role: llm.RoleAssistant,
			Source:  llm.MessageSource{Kind: llm.SourceModel, Provider: "deepseek", Model: "m1"},
			Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "found config"}},
		}},
		&session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}, SourceEventSeqs: []int64{0, call.Seq}, SourceSeqsPresent: true})
	mustAppend(t, s, session.EventAssistantChunk, map[string]any{"turn": 1}, nil)
	return s
}

func TestBuildSessionEventRecordsClassification(t *testing.T) {
	s := fixtureLog(t, "surface")
	records, err := BuildSessionEventRecords(s.ID(), s.Events())
	if err != nil {
		t.Fatalf("records failed: %v", err)
	}
	want := []string{SurfaceCurrent, SurfaceLogOnly, SurfaceShadowed, SurfaceCurrent, SurfaceCurrent, SurfaceLogOnly}
	for index, surface := range want {
		if records[index].Surface != surface {
			t.Fatalf("record %d surface = %q, want %q", index, records[index].Surface, surface)
		}
	}
	if records[0].SessionID != "surface" {
		t.Fatalf("sessionId = %q", records[0].SessionID)
	}
}

func TestCurrentSurfaceEventsOrder(t *testing.T) {
	s := fixtureLog(t, "surface")
	events, err := CurrentSurfaceEvents(s.ID(), s.Events())
	if err != nil {
		t.Fatalf("surface failed: %v", err)
	}
	want := []string{session.EventUserMessage, session.EventToolResult, session.EventAssistantMsg}
	if len(events) != len(want) {
		t.Fatalf("surface size = %d", len(events))
	}
	for index, eventType := range want {
		if events[index].Type != eventType {
			t.Fatalf("surface[%d] = %s, want %s", index, events[index].Type, eventType)
		}
	}
}

func TestBuildSessionEventSearchDocumentsOmitsStructural(t *testing.T) {
	s := fixtureLog(t, "docs")
	documents, err := BuildSessionEventSearchDocuments(s.ID(), s.Events())
	if err != nil {
		t.Fatalf("documents failed: %v", err)
	}
	// user message, tool/call, replacing tool/result, shadowed tool/result,
	// and assistant message carry text; the chunk stays structural.
	want := map[int64]bool{}
	for _, document := range documents {
		want[document.Seq] = true
		if document.Text == "" {
			t.Fatalf("document at %d has empty text", document.Seq)
		}
	}
	if !want[0] || !want[1] || !want[3] || !want[4] || want[5] {
		t.Fatalf("document selection wrong: %v", want)
	}
	shadowed := documents[2]
	if shadowed.Surface != SurfaceShadowed || !strings.Contains(shadowed.Text, "raw dump") {
		t.Fatalf("shadowed document = %+v", shadowed)
	}
}

func TestTraceEventRelationships(t *testing.T) {
	s := fixtureLog(t, "trace")
	events := s.Events()
	trace, err := TraceEvent(s.ID(), events, 2)
	if err != nil {
		t.Fatalf("trace failed: %v", err)
	}
	if trace.ReplacedBy == nil || *trace.ReplacedBy != 3 {
		t.Fatalf("replacedBy = %v", trace.ReplacedBy)
	}
	if !sameSeqs(trace.ReplacementChain, []int64{3}) {
		t.Fatalf("chain = %v", trace.ReplacementChain)
	}
	if !sameSeqs(trace.DerivedEventSeqs, []int64{3}) {
		t.Fatalf("derived = %v", trace.DerivedEventSeqs)
	}
	if !sameSeqs(trace.SourceEventSeqs, []int64{1}) {
		t.Fatalf("sources = %v", trace.SourceEventSeqs)
	}
	if trace.Target.Surface != SurfaceShadowed {
		t.Fatalf("surface = %q", trace.Target.Surface)
	}
	replacer, err := TraceEvent(s.ID(), events, 3)
	if err != nil {
		t.Fatalf("replacer trace failed: %v", err)
	}
	if replacer.ReplacedBy != nil {
		t.Fatalf("replacer replacedBy = %v", replacer.ReplacedBy)
	}
	if !sameSeqs(replacer.ReplacedEventSeqs, []int64{2}) {
		t.Fatalf("replacedEventSeqs = %v", replacer.ReplacedEventSeqs)
	}
	if _, err := TraceEvent(s.ID(), events, 99); err == nil {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeEventNotFound {
			t.Fatalf("missing event error = %v", err)
		}
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeEventNotFound {
			t.Fatalf("missing event error = %v", err)
		}
	}
}

func TestTraceSessionLineage(t *testing.T) {
	mk := func(id, parent string, createdAt int64) SessionRecord {
		record := SessionRecord{Header: session.SessionHeader{ID: id, CreatedAt: createdAt}, Live: true}
		if parent != "" {
			record.Header.ParentSession = parent
		}
		return record
	}
	records := []SessionRecord{
		mk("child1", "root", 30),
		mk("child2", "root", 10),
		mk("grand", "child2", 40),
		mk("root", "", 1),
	}
	trace, err := TraceSession(records, "child2")
	if err != nil {
		t.Fatalf("trace failed: %v", err)
	}
	if !trace.Complete || trace.Root == nil || trace.Root.Header.ID != "root" {
		t.Fatalf("complete trace = %+v", trace)
	}
	if len(trace.Ancestors) != 1 || trace.Ancestors[0].Header.ID != "root" {
		t.Fatalf("ancestors = %+v", trace.Ancestors)
	}
	if len(trace.Descendants) != 1 || trace.Descendants[0].Session.Header.ID != "grand" {
		t.Fatalf("descendants = %+v", trace.Descendants)
	}
	// Without root in the corpus, grand's chain leaves at child2's missing
	// parent.
	partial, err := TraceSession(records[:3], "grand")
	if err != nil {
		t.Fatalf("grand trace failed: %v", err)
	}
	if partial.Complete || partial.UnresolvedParentID != "root" {
		t.Fatalf("partial = %+v", partial)
	}
	if len(partial.Ancestors) != 1 || partial.Ancestors[0].Header.ID != "child2" {
		t.Fatalf("partial ancestors = %+v", partial.Ancestors)
	}
	// child1's parent (root) is absent, so its trace stays partial rather
	// than failing; an absent TARGET is the not-found case.
	partialChild1, err := TraceSession(records[:2], "child1")
	if err != nil {
		t.Fatalf("child1 trace failed: %v", err)
	}
	if partialChild1.Complete || partialChild1.UnresolvedParentID != "root" || partialChild1.Root != nil {
		t.Fatalf("child1 partial = %+v", partialChild1)
	}
	if _, err := TraceSession(records[:2], "root"); err == nil {
		t.Fatal("missing target accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeSessionNotFound {
			t.Fatalf("missing target error = %v", err)
		}
	}
	cycled := []SessionRecord{mk("x", "y", 1), mk("y", "x", 2)}
	if _, err := TraceSession(cycled, "x"); err == nil {
		t.Fatal("lineage cycle accepted")
	} else {
		var queryErr *SessionQueryError
		if !errors.As(err, &queryErr) || queryErr.Code != CodeInvalidLineage {
			t.Fatalf("cycle error = %v", err)
		}
	}
}

var _ = context.Background
