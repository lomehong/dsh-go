package webserver

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// IndexInjectionPlacement is the document region a rendered row lands in:
// after the opening head or body tag.
type IndexInjectionPlacement string

const (
	// PlacementHead places the row after the opening <head> tag.
	PlacementHead IndexInjectionPlacement = "head"
	// PlacementBody places the row after the opening <body> tag.
	PlacementBody IndexInjectionPlacement = "body"
)

// IndexInjection row kinds — the closed union of the official IndexInjection.
const (
	RowGlobal        = "global"
	RowScript        = "script"
	RowScriptSrc     = "script-src"
	RowScriptPreload = "script-preload"
	RowStyle         = "style"
	RowHTML          = "html"
)

// IndexInjection is one structured index injection row: pure JSON-serializable
// data because one table feeds the served form (rendered into the index.html
// text) and a static worker deployment (shipped over its boot payload).
// Anything not expressible as a row stays on tapIndex, which runs after row
// rendering.
type IndexInjection struct {
	// Kind discriminates the row union: global, script, script-src,
	// script-preload, style, or html.
	Kind string
	// Global row: the globalThis property name.
	Name string
	// Global row: the JSON-serializable property value (nil renders as
	// `undefined`).
	Value any
	// Script and html rows: the placement region. Global, script-preload, and
	// style rows have a fixed placement.
	Placement IndexInjectionPlacement
	// Script and style rows: the inline text. Script text must not contain
	// "</script" (it would close the element early); style text must not
	// contain "</style".
	Text string
	// Script-src and script-preload rows: the external script URL.
	Src string
	// HTML row: the raw markup fragment.
	HTML string
}

// escapeHTMLAttribute escapes a row value before placing it in a quoted HTML
// attribute.
func escapeHTMLAttribute(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, `"`, "&quot;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	return strings.ReplaceAll(value, ">", "&gt;")
}

// jsonScriptSafe renders v as JSON with `<` escaped, so row-controlled strings
// cannot break out of a script element.
func jsonScriptSafe(v any) string {
	if v == nil {
		return "undefined"
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return "undefined"
	}
	return strings.ReplaceAll(string(encoded), "<", `\u003c`)
}

var (
	headOpen = regexp.MustCompile(`(?i)<head(?:\s[^>]*)?>`)
	bodyOpen = regexp.MustCompile(`(?i)<body(?:\s[^>]*)?>`)
)

// readyMarkup settles the boot-readiness deferred (__DSH_BOOT_READY__): the
// client entry awaits its promise before reading any injected state.
const readyMarkup = "<script>(globalThis.__DSH_BOOT_READY__ ??= Promise.withResolvers()).resolve()</script>"

// renderRow renders one row to markup with its placement. Unknown kinds fail
// loud — a row is composition-authored data, not external input.
func renderRow(row IndexInjection) (IndexInjectionPlacement, string, error) {
	switch row.Kind {
	case RowGlobal:
		name := jsonScriptSafe(row.Name)
		return PlacementHead, fmt.Sprintf("<script>globalThis[%s] = %s</script>", name, jsonScriptSafe(row.Value)), nil
	case RowScript:
		return row.Placement, "<script>" + row.Text + "</script>", nil
	case RowScriptSrc:
		return row.Placement, `<script src="` + escapeHTMLAttribute(row.Src) + `"></script>`, nil
	case RowScriptPreload:
		return PlacementHead, `<link rel="preload" as="script" href="` + escapeHTMLAttribute(row.Src) + `">`, nil
	case RowStyle:
		return PlacementHead, "<style>" + row.Text + "</style>", nil
	case RowHTML:
		return row.Placement, row.HTML, nil
	default:
		return "", "", fmt.Errorf("webserver: unknown index injection row kind %q", row.Kind)
	}
}

func splice(html string, at int, markup string) string {
	return html[:at] + markup + html[at:]
}

// RenderIndexInjections renders rows into an index.html body: head rows
// immediately after the opening head tag, body rows immediately after the
// opening body tag, each group in table order, and the boot-readiness tail
// after the last body row. Headless fixture pages may lack <head> (prepending
// keeps the rows ahead of every document script); body-less fragments receive
// the rows at the end, where the HTML parser has already synthesized a body.
func RenderIndexInjections(html string, rows []IndexInjection) (string, error) {
	var head, body strings.Builder
	for _, row := range rows {
		placement, markup, err := renderRow(row)
		if err != nil {
			return "", err
		}
		if placement == PlacementHead {
			head.WriteString(markup)
		} else {
			body.WriteString(markup)
		}
	}
	body.WriteString(readyMarkup)
	out := html
	if head.Len() > 0 {
		if open := headOpen.FindStringIndex(out); open != nil {
			out = splice(out, open[1], head.String())
		} else {
			out = head.String() + out
		}
	}
	if body.Len() > 0 {
		if open := bodyOpen.FindStringIndex(out); open != nil {
			out = splice(out, open[1], body.String())
		} else {
			out += body.String()
		}
	}
	return out, nil
}
