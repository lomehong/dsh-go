package toolweb

import (
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Lexical HTML→markdown conversion for the web_fetch render path. The
// official converter is turndown+GFM over a full DOM; the Go build uses this
// dependency-free tokenizer instead. Recorded degradation: GFM tables,
// strikethrough, and nested-structure fidelity are approximated — block
// boundaries, headings, lists, links, emphasis, and code survive.

var (
	spaceRuns   = regexp.MustCompile(`[ \t\r\f]+`)
	newlineRuns = regexp.MustCompile(`\n{3,}`)
	commentRun  = regexp.MustCompile(`(?s)<!--.*?-->`)
	tagToken    = regexp.MustCompile(`(?s)<(/?)([a-zA-Z][a-zA-Z0-9]*)((?:\s+[^<>]*?)?)(/?)>`)
	attrHref    = regexp.MustCompile(`(?i)href\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrSrc     = regexp.MustCompile(`(?i)src\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	attrAlt     = regexp.MustCompile(`(?i)alt\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

// rawTextElements swallow all content up to their matching end tag.
var rawTextElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
}

// voidElements never take a closing tag.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

// blockElements force line boundaries around their content.
var blockElements = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"div": true, "dl": true, "dd": true, "dt": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hr": true, "li": true, "main": true, "nav": true,
	"ol": true, "p": true, "pre": true, "section": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"tr": true, "ul": true,
}

// headingLevels map heading tags to their ATX marker width.
var headingLevels = map[string]int{
	"h1": 1, "h2": 2, "h3": 3, "h4": 4, "h5": 5, "h6": 6,
}

type htmlConverter struct {
	out     strings.Builder
	line    strings.Builder
	openURL string
}

func (c *htmlConverter) flushLine() {
	text := strings.TrimSpace(c.line.String())
	c.line.Reset()
	if text == "" {
		return
	}
	written := c.out.String()
	if written != "" && !strings.HasSuffix(written, "\n\n") {
		if strings.HasSuffix(written, "\n") {
			c.out.WriteString("\n")
		} else {
			c.out.WriteString("\n\n")
		}
	}
	c.out.WriteString(text)
	c.out.WriteString("\n")
}

func (c *htmlConverter) text(chunk string) {
	c.line.WriteString(chunk)
}

func (c *htmlConverter) blockBreak() {
	c.flushLine()
}

// convertHTMLToMarkdown renders one HTML document fragment as markdown.
func convertHTMLToMarkdown(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	stripped := commentRun.ReplaceAllString(content, "")
	c := &htmlConverter{}
	position := 0
	for position < len(stripped) {
		match := tagToken.FindStringSubmatchIndex(stripped[position:])
		if match == nil {
			c.text(decodeEntities(spaceRuns.ReplaceAllString(stripped[position:], " ")))
			break
		}
		if match[0] > 0 {
			c.text(decodeEntities(spaceRuns.ReplaceAllString(stripped[position:position+match[0]], " ")))
		}
		full := stripped[position+match[0] : position+match[1]]
		groups := tagToken.FindStringSubmatch(full)
		isClosing := groups[1] == "/"
		name := strings.ToLower(groups[2])
		attrs := groups[3]
		selfClosing := groups[4] == "/"
		position += match[1]

		if rawTextElements[name] && !isClosing {
			// Swallow raw content up to the matching end tag.
			endIndex := indexFold(stripped[position:], "</"+name)
			if endIndex < 0 {
				position = len(stripped)
			} else {
				position += endIndex
			}
			c.blockBreak()
			continue
		}
		if isClosing || selfClosing || voidElements[name] {
			c.closeTag(name)
			continue
		}
		c.openTag(name, attrs)
	}
	c.blockBreak()
	out := newlineRuns.ReplaceAllString(c.out.String(), "\n\n")
	return strings.TrimSpace(out)
}

func indexFold(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if strings.EqualFold(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

func (c *htmlConverter) openTag(name, attrs string) {
	switch {
	case name == "br":
		c.text("\n")
	case name == "hr":
		c.blockBreak()
		c.text("---")
		c.blockBreak()
	case name == "img":
		alt := firstAttr(attrs, attrAlt)
		src := firstAttr(attrs, attrSrc)
		label := alt
		if label == "" {
			label = src
		}
		if src != "" {
			c.text(fmt.Sprintf("![%s](%s)", label, src))
		} else if label != "" {
			c.text(label)
		}
	case name == "a":
		c.openURL = firstAttr(attrs, attrHref)
		c.text("[")
	case name == "strong" || name == "b":
		c.text("**")
	case name == "em" || name == "i":
		c.text("*")
	case name == "code":
		c.text("`")
	case name == "li":
		c.text("- ")
	case headingLevels[name] > 0:
		c.blockBreak()
		c.text(strings.Repeat("#", headingLevels[name]) + " ")
	case blockElements[name]:
		c.blockBreak()
	}
}

func (c *htmlConverter) closeTag(name string) {
	switch {
	case name == "a":
		c.text("](" + c.openURL + ")")
		c.openURL = ""
	case name == "strong" || name == "b":
		c.text("**")
	case name == "em" || name == "i":
		c.text("*")
	case name == "code":
		c.text("`")
	case headingLevels[name] > 0 || blockElements[name]:
		c.blockBreak()
	}
}

func firstAttr(attrs string, pattern *regexp.Regexp) string {
	match := pattern.FindStringSubmatch(attrs)
	if match == nil {
		return ""
	}
	for _, group := range match[2:] {
		if group != "" {
			return group
		}
	}
	return ""
}

// decodeEntities resolves the entities the lexical converter meets; numeric
// and named entities both route through html.UnescapeString at text intake.
func decodeEntities(text string) string {
	if !strings.Contains(text, "&") {
		return text
	}
	return html.UnescapeString(text)
}
