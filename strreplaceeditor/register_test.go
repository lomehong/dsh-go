package strreplaceeditor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/fs"
	"dshgo/fslocal"
	"dshgo/tools"
)

func newHarness(t *testing.T) (*tools.ToolRuntime, *cordis.Context, string, func()) {
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
	undo, err := Register(runtime, Deps{FS: backend, Ctx: root}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, root, backend.Cwd(), undo
}

func execute(t *testing.T, runtime *tools.ToolRuntime, args map[string]any) (any, error) {
	t.Helper()
	definition, ok := runtime.Get("str_replace_editor", nil)
	if !ok {
		t.Fatal("str_replace_editor must be registered")
	}
	return definition.Execute(args, &tools.ToolRunContext{})
}

func TestViewCreateAndListDirectory(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t)
	defer undo()
	ctx := context.Background()

	// create
	created, err := execute(t, runtime, map[string]any{
		"command": "create", "path": filepath.Join(cwd, "doc.txt"),
		"file_text": "alpha\nbeta\ngamma\n",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(created.(string), "New file created successfully") {
		t.Fatalf("create text: %v", created)
	}
	// create onto existing fails plain.
	if _, err := execute(t, runtime, map[string]any{
		"command": "create", "path": filepath.Join(cwd, "doc.txt"), "file_text": "x",
	}); err == nil || strings.Contains(err.Error(), "FS_") {
		t.Fatalf("create-on-existing must be a plain usage error: %v", err)
	}

	// view full file with line numbers.
	view, err := execute(t, runtime, map[string]any{"command": "view", "path": filepath.Join(cwd, "doc.txt")})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	text := view.(string)
	if !strings.Contains(text, "     1  alpha") || !strings.Contains(text, "total of 4 lines") {
		t.Fatalf("view text: %q", text)
	}

	// view_range [2, -1] shows from line 2 to the end.
	ranged, err := execute(t, runtime, map[string]any{
		"command": "view", "path": filepath.Join(cwd, "doc.txt"), "view_range": []any{float64(2), float64(-1)},
	})
	if err != nil {
		t.Fatalf("ranged view: %v", err)
	}
	if !strings.Contains(ranged.(string), "with view_range=[2, -1]") || strings.Contains(ranged.(string), "  alpha") {
		t.Fatalf("ranged view text: %q", ranged.(string))
	}

	// invalid view_range fails plain.
	if _, err := execute(t, runtime, map[string]any{
		"command": "view", "path": filepath.Join(cwd, "doc.txt"), "view_range": []any{float64(9), float64(9)},
	}); err == nil {
		t.Fatal("out-of-range view must fail")
	}

	// directory view lists the tree, excluding dotfiles.
	if err := os.Mkdir(filepath.Join(cwd, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "sub", "leaf.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	listing, err := execute(t, runtime, map[string]any{"command": "view", "path": cwd})
	if err != nil {
		t.Fatalf("dir view: %v", err)
	}
	listed := listing.(string)
	if !strings.Contains(listed, "f\t"+filepath.Join(cwd, "doc.txt")) || !strings.Contains(listed, "d\t"+filepath.Join(cwd, "sub")) {
		t.Fatalf("listing: %q", listed)
	}
	if strings.Contains(listed, ".hidden") {
		t.Fatalf("hidden entries must be excluded: %q", listed)
	}
	if strings.Contains(listed, "view_range") {
		t.Fatal("directory view must not take view_range")
	}
	_ = ctx
}

func TestStrReplaceUniquenessAndGuardedWrite(t *testing.T) {
	runtime, root, cwd, undo := newHarness(t)
	defer undo()
	file := filepath.Join(cwd, "code.txt")
	if err := os.WriteFile(file, []byte("aaa\nbbb\naaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ambiguity lists line numbers and is an FS_AMBIGUOUS_EDIT.
	_, err := execute(t, runtime, map[string]any{
		"command": "str_replace", "path": file, "old_str": "aaa", "new_str": "z",
	})
	if err == nil {
		t.Fatal("ambiguous edit must fail")
	}
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeAmbiguousEdit {
		t.Fatalf("must be FS_AMBIGUOUS_EDIT: %v", err)
	}
	if !strings.Contains(err.Error(), "lines [1, 3]") {
		t.Fatalf("ambiguity must list lines: %v", err)
	}

	// Missing match is FS_EDIT_NOT_FOUND.
	_, err = execute(t, runtime, map[string]any{
		"command": "str_replace", "path": file, "old_str": "ghost", "new_str": "z",
	})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeEditNotFound {
		t.Fatalf("must be FS_EDIT_NOT_FOUND: %v", err)
	}

	// Unique replace succeeds and records the observation on the context.
	type observed struct {
		rows int
	}
	records := 0
	detach := root.On(fs.EventObserved, func(value any, next func(any) any) any {
		if _, ok := value.(fs.ObservedEvent); ok {
			records++
		}
		return next(value)
	})
	defer detach()
	result, err := execute(t, runtime, map[string]any{
		"command": "str_replace", "path": file, "old_str": "bbb", "new_str": "B",
	})
	if err != nil {
		t.Fatalf("unique replace: %v", err)
	}
	if !strings.Contains(result.(string), "has been edited successfully") {
		t.Fatalf("replace text: %v", result)
	}
	if records != 1 {
		t.Fatalf("one observation record expected, got %d", records)
	}
	stored, err := os.ReadFile(file)
	if err != nil || string(stored) != "aaa\nB\naaa\n" {
		t.Fatalf("stored: %q, %v", string(stored), err)
	}
}

func TestInsertCommand(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t)
	defer undo()
	file := filepath.Join(cwd, "lines.txt")
	if err := os.WriteFile(file, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := execute(t, runtime, map[string]any{
		"command": "insert", "path": file, "insert_line": float64(9), "new_str": "x",
	}); err == nil {
		t.Fatal("out-of-range insert must fail")
	}
	if _, err := execute(t, runtime, map[string]any{
		"command": "insert", "path": file, "new_str": "x",
	}); err == nil {
		t.Fatal("missing insert_line must fail")
	}
	if _, err := execute(t, runtime, map[string]any{
		"command": "insert", "path": file, "insert_line": float64(1),
	}); err == nil {
		t.Fatal("missing new_str must fail")
	}

	if _, err := execute(t, runtime, map[string]any{
		"command": "insert", "path": file, "insert_line": float64(1), "new_str": "middle\npair",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	stored, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "one\nmiddle\npair\ntwo\n" {
		t.Fatalf("stored: %q", string(stored))
	}
}

func TestAbsolutePathDisciplineAndMissingTarget(t *testing.T) {
	runtime, _, cwd, undo := newHarness(t)
	defer undo()

	_, err := execute(t, runtime, map[string]any{"command": "view", "path": "relative/file.txt"})
	if err == nil || !strings.Contains(err.Error(), "not an absolute path") {
		t.Fatalf("relative path must be rejected with guidance: %v", err)
	}
	_, err = execute(t, runtime, map[string]any{"command": "view", "path": "   "})
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Fatalf("blank path must be rejected: %v", err)
	}
	_, err = execute(t, runtime, map[string]any{"command": "str_replace", "path": filepath.Join(cwd, "ghost.txt"), "old_str": "a", "new_str": "b"})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotFound {
		t.Fatalf("absent target must be FS_NOT_FOUND: %v", err)
	}
	// Directory + non-view command: FS_NOT_REGULAR_FILE.
	_, err = execute(t, runtime, map[string]any{"command": "insert", "path": cwd, "insert_line": float64(0), "new_str": "x"})
	if codeErr, ok := err.(*fs.Error); !ok || codeErr.Code != fs.CodeNotRegularFile {
		t.Fatalf("directory insert must be FS_NOT_REGULAR_FILE: %v", err)
	}
}
