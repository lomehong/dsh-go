package webfetchhttp

import (
	"io"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"

	"dshgo/web"
)

// decoderForCharset resolves the declared charset to a byte-to-rune decoder,
// falling back to UTF-8 when none is declared. A non-UTF-8 response is
// decoded with its declared encoding rather than silently mangled into
// replacement characters. Returns WEB_UNSUPPORTED_CONTENT_TYPE when the
// label is present but unrecognized — better to fail loud than return
// mojibake (the official TextDecoder constructor behavior).
func decoderForCharset(charset string) (encoding.Encoding, error) {
	if charset == "" {
		return unicode.UTF8, nil
	}
	enc, err := ianaindex.IANA.Encoding(charset)
	if err != nil || enc == nil {
		return nil, web.NewWebError("WEB_UNSUPPORTED_CONTENT_TYPE",
			`unsupported charset "`+charset+`"`, err)
	}
	return enc, nil
}

// decodeBody runs the capped byte stream through the charset decoder.
func decodeBody(enc encoding.Encoding, bytes io.Reader) (string, error) {
	decoded, err := io.ReadAll(enc.NewDecoder().Reader(bytes))
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
