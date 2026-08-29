package filereference

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildWorkspace materializes one deterministic tree:
//
//	src/main.go, src/util/helper.go, docs/readme.md, notes.txt,
//	.hidden/secret.txt, .git/config, node_modules/pkg/index.js,
//	dist/bundle.js
func buildWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := []string{
		"src/main.go", "src/util/helper.go", "docs/readme.md", "notes.txt",
		".hidden/secret.txt", ".git/config", "node_modules/pkg/index.js",
		"dist/bundle.js",
	}
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return root
}

func testSearch(t *testing.T, root string) *WorkspaceFileSearch {
	t.Helper()
	search, err := NewWorkspaceFileSearch(root, SearchConfig{
		MaxResults:          DefaultMaxResults,
		MaxEntries:          DefaultMaxEntries,
		ExcludedDirectories: append([]string{}, DefaultExcludedDirectories...),
	})
	if err != nil {
		t.Fatalf("new search: %v", err)
	}
	return search
}

func paths(candidates []Candidate) []string {
	ordered := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ordered = append(ordered, candidate.Path)
	}
	return ordered
}

func TestActiveAtTokenGrammar(t *testing.T) {
	cases := []struct {
		line   string
		cursor int
		want   *ActiveAtToken
	}{
		{"@src/m", 6, &ActiveAtToken{Prefix: "@src/m", Query: "src/m"}},
		{"run @src", 8, &ActiveAtToken{Prefix: "@src", Query: "src"}},
		{"@src/", 5, &ActiveAtToken{Prefix: "@src/", Query: "src/"}},
		{"a@src", 5, nil},          // @ inside another token
		{"user@mail.com", 13, nil}, // email is not a trigger
		{`@"src ma`, 8, &ActiveAtToken{Prefix: `@"src ma`, Query: "src ma", Quoted: true}},
		{"see @", 5, &ActiveAtToken{Prefix: "@", Query: ""}},
		{"@a @b", 2, &ActiveAtToken{Prefix: "@a", Query: "a"}},
	}
	for _, item := range cases {
		got := ActiveAtTokenOf(item.line, item.cursor)
		if item.want == nil {
			if got != nil {
				t.Fatalf("line %q: expected no token, got %+v", item.line, got)
			}
			continue
		}
		if got == nil || *got != *item.want {
			t.Fatalf("line %q: got %+v want %+v", item.line, got, item.want)
		}
	}
}

func TestFormatFileMention(t *testing.T) {
	cases := []struct {
		candidate Candidate
		preserve  bool
		want      string
	}{
		{Candidate{Path: "src/main.go", Kind: "file"}, false, "@src/main.go"},
		{Candidate{Path: "my docs", Kind: "file"}, false, `@"my docs"`},
		{Candidate{Path: "src", Kind: "directory"}, false, "@src/"},
		{Candidate{Path: "my docs", Kind: "directory"}, false, `@"my docs/`},
		{Candidate{Path: "src", Kind: "directory"}, true, `@"src/`},
		{Candidate{Path: "bad\"quote", Kind: "file"}, false, ""},
		{Candidate{Path: "bad\nline", Kind: "file"}, false, ""},
	}
	for _, item := range cases {
		if got := FormatFileMention(item.candidate, item.preserve); got != item.want {
			t.Fatalf("mention %+v preserve=%v: got %q want %q", item.candidate, item.preserve, got, item.want)
		}
	}
}

func TestDirectoryScopedQueryListsLiveState(t *testing.T) {
	root := buildWorkspace(t)
	search := testSearch(t, root)
	ctx := context.Background()
	// Root listing hides dot entries and excluded dirs; directories sort
	// before files, then text order.
	got, err := search.List(ctx, "")
	if err != nil {
		t.Fatalf("root list: %v", err)
	}
	if got[0].Path != "docs" || got[0].Kind != "directory" || got[1].Path != "src" || got[2].Path != "notes.txt" {
		t.Fatalf("root = %v", paths(got))
	}
	// Descend with a trailing slash; excluded subtree contributes nothing.
	got, err = search.List(ctx, "src/")
	if err != nil {
		t.Fatalf("src list: %v", err)
	}
	if len(got) != 2 || got[0].Path != "src/util" || got[1].Path != "src/main.go" {
		t.Fatalf("src = %v", paths(got))
	}
	// A fragment filters live directory contents.
	got, _ = search.List(ctx, "src/mai")
	if len(got) != 1 || got[0].Path != "src/main.go" {
		t.Fatalf("fragment = %v", paths(got))
	}
	// A dot query routes through the index, where dot paths are visible.
	got, _ = search.List(ctx, ".hi")
	if len(got) != 2 || got[0].Path != ".hidden" || got[1].Path != ".hidden/secret.txt" {
		t.Fatalf("dot = %v", paths(got))
	}
	// Escape above the root offers nothing.
	got, _ = search.List(ctx, "../")
	if len(got) != 0 {
		t.Fatalf("escape = %v", paths(got))
	}
	// An excluded path segment short-circuits to nothing.
	got, _ = search.List(ctx, "node_modules/pkg")
	if len(got) != 0 {
		t.Fatalf("excluded segment = %v", paths(got))
	}
}

