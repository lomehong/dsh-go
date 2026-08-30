package webfetchhttp

import (
	"fmt"
	"strconv"

	"dshgo/cordis"
	"dshgo/web"
)

// DefaultUserAgent is the explicit product agent sent on every request —
// never a browser disguise.
const DefaultUserAgent = "deepseek-harness/0.0.1 (+https://github.com/deepseek-ai)"

// PluginName is the cordis plugin name used by loader diagnostics.
const PluginName = "web-fetch-http"

// maxNodeTimerDelayMs mirrors the official configuration-time rejection:
// Node coerces larger timer delays to 1 ms, so the official schema rejects
// them at configuration time. The bound is kept verbatim so configs remain
// interchangeable across implementations.
const maxNodeTimerDelayMs = 2_147_483_647

// Config is the plugin configuration: the provider's transport and size
// limits plus its User-Agent. Every field is optional and defaults to the
// official schemastery defaults.
type Config struct {
	// Maximum response body size in bytes.
	MaxResponseBytes *int
	// Maximum decoded body length in characters.
	MaxBodyChars *int
	// Default fetch timeout in milliseconds.
	TimeoutMs *int
	// Maximum number of same-origin redirect hops to follow.
	MaxRedirects *int
	// User-Agent header sent on every request.
	UserAgent string
}

// The official schemastery field defaults.
const (
	defaultMaxResponseBytes = 5_000_000
	defaultMaxBodyChars     = 100_000
	defaultTimeoutMs        = 30_000
	defaultMaxRedirects     = 5
)

func defaultedInt(overridden *int, fallback int) int {
	if overridden != nil {
		return *overridden
	}
	return fallback
}

// assertPositiveFinite: a resource limit (byte/char/timeout cap) must be a
// positive finite number.
func assertPositiveFinite(name string, value int) error {
	if value <= 0 {
		return fmt.Errorf("web-fetch-http: %s must be a positive finite number", name)
	}
	return nil
}

// assertTimeoutMs enforces the official timer-range ceiling in addition to
// the positive-finite check.
func assertTimeoutMs(value int) error {
	if err := assertPositiveFinite("timeoutMs", value); err != nil {
		return err
	}
	if value > maxNodeTimerDelayMs {
		return fmt.Errorf("web-fetch-http: timeoutMs must be no greater than %s",
			strconv.Itoa(maxNodeTimerDelayMs))
	}
	return nil
}

// assertNonNegativeInteger: the redirect hop cap must be a non-negative
// integer (0 follows no redirects).
func assertNonNegativeInteger(name string, value int) error {
	if value < 0 {
		return fmt.Errorf("web-fetch-http: %s must be a non-negative integer", name)
	}
	return nil
}

// AsPlugin builds the plugin: register the local HTTP(S) fetch provider
// with ctx.web (official inject ["web"]). The provider contributes to the
// registry without owning the web service.
func AsPlugin(config Config) *cordis.Plugin {
	return &cordis.Plugin{
		Name:   PluginName,
		Inject: []string{"web"},
		Apply: func(ctx *cordis.Context) error {
			limits := HttpFetchLimits{
				MaxResponseBytes: defaultedInt(config.MaxResponseBytes, defaultMaxResponseBytes),
				MaxBodyChars:     defaultedInt(config.MaxBodyChars, defaultMaxBodyChars),
				TimeoutMs:        defaultedInt(config.TimeoutMs, defaultTimeoutMs),
				MaxRedirects:     defaultedInt(config.MaxRedirects, defaultMaxRedirects),
				UserAgent:        config.UserAgent,
			}
			if limits.UserAgent == "" {
				limits.UserAgent = DefaultUserAgent
			}
			if err := assertPositiveFinite("maxResponseBytes", limits.MaxResponseBytes); err != nil {
				return err
			}
			if err := assertPositiveFinite("maxBodyChars", limits.MaxBodyChars); err != nil {
				return err
			}
			if err := assertTimeoutMs(limits.TimeoutMs); err != nil {
				return err
			}
			if err := assertNonNegativeInteger("maxRedirects", limits.MaxRedirects); err != nil {
				return err
			}
			runtime, _ := ctx.Get("web").(*web.Runtime)
			if runtime == nil {
				return fmt.Errorf("web-fetch-http: the web service is not provided")
			}
			_, err := runtime.RegisterFetchProvider(NewProvider(limits))
			return err
		},
	}
}
