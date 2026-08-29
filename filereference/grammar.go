// Package filereference ports @deepseek-ai/dsh-file-reference and its
// -local provider: the `@file` completion grammar shared by every client and
// the local-workspace fuzzy discovery behind it. The index holds paths only
// — selected values stay ordinary prompt text and file contents remain
// behind the model-facing read tool.
//
// Go adaptation: cursor columns are rune offsets (the editor grammar counts
// UTF-16 code units), agent-keyed service wiring carries the session cwd
// explicitly, and the system-prompt/tool-lookup section install is host
// wiring documented in service.go.
package filereference

import (
	"regexp"
	"strings"
	"unicode"
)

// Candidate is one path-only completion candidate inside the target session
// cwd.
type Candidate struct {
	// Path is the user-facing path accepted by normal prompts and
	// filesystem tools, always `/`-separated.
	Path string
	// Kind: directories keep completion open; files finish the mention.
	Kind string // "file" | "directory"
}

// ActiveAtToken is the active `@` token ending at the editor cursor.
type ActiveAtToken struct {
	// Prefix is the complete token replaced when the user accepts a
	// completion.
	Prefix string
	// Query is the path text after `@` or `@"`.
	Query string
	// Quoted reports whether the user opened a quoted path.
	Quoted bool
}

var (
	quotedAtToken = regexp.MustCompile(`(?:^|\s)(@"([^"]*))$`)
	plainAtToken  = regexp.MustCompile(`(?:^|\s)(@([^\s]*))$`)
)

// ActiveAtTokenOf extracts an `@path` or `@"path with spaces` token at the
// cursor. An `@` inside another token, such as an email address, is not a
// completion trigger. cursorCol is a rune offset into line; the result is
// nil outside an `@` token.
func ActiveAtTokenOf(line string, cursorCol int) *ActiveAtToken {
	runes := []rune(line)
	if cursorCol < 0 {
		cursorCol = 0
	}
	if cursorCol > len(runes) {
		cursorCol = len(runes)
	}
	beforeCursor := string(runes[:cursorCol])
	if match := quotedAtToken.FindStringSubmatch(beforeCursor); match != nil {
		return &ActiveAtToken{Prefix: match[1], Query: match[2], Quoted: true}
	}
	if match := plainAtToken.FindStringSubmatch(beforeCursor); match != nil {
		return &ActiveAtToken{Prefix: match[1], Query: match[2], Quoted: false}
	}
	return nil
}

// formatRejectPredicate is the set a path must not contain for the editor
// grammar to represent it safely: C0/C1 control characters and quotes.
func formatRejectPredicate(r rune) bool {
	return (r >= 0x00 && r <= 0x1f) || (r >= 0x7f && r <= 0x9f) || r == '"'
}

// FormatFileMention formats a selected path as prompt text. Whitespace uses
// the quoted `@"path"` grammar; a quoted directory keeps that quote open
// after its trailing slash so completion can descend another level. The
// result is "" for a path the editor grammar cannot represent safely.
func FormatFileMention(candidate Candidate, preserveQuote bool) string {
	path := candidate.Path
	if candidate.Kind == "directory" {
		path += "/"
	}
	if strings.ContainsFunc(path, formatRejectPredicate) {
		return ""
	}
	quoted := preserveQuote || strings.ContainsFunc(path, func(r rune) bool {
		// Mirrors the JavaScript /\s/u set: Unicode whitespace plus BOM.
		return unicode.IsSpace(r) || r == 0xFEFF
	})
	if !quoted {
		return "@" + path
	}
	if candidate.Kind == "directory" {
		return `@"` + path
	}
	return `@"` + path + `"`
}

// FileReferencePrompt is the model guidance for path-only references
// selected by a user interface (pinned verbatim).
const FileReferencePrompt = "Tokens prefixed with @ are workspace paths the user explicitly referenced, relative to the workspace root. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @\"...\" quotes a path containing spaces."
