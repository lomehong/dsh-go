// Package llm re-implements the provider-neutral message and streaming
// vocabulary of @deepseek-ai/dsh-llm (official tag dsh-v0.1.2-alpha.1): the
// canonical message/content/stream types shared by the agent loop, the
// session log, and plugins. Adapters alone translate provider wire messages;
// the unions are merge-extensible and switch on their discriminant.
//
// JSON field names are the locked wire contract and mirror the official
// camelCase exactly — these values ride the session log and the SDK seam
// byte-compatibly.
package llm

import (
	"context"
	"crypto/rand"
	"encoding/json"
)

// Opaque cross-boundary identities. Go renders the official branded strings
// as their own types; no validation is performed, exactly like the official
// brand constructors.
type (
	// MessageID is the stable identity carried by one message across inbox,
	// log, and model-request boundaries.
	MessageID = string
	// ToolCallID correlates a model-issued tool call with its result.
	ToolCallID = string
	// ProviderRequestID is a provider-issued request identifier retained for
	// diagnostics across package boundaries.
	ProviderRequestID = string
	// ReasoningEffortID is an adapter-owned identifier for one model's
	// selectable reasoning effort.
	ReasoningEffortID = string
)

// ContentBlockType tags the merge-extensible content block vocabulary.
const (
	BlockText       = "text"
	BlockReasoning  = "reasoning"
	BlockImage      = "image"
	BlockToolCall   = "tool-call"
	BlockToolResult = "tool-result"
)

// ContentBlock is one model-facing content block. The Type discriminant
// selects the fields in play; unknown types fall through (merge-extensible).
type ContentBlock struct {
	Type string `json:"type"`
	// TextBlock and ReasoningBlock: visible text / reasoning content.
	Text string `json:"text,omitempty"`
	// ImageBlock: immutable bytes and display metadata owned by the
	// attachment service.
	Attachment any `json:"attachment,omitempty"`
	// ToolCallBlock: provider-issued call id, tool name, raw JSON arguments.
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// ToolResultBlock: call correlation, nested content, outcome.
	ToolCallID string         `json:"toolCallId,omitempty"`
	Content    []ContentBlock `json:"content,omitempty"`
	IsError    bool           `json:"isError,omitempty"`
}

// MessageSource kinds (merge-extensible).
const (
	SourceUser    = "user"
	SourcePlugin  = "plugin"
	SourceModel   = "model"
	SourceTool    = "tool"
	SourceWebhook = "webhook"
	// SourceGoal attributes an admitted goal continuation round (dsh-goal
	// domain: the official MessageSourceMap goal entry).
	SourceGoal = "goal"
)

// ContextForm values: the SEMANTIC kind of producer-supplied context —
// never visual vocabulary.
const (
	FormInstructions = "instructions"
	FormCatalog      = "catalog"
	FormSnapshot     = "snapshot"
	FormNotice       = "notice"
	FormRelay        = "relay"
	FormRecall       = "recall"
)

// ContextSnapshotSection is one named contribution to a snapshot-form
// context, in assembly order.
type ContextSnapshotSection struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

// MessageSource is where a message came from. Kind answers who produced
// this; Form answers what kind of thing it is; the two axes are independent.
type MessageSource struct {
	Kind string `json:"kind"`
	// Plugin source: the contributing plugin's registered name.
	Plugin string `json:"plugin,omitempty"`
	// Model source: provider route, model id, and adapter-private replay
	// state (lossless JSON).
	Provider    string          `json:"provider,omitempty"`
	Model       string          `json:"model,omitempty"`
	ReplayState json.RawMessage `json:"replayState,omitempty"`
	// Tool source: the call this result answers.
	CallID ToolCallID `json:"callId,omitempty"`
	// User-rpc source: the request that delivered the message and the
	// Host-canonicalized browser zone reported by that request.
	RPCID          string `json:"rpcId,omitempty"`
	ClientTimeZone string `json:"clientTimeZone,omitempty"`
	// Webhook source: the delivery provenance the acting rule recorded —
	// the adapter instance, the provider delivery id, and the rule that
	// acted (createWebhookSession).
	WebhookSource string `json:"source,omitempty"`
	DeliveryID    string `json:"deliveryId,omitempty"`
	RuleID        string `json:"ruleId,omitempty"`
	// Plugin context form and the fields that form requires.
	Form     string                   `json:"form,omitempty"`
	Sections []ContextSnapshotSection `json:"sections,omitempty"`
	Summary  string                   `json:"summary,omitempty"`
	// Relay sources: the session id of the agent whose tool call produced the
	// message (coordinator relay, subagent report, subagent settled notice).
	SenderSessionID string `json:"senderSessionId,omitempty"`
	// Session-reference source: the durable provenance of one aggregated
	// cross-session snapshot (dsh-session-reference types.ts). ReferenceVersion
	// is always 1.
	ReferenceVersion int                     `json:"version,omitempty"`
	References       []SessionReferenceEntry `json:"references,omitempty"`
	// Skill-catalog source: whether this publication replaces every
	// earlier catalog and exactly the entries it published, in catalog
	// order. Consumers presenting the list must read these entries, not
	// re-parse the model-facing prose frame.
	CatalogUpdate  bool           `json:"catalogUpdate,omitempty"`
	CatalogEntries []CatalogEntry `json:"catalogEntries,omitempty"`
	// Goal source: the admitted continuation round attribution (dsh-goal
	// GoalMessageSource — goal identity, durable revision, positive round).
	GoalID       string `json:"goalId,omitempty"`
	GoalRevision int64  `json:"revision,omitempty"`
	GoalRound    int64  `json:"round,omitempty"`
	// Agent-instructions source: whether this message is the complete
	// startup/resume baseline, the discovery/precedence/budget identity
	// that validates a resumed baseline, and the instruction transitions
	// it renders.
	Baseline         bool                `json:"baseline,omitempty"`
	BaselineIdentity string              `json:"baselineIdentity,omitempty"`
	Changes          []InstructionChange `json:"changes,omitempty"`
}

