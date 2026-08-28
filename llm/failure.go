// Normalization for values thrown by a final LLM adapter boundary, and the
// typed LLM error carrying serializable failure facts. Port of
// adapter-failure.ts plus the LlmError class.
package llm

import (
	"context"
	"errors"
)

// LlmError is the typed LLM failure: the shared HarnessError code taxonomy
// plus validated serializable provider facts (status, retry-after, request
// id). Carried as a distinct type so NormalizeLlmFailure can trust its own
// facts.
type LlmError struct {
	// Harness carries the shared code/message/cause triple.
	Harness *Error
	// Failure is the frozen serializable fact set.
	Failure LlmFailure
}

// Error renders the human-readable summary.
func (e *LlmError) Error() string { return e.Harness.Error() }

// Code is the shared machine taxonomy code.
func (e *LlmError) Code() string { return e.Harness.Code() }

// Unwrap exposes the cause chain.
func (e *LlmError) Unwrap() error { return e.Harness.Unwrap() }

// NewLlmError validates the fact bounds the official constructor enforces.
func NewLlmError(message, code string, failure LlmFailure) *LlmError {
	if message == "" {
		panic("LlmError message must be a non-empty string")
	}
	if code == "" {
		panic("LlmError code must be a non-empty string")
	}
	if failure.Status != 0 && (failure.Status < 100 || failure.Status > 599) {
		panic("LlmError status must be an integer from 100 through 599")
	}
	if failure.ProviderRetryAfterMs != 0 && failure.ProviderRetryAfterMs <= 0 {
		panic("LlmError providerRetryAfterMs must be a positive finite number")
	}
	failure.Message = message
	failure.Code = code
	return &LlmError{Harness: NewError(code, message, nil), Failure: failure}
}

// NormalizeLlmFailure detaches serializable provider facts from a value
// thrown by an adapter: an *LlmError's own facts are trusted when they
// agree with its code; any other error renders as message plus its
// HarnessError code, or UNKNOWN.
func NormalizeLlmFailure(value error) LlmFailure {
	if value == nil {
		return LlmFailure{Message: "LLM adapter failed", Code: "UNKNOWN"}
	}
	var llmErr *LlmError
	if errors.As(value, &llmErr) && llmErr.Failure.Code == llmErr.Code() {
		return llmErr.Failure
	}
	var harness *Error
	if errors.As(value, &harness) {
		return LlmFailure{Message: value.Error(), Code: harness.Code()}
	}
	return LlmFailure{Message: value.Error(), Code: "UNKNOWN"}
}

// TerminalFailureChunk is the exported adapter boundary: adapters convert
// their own request failures into exactly one terminal finish chunk through
// this helper (the official adapterStream try/catch surface).
func TerminalFailureChunk(value error, aborted bool) StreamChunk {
	return adapterFailureChunk(value, aborted)
}

// adapterFailureChunk converts one adapter throw into the stream protocol's
// terminal outcome; a cancelled call reports aborted instead of error.
func adapterFailureChunk(value error, aborted bool) StreamChunk {
	failure := NormalizeLlmFailure(value)
	reason := &FinishReason{Kind: FinishError, Failure: &failure}
	if aborted || failure.Code == "ABORTED" ||
		errors.Is(value, context.Canceled) || errors.Is(value, context.DeadlineExceeded) {
		reason.Kind = FinishAborted
	}
	return StreamChunk{Type: ChunkFinish, Reason: reason}
}