func TestBareQueryRanksIndexedPaths(t *testing.T) {
	root := buildWorkspace(t)
	search := testSearch(t, root)
	ctx := context.Background()
	got, err := search.List(ctx, "helper")
	if err != nil {
		t.Fatalf("bare list: %v", err)
	}
	if len(got) != 1 || got[0].Path != "src/util/helper.go" {
		t.Fatalf("helper = %v", paths(got))
	}
	// Exact name match outranks prefix and substring; directories outrank
	// equally-scored files; excluded dirs never appear.
	got, _ = search.List(ctx, "main")
	if len(got) != 1 || got[0].Path != "src/main.go" {
		t.Fatalf("main = %v", paths(got))
	}
	// Hidden files stay out of global queries unless the query wants one.
	got, _ = search.List(ctx, "secret")
	if len(got) != 0 {
		t.Fatalf("secret leaked = %v", paths(got))
	}
	// Subsequence matching works: "srg" ⊂ "src/main.go".
	got, _ = search.List(ctx, "srg")
	if len(got) == 0 || got[0].Path != "src/main.go" {
		t.Fatalf("subsequence = %v", paths(got))
	}
	// No match anywhere yields nothing.
	got, _ = search.List(ctx, "zzzz")
	if len(got) != 0 {
		t.Fatalf("zzzz = %v", paths(got))
	}
}

func TestInvalidateRebuildsInBackground(t *testing.T) {
	root := buildWorkspace(t)
	search := testSearch(t, root)
	ctx := context.Background()
	if _, err := search.List(ctx, "helper"); err != nil {
		t.Fatalf("first list: %v", err)
	}
	// New file: a stale index still answers until invalidation triggers a
	// rebuild.
	if err := os.WriteFile(filepath.Join(root, "src", "fresh.go"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := search.List(ctx, "fresh")
	if len(got) != 0 {
		t.Fatalf("uninvalidated saw the new file: %v", paths(got))
	}
	search.Invalidate()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ = search.List(ctx, "fresh")
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rebuild never observed the new file: %v", paths(got))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMaxEntriesBudgetAndUnreadableRoot(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	search, err := NewWorkspaceFileSearch(root, SearchConfig{MaxResults: 10, MaxEntries: 2, ExcludedDirectories: nil})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// The entry budget bounds the shared fuzzy index; the bare query must
	// carry no slash to route there.
	got, err := search.List(context.Background(), "txt")
	if err != nil || len(got) != 2 {
		t.Fatalf("budget = %v %v", paths(got), err)
	}
	// An unreadable root fails the traversal instead of publishing an
	// empty index; a bare query (no slash) routes through the index.
	missing, err := NewWorkspaceFileSearch(filepath.Join(root, "absent"), SearchConfig{MaxResults: 5, MaxEntries: 100})
	if err != nil {
		t.Fatalf("missing root: %v", err)
	}
	if _, err := missing.List(context.Background(), "txt"); err == nil {
		t.Fatal("unreadable root settled as an empty index")
	}
}

func TestDisposeStopsAnswering(t *testing.T) {
	root := buildWorkspace(t)
	search := testSearch(t, root)
	ctx := context.Background()
	if _, err := search.List(ctx, "helper"); err != nil {
		t.Fatalf("first: %v", err)
	}
	search.Dispose()
	got, err := search.List(ctx, "helper")
	if err != nil || len(got) != 0 {
		t.Fatalf("after dispose = %v %v", paths(got), err)
	}
	// Double dispose is a no-op.
	search.Dispose()
}

func TestConfigValidation(t *testing.T) {
	valid := SearchConfig{MaxResults: 1, MaxEntries: 1, ExcludedDirectories: []string{"dist"}}
	if _, err := NewWorkspaceFileSearch(t.TempDir(), valid); err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	for name, broken := range map[string]SearchConfig{
		"zero results":  {MaxResults: 0, MaxEntries: 1},
		"zero entries":  {MaxResults: 1, MaxEntries: 0},
		"empty exclude": {MaxResults: 1, MaxEntries: 1, ExcludedDirectories: []string{""}},
		"path exclude":  {MaxResults: 1, MaxEntries: 1, ExcludedDirectories: []string{"a/b"}},
		"win exclude":   {MaxResults: 1, MaxEntries: 1, ExcludedDirectories: []string{`a\b`}},
	} {
		if _, err := NewWorkspaceFileSearch(t.TempDir(), broken); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestServicePerAgentIndexesAndDisposal(t *testing.T) {
	root := buildWorkspace(t)
	service, err := NewService(DefaultServiceConfig())
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	ctx := context.Background()
	got, err := service.List(ctx, "agent-a", root, "helper")
	if err != nil || len(got) != 1 {
		t.Fatalf("agent-a = %v %v", paths(got), err)
	}
	service.InvalidateAgent("agent-b") // unknown agent is a no-op
	service.DisposeAgent("agent-b")    // unknown agent is a no-op
	got, _ = service.List(ctx, "agent-a", root, "helper")
	if len(got) != 1 {
		t.Fatalf("index lost after unrelated disposal = %v", paths(got))
	}
	service.Dispose()
	got, err = service.List(ctx, "agent-a", root, "helper")
	if err != nil || len(got) != 1 {
		// Dispose clears indexes; the next List roots a fresh one.
		t.Fatalf("post-dispose re-root = %v %v", paths(got), err)
	}
}