// InstructionChange is one workspace-instruction state transition rendered
// by an agent-instructions context.
type InstructionChange struct {
	Action string `json:"action"` // "set" | "replace" | "remove"
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	Digest string `json:"digest,omitempty"`
}

// CatalogEntry is one skill-catalog publication entry.
type CatalogEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SessionReferenceEntry is one source session cited by a session-reference
// snapshot, with its retention facts.
type SessionReferenceEntry struct {
	SessionID          string `json:"sessionId"`
	Label              string `json:"label"`
	CapturedThroughSeq *int64 `json:"capturedThroughSeq"`
	Compacted          bool   `json:"compacted"`
	OriginalMessages   int    `json:"originalMessages"`
	RetainedMessages   int    `json:"retainedMessages"`
	OmittedMessages    int    `json:"omittedMessages"`
	OmittedBytes       int    `json:"omittedBytes"`
	Truncated          bool   `json:"truncated"`
	InputIndex         int    `json:"inputIndex"`
}

// Message is the one immutable message representation shared by delivery,
// durable history, and model requests.
type Message struct {
	ID      MessageID      `json:"id"`
	Role    string         `json:"role"` // "system" | "user" | "assistant"
	Content []ContentBlock `json:"content"`
	Source  MessageSource  `json:"source"`
}

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// CONTEXT_SUMMARY_MAX_CHARS bounds a notice summary: the account rides a
// collapsed transcript row and is committed to the durable log, while its
// inputs are caller text with no length of their own.
const CONTEXT_SUMMARY_MAX_CHARS = 120

// BoundContextSummary ellipsizes a producer's one-line account to the seam
// bound. The bound counts runes — the Go rendering of the official
// character bound.
func BoundContextSummary(summary string) string {
	runes := []rune(summary)
	if len(runes) <= CONTEXT_SUMMARY_MAX_CHARS {
		return summary
	}
	return string(runes[:CONTEXT_SUMMARY_MAX_CHARS-1]) + "…"
}

// LlmFailure holds serializable provider or transport failure facts; policy
// decides whether they are retryable.
type LlmFailure struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	// Status is the HTTP status returned by the provider, when available.
	Status int `json:"status,omitempty"`
	// ProviderRetryAfterMs is a provider-requested delay in milliseconds,
	// when valid and available.
	ProviderRetryAfterMs int64 `json:"providerRetryAfterMs,omitempty"`
	// RequestID is an opaque provider-issued request identifier.
	RequestID ProviderRequestID `json:"requestId,omitempty"`
}

// FinishReason kinds (merge-extensible so adapters can surface
// provider-specific reasons).
const (
	FinishStop      = "stop"
	FinishToolCalls = "tool-calls"
	FinishMaxTokens = "max-tokens"
	FinishAborted   = "aborted"
	FinishError     = "error"
)

// FinishReason is why a model response stopped.
type FinishReason struct {
	Kind    string      `json:"kind"`
	Failure *LlmFailure `json:"failure,omitempty"`
}

// TokenUsage is the token accounting for one model call. Counts are
// DISJOINT: InputTokens is uncached input only; cached input is reported
// separately (billed input = sum of the three).
type TokenUsage struct {
	InputTokens      int64  `json:"inputTokens"`
	OutputTokens     int64  `json:"outputTokens"`
	TotalTokens      *int64 `json:"totalTokens,omitempty"`
	CacheReadTokens  *int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens  *int64 `json:"reasoningTokens,omitempty"`
}

