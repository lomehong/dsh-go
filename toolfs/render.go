// Package toolfs registers the model-facing read, write, and edit tools over
// the fs seam (official @deepseek-ai/dsh-tool-fs). This package owns schemas,
// validation, read windows, formatting, and observation events, never a
// concrete provider. read_image stays unported by the source's own rule: it
// registers only while an attachment store is mounted, and the Go composition
// has none yet.
package toolfs

import (
	"fmt"
	"path"
	"strings"

	"dshgo/fs"
)

// Read rendering caps (official read-render.ts defaults).
const (
	// ReadLimit is the default and maximum number of lines returned by one
	// read call.
	ReadLimit = 2000
	// StreamMinSize is the default streaming threshold: files at or above
	// this size stream; smaller files read whole into memory.
	StreamMinSize = 10 * 1024 * 1024
	// ReadMaxLineLength is the default maximum characters for one line
	// before truncation.
	ReadMaxLineLength = 2000
	// ReadMaxBytes is the default maximum bytes returned for the selected
	// lines of one read call.
	ReadMaxBytes = 50 * 1024
)

// FileTextLine is one line returned from a text file.
type FileTextLine struct {
	// Number is the 1-based line number in the file.
	Number int
	// Text is the line without its trailing newline.
	Text string
}

// ReadWindow is the resolved read window: the consumer applies its defaults
// and caps before calling.
type ReadWindow struct {
	Offset        int
	Limit         int
	MaxLineLength int
	MaxBytes      int
}

// WindowResult is the windowed result BuildWindow produces.
type WindowResult struct {
	Lines            []FileTextLine
	TotalLines       int
	TruncatedByBytes bool
}

// TextChunks is the chunk source shape both read routes adapt to:
// fs.StreamText's iterator or a single whole-file read.
type TextChunks func() (string, bool)

// windowAccumulator is the scan state.
type windowAccumulator struct {
	lines            []FileTextLine
	totalLines       int
	outputBytes      int
	truncatedByBytes bool
}

func truncateLine(line string, maxLineLength int) string {
	if len(line) > maxLineLength {
		return fmt.Sprintf("%s... (line truncated to %d chars)", line[:maxLineLength], maxLineLength)
	}
	return line
}

func consumeLine(acc *windowAccumulator, rawLine string, request ReadWindow) {
	acc.totalLines++
	if acc.truncatedByBytes || acc.totalLines < request.Offset || len(acc.lines) >= request.Limit {
		return
	}
	text := truncateLine(rawLine, request.MaxLineLength)
	bytes := len(text)
	if len(acc.lines) > 0 {
		bytes++ // the newline separating this line from the previous one
	}
	if acc.outputBytes+bytes > request.MaxBytes {
		acc.truncatedByBytes = true
		return
	}
	acc.outputBytes += bytes
	acc.lines = append(acc.lines, FileTextLine{Number: acc.totalLines, Text: text})
}

func stripCarriageReturn(line string) string {
	return strings.TrimSuffix(line, "\r")
}

// BuildWindow builds one window from streamed or whole-file chunks, enforcing
// line and byte caps while still scanning to an exact total line count, and
// failing FS_NOT_FOUND when the requested offset is past EOF. Chunk scanning
// caps the current line, so even one newline-free giant line cannot grow
// memory without bound.
func BuildWindow(chunks TextChunks, request ReadWindow, displayPath string) (WindowResult, error) {
	acc := &windowAccumulator{lines: []FileTextLine{}}
	// One char past the truncation point is enough to prove a line overflows.
	lineBufferCap := request.MaxLineLength + 1
	lineBuffer := ""
	appendSegment := func(segment string) {
		if len(lineBuffer) >= lineBufferCap {
			return
		}
		lineBuffer += segment
		if len(lineBuffer) > lineBufferCap {
			lineBuffer = lineBuffer[:lineBufferCap]
		}
	}
	flush := func() {
		consumeLine(acc, stripCarriageReturn(lineBuffer), request)
		lineBuffer = ""
	}
	for {
		chunk, ok := chunks()
		if !ok {
			break
		}
		start := 0
		for {
			at := strings.Index(chunk[start:], "\n")
			if at < 0 {
				break
			}
			at += start
			appendSegment(chunk[start:at])
			flush()
			start = at + 1
		}
		appendSegment(chunk[start:])
	}
	if lineBuffer != "" {
		flush()
	}
	if !acc.truncatedByBytes && request.Offset > acc.totalLines && !(acc.totalLines == 0 && request.Offset == 1) {
		return WindowResult{}, fs.NewError(fs.CodeNotFound, fmt.Sprintf("offset %d is out of range for %q (%d lines)", request.Offset, displayPath, acc.totalLines), nil)
	}
	return WindowResult{Lines: acc.lines, TotalLines: acc.totalLines, TruncatedByBytes: acc.truncatedByBytes}, nil
}

// FormatReadOutput renders the model-facing envelope: numbered lines plus a
// continuation or end-of-file footer.
func FormatReadOutput(displayPath string, offset int, lines []FileTextLine, totalLines int, truncatedByBytes bool) string {
	endLine := offset - 1
	if len(lines) > 0 {
		endLine = lines[len(lines)-1].Number
	}
	var footer string
	switch {
	case truncatedByBytes:
		footer = fmt.Sprintf("(Output capped. Showing lines %d-%d. Use offset=%d to continue.)", offset, endLine, endLine+1)
	case endLine < totalLines:
		footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, endLine, totalLines, endLine+1)
	default:
		footer = fmt.Sprintf("(End of file - total %d lines)", totalLines)
	}
	body := footer
	if len(lines) > 0 {
		rows := make([]string, 0, len(lines))
		for _, line := range lines {
			rows = append(rows, fmt.Sprintf("%d: %s", line.Number, line.Text))
		}
		body = strings.Join(rows, "\n") + "\n\n" + footer
	}
	return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n</content>", displayPath, body)
}

// langByExtension maps a lowercased file extension (without its dot) to a
// syntax-highlighting hint. The map is intentionally small — common source,
// config, and markup extensions — not an exhaustive registry.
var langByExtension = map[string]string{
	"ts": "ts", "tsx": "tsx", "mts": "ts", "cts": "ts",
	"js": "js", "jsx": "jsx", "mjs": "js", "cjs": "js",
	"json": "json", "jsonc": "json",
	"py": "py", "rb": "rb", "go": "go", "rs": "rs", "java": "java",
	"c": "c", "h": "c", "cc": "cpp", "cpp": "cpp", "hpp": "cpp", "cxx": "cpp",
	"cs": "cs", "kt": "kotlin", "swift": "swift", "php": "php",
	"sh": "sh", "bash": "sh", "zsh": "sh",
	"yaml": "yaml", "yml": "yaml", "toml": "toml", "ini": "ini",
	"md": "md", "markdown": "md", "mdx": "mdx",
	"html": "html", "htm": "html", "css": "css", "scss": "scss", "less": "less",
	"sql": "sql", "xml": "xml", "lua": "lua",
}

// LangFromPath derives a syntax-highlighting language hint from a read
// path's extension. Pure and case-insensitive; a dotfile with no extension
// and an unknown extension both yield "".
func LangFromPath(filePath string) string {
	base := path.Base(strings.ReplaceAll(filePath, "\\", "/"))
	dot := strings.LastIndex(base, ".")
	// A leading dot is a dotfile (no extension), not an empty extension.
	if dot <= 0 {
		return ""
	}
	return langByExtension[strings.ToLower(base[dot+1:])]
}
