package webfetchhttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/web"
)

// The tests never touch real DNS or public networks: the provider's resolver
// seam is injected so the pinned connection lands on the local test server
// while the URL keeps a public-looking hostname.

const testHostname = "example.test"

func webErrorCode(t *testing.T, err error) string {
	t.Helper()
	var webErr *llm.Error
	if !errors.As(err, &webErr) {
		t.Fatalf("expected a WebError, got %v", err)
	}
	return webErr.Code()
}

func testLimits() HttpFetchLimits {
	return HttpFetchLimits{
		MaxResponseBytes: 100_000,
		MaxBodyChars:     10_000,
		TimeoutMs:        5_000,
		MaxRedirects:     5,
		UserAgent:        DefaultUserAgent,
	}
}

// newPinnedProvider wires the provider's resolver seam to the given local
// address, so requests to http://example.test:<port> land on the test
// server. The URL keeps an explicit port so it stays fully parseable while
// the dialer swaps in the local address.
func newPinnedProvider(t *testing.T, limits HttpFetchLimits, address string) (*Provider, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split test address: %v", err)
	}
	provider := NewProviderWithResolver(limits, func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		return []PublicAddress{{Address: host, Family: 4}}, nil
	})
	return provider, "http://" + testHostname + ":" + port
}

func TestValidateFetchUrlPolicy(t *testing.T) {
	cases := []struct {
		input    string
		wantCode string
		wantText string
	}{
		{"not a url", "WEB_INVALID_URL", ""},
		{"ftp://example.test/file", "WEB_INVALID_URL", `unsupported URL scheme "ftp" (only http and https are allowed)`},
		{"http://user:pass@example.test/", "WEB_BLOCKED_URL", "credentials in URLs are not allowed"},
		{"http://example.test/" + strings.Repeat("a", MaxURLLength), "WEB_INVALID_URL", "URL exceeds the maximum length of 2048"},
	}
	for _, c := range cases {
		_, err := validateFetchURL(c.input)
		if got := webErrorCode(t, err); got != c.wantCode {
			t.Fatalf("%q code = %q, want %q", c.input, got, c.wantCode)
		}
		if c.wantText != "" && err.Error() != c.wantText {
			t.Fatalf("%q wording = %q", c.input, err.Error())
		}
	}
	if _, err := validateFetchURL("https://" + testHostname + "/ok"); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
}

func TestContentTypeClassification(t *testing.T) {
	cases := []struct {
		header   string
		wantKind web.WebFetchBodyKind
		wantOk   bool
	}{
		{"text/html; charset=utf-8", web.BodyHTML, true},
		{"application/xhtml+xml", web.BodyHTML, true},
		{"TEXT/HTML", web.BodyHTML, true},
		{"text/plain", web.BodyText, true},
		{"application/json", web.BodyText, true},
		{"application/ld+json", web.BodyText, true},
		{"application/xml", web.BodyText, true},
		{"image/png", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		kind, ok := classifyContentType(c.header)
		if kind != c.wantKind || ok != c.wantOk {
			t.Fatalf("classify %q = %q/%v", c.header, kind, ok)
		}
	}
	if got := parseCharset(`text/html; Charset = "UTF-8"`); got != "utf-8" {
		t.Fatalf("parseCharset = %q", got)
	}
	if got := parseCharset("text/html"); got != "" {
		t.Fatalf("parseCharset absent = %q", got)
	}
}

func TestIsPublicUnicast(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2001:4860:4860::8888", true},
		{"::ffff:8.8.8.8", true},
		{"127.0.0.1", false},
		{"10.0.0.1", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.1.1", false},
		{"100.64.0.1", false},  // CGNAT
		{"192.0.0.170", false}, // IETF protocol assignments
		{"198.18.0.1", false},  // benchmarking
		{"240.0.0.1", false},   // reserved
		{"0.0.0.0", false},
		{"255.255.255.255", false},
		{"::1", false},
		{"fc00::1", false},
		{"fe80::1", false},
		{"ff02::1", false},
		{"2001:db8::1", false},
		{"::ffff:10.0.0.1", false},
	}
	for _, c := range cases {
		if got := isPublicUnicast(net.ParseIP(c.address)); got != c.want {
			t.Fatalf("isPublicUnicast(%s) = %v", c.address, got)
		}
	}
}

func TestResolveRejectsNonPublicAnswerSets(t *testing.T) {
	// One private address in the set rejects the complete set.
	resolver := AddressResolver(func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		return []PublicAddress{
			{Address: "93.184.216.34", Family: 4},
			{Address: "127.0.0.1", Family: 4},
		}, nil
	})
	_, err := resolveWith(context.Background(), testHostname, resolver)
	if got := webErrorCode(t, err); got != "WEB_BLOCKED_URL" {
		t.Fatalf("code = %q", got)
	}
	if want := `URL hostname "example.test" resolves to a non-public IP address`; err.Error() != want {
		t.Fatalf("wording = %q", err.Error())
	}

	// Empty answer sets fail loud.
	empty := AddressResolver(func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		return nil, nil
	})
	_, err = resolveWith(context.Background(), testHostname, empty)
	if webErrorCode(t, err) != "WEB_PROVIDER_ERROR" ||
		err.Error() != `hostname "example.test" resolved to no addresses` {
		t.Fatalf("empty set = %q", err.Error())
	}

	// IP literals skip resolution entirely but stay policy-checked.
	addresses, err := resolveWith(context.Background(), "93.184.216.34", empty)
	if err != nil || len(addresses) != 1 || addresses[0].Address != "93.184.216.34" {
		t.Fatalf("literal = %+v, %v", addresses, err)
	}
	_, err = resolveWith(context.Background(), "[::1]", empty)
	if webErrorCode(t, err) != "WEB_BLOCKED_URL" {
		t.Fatalf("loopback literal = %v", err)
	}
}