// ReplayEnvelope carries adapter-private lossless-JSON state for replaying a
// successful response. Both halves stay opaque to the harness; when assembly
// drops a block it drops the entry at the same position, and a length
// mismatch discards the whole envelope.
type ReplayEnvelope struct {
	Response json.RawMessage `json:"response"`
	// Blocks holds one entry per emitted block in first-seen stream order.
	Blocks []json.RawMessage `json:"blocks,omitempty"`
}

// StreamChunk types — the raw streaming protocol adapters emit.
const (
	ChunkBlockStart     = "block-start"
	ChunkTextDelta      = "text-delta"
	ChunkReasoningDelta = "reasoning-delta"
	ChunkToolCallDelta  = "tool-call-delta"
	ChunkBlockEnd       = "block-end"
	ChunkUsage          = "usage"
	ChunkFinish         = "finish"
)

// StreamChunk is one raw streaming protocol event. Block indexes correlate
// interleaved deltas; block-end carries the assembled block; adapters emit
// usage before the terminal finish and nothing afterward; tool arguments
// stay raw JSON strings.
type StreamChunk struct {
	Type string `json:"type"`
	// Index correlates interleaved deltas for block-start/delta/block-end.
	Index int `json:"index,omitempty"`
	// BlockStart: the block type that opened.
	BlockType string `json:"blockType,omitempty"`
	// Text and reasoning deltas.
	Text string `json:"text,omitempty"`
	// Tool-call deltas: call identity, optional name, raw arguments fragment.
	ID             ToolCallID `json:"id,omitempty"`
	Name           string     `json:"name,omitempty"`
	ArgumentsDelta string     `json:"argumentsDelta,omitempty"`
	// BlockEnd: the assembled block.
	Block *ContentBlock `json:"block,omitempty"`
	// Usage: token accounting for the call.
	Usage *TokenUsage `json:"usage,omitempty"`
	// Finish: why the response stopped, plus replay metadata when successful.
	Reason      *FinishReason   `json:"reason,omitempty"`
	ReplayState *ReplayEnvelope `json:"replayState,omitempty"`
}

// ToolSchema is the JSON-schema description of a tool, as sent to the model.
type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// GenerateOptions is a single model request, fully assembled.
type GenerateOptions struct {
	// Provider is the registered provider route selecting the adapter
	// instance.
	Provider string `json:"provider"`
	// Model is the provider-owned model id.
	Model string `json:"model"`
	// ReasoningEffort is the adapter-owned effort selected for this model.
	ReasoningEffort ReasoningEffortID `json:"reasoningEffort,omitempty"`
	// Messages is the ordered conversation, exactly as the provider sees it
	// (after the system slot).
	Messages []Message `json:"messages"`
	// System is the system prompt text.
	System string `json:"system,omitempty"`
	// Tools holds the tool schemas.
	Tools []ToolSchema `json:"tools,omitempty"`
	// Temperature, MaxTokens, Stop map 1:1 onto provider sampling fields.
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int64   `json:"maxTokens,omitempty"`
	// Stop sequences halt generation as soon as the model produces any one
	// of them; the stop string itself is not included in the output.
	Stop []string `json:"stop,omitempty"`
	// SessionID is stamped by the loop for request routing and replay
	// cursor separation.
	SessionID string `json:"sessionId,omitempty"`
	// Purpose classifies an auxiliary model call; ordinary conversation
	// requests leave it unset.
	Purpose string `json:"purpose,omitempty"`
	// Context carries process-local cancellation; it is never serialized.
	Context context.Context `json:"-"`
}

// Auxiliary call purposes.
const (
	PurposeCompaction   = "compaction"
	PurposeSessionTitle = "session-title"
)

