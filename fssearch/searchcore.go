// Package fssearch ports @deepseek-ai/dsh-tool-fs-search: the model-facing
// filesystem discovery tools (`glob`, `grep`) over a ripgrep binary. Both
// tools execute as ordinary foreground spawns through the subprocess seam
// with fixed ripgrep argv templates — never a shell layer, never a
// model-visible background task.
//
// Go deployment adaptation (documented at the seam): the official package
// ships `@vscode/ripgrep` inside its npm dependency; the Go composition
// resolves `rg` from PATH once per process instead. A missing or corrupt
// binary surfaces at the first search call as SEARCH_FAILED — the same
// call-boundary classification the official resolver uses (it deliberately
// does not fail the Loader composition).
package fssearch

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"dshgo/cordis"
	"dshgo/subprocess"
)

// RawOutputMaxBytes is the default cap on the complete raw `rg` stdout the
// tools will parse (the `rawOutputMaxBytes` config), matching Claude Code's
// ripgrep raw buffer.
const RawOutputMaxBytes = 20_000_000

// SearchTimeoutMs is the default cooperative tool-call timeout budget in
// milliseconds (the `timeoutMs` config).
const SearchTimeoutMs = 30_000

// StderrMaxBytes is the default cap in bytes on the retained stderr tail of
// one search run — a diagnostic excerpt only (the tool never reads a stderr
// spill path, and the collect disposition requests none).
const StderrMaxBytes = 64 * 1024

// SearchGraceMs is the default terminate grace period for a search process.
const SearchGraceMs = 3_000

// SearchCaps carries the resolved deployment caps (plugin config after
// defaulting).
type SearchCaps struct {
	// GlobMaxResults is the inline path cap for glob.
	GlobMaxResults int
	// SampleOverCapGlobResults: over-cap pages are sampled across
	// top-level entries instead of taking the modification-time head.
	SampleOverCapGlobResults bool
	// GrepMaxMatches is the inline flat-match cap for grep.
	GrepMaxMatches int
	// GrepMaxLineBytes is the per-matched-line preview budget in bytes.
	GrepMaxLineBytes int
	// RawOutputMaxBytes caps the complete raw `rg` stdout the tool parses.
	RawOutputMaxBytes int
	// GraceMs is the terminate-escalation grace period for the search
	// process.
	GraceMs int
	// StderrMaxBytes caps the retained stderr diagnostic tail.
	StderrMaxBytes int
	// TimeoutMs is the cooperative tool-call budget.
	TimeoutMs int
	// RGPath pins the ripgrep binary; empty resolves from PATH once.
	RGPath string
}

// DefaultCaps resolves the official defaults.
func DefaultCaps() SearchCaps {
	return SearchCaps{
		GlobMaxResults:           100,
		SampleOverCapGlobResults: false,
		GrepMaxMatches:           250,
		GrepMaxLineBytes:         2000,
		RawOutputMaxBytes:        RawOutputMaxBytes,
		GraceMs:                  SearchGraceMs,
		StderrMaxBytes:           StderrMaxBytes,
		TimeoutMs:                SearchTimeoutMs,
	}
}

// Search error codes: package-owned (not FS_*) because these tools are
// spawn-backed discovery, not fs provider operations.
const (
	CodeSearchInvalidPattern    = "SEARCH_INVALID_PATTERN"
	CodeSearchFailed            = "SEARCH_FAILED"
	CodeSearchRawOutputOverflow = "SEARCH_RAW_OUTPUT_OVERFLOW"
	CodeSearchAborted           = "SEARCH_ABORTED"
)

// SearchError is the typed search failure with a stable code; the tool
// registry exposes {name, code} on isError results so retry/permission/UI
// layers can branch without parsing messages.
type SearchError struct {
	Message string
	Code    string
}

func (e *SearchError) Error() string { return e.Message }

func searchErrf(code, format string, args ...any) *SearchError {
	return &SearchError{Message: fmt.Sprintf(format, args...), Code: code}
}

// GrepMatch is one parsed match: the file, the 1-based line number, and the
// (possibly previewed) line text.
type GrepMatch struct {
	Path       string
	LineNumber int
	Line       string
}

// RipgrepRun is the completed acquisition of one `rg` run: complete stdout
// plus the resolved workdir.
type RipgrepRun struct {
	// Stdout is the complete raw stdout retained by the subprocess seam
	// within the requested cap.
	Stdout string
	// NoMatches is true when ripgrep exited 1: a successful search with
	// zero results.
	NoMatches bool
	// Workdir is the resolved working directory the command ran in (the
	// display-relativization base).
	Workdir string
}