func TestResolveDetectsNat64TranslatedPrivateDestinations(t *testing.T) {
	resolver := AddressResolver(func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		switch hostname {
		case ipv4onlyDiscoveryHost:
			// DNS64 synthesizes the well-known RFC 6052 96-bit prefix from
			// the RFC 7050 sentinel addresses (192.0.0.170/171).
			return []PublicAddress{{Address: "64:ff9b::c000:aa", Family: 6}}, nil
		case testHostname:
			// Public IPv6 whose embedded IPv4 (10.0.0.1) is private.
			return []PublicAddress{{Address: "64:ff9b::a00:1", Family: 6}}, nil
		}
		t.Fatalf("unexpected hostname %q", hostname)
		return nil, nil
	})
	_, err := resolveWith(context.Background(), testHostname, resolver)
	if got := webErrorCode(t, err); got != "WEB_BLOCKED_URL" {
		t.Fatalf("code = %q", got)
	}
	if want := `URL hostname "example.test" resolves through NAT64 to a non-public IPv4 address`; err.Error() != want {
		t.Fatalf("wording = %q", err.Error())
	}

	// A public embedded IPv4 passes the same path.
	public := AddressResolver(func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		if hostname == ipv4onlyDiscoveryHost {
			return []PublicAddress{{Address: "64:ff9b::c000:ab", Family: 6}}, nil
		}
		return []PublicAddress{{Address: "64:ff9b::808:808", Family: 6}}, nil
	})
	if _, err := resolveWith(context.Background(), testHostname, public); err != nil {
		t.Fatalf("public NAT64 destination blocked: %v", err)
	}
}

func TestFetchDecodesBodiesByContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html>hello</html>"))
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"ok":true}`))
		default:
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		}
	}))
	defer server.Close()
	provider, base := newPinnedProvider(t, testLimits(), server.Listener.Addr().String())

	result, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/html"})
	if err != nil || result.StatusCode != 200 || result.Body.Kind != web.BodyHTML || result.Body.Content != "<html>hello</html>" || result.Truncated {
		t.Fatalf("html fetch = %+v, %v", result, err)
	}
	if result.URL != base+"/html" {
		t.Fatalf("final URL = %q", result.URL)
	}

	result, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/json"})
	if err != nil || result.Body.Kind != web.BodyText || result.Body.Content != `{"ok":true}` {
		t.Fatalf("json fetch = %+v, %v", result, err)
	}

	// Unsupported (binary) content is a loud error, not a result.
	_, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/binary"})
	if got := webErrorCode(t, err); got != "WEB_UNSUPPORTED_CONTENT_TYPE" {
		t.Fatalf("binary code = %q", got)
	}
	if want := `unsupported content type "image/png"`; err.Error() != want {
		t.Fatalf("binary wording = %q", err.Error())
	}
}

func TestFetchDecodesDeclaredCharsets(t *testing.T) {
	// GBK bytes for 你好.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gbk" {
			w.Header().Set("Content-Type", "text/plain; charset=GBK")
			w.Write([]byte{0xC4, 0xE3, 0xBA, 0xC3})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=bogus-charset")
		w.Write([]byte("never read"))
	}))
	defer server.Close()
	provider, base := newPinnedProvider(t, testLimits(), server.Listener.Addr().String())

	result, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/gbk"})
	if err != nil || result.Body.Content != "你好" {
		t.Fatalf("gbk fetch = %+v, %v", result, err)
	}

	// Unrecognized charsets fail loud rather than return mojibake.
	_, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/bogus"})
	if got := webErrorCode(t, err); got != "WEB_UNSUPPORTED_CONTENT_TYPE" {
		t.Fatalf("charset code = %q", got)
	}
	if want := `unsupported charset "bogus-charset"`; err.Error() != want {
		t.Fatalf("charset wording = %q", err.Error())
	}
}

// rawServer scripts one byte-exact HTTP response over a raw socket, for the
// size-cap cases a well-behaved test server cannot produce.
func rawServer(t *testing.T, response []byte) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 4096)
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				for {
					n, err := conn.Read(buf)
					if err != nil || strings.Contains(string(buf[:n]), "\r\n\r\n") {
						break
					}
				}
				conn.Write(response)
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func TestFetchByteCaps(t *testing.T) {
	// Declared Content-Length over the cap rejects immediately.
	declared := rawServer(t, []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 500\r\n\r\n"+strings.Repeat("a", 500)))
	provider, base := newPinnedProvider(t, HttpFetchLimits{
		MaxResponseBytes: 100, MaxBodyChars: 10_000, TimeoutMs: 5_000, MaxRedirects: 5, UserAgent: DefaultUserAgent,
	}, declared)
	_, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/"})
	if got := webErrorCode(t, err); got != "WEB_FETCH_TOO_LARGE" {
		t.Fatalf("declared code = %q", got)
	}
	if want := "response exceeds the maximum of 100 bytes"; err.Error() != want {
		t.Fatalf("declared wording = %q", err.Error())
	}

	// Go's net/http body reads are bounded by the declared Content-Length,
	// so a stream that outgrows its declaration cannot reach readCapped; the
	// exact-at-cap case covers the truncation boundary (kept whole, no flag)
	// and the declared-over-cap case covers the rejection boundary.
	exact := rawServer(t, []byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\n"+strings.Repeat("c", 100)))
	provider, base = newPinnedProvider(t, HttpFetchLimits{
		MaxResponseBytes: 100, MaxBodyChars: 10_000, TimeoutMs: 5_000, MaxRedirects: 5, UserAgent: DefaultUserAgent,
	}, exact)
	result, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/"})
	if err != nil || result.Truncated || len(result.Body.Content) != 100 {
		t.Fatalf("exact cap = %+v, %v", result, err)
	}
}

func TestFetchTruncatesByChars(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("字", 300)))
	}))
	defer server.Close()
	limits := testLimits()
	limits.MaxBodyChars = 100
	provider, base := newPinnedProvider(t, limits, server.Listener.Addr().String())

	result, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/"})
	if err != nil || !result.Truncated {
		t.Fatalf("char truncation = %+v, %v", result, err)
	}
	if runes := []rune(result.Body.Content); len(runes) != 100 {
		t.Fatalf("char count = %d", len(runes))
	}
}

func TestFetchRedirectSemantics(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/final")
		w.WriteHeader(301)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("arrived"))
	})
	mux.HandleFunc("/loop-a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop-b")
		w.WriteHeader(302)
	})
	mux.HandleFunc("/loop-b", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/loop-a")
		w.WriteHeader(302)
	})
	mux.HandleFunc("/no-location", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(301)
	})
	mux.HandleFunc("/cross", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://"+testHostname+":9/elsewhere")
		w.WriteHeader(302)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	// Same-origin redirects land on the final resource with the final URL.
	provider, base := newPinnedProvider(t, testLimits(), server.Listener.Addr().String())
	result, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/moved"})
	if err != nil || result.StatusCode != 200 || result.Body.Content != "arrived" || result.URL != base+"/final" {
		t.Fatalf("redirect follow = %+v, %v", result, err)
	}

	// The hop budget is enforced before following further.
	tight := testLimits()
	tight.MaxRedirects = 1
	provider, base = newPinnedProvider(t, tight, server.Listener.Addr().String())
	_, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/loop-a"})
	if got := webErrorCode(t, err); got != "WEB_REDIRECT_BLOCKED" {
		t.Fatalf("budget code = %q", got)
	}
	if want := "exceeded the maximum of 1 redirects"; err.Error() != want {
		t.Fatalf("budget wording = %q", err.Error())
	}

	// A redirect without a Location is not a usable resource.
	_, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/no-location"})
	if webErrorCode(t, err) != "WEB_PROVIDER_ERROR" ||
		err.Error() != "redirect response (HTTP 301) without a Location header" {
		t.Fatalf("no-location = %q", err.Error())
	}

	// Cross-origin redirects are refused, naming the target origin.
	provider, base = newPinnedProvider(t, testLimits(), server.Listener.Addr().String())
	_, err = provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/cross"})
	if got := webErrorCode(t, err); got != "WEB_REDIRECT_BLOCKED" {
		t.Fatalf("cross-origin code = %q", got)
	}
	if want := "cross-origin redirect to http://" + testHostname + ":9 is not followed automatically; retry against that URL directly"; err.Error() != want {
		t.Fatalf("cross-origin wording = %q", err.Error())
	}
}

func TestFetchClassifiesCancellationAndTimeouts(t *testing.T) {
	// Our own timeout fires: WEB_FETCH_TIMEOUT.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte("too late"))
	}))
	defer slow.Close()
	limits := testLimits()
	limits.TimeoutMs = 100
	provider, base := newPinnedProvider(t, limits, slow.Listener.Addr().String())
	start := time.Now()
	_, err := provider.Fetch(context.Background(), web.WebFetchRequest{URL: base + "/"})
	if got := webErrorCode(t, err); got != "WEB_FETCH_TIMEOUT" {
		t.Fatalf("timeout code = %q", got)
	}
	if err.Error() != "web fetch timed out" {
		t.Fatalf("timeout wording = %q", err.Error())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %v", elapsed)
	}

	// An already cancelled context is refused at the door.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.Fetch(cancelled, web.WebFetchRequest{URL: base + "/"})
	if webErrorCode(t, err) != "WEB_ABORTED" || err.Error() != "web fetch aborted" {
		t.Fatalf("pre-cancelled = %q", err.Error())
	}

	// An upstream cancel mid-flight is WEB_ABORTED, not a timeout.
	mid, cancelMid := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelMid()
	}()
	_, err = provider.Fetch(mid, web.WebFetchRequest{URL: base + "/"})
	if got := webErrorCode(t, err); got != "WEB_ABORTED" {
		t.Fatalf("mid-cancel code = %q", got)
	}

	// A resolver fault with a live context is a transport failure.
	faulty := NewProviderWithResolver(testLimits(), func(ctx context.Context, hostname string) ([]PublicAddress, error) {
		return nil, errors.New("dns blew up")
	})
	_, err = faulty.Fetch(context.Background(), web.WebFetchRequest{URL: "http://" + testHostname + "/"})
	if got := webErrorCode(t, err); got != "WEB_PROVIDER_ERROR" {
		t.Fatalf("resolver fault code = %q", got)
	}
	if want := "web fetch failed: dns blew up"; err.Error() != want {
		t.Fatalf("resolver fault wording = %q", err.Error())
	}
}

func TestAsPluginRegistersIntoTheWebSeam(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	defer root.Dispose()
	runtime := web.NewRuntime(root, web.Config{})
	root.Provide("web", runtime)

	if err := AsPlugin(Config{}).Apply(root); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// The seam now resolves the fetch capability to our provider: a policy
	// rejection from the provider (not a seam selection error) proves it.
	_, err := runtime.Fetch(context.Background(), web.WebFetchRequest{URL: "ftp://nope"})
	if webErrorCode(t, err) != "WEB_INVALID_URL" {
		t.Fatalf("seam fetch = %v", err)
	}
	// Re-registration under the same id fails loud at the seam.
	if err := AsPlugin(Config{}).Apply(root); err == nil ||
		webErrorCode(t, err) != web.CodeDuplicateProvider {
		t.Fatalf("duplicate apply = %v", err)
	}
}

func TestAsPluginValidatesConfigLoudly(t *testing.T) {
	root := cordis.NewRoot(cordis.Discard{})
	defer root.Dispose()
	root.Provide("web", web.NewRuntime(root, web.Config{}))

	zero := 0
	if err := AsPlugin(Config{MaxResponseBytes: &zero}).Apply(root); err == nil ||
		err.Error() != "web-fetch-http: maxResponseBytes must be a positive finite number" {
		t.Fatalf("maxResponseBytes = %v", err)
	}
	negative := -1
	if err := AsPlugin(Config{MaxRedirects: &negative}).Apply(root); err == nil ||
		err.Error() != "web-fetch-http: maxRedirects must be a non-negative integer" {
		t.Fatalf("maxRedirects = %v", err)
	}
	huge := maxNodeTimerDelayMs + 1
	if err := AsPlugin(Config{TimeoutMs: &huge}).Apply(root); err == nil ||
		err.Error() != "web-fetch-http: timeoutMs must be no greater than 2147483647" {
		t.Fatalf("timeoutMs = %v", err)
	}
	// Without the web service the plugin fails loud instead of no-op.
	bare := cordis.NewRoot(cordis.Discard{})
	defer bare.Dispose()
	if err := AsPlugin(Config{}).Apply(bare); err == nil ||
		err.Error() != "web-fetch-http: the web service is not provided" {
		t.Fatalf("missing seam = %v", err)
	}
}
