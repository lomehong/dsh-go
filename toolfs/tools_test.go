package toolfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/llm"
	"dshgo/tools"
)

func newHarness(t *testing.T, caps Caps) (*tools.ToolRuntime, *cordis.Context, string, func()) {
	t.Helper()
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	undo, err := Register(runtime, RegisterDeps{Backend: backend, Ctx: root}, caps)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, root, backend.Cwd(), undo
}

func harnessWithRegistry(t *testing.T, caps Caps) (*tools.ToolRuntime, *cordis.Context, string, *agent.AgentRegistry, func()) {
	t.Helper()
	backend, err := fslocal.New(fslocal.Config{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	root := cordis.NewRoot(cordis.Discard{})
	runtime, err := tools.NewToolRuntime(cordis.Discard{}, tools.Config{})
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewAgentRegistry(root, cordis.Discard{})
	undo, err := Register(runtime, RegisterDeps{
		Backend: backend, Ctx: root,
		Agents:          RegistryAgentSource{Registry: registry},
		PermissionFolds: func(*agent.Agent) string { return "" },
	}, caps)
	if err != nil {
		t.Fatal(err)
	}
	return runtime, root, backend.Cwd(), registry, undo
}

func execute(t *testing.T, runtime *tools.ToolRuntime, name string, args map[string]any) (any, error) {
	t.Helper()
	definition, ok := runtime.Get(name, nil)
	if !ok {
		t.Fatalf("%s must be registered", name)
	}
	return definition.Execute(args, &tools.ToolRunContext{})
}

// renderText drives the canonical dispatch pipeline and returns the
// model-facing text for one successful call.
func renderText(t *testing.T, runtime *tools.ToolRuntime, name string, args map[string]any) string {
	t.Helper()
	result := runtime.Execute(&tools.ToolExecutionInput{Name: name, Arguments: args, Signal: context.Background()})
	if result.IsError {
		t.Fatalf("%s dispatch failed: %+v", name, result.Error)
	}
	if len(result.Content) != 1 || result.Content[0].Type != llm.BlockText {
		t.Fatalf("render must yield one text block: %+v", result.Content)
	}
	return result.Content[0].Text
}

func TestReadWindowsLinesBytesAndOffset(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t, DefaultCaps())
	defer undo()
	file := filepath.Join(cwd, "doc.txt")
	var builder strings.Builder
	for index := 1; index <= 30; index++ {
		builder.WriteString(strings.Repeat("x", 10) + "\n")
	}
	if err := os.WriteFile(file, []byte(builder.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := execute(t, runtime, "read", map[string]any{"file_path": file, "offset": float64(5), "limit": float64(3)})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	outcome := result.(map[string]any)
	rawLines := outcome["lines"].([]any)
	if len(rawLines) != 3 {
		t.Fatalf("window size: %d", len(rawLines))
	}
	first := rawLines[0].(map[string]any)
	third := rawLines[2].(map[string]any)
	if first["number"].(int) != 5 || third["number"].(int) != 7 {
		t.Fatalf("window: %+v", rawLines)
	}
	if outcome["totalLines"].(int) != 30 {
		t.Fatalf("totalLines: %v", outcome["totalLines"])
	}

	// Render carries the envelope, numbering, and continuation footer.
	rendered := renderText(t, runtime, "read", map[string]any{"file_path": file, "offset": float64(5), "limit": float64(3)})
	if !strings.Contains(rendered, "<path>"+file+"</path>") || !strings.Contains(rendered, "5: xxxxxxxxxx") {
		t.Fatalf("envelope: %q", rendered)
	}
	if !strings.Contains(rendered, "(Showing lines 5-7 of 30. Use offset=8 to continue.)") {
		t.Fatalf("footer: %q", rendered)
	}

	// Past-EOF offset is FS_NOT_FOUND with the official text.
	_, err = execute(t, runtime, "read", map[string]any{"file_path": file, "offset": float64(99)})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotFound {
		t.Fatalf("offset past EOF: %v", err)
	}
	if !strings.Contains(err.Error(), `offset 99 is out of range for`) {
		t.Fatalf("offset text: %v", err)
	}

	// The byte cap trips truncation and the footer changes.
	capped := Caps{Limit: 2000, MaxLineLength: 2000, MaxBytes: 40, StreamMinSize: StreamMinSize}
	runtime2, _, cwd2, undo2 := newHarness(t, capped)
	defer undo2()
	big := filepath.Join(cwd2, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("0123456789", 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, runtime2, "read", map[string]any{"file_path": big}); err != nil {
		t.Fatalf("capped read: %v", err)
	}
	rendered2 := renderText(t, runtime2, "read", map[string]any{"file_path": big})
	if !strings.Contains(rendered2, "(Output capped.") {
		t.Fatalf("capped footer: %q", rendered2)
	}

	// Over-long lines truncate with the marker (the line must FIT the byte
	// cap but exceed the line-length cap, or the byte cap wins first).
	lineCapped := Caps{Limit: 2000, MaxLineLength: 20, MaxBytes: ReadMaxBytes, StreamMinSize: StreamMinSize}
	runtime3, _, cwd3, undo3 := newHarness(t, lineCapped)
	defer undo3()
	if err := os.WriteFile(filepath.Join(cwd3, "long.txt"), []byte(strings.Repeat("y", 40)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t, runtime3, "read", map[string]any{"file_path": filepath.Join(cwd3, "long.txt")}); err != nil {
		t.Fatalf("long-line read: %v", err)
	}
	rendered3 := renderText(t, runtime3, "read", map[string]any{"file_path": filepath.Join(cwd3, "long.txt")})
	if !strings.Contains(rendered3, "... (line truncated to 20 chars)") {
		t.Fatalf("line truncation marker missing: %q", rendered3)
	}
}

func TestWriteAndUpdateWordingAndRemediation(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t, DefaultCaps())
	defer undo()
	file := filepath.Join(cwd, "doc.txt")

	// The render dispatch performs the create (mutating calls are not
	// re-dispatched for wording checks).
	created := renderText(t, runtime, "write", map[string]any{"file_path": file, "content": "v1"})
	if !strings.Contains(created, "Created file") {
		t.Fatalf("create wording: %q", created)
	}
	stored, err := os.ReadFile(file)
	if err != nil || string(stored) != "v1" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}
	// Rewrite with the same content: an update, idempotent for re-render.
	updated := renderText(t, runtime, "write", map[string]any{"file_path": file, "content": "v2"})
	if !strings.Contains(updated, "Updated file") {
		t.Fatalf("update wording: %q", updated)
	}
	stored, err = os.ReadFile(file)
	if err != nil || string(stored) != "v2" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}

	// Blank path fails plain.
	if _, err := execute(t, runtime, "write", map[string]any{"file_path": "  ", "content": "x"}); err == nil {
		t.Fatal("blank path must fail")
	}
}

func TestEditGuardsRemediesAndReplaceAll(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t, DefaultCaps())
	defer undo()
	file := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(file, []byte("aaa\nbbb\naaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Equal pair rejected.
	if _, err := execute(t, runtime, "edit", map[string]any{"file_path": file, "old_string": "same", "new_string": "same"}); err == nil ||
		!strings.Contains(err.Error(), "must differ") {
		t.Fatalf("equal pair: %v", err)
	}
	// Empty old_string rejected.
	if _, err := execute(t, runtime, "edit", map[string]any{"file_path": file, "old_string": "", "new_string": "x"}); err == nil {
		t.Fatal("empty old_string must fail")
	}
	// Unread file: the edit-intent default is unconditional here (no policy
	// plugin), so this succeeds — the observation-policy round owns the
	// FS_NOT_OBSERVED refusal. The render pipeline run PERFORMS the edit
	// (mutating calls are not re-dispatched for wording checks).
	single := renderText(t, runtime, "edit", map[string]any{"file_path": file, "old_string": "bbb", "new_string": "B"})
	if !strings.Contains(single, "has been updated successfully.") {
		t.Fatalf("single wording: %q", single)
	}
	stored, err := os.ReadFile(file)
	if err != nil || string(stored) != "aaa\nB\naaa\n" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}
	// Multiple matches without replace_all fail FS_AMBIGUOUS_EDIT with the
	// provider's uniqueness guidance — no remedy is appended for it.
	_, err = execute(t, runtime, "edit", map[string]any{"file_path": file, "old_string": "aaa", "new_string": "z"})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeAmbiguousEdit {
		t.Fatalf("ambiguous edit: %v", err)
	}
	if !strings.Contains(err.Error(), "more specific old_string") {
		t.Fatalf("ambiguity guidance: %v", err)
	}
	// replace_all succeeds with the all-occurrences wording (checked on a
	// fresh file so the render dispatch is the first and only application).
	other := filepath.Join(cwd, "other.txt")
	if err := os.WriteFile(other, []byte("aaa\nbbb\naaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	all := renderText(t, runtime, "edit", map[string]any{"file_path": other, "old_string": "aaa", "new_string": "z", "replace_all": true})
	if !strings.Contains(all, "All occurrences were successfully replaced.") {
		t.Fatalf("replace-all wording: %q", all)
	}
	stored, err = os.ReadFile(other)
	if err != nil || string(stored) != "z\nbbb\nz\n" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}
}

func TestDirectoryReadFailsNotRegularFile(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t, DefaultCaps())
	defer undo()
	_, err := execute(t, runtime, "read", map[string]any{"file_path": cwd})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotRegularFile {
		t.Fatalf("directory read: %v", err)
	}
	// Absent file records the negative observation and fails not-found.
	if _, err := execute(t, runtime, "read", map[string]any{"file_path": filepath.Join(cwd, "ghost.txt")}); err == nil {
		t.Fatal("absent read must fail")
	}
}

func TestLangFromPath(t *testing.T) {
	cases := map[string]string{
		"a.go": "go", "b.TS": "ts", "c.yaml": "yaml",
		".gitignore": "", "d.unknown": "", "noext": "",
	}
	for input, want := range cases {
		if got := LangFromPath(input); got != want {
			t.Fatalf("LangFromPath(%q) = %q, want %q", input, got, want)
		}
	}
}
