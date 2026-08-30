package webfetchhttp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"dshgo/llm"
	"dshgo/web"
)

// ProviderID is the stable id this provider registers under.
const ProviderID = "http"

// HttpFetchLimits are the resolved provider limits (the plugin config
// supplies every default).
type HttpFetchLimits struct {
	// Maximum response body size in bytes (read stops past this).
	MaxResponseBytes int
	// Maximum decoded body length in characters (truncated past this).
	MaxBodyChars int
	// Default fetch timeout in milliseconds.
	TimeoutMs int
	// Maximum number of (same-origin) redirect hops to follow.
	MaxRedirects int
	// User-Agent header sent on every request.
	UserAgent string
}

// Provider is the anonymous public HTTP(S) fetch provider: it validates and
// pins public IP destinations, follows only same-origin redirects, enforces
// time and size limits, classifies and decodes text, and leaves
// presentation to dsh-tool-web. Requests carry no browser cookies or
// ambient credentials.
type Provider struct {
	limits  HttpFetchLimits
	resolve AddressResolver
}

// NewProvider builds the provider with production public-address
// resolution.
func NewProvider(limits HttpFetchLimits) *Provider {
	return NewProviderWithResolver(limits, resolvePublicAddresses)
}

// NewProviderWithResolver builds the provider with an injected resolver
// that rejects non-public destinations before returning (the official
// resolver seam, overridden only by focused tests).
func NewProviderWithResolver(limits HttpFetchLimits, resolve AddressResolver) *Provider {
	return &Provider{limits: limits, resolve: resolve}
}

// ID returns the registration id.
func (p *Provider) ID() string { return ProviderID }

// Available is always true — no credentials to check; an anonymous public
// fetcher is always usable.
func (p *Provider) Available() bool { return true }

// errFetchTimeout marks our own deadline so translation can distinguish
// this provider's timeout from caller or outer-deadline cancellation (the
// official TimeoutReason seam).
var errFetchTimeout = errors.New("web fetch timed out")

// Fetch retrieves one URL. One context stops both the request and the body
// read.
func (p *Provider) Fetch(ctx context.Context, request web.WebFetchRequest) (web.WebFetchResult, error) {
	if ctx.Err() != nil {
		return web.WebFetchResult{}, web.NewWebError("WEB_ABORTED", "web fetch aborted", nil)
	}
	fetchCtx, cancel := context.WithTimeoutCause(ctx,
		time.Duration(p.limits.TimeoutMs)*time.Millisecond, errFetchTimeout)
	defer cancel()
	return p.followAndRead(fetchCtx, request.URL)
}

// followAndRead follows same-origin redirects up to the hop cap, then reads
// the final response.
func (p *Provider) followAndRead(ctx context.Context, initialURL string) (web.WebFetchResult, error) {
	current, err := validateFetchURL(initialURL)
	if err != nil {
		return web.WebFetchResult{}, err
	}
	redirectsFollowed := 0

	for {
		resp, closer, err := p.requestOnce(ctx, current)
		if err != nil {
			return web.WebFetchResult{}, err
		}
		if isRedirectStatus(resp.StatusCode) {
			result, redirectErr := p.followRedirect(ctx, resp, current, &redirectsFollowed)
			resp.Body.Close()
			closer()
			if redirectErr != nil {
				return web.WebFetchResult{}, redirectErr
			}
			current = result
			continue
		}
		result, readErr := p.readBody(ctx, resp, current)
		resp.Body.Close()
		closer()
		return result, readErr
	}
}

// followRedirect validates one redirect hop against the same transport
// hygiene a direct request gets: a redirect must not be a back door to a
// credentialed, non-http(s), or over-long URL, and never crosses origins.
// On success it returns the validated next URL.
func (p *Provider) followRedirect(ctx context.Context, resp *http.Response, current *url.URL, followed *int) (*url.URL, error) {
	// Enforce the redirect budget before resolving or validating the next hop.
	if *followed >= p.limits.MaxRedirects {
		return nil, web.NewWebError("WEB_REDIRECT_BLOCKED",
			"exceeded the maximum of "+strconv.Itoa(p.limits.MaxRedirects)+" redirects", nil)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		// A redirect status with no Location is not a usable resource.
		return nil, web.NewWebError("WEB_PROVIDER_ERROR",
			"redirect response (HTTP "+strconv.Itoa(resp.StatusCode)+") without a Location header", nil)
	}
	target, err := resolveRedirect(location, current)
	if err != nil {
		return nil, err
	}
	validated, err := validateFetchURL(target.String())
	if err != nil {
		return nil, err
	}
	if !isSameOrigin(validated, current) {
		return nil, web.NewWebError("WEB_REDIRECT_BLOCKED",
			"cross-origin redirect to "+validated.Scheme+"://"+validated.Host+
				" is not followed automatically; retry against that URL directly", nil)
	}
	*followed++
	return validated, nil
}

