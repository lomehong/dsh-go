package sessionreference

import (
	"fmt"
	"strings"
	"testing"

	"dshgo/llm"
)

// stubReader serves one in-memory surface per session id.
type stubReader struct {
	surfaces map[string]SessionSnapshot
	err      map[string]error
	records  []SessionRecord
}

func (r *stubReader) ReadSurface(sessionID string) (SessionSnapshot, error) {
	if err := r.err[sessionID]; err != nil {
		return SessionSnapshot{}, err
	}
	snapshot, ok := r.surfaces[sessionID]
	if !ok {
		return SessionSnapshot{}, fmt.Errorf("session %q not found", sessionID)
	}
	return snapshot, nil
}

func (r *stubReader) ListSessions() ([]SessionRecord, error) {
	return r.records, nil
}

func TestNormalizeReferences(t *testing.T) {
	if _, err := NormalizeReferences("self", []Input{{SessionID: "self"}}, 3); err == nil ||
		!strings.Contains(err.Error(), "cannot reference itself") {
		t.Fatalf("self = %v", err)
	}
	normalized, err := NormalizeReferences("me", []Input{
		{SessionID: "a"},
		{SessionID: "b", Label: "bee"},
		{SessionID: "a"}, // duplicate dropped
	}, 3)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(normalized) != 2 || normalized[0].Label != "a" || normalized[1].Label != "bee" {
		t.Fatalf("normalized = %+v", normalized)
	}
	if _, err := NormalizeReferences("me", []Input{{SessionID: "1"}, {SessionID: "2"}, {SessionID: "3"}, {SessionID: "4"}}, 3); err == nil ||
		!strings.Contains(err.Error(), "at most 3 sessions") {
		t.Fatalf("too many = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	if _, err := NewResolver(Config{MaxReferences: 0, CandidateLimit: 1, MaxReferenceBytes: 1}, nil, nil); err == nil {
		t.Fatal("zero maxReferences accepted")
	}
	if _, err := NewResolver(Config{MaxReferences: 4, CandidateLimit: 1, MaxReferenceBytes: 1}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "must not exceed 3") {
		t.Fatalf("over max = %v", err)
	}
	if _, err := NewResolver(DefaultConfig(), &stubReader{}, nil); err != nil {
		t.Fatalf("default rejected: %v", err)
	}
}

func TestPrepareBuildsDurableContext(t *testing.T) {
	reader := &stubReader{surfaces: map[string]SessionSnapshot{
		"a": {SessionID: "a", Cwd: "/w", Events: []SurfaceEvent{userEvent("hello there")}},
	}}
	resolver, err := NewResolver(DefaultConfig(), reader, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	prepared, err := resolver.Prepare("me", []llm.ContentBlock{{Type: llm.BlockText, Text: "see @a"}}, []Input{{SessionID: "a"}})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ctx := prepared.AdditionalContext
	if ctx == nil || ctx.Role != llm.RoleUser {
		t.Fatalf("context = %+v", ctx)
	}
	if ctx.Source.Kind != "session-reference" || ctx.Source.Form != "recall" || ctx.Source.ReferenceVersion != 1 {
		t.Fatalf("source = %+v", ctx.Source)
	}
	if len(ctx.Source.References) != 1 || ctx.Source.References[0].SessionID != "a" || ctx.Source.References[0].InputIndex != 0 {
		t.Fatalf("references = %+v", ctx.Source.References)
	}
	text := ctx.Content[0].Text
	if !strings.HasPrefix(text, "## Referenced sessions") || !strings.Contains(text, "<referenced-sessions>") ||
		!strings.Contains(text, `"sessionId":"a"`) || !strings.HasSuffix(text, "</referenced-sessions>") {
		t.Fatalf("prompt = %q", text)
	}
	// No references → content passes through untouched, no context.
	empty, err := resolver.Prepare("me", []llm.ContentBlock{{Type: llm.BlockText, Text: "plain"}}, nil)
	if err != nil || empty.AdditionalContext != nil {
		t.Fatalf("empty = %+v %v", empty, err)
	}
	// Read failure is a typed read failure.
	broken, err := NewResolver(DefaultConfig(), &stubReader{err: map[string]error{"a": fmt.Errorf("disk gone")}}, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if _, err := broken.Prepare("me", nil, []Input{{SessionID: "a"}}); err == nil ||
		!strings.Contains(err.Error(), "failed to read referenced session") {
		t.Fatalf("read = %v", err)
	}
	// A snapshot that cannot fit the budget is a typed budget failure.
	tiny, err := NewResolver(Config{MaxReferences: 3, CandidateLimit: 1, MaxReferenceBytes: 5}, reader, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	if _, err := tiny.Prepare("me", nil, []Input{{SessionID: "a"}}); err == nil ||
		!strings.Contains(err.Error(), "cannot fit the configured byte budget") {
		t.Fatalf("budget = %v", err)
	}
}

func TestPrepareDirectMessagesSplitsMentions(t *testing.T) {
	reader := &stubReader{surfaces: map[string]SessionSnapshot{
		"a": {SessionID: "a", Events: []SurfaceEvent{userEvent("snapshot body")}},
	}}
	resolver, err := NewResolver(DefaultConfig(), reader, nil)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	direct := llm.Message{Role: llm.RoleUser, Source: llm.MessageSource{Kind: "user"},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "look " + FormatSessionReferenceMention(Input{SessionID: "a"})}}}
	assistant := llm.Message{Role: llm.RoleAssistant, Source: llm.MessageSource{Kind: "model"}}
	out, err := resolver.PrepareDirectMessages("me", []llm.Message{direct, assistant})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// The direct message loses its mention token, and the snapshot context
	// lands immediately after it; the assistant message is untouched.
	if len(out) != 3 {
		t.Fatalf("messages = %d", len(out))
	}
	if out[0].Content[0].Text != "look @a" {
		t.Fatalf("direct = %q", out[0].Content[0].Text)
	}
	if out[1].Source.Kind != "session-reference" {
		t.Fatalf("context = %+v", out[1].Source)
	}
	if out[2].Role != llm.RoleAssistant {
		t.Fatalf("assistant = %+v", out[2])
	}
	// A malformed mention fails the whole batch.
	bad := llm.Message{Role: llm.RoleUser, Source: llm.MessageSource{Kind: "user"},
		Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "@[x](dsh-session:!!!)"}}}
	if _, err := resolver.PrepareDirectMessages("me", []llm.Message{bad}); err == nil {
		t.Fatal("malformed mention accepted")
	}
}

