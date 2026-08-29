// Matcher ports hook-protocol/src/matcher.ts: the matcher shared by both
// hook dialects. Claude treats alphanumeric/underscore/pipe patterns as
// literal alternatives and other patterns as regex; Codex treats every
// non-empty pattern as an unanchored regex. Missing, empty, and '*' match
// all. Runtime matching contains invalid regexes as non-matches; config
// parsers use MatcherDiagnostic to reject them with a diagnostic.
//
// Regexes compile through Go's regexp (RE2); a pattern RE2 rejects is
// invalid here even where the JS engine would accept it — a documented
// engine-adaptation divergence surfaced by the same diagnostics.
package hookprotocol

import (
	"encoding/json"
	"regexp"
)

// claudeLiteral is true for a purely word-char-and-pipe pattern (the
// regex-vs-literal discriminator).
var claudeLiteral = regexp.MustCompile(`^[A-Za-z0-9_|]+$`)

// isMatchAll is true for an absent / empty / '*' pattern — the match-all
// sentinels.
func isMatchAll(matcher *string) bool {
	return matcher == nil || *matcher == "" || *matcher == "*"
}

// matcherPattern extracts the non-empty pattern past the match-all guard.
func matcherPattern(matcher *string) string {
	if matcher == nil {
		return ""
	}
	return *matcher
}

// compileRegex compiles an unanchored matcher regex; invalid patterns
// return nil.
func compileRegex(pattern string) *regexp.Regexp {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

// MatcherDiagnostic validates one matcher before a bridge accepts its
// config group. It returns "" for a valid matcher (match-all sentinels are
// valid), otherwise a stable diagnostic.
func MatcherDiagnostic(matcher *string, mode MatcherMode) string {
	if isMatchAll(matcher) {
		return ""
	}
	pattern := matcherPattern(matcher)
	if mode == MatcherModeClaudeCode && claudeLiteral.MatchString(pattern) {
		return ""
	}
	if compileRegex(pattern) == nil {
		return "invalid " + string(mode) + " regex matcher " + quotePattern(pattern)
	}
	return ""
}

// quotePattern renders a pattern the way JSON.stringify would, keeping the
// diagnostic byte-identical to the reference.
func quotePattern(pattern string) string {
	encoded, err := json.Marshal(pattern)
	if err != nil {
		return `"` + pattern + `"`
	}
	return string(encoded)
}

// MatchesMatcher reports whether matcher selects query under the given
// dialect. Claude literal patterns exact-match pipe-separated alternatives;
// all other patterns are unanchored regexes. Invalid regexes return false
// rather than panicking; bridge config parsers surface them through
// MatcherDiagnostic before use.
func MatchesMatcher(matcher *string, query string, mode MatcherMode) bool {
	if isMatchAll(matcher) {
		return true
	}
	pattern := matcherPattern(matcher)
	if mode == MatcherModeClaudeCode && claudeLiteral.MatchString(pattern) {
		for _, alternative := range regexp.MustCompile(`\|`).Split(pattern, -1) {
			if alternative == query {
				return true
			}
		}
		return false
	}
	re := compileRegex(pattern)
	if re == nil {
		return false
	}
	return re.MatchString(query)
}