func (p *Provider) requestOnce(ctx context.Context, u *url.URL) (*http.Response, func(), error) {
	addresses, err := p.resolve(ctx, u.Hostname())
	if err != nil {
		return nil, nil, translateIfWebError(err, ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, translateFetchError(err, ctx)
	}
	req.Header.Set("User-Agent", p.limits.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8")
	resp, closer, err := requestPinned(ctx, req, addresses)
	if err != nil {
		return nil, nil, translateIfWebError(err, ctx)
	}
	return resp, closer, nil
}

// readBody reads, byte-caps, classifies, and decodes the final response
// body.
func (p *Provider) readBody(ctx context.Context, resp *http.Response, finalURL *url.URL) (web.WebFetchResult, error) {
	contentType := resp.Header.Get("Content-Type")
	kind, ok := classifyContentType(contentType)
	if !ok {
		reported := contentType
		if reported == "" {
			reported = "unknown"
		}
		return web.WebFetchResult{}, web.NewWebError("WEB_UNSUPPORTED_CONTENT_TYPE",
			`unsupported content type "`+reported+`"`, nil)
	}

	// Resolve the decoder BEFORE reading the body so an unsupported charset
	// fails without consuming the stream.
	decoder, err := decoderForCharset(parseCharset(contentType))
	if err != nil {
		return web.WebFetchResult{}, err
	}
	body, truncatedByBytes, err := p.readCapped(ctx, resp)
	if err != nil {
		return web.WebFetchResult{}, err
	}
	decoded, err := decodeBody(decoder, bytes.NewReader(body))
	if err != nil {
		return web.WebFetchResult{}, translateIfWebError(err, ctx)
	}
	runes := []rune(decoded)
	truncatedByChars := len(runes) > p.limits.MaxBodyChars
	content := decoded
	if truncatedByChars {
		content = string(runes[:p.limits.MaxBodyChars])
	}
	return web.WebFetchResult{
		URL:        finalURL.String(),
		StatusCode: resp.StatusCode,
		Body:       web.WebFetchBody{Kind: kind, Content: content},
		Truncated:  truncatedByBytes || truncatedByChars,
	}, nil
}

// readCapped reads the response stream up to MaxResponseBytes. A
// Content-Length over the cap rejects immediately with WEB_FETCH_TOO_LARGE;
// a stream that grows past the cap is cut short (truncated) rather than
// rejected, so a server that under-reports still yields a bounded usable
// body.
func (p *Provider) readCapped(ctx context.Context, resp *http.Response) ([]byte, bool, error) {
	if declared := resp.Header.Get("Content-Length"); declared != "" {
		if length, err := strconv.Atoi(declared); err == nil && length > p.limits.MaxResponseBytes {
			return nil, false, web.NewWebError("WEB_FETCH_TOO_LARGE",
				"response exceeds the maximum of "+strconv.Itoa(p.limits.MaxResponseBytes)+" bytes", nil)
		}
	}

	var chunks [][]byte
	total := 0
	truncatedByBytes := false
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			remaining := p.limits.MaxResponseBytes - total
			// Only DROPPED bytes count as truncation: a chunk that exactly
			// fills the remaining capacity keeps all its bytes and we read
			// on to observe EOF, so an exactly-at-cap body is not falsely
			// flagged truncated.
			if n > remaining {
				chunks = append(chunks, append([]byte(nil), buf[:remaining]...))
				total += remaining
				truncatedByBytes = true
				break
			}
			chunks = append(chunks, append([]byte(nil), buf[:n]...))
			total += n
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false, translateIfWebError(readErr, ctx)
		}
	}

	body := make([]byte, 0, total)
	for _, chunk := range chunks {
		body = append(body, chunk...)
	}
	return body, truncatedByBytes, nil
}

// isRedirectStatus reports the HTTP redirect statuses that carry a Location.
func isRedirectStatus(status int) bool {
	return status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

// resolveRedirect resolves a (possibly relative) Location against the
// current URL.
func resolveRedirect(location string, base *url.URL) (*url.URL, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return nil, web.NewWebError("WEB_PROVIDER_ERROR",
			`invalid redirect Location "`+location+`"`, err)
	}
	return base.ResolveReference(parsed), nil
}

// translateIfWebError passes typed web errors through untouched (official
// error instanceof WebError rethrow) and translates everything else.
func translateIfWebError(err error, ctx context.Context) error {
	var webErr *llm.Error
	if errors.As(err, &webErr) {
		return err
	}
	return translateFetchError(err, ctx)
}

// translateFetchError classifies a transport failure by the context state
// rather than the error value: our own timeout cause means WEB_FETCH_TIMEOUT;
// any other cancellation — upstream or a foreign/outer deadline under
// nesting — is WEB_ABORTED; a failure with a live context is a network
// fault (WEB_PROVIDER_ERROR).
func translateFetchError(err error, ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && errors.Is(context.Cause(ctx), errFetchTimeout) {
		return web.NewWebError("WEB_FETCH_TIMEOUT", "web fetch timed out", err)
	}
	if ctx.Err() != nil {
		return web.NewWebError("WEB_ABORTED", "web fetch aborted", err)
	}
	return web.NewWebError("WEB_PROVIDER_ERROR", "web fetch failed: "+err.Error(), err)
}
