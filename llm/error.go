// Harness error base with a stable machine-routable code and chained cause,
// mirroring @deepseek-ai/dsh-llm/error. Route on Code, never by parsing the
// message.
package llm

import (
	"errors"
	"regexp"
	"strings"
)

// Error is the harness error: a stable machine-routable Code (e.g.
// NO_ADAPTER, INVALID_ARGS, INVARIANT) distinct from the human-readable
// message, with cause chaining.
type Error struct {
	code  string
	msg   string
	cause error
}

// NewError builds a harness error.
func NewError(code, message string, cause error) *Error {
	return &Error{code: code, msg: message, cause: cause}
}

// Error renders the message alone; the cause chain renders through
// ErrorChain.
func (e *Error) Error() string { return e.msg }

// Code returns the stable machine-routable failure class.
func (e *Error) Code() string { return e.code }

// Unwrap exposes the cause.
func (e *Error) Unwrap() error { return e.cause }

// Canonical provider-neutral failure codes.
const (
	// ContextWindowExceededCode: a model request rejected because its
	// context window was exceeded.
	ContextWindowExceededCode = "CONTEXT_WINDOW_EXCEEDED"
	// QuotaExceededCode: an exhausted account quota or balance.
	QuotaExceededCode = "QUOTA"
	// EmptyResponseCode: a response that completed normally but carried no
	// content blocks at all. The attempt produced nothing durable, so retry
	// policy treats it as safe to repeat.
	EmptyResponseCode = "EMPTY_RESPONSE"
	// InvalidCredentialCode: a credential that was supplied but cannot be
	// used — malformed rather than absent. Deliberately outside the
	// default retryable set: a malformed credential fails identically on
	// every attempt.
	InvalidCredentialCode = "INVALID_CREDENTIAL"
)

// Registry failure codes of the LLM service definition.
const (
	// CodeNoAdapter: no adapter registered for the requested provider route.
	CodeNoAdapter = "NO_ADAPTER"
	// CodeInvalidCatalog: an adapter's model catalog violated its schema.
	CodeInvalidCatalog = "INVALID_CATALOG"
	// CodeInvalidAdapter: an adapter failed registration validation.
	CodeInvalidAdapter = "INVALID_ADAPTER"
	// CodeDuplicateAdapter: two adapters claimed one provider route.
	CodeDuplicateAdapter = "DUPLICATE_ADAPTER"
)

var (
	structuredContextOverflow = regexp.MustCompile(
		`(?i)(?:^|[^a-z0-9])context[\s_-]*(?:length|window)[\s_-]*(?:exceed(?:ed|s)?|overflow(?:ed)?|limit[\s_-]*exceeded)(?:$|[^a-z0-9])`)
	tooLargeForContext = regexp.MustCompile(
		`(?i)\b(?:request|prompt|input|messages?)\s+(?:is\s+|are\s+)?too\s+(?:large|long)\s+for\s+(?:(?:this|the)\s+)?(?:model(?:'s)?\s+)?context(?:\s+window)?\b`)
	exceedsModelContext = regexp.MustCompile(
		`(?i)\b(?:input|prompt|request|messages?)\b.{0,40}\b(?:exceed(?:s|ed)?|overflows?|is\s+larger\s+than)\b.{0,40}\b(?:the\s+)?(?:model(?:'s)?\s+)?context(?:\s+(?:length|window))?\b`)
	maximumContext = regexp.MustCompile(
		`(?i)\b(?:maximum|max)(?:\s+(?:allowed|supported))?\s+context\s+(?:length|window)\b`)
	tooLongForModel = regexp.MustCompile(
		`(?i)\b(?:input|prompt|request)\s+(?:is\s+)?too\s+(?:long|large)\s+for\s+(?:this|the)\s+model\b`)
)

// IsContextWindowExceededError recognizes the context-overflow wording used
// by OpenAI-compatible providers and library adapters. Adapters pass all
// available provider code, type, and message text so both thrown and
// in-band delivery styles share one classifier.
func IsContextWindowExceededError(detail string) bool {
	return structuredContextOverflow.MatchString(detail) ||
		maximumContext.MatchString(detail) ||
		tooLargeForContext.MatchString(detail) ||
		tooLongForModel.MatchString(detail) ||
		exceedsModelContext.MatchString(detail)
}

var (
	quotaInsufficient = regexp.MustCompile(`(?i)\binsufficient[\s_-]+(?:quota|balance|credits?)\b`)
	quotaLimitReached = regexp.MustCompile(`(?i)\b(?:quota|usage[\s_-]+limit)[\s_-]+(?:exceeded|exhausted|reached)\b`)
	exceedsQuota      = regexp.MustCompile(`(?i)\bexceed(?:ed|s)?[\s_-]+(?:(?:your|the)[\s_-]+)?(?:current[\s_-]+)?quota\b`)
	balanceExhausted  = regexp.MustCompile(`(?i)\b(?:balance|credits?)[\s_-]+(?:exhausted|depleted)\b`)
	outOfBudget       = regexp.MustCompile(`(?i)\bout[\s_-]+of[\s_-]+(?:credits?|budget)\b`)
)

// IsQuotaExceededError recognizes provider wording that identifies an
// exhausted account quota rather than a transient request-rate limit.
func IsQuotaExceededError(detail string) bool {
	return quotaInsufficient.MatchString(detail) ||
		quotaLimitReached.MatchString(detail) ||
		exceedsQuota.MatchString(detail) ||
		balanceExhausted.MatchString(detail) ||
		outOfBudget.MatchString(detail)
}

// ErrorChain renders an error with its full Unwrap chain, so transport
// wrappers surface the underlying failure instead of masking it.
// Diagnostic-surface rendering only — never parse the result; route on
// Error.Code. Aggregate members (errors.Join) render bracketed and `; `
// -joined; a cause the wrapper already embedded verbatim renders once.
func ErrorChain(value error) string {
	if value == nil {
		return "<nil>"
	}
	var b strings.Builder
	renderError(&b, value, &seenSet{entries: map[error]bool{}})
	return b.String()
}

type seenSet struct {
	entries map[error]bool
}

func renderError(b *strings.Builder, current error, seen *seenSet) {
	if current == nil {
		return
	}
	if seen.entries[current] {
		b.WriteString("<circular cause>")
		return
	}
	seen.entries[current] = true
	defer delete(seen.entries, current)

	// errors.Join-style aggregate members render in brackets.
	var joiner interface{ Unwrap() []error }
	if errors.As(current, &joiner) {
		if members := joiner.Unwrap(); len(members) > 0 {
			b.WriteString(" [")
			for i, member := range members {
				if i > 0 {
					b.WriteString("; ")
				}
				renderError(b, member, seen)
			}
			b.WriteString("]")
			return
		}
	}

	text := current.Error()
	cause := errors.Unwrap(current)
	var causeText string
	if cause != nil {
		var cb strings.Builder
		renderError(&cb, cause, seen)
		causeText = cb.String()
	}
	// A wrapper whose own text already embeds its cause ("wrap: cause", the
	// fmt.Errorf "%w" shape) renders the prefix only — repeating the cause
	// would only add noise.
	if causeText != "" && strings.HasSuffix(text, causeText) {
		text = strings.TrimSuffix(text, causeText)
		text = strings.TrimSuffix(text, ": ")
		if text == "" {
			text = "(error)"
		}
	}
	b.WriteString(text)
	if causeText != "" && causeText != text {
		b.WriteString(": ")
		b.WriteString(causeText)
	}
}
