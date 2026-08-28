// Provider-owned request-retry policy configuration and resolution. Port of
// retry-policy.ts.
package llm

import (
	"fmt"
)

// Retry policy defaults. The bounds are format constants, not tunables.
const (
	DefaultMaxRetries   = 5
	DefaultInitialDelay = 500
	DefaultMaxDelay     = 10_000
	DefaultJitterRatio  = 0.1
	MaxTimerDelay       = 2_147_483_647
	RetryModeNormal     = "normal"
	RetryModeAlways     = "always"
)

// DefaultRetryableCodes are the stable failure codes eligible for the
// default policy.
var DefaultRetryableCodes = []string{
	EmptyResponseCode,
	"RATE_LIMIT",
	"SERVER",
	"TIMEOUT",
	"TRANSPORT",
}

// RetryBackoffConfig is bounded exponential backoff with symmetric jitter
// around each local delay. Nil fields select defaults.
type RetryBackoffConfig struct {
	InitialDelayMs *int64   `json:"initialDelayMs,omitempty"`
	MaxDelayMs     *int64   `json:"maxDelayMs,omitempty"`
	JitterRatio    *float64 `json:"jitterRatio,omitempty"`
}

// RetryPolicyConfig is the provider-owned policy configuration; Mode
// selects normal or always. An omitted config (nil pointer at the call
// site) selects normal defaults.
type RetryPolicyConfig struct {
	Mode           string              `json:"mode"`
	MaxRetries     *int64              `json:"maxRetries,omitempty"`
	RetryableCodes []string            `json:"retryableCodes,omitempty"`
	Backoff        *RetryBackoffConfig `json:"backoff,omitempty"`
}

// ResolvedRetryPolicy is the immutable policy captured when a provider
// route is registered.
type ResolvedRetryPolicy struct {
	Mode           string
	MaxRetries     int64    // normal mode only
	RetryableCodes []string // normal mode only
	InitialDelayMs int64
	MaxDelayMs     int64
	JitterRatio    float64
}

func resolveBackoff(config *RetryBackoffConfig, path string) (initial, max int64, jitter float64, err error) {
	initial, max, jitter = DefaultInitialDelay, DefaultMaxDelay, DefaultJitterRatio
	if config != nil {
		if config.InitialDelayMs != nil {
			initial = *config.InitialDelayMs
		}
		if config.MaxDelayMs != nil {
			max = *config.MaxDelayMs
		}
		if config.JitterRatio != nil {
			jitter = *config.JitterRatio
		}
	}
	if initial <= 0 || initial > MaxTimerDelay {
		return 0, 0, 0, fmt.Errorf("%s.initialDelayMs must be a positive finite number no greater than %d", path, MaxTimerDelay)
	}
	if max <= 0 || max > MaxTimerDelay {
		return 0, 0, 0, fmt.Errorf("%s.maxDelayMs must be a positive finite number no greater than %d", path, MaxTimerDelay)
	}
	if initial > max {
		return 0, 0, 0, fmt.Errorf("%s.initialDelayMs must be less than or equal to maxDelayMs", path)
	}
	if jitter < 0 || jitter > 1 {
		return 0, 0, 0, fmt.Errorf("%s.jitterRatio must be between 0 and 1", path)
	}
	return initial, max, jitter, nil
}

// ResolveRetryPolicy validates, defaults, and detaches one provider-owned
// retry policy. Unknown configuration keys are rejected at JSON decode time
// (DisallowUnknownFields at the schema boundary), matching the official
// schema's unknown-key refusal.
func ResolveRetryPolicy(config *RetryPolicyConfig, path string) (*ResolvedRetryPolicy, error) {
	if config == nil {
		initial, max, jitter, err := resolveBackoff(nil, path+".backoff")
		if err != nil {
			return nil, err
		}
		return &ResolvedRetryPolicy{
			Mode: RetryModeNormal, MaxRetries: DefaultMaxRetries,
			RetryableCodes: append([]string(nil), DefaultRetryableCodes...),
			InitialDelayMs: initial, MaxDelayMs: max, JitterRatio: jitter,
		}, nil
	}
	switch config.Mode {
	case RetryModeNormal:
		maxRetries := int64(DefaultMaxRetries)
		if config.MaxRetries != nil {
			maxRetries = *config.MaxRetries
		}
		if maxRetries < 0 {
			return nil, fmt.Errorf("%s.maxRetries must be a non-negative safe integer", path)
		}
		codes := config.RetryableCodes
		if codes == nil {
			codes = DefaultRetryableCodes
		}
		if len(codes) == 0 {
			return nil, fmt.Errorf("%s.retryableCodes must not be empty", path)
		}
		seen := map[string]bool{}
		for _, code := range codes {
			if code == "" {
				return nil, fmt.Errorf("%s.retryableCodes must contain only non-empty strings", path)
			}
			if seen[code] {
				return nil, fmt.Errorf("%s.retryableCodes must not contain duplicates", path)
			}
			seen[code] = true
		}
		initial, max, jitter, err := resolveBackoff(config.Backoff, path+".backoff")
		if err != nil {
			return nil, err
		}
		return &ResolvedRetryPolicy{
			Mode: RetryModeNormal, MaxRetries: maxRetries,
			RetryableCodes: append([]string(nil), codes...),
			InitialDelayMs: initial, MaxDelayMs: max, JitterRatio: jitter,
		}, nil
	case RetryModeAlways:
		initial, max, jitter, err := resolveBackoff(config.Backoff, path+".backoff")
		if err != nil {
			return nil, err
		}
		return &ResolvedRetryPolicy{
			Mode: RetryModeAlways, InitialDelayMs: initial, MaxDelayMs: max, JitterRatio: jitter,
		}, nil
	default:
		return nil, fmt.Errorf("%s.mode must be %q or %q", path, RetryModeNormal, RetryModeAlways)
	}
}