// LlmCallConfig is the provider, model, reasoning-effort, and sampling
// scalars of one conversation's requests. Every field maps 1:1 onto the
// same-named GenerateOptions field; the loop builds requests from the logged
// header rather than accepting these per call.
type LlmCallConfig struct {
	Provider        string            `json:"provider"`
	Model           string            `json:"model"`
	ReasoningEffort ReasoningEffortID `json:"reasoningEffort,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       *int64            `json:"maxTokens,omitempty"`
	Stop            []string          `json:"stop,omitempty"`
}

// LlmCallConfigAdapterDefaults marks effective config fields supplied by
// exact-model adapter resolution rather than by a caller's proposal.
type LlmCallConfigAdapterDefaults struct {
	ReasoningEffort bool `json:"reasoningEffort,omitempty"`
	MaxTokens       bool `json:"maxTokens,omitempty"`
}

// CallConfigEquals is field-wise equality — the comparison a caller runs to
// decide whether a proposed configuration is a real change (worth a logged
// header snapshot) or the held one restated.
func CallConfigEquals(a, b LlmCallConfig) bool {
	if a.Provider != b.Provider || a.Model != b.Model ||
		a.ReasoningEffort != b.ReasoningEffort ||
		!floatPtrEqual(a.Temperature, b.Temperature) ||
		!intPtrEqual(a.MaxTokens, b.MaxTokens) {
		return false
	}
	if (a.Stop == nil) != (b.Stop == nil) {
		return false
	}
	for i := range a.Stop {
		if a.Stop[i] != b.Stop[i] {
			return false
		}
	}
	return true
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func intPtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// LlmProviderInfo is display metadata for one registered provider route.
type LlmProviderInfo struct {
	// ID is the provider route key used by GenerateOptions.Provider.
	ID string `json:"id"`
	// Name is the human-readable provider name.
	Name string `json:"name"`
}

// Model modality vocabulary.
const (
	ModalityText  = "text"
	ModalityImage = "image"
)

// LlmModelInfo is one adapter-discovered model; catalog membership is
// advisory, not request validation.
type LlmModelInfo struct {
	// Provider is the route that owns this model entry.
	Provider string `json:"provider"`
	// ID is the model id passed to GenerateOptions.Model.
	ID string `json:"id"`
	// Name is the human-readable model name for selectors.
	Name string `json:"name"`
	// Description distinguishes otherwise similar models.
	Description string `json:"description,omitempty"`
	// InputModalities are accepted request modalities; nil means unknown,
	// while an explicit empty list is negative capability.
	InputModalities []string `json:"inputModalities,omitempty"`
}

// LlmModelContext is provider-owned context capacity for one exact route.
type LlmModelContext struct {
	ContextWindow int64 `json:"contextWindow"`
}

// LlmReasoningEffortInfo is display metadata for one adapter-owned effort.
type LlmReasoningEffortInfo struct {
	ID          ReasoningEffortID `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
}

// LlmModelReasoningInfo is the selectable reasoning efforts for one exact
// provider/model route.
type LlmModelReasoningInfo struct {
	// Efforts are supported efforts in adapter-preferred display order.
	Efforts []LlmReasoningEffortInfo `json:"efforts"`
	// DefaultEffort is materialized into requests when callers omit one;
	// absence preserves the provider's own default.
	DefaultEffort ReasoningEffortID `json:"defaultEffort,omitempty"`
}

// LlmResolvedModelInfo is exact-route model metadata resolved by its owning
// adapter.
type LlmResolvedModelInfo struct {
	LlmModelInfo
	// Context is the provider-owned context capacity when known.
	Context *LlmModelContext `json:"context,omitempty"`
	// DefaultMaxTokens is the adapter-configured per-request output cap
	// materialized when callers omit one.
	DefaultMaxTokens *int64 `json:"defaultMaxTokens,omitempty"`
	// Reasoning lists adapter-owned selectable reasoning levels when exposed.
	Reasoning *LlmModelReasoningInfo `json:"reasoning,omitempty"`
}

// NewMessageID returns a fresh stable message identity (UUID v4, matching
// the official randomUUID source).
func NewMessageID() MessageID {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return MessageID(formatUUID(b))
}

func formatUUID(b [16]byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, 36)
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 || i == 12 || i == 14 {
			out = append(out, '-')
		}
		out = append(out, hex[v>>4], hex[v&0x0f])
	}
	return string(out)
}

// NewUserMessage creates one identified user-role message.
func NewUserMessage(content []ContentBlock, source MessageSource) Message {
	// The source is honored verbatim — a user-ROLE message may carry any
	// source kind (plugin-snapshot context, tool results, user-rpc). Only a
	// blank kind defaults to the ordinary user source.
	if source.Kind == "" {
		source.Kind = SourceUser
	}
	return Message{ID: NewMessageID(), Role: RoleUser, Content: content, Source: source}
}

// NewAssistantMessage creates one identified model-produced assistant
// message with fixed role and model-source tags.
func NewAssistantMessage(content []ContentBlock, provider, model string, replayState json.RawMessage) Message {
	return Message{
		ID:      NewMessageID(),
		Role:    RoleAssistant,
		Content: content,
		Source: MessageSource{
			Kind:        SourceModel,
			Provider:    provider,
			Model:       model,
			ReplayState: replayState,
		},
	}
}

// NewToolResultMessage creates one identified tool-result message: a
// user-role message whose model-facing block retains call correlation.
func NewToolResultMessage(callID ToolCallID, content []ContentBlock, isError bool) Message {
	return Message{
		ID:   NewMessageID(),
		Role: RoleUser,
		Content: []ContentBlock{{
			Type:       BlockToolResult,
			ToolCallID: callID,
			Content:    content,
			IsError:    isError,
		}},
		Source: MessageSource{Kind: SourceTool, CallID: callID},
	}
}
