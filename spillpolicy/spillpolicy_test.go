package spillpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/spill"
	"dshgo/tools"
)

// fakeStore captures saves and serves one reference.
type fakeStore struct {
	saves   []spill.SaveTextSpill
	ref     spill.SpillRef
	err     error
	present bool
}

func (f *fakeStore) SaveText(ctx context.Context, input spill.SaveTextSpill) (spill.SpillRef, error) {
	f.saves = append(f.saves, input)
	if f.err != nil {
		return spill.SpillRef{}, f.err
	}
	return f.ref, nil
}

func capOf(n int) *int { return &n }

func ownerOf(sessionID string) func(tools.ScopeKey) (string, bool) {
	return func(tools.ScopeKey) (string, bool) { return sessionID, true }
}

func newRuntime(t *testing.T) *tools.ToolRuntime {
	t.Helper()
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	echo, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "web_fetch", Description: "fetch",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: fmt.Sprintf("%v", value)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"body": strings.Repeat("x", 5000)}, nil
		},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := runtime.Register(echo); err != nil {
		t.Fatalf("register: %v", err)
	}
	return runtime
}

func execute(t *testing.T, runtime *tools.ToolRuntime, name string) *tools.ToolExecutionResult {
	t.Helper()
	return runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c1", Name: name,
		Arguments: map[string]any{},
		Agent:     scope.NewScopeKey(nil),
		Signal:    context.Background(),
	})
}

func textResult(runtime *tools.ToolRuntime, name string, text string) *tools.ToolExecutionResult {
	// Register a one-shot tool rendering the exact text.
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: name, Description: "fixed text",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: value.(string)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return text, nil
		},
	})
	if err != nil {
		panic(err)
	}
	if _, err := runtime.Register(definition); err != nil {
		panic(err)
	}
	return runtime.Execute(&tools.ToolExecutionInput{
		CallID: "c-" + name, Name: name,
		Arguments: map[string]any{},
		Agent:     scope.NewScopeKey(nil),
		Signal:    context.Background(),
	})
}

func TestNilCapRegistersNothingAndNegativeFailsLoud(t *testing.T) {
	runtime := newRuntime(t)
	detach, err := Attach(runtime, &fakeStore{present: true}, cordis.Discard{}, Config{}, ownerOf("s1"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	result := textResult(runtime, "big_nil", strings.Repeat("y", 2000))
	if len(result.Content) == 0 || result.Content[0].Text != strings.Repeat("y", 2000) {
		t.Fatal("nil-cap policy touched the result")
	}
	err = ValidateConfig(Config{MaxInlineBytes: capOf(-1)})
	if err == nil || err.Error() != "spill-policy: maxInlineBytes must be a non-negative integer (got -1)" {
		t.Fatalf("err = %v", err)
	}
}

func TestOversizedPlainTextSpillsAndBoundsPreview(t *testing.T) {
	store := &fakeStore{ref: spill.SpillRef{
		Locator: "spill:///artifacts/1", Bytes: 0,
		RetrievalHint: "Use read with offset/limit, or grep this path to search within it.",
	}}
	runtime := newRuntime(t)
	cap := 300
	detach, err := Attach(runtime, store, cordis.Discard{}, Config{MaxInlineBytes: capOf(cap)}, ownerOf("session-a"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	body := strings.Repeat("A", 250) + strings.Repeat("B", 250) + strings.Repeat("C", 10)
	result := textResult(runtime, "big_text", body)
	if result.IsError {
		t.Fatalf("spill turned the call into an error: %+v", result.Error)
	}
	if len(result.Content) != 1 || result.Content[0].Type != llm.BlockText {
		t.Fatalf("content = %+v", result.Content)
	}
	replaced := result.Content[0].Text
	if len(replaced) > cap {
		t.Fatalf("replacement is %d bytes, cap %d", len(replaced), cap)
	}
	if !strings.Contains(replaced, "\n\n(Omitted ") || !strings.HasSuffix(replaced, "bytes. Full formatted result stored at: spill:///artifacts/1. Use read with offset/limit, or grep this path to search within it.)") {
		t.Fatalf("notice = %q", replaced)
	}
	// Head and tail both survive; the long middle run is gone.
	if !strings.Contains(replaced, "AAAA") || !strings.Contains(replaced, "CCCCC") ||
		strings.Contains(replaced, strings.Repeat("A", 200)) {
		t.Fatalf("preview shape = %q", replaced)
	}
	// The FULL text reached the backend, tagged with owner and source.
	if len(store.saves) != 1 || store.saves[0].Content != body {
		t.Fatalf("saved = %+v", store.saves)
	}
	if store.saves[0].Owner.SessionID != "session-a" || store.saves[0].Source.ToolName != "big_text" ||
		store.saves[0].Source.CallID != "c-big_text" || store.saves[0].Source.Label != "result" ||
		store.saves[0].SuggestedName != "big_text.txt" {
		t.Fatalf("save = %+v", store.saves[0])
	}
	// A within-cap result is untouched.
	small := textResult(runtime, "small_text", strings.Repeat("z", 100))
	if len(small.Content) != 1 || small.Content[0].Text != strings.Repeat("z", 100) {
		t.Fatal("within-cap result touched")
	}
	if len(store.saves) != 1 {
		t.Fatalf("within-cap result spilled: %+v", store.saves)
	}
}

func TestBestEffortDegradationsKeepInline(t *testing.T) {
	body := strings.Repeat("q", 1000)
	cases := map[string]spill.Store{
		"no backend":  nil,
		"save failed": &fakeStore{err: errors.New("ENOSPC")},
	}
	for name, store := range cases {
		runtime := newRuntime(t)
		detach, err := Attach(runtime, store, cordis.Discard{}, Config{MaxInlineBytes: capOf(100)}, ownerOf("s"))
		if err != nil {
			t.Fatalf("%s attach: %v", name, err)
		}
		result := textResult(runtime, "tool_"+strings.ReplaceAll(name, " ", "_"), body)
		if len(result.Content) != 1 || result.Content[0].Text != body {
			t.Fatalf("%s hid the inline content", name)
		}
		detach()
	}
	// No session owner: same containment.
	runtime := newRuntime(t)
	detach, err := Attach(runtime, &fakeStore{}, cordis.Discard{}, Config{MaxInlineBytes: capOf(100)}, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	if result := textResult(runtime, "ownerless", body); result.Content[0].Text != body {
		t.Fatal("ownerless result replaced")
	}
}

func TestSkipsNonTextValueReplacementsBlocksReadAndNested(t *testing.T) {
	store := &fakeStore{}
	runtime := newRuntime(t)
	cap := 100
	detach, err := Attach(runtime, store, cordis.Discard{}, Config{MaxInlineBytes: capOf(cap)}, ownerOf("s"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()

	// Non-text block: untouched.
	mixed, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "mixed_tool", Description: "mixed",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{
					{Type: llm.BlockText, Text: strings.Repeat("t", 500)},
					{Type: "image", Attachment: "png-bytes"},
				}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return map[string]any{"mixed": true}, nil
		},
	})
	if err != nil {
		t.Fatalf("define: %v", err)
	}
	if _, err := runtime.Register(mixed); err != nil {
		t.Fatalf("register: %v", err)
	}
	result := runtime.Execute(&tools.ToolExecutionInput{CallID: "m1", Name: "mixed_tool", Arguments: map[string]any{}, Signal: context.Background()})
	if len(result.Content) != 2 {
		t.Fatalf("mixed result reshaped: %+v", result.Content)
	}
	if len(store.saves) != 0 {
		t.Fatal("mixed content spilled")
	}

	// read is skipped by the model-facing arm.
	readTool, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "read", Description: "read",
		Parameters: map[string]tools.PropSpec{},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{Type: "json"},
			Render: func(_ map[string]any, value any) []llm.ContentBlock {
				return []llm.ContentBlock{{Type: llm.BlockText, Text: value.(string)}}
			},
		},
		Execute: func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
			return strings.Repeat("r", 900), nil
		},
	})
	if err != nil {
		t.Fatalf("define read: %v", err)
	}
	if _, err := runtime.Register(readTool); err != nil {
		t.Fatalf("register read: %v", err)
	}
	if result := runtime.Execute(&tools.ToolExecutionInput{CallID: "r1", Name: "read", Arguments: map[string]any{}, Signal: context.Background()}); result.Content[0].Text != strings.Repeat("r", 900) {
		t.Fatal("read result spilled")
	}
	if len(store.saves) != 0 {
		t.Fatalf("read spilled: %+v", store.saves)
	}
}

