package fssearch

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GrepMaxMatchesDefault is the default cap on flat matches retained inline
// by one grep call, matching Claude Code's default GrepTool head_limit.
const GrepMaxMatchesDefault = 250

// GrepMaxLineBytesDefault is the default cap in bytes on one matched-line
// preview; the cut preserves UTF-8 boundaries.
const GrepMaxLineBytesDefault = 2000

// GrepInput is the validated grep arguments.
type GrepInput struct {
	Pattern string
	Path    string
	Include string
}

// validateInclude rejects an include that is not ONE positive glob filter:
// blank strings, negated patterns (`!…`), and comma-separated lists. A
// comma inside a brace group is fine — `*.{ts,tsx}` is one glob with
// alternation, not a list.
func validateInclude(include string) error {
	if strings.TrimSpace(include) == "" {
		return errArgs("include must be a non-empty glob when given")
	}
	if strings.HasPrefix(include, "!") {
		return errArgs(`include must be a positive glob filter; negated patterns ("!…") are not supported`)
	}
	braceDepth := 0
	for _, char := range include {
		switch {
		case char == '{':
			braceDepth++
		case char == '}':
			if braceDepth > 0 {
				braceDepth--
			}
		case char == ',' && braceDepth == 0:
			return errArgs("include must be one glob, not a comma-separated list (use {a,b} alternation instead)")
		}
	}
	return nil
}

// ParseGrepArgs validates value constraints the schema DSL can't express: a
// non-EMPTY pattern (whitespace is a legitimate regex), a non-blank path
// when given, and a single positive include glob.
func ParseGrepArgs(args map[string]any) (GrepInput, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return GrepInput{}, errArgs("pattern must be a non-empty string")
	}
	input := GrepInput{Pattern: pattern}
	if raw, has := args["path"]; has {
		path, _ := raw.(string)
		if strings.TrimSpace(path) == "" {
			return GrepInput{}, errArgs("path must be a non-empty string when given")
		}
		input.Path = path
	}
	if raw, has := args["include"]; has {
		include, _ := raw.(string)
		if err := validateInclude(include); err != nil {
			return GrepInput{}, err
		}
		input.Include = include
	}
	return input, nil
}

// BuildGrepCommand builds the fixed line-oriented `rg --json` argv for one
// grep call. Every model-controlled value (pattern, path, include) is a
// plain argv element — no shell layer exists, so no quoting applies; the
// pattern and include ride in `--flag=value` form and the target behind
// `--`, so a leading-dash value can never be parsed as a flag.
func BuildGrepCommand(input GrepInput) []string {
	parts := []string{"--json", "--regexp=" + input.Pattern}
	if input.Include != "" {
		parts = append(parts, "--glob="+input.Include)
	}
	if input.Path != "" {
		parts = append(parts, "--", input.Path)
	}
	return parts
}

// malformedRecord is the uniform malformed-output failure: raw `rg --json`
// is an internal transport, so missing or invalid response fields cause a
// search failure, not a partial result.
func malformedRecord(detail string) error {
	return searchErrf(CodeSearchFailed, "grep received malformed ripgrep --json output (%s)", detail)
}

