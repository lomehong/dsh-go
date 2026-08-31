package fssearch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/subprocess"
)

func TestParseGlobArgsValidation(t *testing.T) {
	if _, err := ParseGlobArgs(map[string]any{"pattern": "   "}); err == nil || err.Error() != "pattern must be a non-empty string" {
		t.Fatalf("blank pattern: %v", err)
	}
	if _, err := ParseGlobArgs(map[string]any{"pattern": "x", "path": " "}); err == nil || err.Error() != "path must be a non-empty string when given" {
		t.Fatalf("blank path: %v", err)
	}
	input, err := ParseGlobArgs(map[string]any{"pattern": "**/*.ts", "path": "src"})
	if err != nil || input.Pattern != "**/*.ts" || input.Path != "src" {
		t.Fatalf("accepted: %+v, %v", input, err)
	}
}

func TestBuildGlobCommandShape(t *testing.T) {
	argv := BuildGlobCommand(GlobInput{Pattern: "**/*.ts", Path: "src"})
	want := []string{
		"--files", "--glob=**/*.ts", "--sort=modified", "--no-ignore", "--hidden",
		"--glob=!**/.git", "--glob=!**/.git/**",
		"--glob=!**/.svn", "--glob=!**/.svn/**",
		"--glob=!**/.hg", "--glob=!**/.hg/**",
		"--glob=!**/.bzr", "--glob=!**/.bzr/**",
		"--glob=!**/.jj", "--glob=!**/.jj/**",
		"--glob=!**/.sl", "--glob=!**/.sl/**",
		"--", "src",
	}
	if len(argv) != len(want) {
		t.Fatalf("argv: %v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	// A leading-dash path rides behind `--` and can never parse as a flag.
	noPath := BuildGlobCommand(GlobInput{Pattern: "x"})
	if noPath[len(noPath)-1] == "--" {
		t.Fatalf("no path must drop the separator: %v", noPath)
	}
}

func TestSampleAcrossTopLevelRoundRobin(t *testing.T) {
	paths := []string{
		filepath.Join("a", "1"), filepath.Join("a", "2"), filepath.Join("a", "3"),
		filepath.Join("b", "1"), filepath.Join("b", "2"),
		filepath.Join("c", "1"),
	}
	sample := sampleAcrossTopLevel(paths, 4, ".")
	// The page is GROUPED by top-level entry (rounds appended within each
	// bucket, bucket order following first appearance): a takes rounds one
	// and two before b's second slot would come up.
	if len(sample.Items) != 4 {
		t.Fatalf("items: %v", sample.Items)
	}
	if sample.Items[0] != paths[0] || sample.Items[1] != paths[1] || sample.Items[2] != paths[3] || sample.Items[3] != paths[5] {
		t.Fatalf("grouped page: %v", sample.Items)
	}
	if sample.Shown != 3 || sample.Total != 3 {
		t.Fatalf("spread: %+v", sample)
	}
	// A flat result reproduces the modification-time head.
	flat := sampleAcrossTopLevel([]string{"x1", "x2", "x3", "x4"}, 2, ".")
	if flat.Items[0] != "x1" || flat.Items[1] != "x2" {
		t.Fatalf("flat: %v", flat.Items)
	}
	// A search root prefix is stripped before grouping: stripping turns
	// src\a.go and src\b.go into distinct top-level entries, so the page is
	// the head of that group order.
	rooted := sampleAcrossTopLevel([]string{filepath.Join("src", "a.go"), filepath.Join("src", "b.go"), filepath.Join("lib", "c.go")}, 2, "src")
	if rooted.Items[0] != filepath.Join("src", "a.go") || rooted.Items[1] != filepath.Join("src", "b.go") {
		t.Fatalf("rooted: %v", rooted.Items)
	}
	if rooted.Total != 3 {
		t.Fatalf("rooted total: %+v", rooted)
	}
}

func TestFormatGlobOutputFooters(t *testing.T) {
	// total == seen (every path its own top-level entry): the plain footer.
	plain := FormatGlobOutput(GlobSample{Items: []string{"a", "b"}, Shown: 2, Total: 5}, 5, nil)
	if !strings.Contains(plain, "(Showing 2 of 5 paths. ") {
		t.Fatalf("plain footer: %q", plain)
	}
	// A sampled page says so, and narrows only when groups were dropped.
	sampled := FormatGlobOutput(GlobSample{Items: []string{"a/1", "b/1"}, Shown: 2, Total: 3}, 30, nil)
	if !strings.Contains(sampled, "sampled across 2 of the 3 top-level entries") || !strings.Contains(sampled, "Narrow path to inspect a specific subtree.") {
		t.Fatalf("sampled footer: %q", sampled)
	}
	// RenderGlobPaths: whole result untouched under the cap.
	whole := RenderGlobPaths([]string{"x", "y"}, SearchCaps{GlobMaxResults: 3}, ".", nil)
	if whole != "x\ny" {
		t.Fatalf("whole: %q", whole)
	}
	if RenderGlobPaths(nil, SearchCaps{GlobMaxResults: 3}, ".", nil) != "No files found" {
		t.Fatal("empty result")
	}
	// Head page (sampling off).
	head := RenderGlobPaths([]string{"p1", "p2", "p3"}, SearchCaps{GlobMaxResults: 2}, ".", nil)
	if !strings.Contains(head, "(Showing 2 of 3 paths. The complete result could not be saved") {
		t.Fatalf("head: %q", head)
	}
}

func TestToWorkdirRelative(t *testing.T) {
	workdir := t.TempDir()
	inside := filepath.Join(workdir, "sub", "file.go")
	if got := toWorkdirRelative(inside, workdir); got != filepath.Join("sub", "file.go") {
		t.Fatalf("inside: %q", got)
	}
	if got := toWorkdirRelative(workdir, workdir); got != "." {
		t.Fatalf("root: %q", got)
	}
	outside := filepath.Join(filepath.Dir(workdir), "elsewhere.txt")
	if got := toWorkdirRelative(outside, workdir); got != outside {
		t.Fatalf("outside must pass through: %q", got)
	}
	if got := toWorkdirRelative("rel.txt", workdir); got != "rel.txt" {
		t.Fatalf("relative passes through: %q", got)
	}
}

func TestParseGrepArgsAndIncludeDiscipline(t *testing.T) {
	if _, err := ParseGrepArgs(map[string]any{"pattern": ""}); err == nil || err.Error() != "pattern must be a non-empty string" {
		t.Fatalf("empty pattern: %v", err)
	}
	// Whitespace is a legitimate regex.
	if _, err := ParseGrepArgs(map[string]any{"pattern": " "}); err != nil {
		t.Fatalf("whitespace pattern: %v", err)
	}
	cases := []struct {
		include string
		want    string
	}{
		{"  ", "include must be a non-empty glob when given"},
		{"!negated", `include must be a positive glob filter; negated patterns ("!…") are not supported`},
		{"a,b", "include must be one glob, not a comma-separated list (use {a,b} alternation instead)"},
	}
	for _, tc := range cases {
		if _, err := ParseGrepArgs(map[string]any{"pattern": "x", "include": tc.include}); err == nil || err.Error() != tc.want {
			t.Fatalf("include %q: %v", tc.include, err)
		}
	}
	// A comma inside a brace group is one glob with alternation.
	if _, err := ParseGrepArgs(map[string]any{"pattern": "x", "include": "*.{ts,tsx}"}); err != nil {
		t.Fatalf("brace alternation: %v", err)
	}
}

func TestBuildGrepCommandShape(t *testing.T) {
	argv := BuildGrepCommand(GrepInput{Pattern: "foo.*bar", Include: "*.{ts,tsx}", Path: "src"})
	want := []string{"--json", "--regexp=foo.*bar", "--glob=*.{ts,tsx}", "--", "src"}
	if len(argv) != len(want) {
		t.Fatalf("argv: %v", argv)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

func TestParseGrepMatchesRecords(t *testing.T) {
	stdout := strings.Join([]string{
		`{"type":"begin","data":{"path":{"text":"a.go"}}}`,
		`{"type":"match","data":{"path":{"text":"a.go"},"line_number":3,"lines":{"text":"hello world\n"}}}`,
		`{"type":"context","data":{}}`,
		`{"type":"match","data":{"path":{"text":"a.go"},"line_number":9,"lines":{"text":"second\r\n"}}}`,
		`{"type":"end","data":{}}`,
		`{"type":"match","data":{"path":{"text":"b.bin"},"line_number":1,"lines":{"bytes":"AA=="}}}`,
		`{"type":"summary","data":{}}`,
	}, "\n")
	matches, err := ParseGrepMatches(stdout)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(matches) != 3 {
		t.Fatalf("matches: %+v", matches)
	}
	if matches[0].Line != "hello world" || matches[0].LineNumber != 3 {
		t.Fatalf("first: %+v", matches[0])
	}
	if matches[1].Line != "second" {
		t.Fatalf("CRLF strip: %+v", matches[1])
	}
	if matches[2].Line != "(line is not valid UTF-8)" {
		t.Fatalf("bytes placeholder: %+v", matches[2])
	}
	// A malformed stream fails SEARCH_FAILED, never a partial result.
	for _, bad := range []string{
		"not json",
		`{"type":"match","data":{}}`,
		`{"type":"match","data":{"line_number":1,"lines":{"text":"x"}}}`,
		`{"type":"match","data":{"path":{"text":"a"},"lines":{"text":"x"}}}`,
		`{"type":"match","data":{"path":{"text":"a"},"line_number":1}}`,
		`{"type":"match","data":{"path":{"text":"a"},"line_number":1,"lines":{}}}`,
	} {
		if _, err := ParseGrepMatches(bad); err == nil {
			t.Fatalf("malformed %q must fail", bad)
		} else if searchErr, ok := err.(*SearchError); !ok || searchErr.Code != CodeSearchFailed {
			t.Fatalf("malformed %q code: %v", bad, err)
		}
	}
}

func TestFormatGrepOutputWording(t *testing.T) {
	matches := []GrepMatch{
		{Path: "a.go", LineNumber: 3, Line: "hello"},
		{Path: "a.go", LineNumber: 9, Line: "second"},
		{Path: "b.go", LineNumber: 1, Line: "other"},
	}
	// Uncapped: plain header with the singular/plural noun.
	out := FormatRetainedGrep(matches, 3, false, nil)
	if !strings.HasPrefix(out, "Found 3 matches\n\n") || !strings.Contains(out, "a.go\nLine 3: hello\nLine 9: second\n\nb.go\nLine 1: other") {
		t.Fatalf("uncapped: %q", out)
	}
	single := FormatRetainedGrep(matches[:1], 1, false, nil)
	if !strings.HasPrefix(single, "Found 1 match\n\n") {
		t.Fatalf("singular: %q", single)
	}
	// Capped: found-of header and the could-not-save recovery.
	capped := FormatRetainedGrep(matches[:2], 5, true, nil)
	if !strings.HasPrefix(capped, "Found 2 of 5 matches") || !strings.Contains(capped, "(The complete result could not be saved; narrow pattern, path, or include to see more.)") {
		t.Fatalf("capped: %q", capped)
	}
	if FormatRetainedGrep(nil, 0, false, nil) != "No matches found" {
		t.Fatal("empty")
	}
}

func TestPreviewLineUtf8Boundary(t *testing.T) {
	long := strings.Repeat("é", 10) // 20 bytes
	kept := previewLine(long, 15)
	// The cut preserves UTF-8 boundaries: 7 runes (14 bytes) survive.
	if !strings.HasSuffix(kept, " (line truncated)") || strings.Count(kept, "é") != 7 {
		t.Fatalf("utf8 cut: %q", kept)
	}
	if got := previewLine("short", 100); got != "short" {
		t.Fatalf("under cap: %q", got)
	}
}

func TestRunRipgrepFailsLoudWithoutService(t *testing.T) {
	// A composition without the subprocess service fails SEARCH_FAILED at
	// the call boundary (search-core's launch-failure classification).
	err := searchErrBoundary(t)
	var searchErr *SearchError
	if !errors.As(err, &searchErr) || searchErr.Code != CodeSearchFailed {
		t.Fatalf("boundary error: %v", err)
	}
}

// runRipgrepWithoutRg exercises the missing-binary classification.
func TestRunRipgrepMissingBinaryClassification(t *testing.T) {
	if _, err := exec.LookPath("rg"); err == nil {
		t.Skip("rg is installed; the missing-binary path cannot be staged")
	}
	caps := DefaultCaps()
	caps.RGPath = ""
	resetRgPathForTest()
	_, err := runRipgrep(nil, nil, caps, "glob", BuildGlobCommand(GlobInput{Pattern: "x"}))
	var searchErr *SearchError
	if !errors.As(err, &searchErr) || searchErr.Code != CodeSearchFailed {
		t.Fatalf("missing binary: %v", err)
	}
}

// A cancelled tool-call signal surfaces as SEARCH_ABORTED at the call
// boundary: the chained context terminates the spawned search process.
func TestRunRipgrepCancelledSignalAborts(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Skipf("cannot resolve the test binary: %v", err)
	}
	caps := DefaultCaps()
	caps.RGPath = binary
	root := cordis.NewRoot(cordis.Discard{})
	root.Provide("subprocess", subprocess.Local{})
	signalCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runRipgrep(root, signalCtx, caps, "grep", []string{"--json"})
	var searchErr *SearchError
	if !errors.As(err, &searchErr) || searchErr.Code != CodeSearchAborted {
		t.Fatalf("cancelled signal: %v", err)
	}
}

// searchErrBoundary drives runRipgrep with a bare (service-less) context.
func searchErrBoundary(t *testing.T) error {
	t.Helper()
	caps := DefaultCaps()
	caps.RGPath = filepath.Join(t.TempDir(), "definitely-missing-rg")
	_, err := runRipgrep(nil, nil, caps, "grep", []string{"--json"})
	return err
}

func TestClassifyRunFailureVocabulary(t *testing.T) {
	invalid := classifyRunFailure("grep", 2, "Regex parse error: bad group", false)
	if searchErr, ok := invalid.(*SearchError); !ok || searchErr.Code != CodeSearchInvalidPattern {
		t.Fatalf("invalid pattern: %v", invalid)
	}
	globInvalid := classifyRunFailure("glob", 2, "error parsing glob", false)
	if searchErr, ok := globInvalid.(*SearchError); !ok || searchErr.Code != CodeSearchInvalidPattern {
		t.Fatalf("invalid glob: %v", globInvalid)
	}
	generic := classifyRunFailure("grep", 2, "permission denied", false)
	if searchErr, ok := generic.(*SearchError); !ok || searchErr.Code != CodeSearchFailed || !strings.Contains(searchErr.Message, "(exit 2)") {
		t.Fatalf("generic: %v", generic)
	}
	// The stderr excerpt appends a truncation note when bytes were dropped.
	truncated := classifyRunFailure("grep", 2, "boom", true)
	if !strings.Contains(truncated.Error(), "[stderr truncated]") {
		t.Fatalf("truncation note: %v", truncated)
	}
	// Overflow and abort vocabularies are direct constructors.
	_, overflow := completeStdout("grep", strings.Repeat("x", 10), false, 5)
	if searchErr, ok := overflow.(*SearchError); !ok || searchErr.Code != CodeSearchRawOutputOverflow {
		t.Fatalf("overflow: %v", overflow)
	}
	_, lossy := completeStdout("grep", "", true, 100)
	if searchErr, ok := lossy.(*SearchError); !ok || searchErr.Code != CodeSearchRawOutputOverflow {
		t.Fatalf("lossy: %v", lossy)
	}
}

// TestSearchToolsRealRipgrep is the spawn-backed integration: it runs only
// where a ripgrep binary is deployed (the documented v1 deployment
// requirement) and is skipped elsewhere — honestly, not silently.
func TestSearchToolsRealRipgrep(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("rg not on PATH: spawn-backed integration needs the ripgrep deployment")
	}
	runtime, _, _ := newHarness(t, mustFindRg(t))

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "needle.txt"), []byte("find the needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "ignored.txt"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// glob: VCS metadata stays excluded even under --hidden --no-ignore.
	globResult, err := executeGlob(t, runtime, "**/*.txt", dir)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if !strings.Contains(globResult, "needle.txt") || strings.Contains(globResult, "ignored.txt") {
		t.Fatalf("glob result: %q", globResult)
	}

	// grep: grouped matches with line numbers.
	grepResult, err := executeGrep(t, runtime, "needle", dir, "")
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if !strings.Contains(grepResult, "Found 1 match") || !strings.Contains(grepResult, "Line 1: find the needle here") {
		t.Fatalf("grep result: %q", grepResult)
	}
}

func mustFindRg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg vanished mid-test")
	}
	return path
}