func TestNoticeExceedingCapKeepsInline(t *testing.T) {
	store := &fakeStore{ref: spill.SpillRef{
		Locator:       "spill:///very/long/root/path/that/alone/exceeds/any/sane/budget/artifact",
		RetrievalHint: "Use read with offset/limit, or grep this path to search within it.",
	}}
	runtime := newRuntime(t)
	detach, err := Attach(runtime, store, cordis.Discard{}, Config{MaxInlineBytes: capOf(40)}, ownerOf("s"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	body := strings.Repeat("n", 900)
	result := textResult(runtime, "tiny_cap", body)
	if result.Content[0].Text != body {
		t.Fatalf("notice-over-cap replacement emitted: %q", result.Content[0].Text)
	}
	// The spilled file is a harmless orphan; cleanup is deferred.
	if len(store.saves) != 1 {
		t.Fatalf("saves = %d", len(store.saves))
	}
}

func TestReplacementSurvivesMultibyteBoundaries(t *testing.T) {
	store := &fakeStore{ref: spill.SpillRef{Locator: "spill:///u", RetrievalHint: "hint"}}
	runtime := newRuntime(t)
	cap := 300
	detach, err := Attach(runtime, store, cordis.Discard{}, Config{MaxInlineBytes: capOf(cap)}, ownerOf("s"))
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer detach()
	body := strings.Repeat("日本語テキスト", 40)
	result := textResult(runtime, "unicode_tool", body)
	replaced := result.Content[0].Text
	if replaced == body {
		t.Fatal("oversized multibyte result kept inline")
	}
	if !utf8.ValidString(replaced) || strings.ContainsRune(replaced, 0xFFFD) {
		t.Fatalf("replacement broke UTF-8: %q", replaced)
	}
	if len(replaced) > cap {
		t.Fatalf("replacement %d > cap %d", len(replaced), cap)
	}
	// The full multibyte text reached the backend intact.
	if len(store.saves) != 1 || store.saves[0].Content != body {
		t.Fatalf("saved = %+v", store.saves)
	}
}
