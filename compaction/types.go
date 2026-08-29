// Package compaction ports packages/compaction/compaction: the compaction
// vocabulary, checkpoint provenance, and tool-pairing balance.
//
// The `compaction/*` events record the lock and summary inputs without
// entering the surface, so they are not surface events; a separate
// replacement user/message carries the summary. Backend packages own
// configuration and retention policy (the basic provider lands with its
// round).
package compaction

import (
	"dshgo/llm"
	"dshgo/session"
)

// CompactionID is the stable identity shared by one compaction's complete
// durable lifecycle.
type CompactionID = string

// CommandID is the human command identity from the commands capability; a
// pointer-free absent form is an empty string.
type CommandID = string

// The `compaction/*` durable event vocabulary. Log-only: no surface
// operations.
const (
	EventCompactionStart   = "compaction/start"
	EventCompactionSummary = "compaction/summary"
	EventCompactionEnd     = "compaction/end"
	EventCompactionPrune   = "compaction/prune"
)

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	session.EnsureEventTypes(
		EventCompactionStart, EventCompactionSummary, EventCompactionEnd, EventCompactionPrune,
	)
}

// SeqRange is one surface-position span: the seqs of the first (Start) and
// last (End) surface nodes of a replaced range. A surface-POSITION span, not
// a numeric seq interval — after a prior replace lands a fresh high-seq
// summary node at an older range's position, Start can be GREATER than End.
type SeqRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// StartPayload is the `compaction/start` payload: it marks the start of a
// compaction — log-only, holds the lock until compaction/end. A numbered
// owner is strictly enclosed by that open turn; Turn nil identifies a
// standalone manual transaction between turns.
type StartPayload struct {
	CompactionID    CompactionID `json:"compactionId"`
	SourceCommandID CommandID    `json:"sourceCommandId,omitempty"`
	// Turn is the owning turn number, or nil for a standalone manual
	// transaction between turns.
	Turn *int64 `json:"turn"`
}

// SummaryPayload is the completed summary, its inputs, and its model call
// facts — log-only, no surfaceOp. The summary content is in Summary; the
// actual surface replacement is performed by the immediately following
// user/message event that shadows the compacted range. That adjacency is
// contractual — the shadowed pricing fields are the replacement's shadow
// price, so a consumer may pair a replacement with the metering event
// directly before it.
type SummaryPayload struct {
	CompactionID       CompactionID       `json:"compactionId"`
	SourceCommandID    CommandID          `json:"sourceCommandId,omitempty"`
	Summary            []llm.ContentBlock `json:"summary"`
	ShadowedRange      SeqRange           `json:"shadowedRange"`
	ShadowedSeqs       []int64            `json:"shadowedSeqs"`
	ShadowedTokenCount int64              `json:"shadowedTokenCount"`
	// Provider is the provider route that wrote the summary.
	Provider string `json:"provider"`
	// Model is the model that wrote the summary — the summarize call's
	// envelope, reported by the backend that made the call, logged so the
	// one-shot request is reconstructable from log + code and "which model
	// wrote this summary" has a durable answer.
	Model string `json:"model"`
	// MaxTokens is the generation cap the summarize call sent, when one
	// applied.
	MaxTokens *int64 `json:"maxTokens,omitempty"`
	// Usage is the provider-reported token usage for the summarization
	// request, when emitted.
	Usage *llm.TokenUsage `json:"usage,omitempty"`
	// RawOutput is the complete provider output before the backend's safe
	// summary projection, present when the summarizer went through the LLM
	// stream seam.
	RawOutput []llm.ContentBlock `json:"rawOutput,omitempty"`
	// LLMStreamCall identifies exactly one call through this context's
	// llm.stream; an unmarked summary (remote, template) leaves it false and
	// does not identify a call.
	LLMStreamCall bool `json:"llmStreamCall,omitempty"`
}

// EndPayload marks the end of a compaction — log-only, releases the lock.
// Its owner matches compaction/start; Error records an unsuccessful attempt.
type EndPayload struct {
	CompactionID    CompactionID `json:"compactionId"`
	SourceCommandID CommandID    `json:"sourceCommandId,omitempty"`
	Turn            *int64       `json:"turn"`
	Error           string       `json:"error,omitempty"`
}

// PrunePayload is the shadow price of one model-free prune replacement —
// log-only, no surfaceOp. The shared shadow-price protocol: a surface
// replace event is priced by the metering event immediately before it
// (compaction/summary for a summarizing compaction, this event for a prune),
// which states the heuristic token price of the exact replaced range so a
// pure consumer can subtract it without retaining per-node prices. The
// replacement MUST be appended synchronously right after this event.
type PrunePayload struct {
	// ShadowedRange is the replaced range's first and last surface-node seqs.
	ShadowedRange SeqRange `json:"shadowedRange"`
	// ShadowedSeqs are the seqs of all shadowed surface nodes, in surface
	// order.
	ShadowedSeqs []int64 `json:"shadowedSeqs"`
	// ShadowedTokenCount is the heuristic price of the shadowed content
	// under the token-meter's fixed estimator.
	ShadowedTokenCount int64 `json:"shadowedTokenCount"`
}

// Result is the result of a successful compaction operation.
type Result struct {
	// CompactionID is the stable identity shared by this compaction's
	// complete durable lifecycle.
	CompactionID CompactionID `json:"compactionId"`
	// SourceCommandID is the human command that initiated this compaction,
	// when it was manual.
	SourceCommandID CommandID `json:"sourceCommandId,omitempty"`
	// StartSeq is the seq of the appended compaction/start event.
	StartSeq int64 `json:"startSeq"`
	// SummarySeq is the seq of the appended compaction/summary event.
	SummarySeq int64 `json:"summarySeq"`
	// EndSeq is the seq of the appended compaction/end event.
	EndSeq int64 `json:"endSeq"`
	// Summary is the summary content blocks produced by the backend.
	Summary []llm.ContentBlock `json:"summary"`
	// ShadowedRange is the surface-boundary pair that was shadowed: the seqs
	// of the first and last surface nodes of the replaced range. See
	// SeqRange for the position-span caveat.
	ShadowedRange SeqRange `json:"shadowedRange"`
	// ShadowedSeqs are the seqs of all shadowed surface nodes, in surface
	// order — the authoritative set of shadowed nodes.
	ShadowedSeqs []int64 `json:"shadowedSeqs"`
	// ShadowedTokenCount is the estimated token count of the shadowed
	// content.
	ShadowedTokenCount int64 `json:"shadowedTokenCount"`
}