var (
	rgPathOnce sync.Once
	rgPath     string
)

// resolveRgPath resolves the ripgrep binary path, lazily once per process.
// The Go seam resolves from PATH; a missing binary surfaces at the call
// boundary as SEARCH_FAILED rather than failing the composition.
func resolveRgPath(pinned string) (string, error) {
	if pinned != "" {
		return pinned, nil
	}
	rgPathOnce.Do(func() {
		found, err := exec.LookPath("rg")
		if err != nil {
			return
		}
		rgPath = found
	})
	if rgPath == "" {
		return "", searchErrf(CodeSearchFailed, "ripgrep binary not found on PATH (deploy rg or set the rgPath config)")
	}
	return rgPath, nil
}

// resetRgPathForTest clears the memoized resolver.
func resetRgPathForTest() {
	rgPathOnce = sync.Once{}
	rgPath = ""
}

// toWorkdirRelative maps an `rg` output path to its display form: absolute
// paths inside the resolved workdir become workdir-relative; everything
// else (relative output, paths outside the workdir) passes through
// unchanged. Display-only — returned paths are follow-up-readable in
// co-located workdir/filesystem deployments where both resolve the same
// workspace (the documented v1 deployment requirement).
func toWorkdirRelative(path, workdir string) string {
	if !isAbsPath(path) {
		return path
	}
	rel, err := relPath(workdir, path)
	if err != nil {
		return path
	}
	if rel == "" {
		return "."
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+sep()) {
		return path
	}
	return rel
}

// previewLine bounds one matched-line preview to maxBytes (UTF-8 boundary
// preserved) and marks the cut. The cap is a per-line budget fact; the
// complete line stays in the searched file for `read`.
func previewLine(line string, maxBytes int) string {
	kept, truncated := headBytes(line, maxBytes)
	if truncated {
		return kept + " (line truncated)"
	}
	return kept
}

// retainGrepMatches applies the shared inline cap to a canonical grep match
// list: preview each line to maxLineBytes and keep the first maxMatches.
// The single retention pass both the model-facing render and any card
// projection consume, so text and card never disagree about which matches
// survived.
func retainGrepMatches(matches []GrepMatch, maxMatches, maxLineBytes int) (kept []GrepMatch, seen int, truncated bool) {
	for _, match := range matches {
		kept = append(kept, GrepMatch{Path: match.Path, LineNumber: match.LineNumber, Line: previewLine(match.Line, maxLineBytes)})
	}
	seen = len(kept)
	if seen > maxMatches {
		kept = kept[:maxMatches]
		truncated = true
	}
	return kept, seen, truncated
}

// retainGlobPaths applies the shared inline cap to a canonical glob path
// list: keep the first maxResults.
func retainGlobPaths(paths []string, maxResults int) (kept []string, seen int, truncated bool) {
	seen = len(paths)
	if seen > maxResults {
		return paths[:maxResults], seen, true
	}
	return paths, seen, false
}