func TestListCandidatesRanksWorkspaceAffinity(t *testing.T) {
	reader := &stubReader{records: []SessionRecord{
		{ID: "elsewhere", Cwd: "/other", CreatedAt: 1},
		{ID: "same", Cwd: "/work", CreatedAt: 2},
		{ID: "me", Cwd: "/work", CreatedAt: 3}, // self excluded
		{ID: "cold", Cwd: "", CreatedAt: 4},
	}}
	resolver, err := NewResolver(DefaultConfig(), reader, func(record SessionRecord) (string, bool) {
		if record.ID == "same" {
			return "Same Session", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	candidates, err := resolver.ListCandidates("me", "/work", "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(candidates) != 3 || candidates[0].SessionID != "same" || candidates[1].SessionID != "cold" || candidates[2].SessionID != "elsewhere" {
		t.Fatalf("order = %+v", candidates)
	}
	if !candidates[0].SameWorkspace || candidates[1].SameWorkspace || candidates[2].SameWorkspace {
		t.Fatalf("sameWorkspace = %+v", candidates)
	}
	// The projected title labels and filters; untitled sessions answer by id.
	byTitle, err := resolver.ListCandidates("me", "/work", "same ses", 10)
	if err != nil || len(byTitle) != 1 || byTitle[0].SessionID != "same" || byTitle[0].Label != "Same Session" {
		t.Fatalf("title filter = %+v %v", byTitle, err)
	}
	// A limit caps the ranked result.
	capped, err := resolver.ListCandidates("me", "/work", "", 1)
	if err != nil || len(capped) != 1 || capped[0].SessionID != "same" {
		t.Fatalf("capped = %+v %v", capped, err)
	}
	// A non-positive limit fails loud.
	if _, err := resolver.ListCandidates("me", "/work", "", 0); err == nil {
		t.Fatal("zero limit accepted")
	}
	// The remote face attaches canonical mentions.
	mentions, err := resolver.ListMentionCandidates("me", "/work", "")
	if err != nil || len(mentions) != 3 {
		t.Fatalf("mentions = %+v %v", mentions, err)
	}
	for _, candidate := range mentions {
		want := "@[" + candidate.Label + "](" + EncodeSessionReferenceURI(candidate.SessionID) + ")"
		if candidate.Mention != want {
			t.Fatalf("mention = %q want %q", candidate.Mention, want)
		}
	}
}
