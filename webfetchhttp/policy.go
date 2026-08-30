// Package webfetchhttp is the anonymous public HTTP(S) WebFetchProvider
// plugin: it contributes to the ctx.web registry without owning the service.
// Ported from @deepseek-ai/dsh-web-fetch-http (provider id "http").
//
// Safe HTTP(S) retrieval: validates and pins public IP destinations, follows
// only same-origin redirects, enforces time and size limits, classifies and
// decodes text, and leaves presentation to dsh-tool-web. Requests carry no
// browser cookies or ambient credentials.
package webfetchhttp

import (
	"net/url"
	"regexp"
	"strings"

	"dshgo/web"
)

// MaxURLLength is the maximum accepted request URL length enforced by the
// public fetch provider.
const MaxURLLength = 2048

// parseFetchURL parses a request URL and enforces network-independent
// transport restrictions: HTTP(S) only and no embedded credentials. The
// provider applies this before resolving a destination.
func parseFetchURL(input string) (*url.URL, error) {
	parsed, err := url.Parse(input)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return nil, web.NewWebError("WEB_INVALID_URL", "invalid URL: "+input, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, web.NewWebError("WEB_INVALID_URL",
			`unsupported URL scheme "`+parsed.Scheme+`" (only http and https are allowed)`, nil)
	}
	if parsed.User != nil {
		return nil, web.NewWebError("WEB_BLOCKED_URL", "credentials in URLs are not allowed", nil)
	}
	return parsed, nil
}

// validateFetchURL validates a request URL against the provider's complete
// pre-network policy: bounded length plus the restrictions enforced by
// parseFetchURL. Public-address resolution and connection pinning run after
// this check.
func validateFetchURL(input string) (*url.URL, error) {
	if len(input) > MaxURLLength {
		return nil, web.NewWebError("WEB_INVALID_URL",
			"URL exceeds the maximum length of 2048", nil)
	}
	return parseFetchURL(input)
}

// isSameOrigin reports whether two URLs share scheme, hostname, and port. A
// redirect that crosses origins is refused so each new origin requires a
// fresh tool call and public-address validation. (Ports compare as declared
// strings: the WHATWG default-port normalization the official URL origin
// applies is mirrored by net/url's Host field for http/https URLs.)
func isSameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && a.Host == b.Host
}

// classifyContentType classifies a response Content-Type into a decodable
// body kind, or false for an unsupported (e.g. binary) type. text/html and
// application/xhtml+xml are html; other text/* plus a few structured text
// types are text. The empty string (no Content-Type header) is unsupported.
func classifyContentType(contentType string) (web.WebFetchBodyKind, bool) {
	mime := contentType
	if index := strings.Index(mime, ";"); index >= 0 {
		mime = mime[:index]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if mime == "text/html" || mime == "application/xhtml+xml" {
		return web.BodyHTML, true
	}
	if strings.HasPrefix(mime, "text/") {
		return web.BodyText, true
	}
	if mime == "application/json" || mime == "application/xml" ||
		strings.HasSuffix(mime, "+json") || strings.HasSuffix(mime, "+xml") {
		return web.BodyText, true
	}
	return "", false
}

// charsetPattern extracts the charset parameter from a Content-Type value.
var charsetPattern = regexp.MustCompile(`(?i);\s*charset\s*=\s*"?([^";]+)"?`)

// parseCharset extracts the charset parameter from a response Content-Type,
// lower-cased, or the empty string when absent.
func parseCharset(contentType string) string {
	match := charsetPattern.FindStringSubmatch(contentType)
	if match == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(match[1]))
}