// runRipgrep runs the ripgrep binary with a plain argv vector and returns
// its complete raw stdout. The working directory is the calling agent's
// session cwd when available, else the process cwd. The context is
// forwarded so cooperative tool timeouts and caller cancellation terminate
// the process tree.
//
// The spawn is unconfined (a plain subprocess call), so `--no-config` is
// prepended: a host RIPGREP_CONFIG_PATH can otherwise inject `--pre` and
// make ripgrep execute an arbitrary preprocessor for every matched file.
// The collect dispositions are the seam's diagnostic-tail shape (no spill
// files): truncated stdout fails as SEARCH_RAW_OUTPUT_OVERFLOW.
//
// Exit semantics are tool-owned: exit 0 is success with results, exit 1 is
// success with zero results, anything else throws a SearchError
// (context cancellation → SEARCH_ABORTED, invalid pattern →
// SEARCH_INVALID_PATTERN, the rest → SEARCH_FAILED / OVERFLOW).
func runRipgrep(ctx *cordis.Context, caps SearchCaps, toolName string, argv []string) (RipgrepRun, error) {
	rgPath, err := resolveRgPath(caps.RGPath)
	if err != nil {
		return RipgrepRun{}, err
	}
	var sub subprocess.Runtime
	if ctx != nil {
		sub, _ = ctx.Get("subprocess").(subprocess.Runtime)
	}
	if sub == nil {
		return RipgrepRun{}, searchErrf(CodeSearchFailed, "%s could not start its search command (no subprocess service is composed)", toolName)
	}
	spec := subprocess.SpawnSpec{
		Argv:    append([]string{rgPath, "--no-config"}, argv...),
		Cwd:     processCwd(),
		Stdio:   subprocess.Stdio{Stdin: subprocess.StdinIgnore{}, Stdout: subprocess.OutputCollect{MaxBytes: caps.RawOutputMaxBytes}, Stderr: subprocess.OutputCollect{MaxBytes: caps.StderrMaxBytes}},
		GraceMs: caps.GraceMs,
	}
	handle, spawnErr := sub.Spawn(contextBackground(), spec)
	if spawnErr != nil {
		return RipgrepRun{}, searchErrf(CodeSearchFailed, "%s could not start its search command (ripgrep launch failed)", toolName)
	}
	outcome, outcomeErr := handle.Outcome()
	if outcomeErr != nil {
		return RipgrepRun{}, searchErrf(CodeSearchFailed, "%s could not start its search command (ripgrep launch failed)", toolName)
	}
	stdoutReader := handle.CollectedStdout()
	stderrReader := handle.CollectedStderr()
	if stdoutReader == nil || stderrReader == nil {
		return RipgrepRun{}, searchErrf(CodeSearchFailed, "%s search command produced no collected output streams", toolName)
	}
	stdout := stdoutReader.ReadFrom(0)
	stderr := stderrReader.ReadFrom(0)
	if outcome.Signal != "" || outcome.ExitCode < 0 {
		return RipgrepRun{}, searchErrf(CodeSearchFailed, "%s search command was killed by signal %s", toolName, signalLabel(outcome.Signal))
	}
	if outcome.ExitCode != 0 && outcome.ExitCode != 1 {
		return RipgrepRun{}, classifyRunFailure(toolName, outcome.ExitCode, stderr.Text, stderr.Lossy)
	}
	text, err := completeStdout(toolName, stdout.Text, stdout.Lossy, caps.RawOutputMaxBytes)
	if err != nil {
		return RipgrepRun{}, err
	}
	return RipgrepRun{Stdout: text, NoMatches: outcome.ExitCode == 1, Workdir: spec.Cwd}, nil
}

// classifyRunFailure classifies a nonzero-exit `rg` run into the search
// error vocabulary. There is no shell layer, so an exit 127 or shell
// "command not found" text cannot occur — a launch failure rejects at
// spawn.
func classifyRunFailure(toolName string, exitCode int, stderrText string, stderrTruncated bool) error {
	stderr := stderrExcerpt(stderrText, stderrTruncated)
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "regex parse error") || strings.Contains(lower, "error parsing glob") {
		return searchErrf(CodeSearchInvalidPattern, "%s pattern rejected by ripgrep: %s", toolName, stderr)
	}
	return searchErrf(CodeSearchFailed, "%s search failed (exit %d)%s", toolName, exitCode, tailExcerpt(stderr))
}

// stderrExcerpt formats the retained stderr tail as a diagnostic excerpt,
// with a truncation note when the subprocess seam dropped bytes.
func stderrExcerpt(stderrText string, truncated bool) string {
	text := strings.TrimSpace(stderrText)
	if text == "" {
		return ""
	}
	if truncated {
		return text + " [stderr truncated]"
	}
	return text
}

func tailExcerpt(stderr string) string {
	if stderr == "" {
		return ""
	}
	return ": " + stderr
}

// completeStdout acquires the COMPLETE raw stdout of a finished run,
// enforcing rawOutputMaxBytes on the in-memory transport. A truncated
// result means the subprocess seam could not retain complete stdout within
// the requested budget, so the tool fails clearly instead of parsing a
// silently-partial stream.
func completeStdout(toolName, text string, lossy bool, rawOutputMaxBytes int) (string, error) {
	narrow := "narrow pattern, path, or include and retry"
	if !lossy {
		inlineBytes := len(text)
		if inlineBytes > rawOutputMaxBytes {
			return "", searchErrf(CodeSearchRawOutputOverflow, "%s produced %d bytes of raw output, over the %d-byte cap; %s", toolName, inlineBytes, rawOutputMaxBytes, narrow)
		}
		return text, nil
	}
	return "", searchErrf(CodeSearchRawOutputOverflow, "%s produced more raw output than the subprocess seam retained within the %d-byte cap; %s", toolName, rawOutputMaxBytes, narrow)
}