// parseRecord parses one `rg --json` NDJSON line into a match; nil for the
// non-match record types (begin/end/context/summary). A line that is not
// JSON, or a match record missing its path / line number / line content, is
// a SEARCH_FAILED. A match whose line is not valid UTF-8 (ripgrep sends
// base64 bytes instead of text) yields a placeholder preview rather than
// failing the whole search.
func parseRecord(line string) (*GrepMatch, error) {
	var parsed struct {
		Type string `json:"type"`
		Data *struct {
			Path *struct {
				Text string `json:"text"`
			} `json:"path"`
			LineNumber *int `json:"line_number"`
			Lines      *struct {
				Text  string `json:"text"`
				Bytes string `json:"bytes"`
			} `json:"lines"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		return nil, malformedRecord("a line is not JSON")
	}
	// Non-match record types (begin/end/context/summary — and any future
	// type) are transport framing, not results: skipped, not malformed.
	if parsed.Type != "match" {
		return nil, nil
	}
	if parsed.Data == nil {
		return nil, malformedRecord("a match record has no data")
	}
	if parsed.Data.Path == nil {
		return nil, malformedRecord("a match record has no path text")
	}
	if parsed.Data.LineNumber == nil {
		return nil, malformedRecord("a match record has no line number")
	}
	if parsed.Data.Lines == nil {
		return nil, malformedRecord("a match record has no line content")
	}
	if parsed.Data.Lines.Text != "" {
		return &GrepMatch{
			Path:       parsed.Data.Path.Text,
			LineNumber: *parsed.Data.LineNumber,
			Line:       strings.TrimRight(parsed.Data.Lines.Text, "\r\n"),
		}, nil
	}
	if parsed.Data.Lines.Bytes != "" {
		return &GrepMatch{
			Path:       parsed.Data.Path.Text,
			LineNumber: *parsed.Data.LineNumber,
			Line:       "(line is not valid UTF-8)",
		}, nil
	}
	return nil, malformedRecord("a match record has neither line text nor bytes")
}

// ParseGrepMatches parses complete `rg --json` stdout into flat matches, in
// output order (ripgrep emits one file's matches contiguously). Only match
// records are consumed; a malformed stream fails as SEARCH_FAILED.
func ParseGrepMatches(stdout string) ([]GrepMatch, error) {
	var matches []GrepMatch
	for _, line := range strings.Split(stdout, "\n") {
		if line == "" {
			continue
		}
		match, err := parseRecord(line)
		if err != nil {
			return nil, err
		}
		if match != nil {
			matches = append(matches, *match)
		}
	}
	return matches, nil
}

// matchNoun is `match` / `matches` for a count.
func matchNoun(count int) string {
	if count == 1 {
		return "match"
	}
	return "matches"
}

// FormatGrepMatches groups flat matches by file (first-seen order) into the
// model-facing body: each file's display path, then one `Line N: <text>`
// row per match.
func FormatGrepMatches(matches []GrepMatch) string {
	type fileGroup struct {
		path    string
		matches []GrepMatch
	}
	var order []fileGroup
	index := map[string]int{}
	for _, match := range matches {
		if at, has := index[match.Path]; has {
			order[at].matches = append(order[at].matches, match)
			continue
		}
		index[match.Path] = len(order)
		order = append(order, fileGroup{path: match.Path, matches: []GrepMatch{match}})
	}
	sections := make([]string, 0, len(order))
	for _, group := range order {
		rows := make([]string, 0, len(group.matches))
		for _, m := range group.matches {
			rows = append(rows, fmt.Sprintf("Line %d: %s", m.LineNumber, m.Line))
		}
		sections = append(sections, group.path+"\n"+strings.Join(rows, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// SpillRef is the saved complete-result reference (the Go spill service is
// not composed yet; refs stay nil and the could-not-save path renders).
type SpillRef struct {
	Locator       string
	RetrievalHint string
}

// FormatGrepOutput formats the model-facing grep result: a found-count
// header, the retained matches grouped by file, then — when the result was
// capped — a footer carrying either the formatted-spill recovery locator or
// the could-not-save explanation. The omitted count is a budget fact: the
// search itself completed.
func FormatGrepOutput(kept []GrepMatch, seen int, truncated bool, spillRef *SpillRef) string {
	var header string
	if truncated {
		header = fmt.Sprintf("Found %d of %d matches", len(kept), seen)
	} else {
		header = fmt.Sprintf("Found %d %s", seen, matchNoun(seen))
	}
	body := FormatGrepMatches(kept)
	if !truncated {
		return header + "\n\n" + body
	}
	recovery := "The complete result could not be saved; narrow pattern, path, or include to see more."
	if spillRef != nil {
		recovery = "Full grep result stored at: " + spillRef.Locator + ". " + spillRef.RetrievalHint
	}
	return header + "\n\n" + body + "\n\n(" + recovery + ")"
}

// FormatRetainedGrep formats one already-retained match list.
func FormatRetainedGrep(kept []GrepMatch, seen int, truncated bool, spillRef *SpillRef) string {
	if seen == 0 {
		return "No matches found"
	}
	return FormatGrepOutput(kept, seen, truncated, spillRef)
}
