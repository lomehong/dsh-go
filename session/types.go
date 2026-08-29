// Package session re-implements the core session log of
// @deepseek-ai/dsh-session (official tag dsh-v0.1.2-alpha.1): the
// merge-extensible, append-only source of truth for an agent interaction.
// Message history is derived from this log; every event is lossless JSON and
// sequence numbers stay contiguous, including raw chunks, so persistence can
// store the canonical log verbatim.
//
// Model-visible ⟺ logged: anything that reaches a model request must be
// reconstructable from this log.
package session

import (
	"encoding/json"
	"fmt"

	"dshgo/llm"
)

// SESSION_FORMAT_VERSION is the on-disk session format version, stamped into
// every newly-written SessionHeader and enforced by every persistence
// backend on load. While the harness is unreleased it is pinned at 0: no
// compatibility is implied, incompatible logs are rejected, and no migration
// is provided. Bump exactly when an older runtime could no longer read a new
// log with full semantic correctness — only structural changes reach that
// bar: the header shape, the event envelope, core event semantics, or the
// surface mechanism. Adding an ordinary event type does not bump: the
// known-event guard makes older runtimes refuse logs containing a type they
// do not understand.
const SESSION_FORMAT_VERSION = 0

// SessionID identifies one session in the store and its persistence
// artifacts.
type SessionID = string

// SessionHeader is immutable validated storage metadata, kept outside the
// conversation event log.
type SessionHeader struct {
	// Version is the on-disk format version stamped from
	// SESSION_FORMAT_VERSION at creation; a backend rejects any other
	// version on load (no migration).
	Version int64     `json:"version"`
	ID      SessionID `json:"id"`
	// CreatedAt is a non-negative safe-integer Unix epoch millisecond stamp.
	CreatedAt int64 `json:"createdAt"`
	// CWD is the absolute working directory the session was created in.
	CWD string `json:"cwd,omitempty"`
	// ParentSession is the session this one was forked from, if any.
	ParentSession SessionID `json:"parentSession,omitempty"`
	// SeedLength is how many leading events were inherited through a seed;
	// persisting it lets resume and replay distinguish parent history from
	// child work.
	SeedLength *int64 `json:"seedLength,omitempty"`
	// Origin is the coarse product classification for a session created as
	// a subagent child; presentation metadata, not proof of continuable.
	Origin string `json:"origin,omitempty"`
	// DelegationDepth is absent (zero) for a top-level session, parent
	// depth + 1 for a subagent child; persisted so a recursion budget
	// survives restart and resume.
	DelegationDepth *int64 `json:"delegationDepth,omitempty"`
	// AgentPreset is the id of the agent preset this session's agent was
	// composed from; durable because the preset decides the session's tools
	// and prompt.
	AgentPreset string `json:"agentPreset,omitempty"`
}

// Event type vocabulary — the appendable keys of SessionEventMap.
const (
	EventTurnStart      = "turn/start"
	EventTurnEnd        = "turn/end"
	EventStepStart      = "step/start"
	EventStepEnd        = "step/end"
	EventUserMessage    = "user/message"
	EventAssistantChunk = "assistant/chunk"
	EventAssistantMsg   = "assistant/message"
	EventToolCall       = "tool/call"
	EventToolResult     = "tool/result"
	EventRequestHeader  = "request/header"
	EventRequestCtx     = "request/context"
	EventEndSeed        = "session/end-seed"
)

// Surface events: the subset whose events produce LLM messages and are
// eligible to appear on the ordered surface. Only these may carry SurfaceOp
// and SourceEventSeqs.
var SurfaceEventTypes = map[string]bool{
	EventUserMessage:  true,
	EventAssistantMsg: true,
	EventToolResult:   true,
}

// IsSurfaceEventType reports whether an event type produces LLM messages.
func IsSurfaceEventType(eventType string) bool {
	return SurfaceEventTypes[eventType]
}

// knownEventTypes is the closed known-vocabulary guard set. A build that
// does not know an event type refuses the log (fail-closed vocabulary);
// plugins extend the vocabulary by registering here.
var knownEventTypes = map[string]bool{
	EventTurnStart:      true,
	EventTurnEnd:        true,
	EventStepStart:      true,
	EventStepEnd:        true,
	EventUserMessage:    true,
	EventAssistantChunk: true,
	EventAssistantMsg:   true,
	EventToolCall:       true,
	EventToolResult:     true,
	EventRequestHeader:  true,
	EventRequestCtx:     true,
	EventEndSeed:        true,
}

// KnownEventType reports whether the build understands this event type.
// Unknown types fail closed: a log carrying one is refused, never skipped.
func KnownEventType(eventType string) bool {
	return knownEventTypes[eventType]
}

// RegisterEventType extends the known vocabulary (the plugin-merge path).
// It returns an error when the type is already known. The assembly layer
// uses EnsureEventTypes instead: idempotent, error-free.
func RegisterEventType(eventType string) error {
	if knownEventTypes[eventType] {
		return fmt.Errorf("session: event type %q is already known", eventType)
	}
	knownEventTypes[eventType] = true
	return nil
}

// EnsureEventTypes extends the known vocabulary with every named type,
// idempotently: already-known types are no-ops, so the assembly layer can
// register the static build's full vocabulary on every assembly without
// tracking what ran before.
func EnsureEventTypes(eventTypes ...string) {
	for _, eventType := range eventTypes {
		knownEventTypes[eventType] = true
	}
}

// TurnEndCancelCause kinds: why an active agent driver was cancelled.
const (
	CancelUser     = "user"
	CancelParent   = "parent"
	CancelHook     = "hook"
	CancelDisposed = "disposed"
	CancelLegacy   = "legacy"
)

// TurnEndCancelCause is why an active agent driver was cancelled, including
// imports whose original coarse record carried no cause (legacy).
type TurnEndCancelCause struct {
	Kind string `json:"kind"`
	// Reason is the hook-provided cancellation reason (hook kind only).
	Reason string `json:"reason,omitempty"`
}

// TurnEndReason kinds.
const (
	TurnEndCompleted   = "completed"
	TurnEndAborted     = "aborted"
	TurnEndBlocked     = "blocked"
	TurnEndError       = "error"
	TurnEndMaxTokens   = "max-tokens"
	TurnEndInterrupted = "interrupted"
)

// TurnEndReason is why a turn ended (merge-extensible sum). Aborted carries
// the cancellation cause; Error always carries a structured failure — the
// LlmFailure facts verbatim, or the original error flattened with code
// UNKNOWN.
type TurnEndReason struct {
	Kind   string              `json:"kind"`
	Reason *TurnEndCancelCause `json:"reason,omitempty"`
	Error  *llm.LlmFailure     `json:"error,omitempty"`
}

// RequestHeaderReason values: why a request/header snapshot was appended.
const (
	HeaderReasonInitial = "initial" // the log's first header (a new conversation)
	HeaderReasonResume  = "resume"  // a loop instance's first request over a log that already has headers
	HeaderReasonChange  = "change"  // a later request used a different header
	HeaderReasonSeries  = "series"  // an unchanged header began an explicitly distinct message series
)

// EpochHeader is logged request state outside derived history: call config,
// system prompt, and tools. The latest full request/header snapshot
// reconstructs it; canonical empty optional fields are absent.
type EpochHeader struct {
	// Config is the conversation's call configuration.
	Config llm.LlmCallConfig `json:"config"`
	// AdapterDefaults are effective config fields materialized from the
	// exact adapter rather than proposed by a caller.
	AdapterDefaults *llm.LlmCallConfigAdapterDefaults `json:"adapterDefaults,omitempty"`
	// System is the rendered system prompt text; absent for a system-less
	// request.
	System string `json:"system,omitempty"`
	// Tools are the assembled tool schemas; absent for a tool-less request.
	Tools []llm.ToolSchema `json:"tools,omitempty"`
}

// RequestContext is registration-bound metadata for one resolved model
// route. It is logged only when the route or capacity changes and does not
// participate in request reconstruction or header equality.
type RequestContext struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// ContextWindow is the maximum combined request and response context in
	// tokens, when advertised.
	ContextWindow *int64 `json:"contextWindow,omitempty"`
}

// SurfaceOp is how a session event entered the ordered surface, valid only
// on surface events. Append adds to the tail; Replace swaps the inclusive
// start..end node range for this node and must cite every shadowed node in
// SourceEventSeqs. Used by compaction; any surface-replacing producer may
// use it.
type SurfaceOp struct {
	// Kind is "append" or "replace".
	Kind string `json:"-"`
	// Start and End bound the replaced node range (inclusive), replace only.
	Start int64 `json:"-"`
	End   int64 `json:"-"`
}

// Surface operation kinds.
const (
	SurfaceAppend  = "append"
	SurfaceReplace = "replace"
)

// MarshalJSON renders append as the bare string "append" and replace as the
// object form, matching the official wire exactly.
func (o SurfaceOp) MarshalJSON() ([]byte, error) {
	switch o.Kind {
	case SurfaceAppend:
		return []byte(`"append"`), nil
	case SurfaceReplace:
		return json.Marshal(struct {
			Op    string `json:"op"`
			Start int64  `json:"start"`
			End   int64  `json:"end"`
		}{SurfaceReplace, o.Start, o.End})
	default:
		return nil, fmt.Errorf("session: unknown surface op kind %q", o.Kind)
	}
}

// UnmarshalJSON accepts both wire shapes.
func (o *SurfaceOp) UnmarshalJSON(data []byte) error {
	if string(data) == `"append"` {
		o.Kind, o.Start, o.End = SurfaceAppend, 0, 0
		return nil
	}
	var decoded struct {
		Op    string `json:"op"`
		Start int64  `json:"start"`
		End   int64  `json:"end"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("session: surface op must be \"append\" or a replace object: %w", err)
	}
	if decoded.Op != SurfaceReplace {
		return fmt.Errorf("session: unknown surface op %q", decoded.Op)
	}
	o.Kind, o.Start, o.End = SurfaceReplace, decoded.Start, decoded.End
	return nil
}

// TurnStartData is the turn/start payload.
type TurnStartData struct {
	Turn int64 `json:"turn"`
}

// StepStartData is the step/start payload.
type StepStartData struct {
	Turn int64 `json:"turn"`
	Step int64 `json:"step"`
}

// StepEndData is the step/end payload.
type StepEndData struct {
	Turn int64 `json:"turn"`
	Step int64 `json:"step"`
}

// TurnEndData is the turn/end payload.
type TurnEndData struct {
	Turn   int64         `json:"turn"`
	Reason TurnEndReason `json:"reason"`
}

// SurfaceIntent is surface placement and cited source-event seqs for
// Session.Append. Required on message-producing events and forbidden on
// log-only events.
type SurfaceIntent struct {
	SurfaceOp SurfaceOp
	// SourceEventSeqs is the complete set of known source-event seqs.
	// assistant/message may use a present empty array for a known empty
	// provider stream; nil means the event does not record which earlier
	// events produced the message. Other surface events require a
	// non-empty set when the field is present.
	SourceEventSeqs   []int64
	SourceSeqsPresent bool
}

// Event is one immutable entry in the session log. The Data payload rides as
// raw JSON — lossless by construction, byte-compatible with the official
// log — and typed decoders pin each known event's payload shape.
type Event struct {
	Type string `json:"type"`
	// Seq is the monotonic sequence number within the session.
	Seq int64 `json:"seq"`
	// Time is Unix epoch milliseconds.
	Time int64           `json:"time"`
	Data json.RawMessage `json:"data"`

	// SourceEventSeqs cites earlier events this event was built from (the
	// chunk seqs behind an assistant/message, or the shadowed nodes under a
	// compaction replace). Surface events only. Presence is meaningful:
	// assistant/message may carry a present empty array (a known empty
	// provider stream), so nil means absent and an empty non-nil slice
	// marshals as [].
	SourceEventSeqs []int64 `json:"-"`
	// SurfaceOp is how this event entered the surface; surface events only.
	SurfaceOp *SurfaceOp `json:"surfaceOp,omitempty"`
}

// MarshalJSON renders the envelope, preserving a present-but-empty
// sourceEventSeqs field.
func (e Event) MarshalJSON() ([]byte, error) {
	wire := struct {
		Type            string          `json:"type"`
		Seq             int64           `json:"seq"`
		Time            int64           `json:"time"`
		Data            json.RawMessage `json:"data"`
		SurfaceOp       *SurfaceOp      `json:"surfaceOp,omitempty"`
		SourceEventSeqs *[]int64        `json:"sourceEventSeqs,omitempty"`
	}{
		Type: e.Type, Seq: e.Seq, Time: e.Time, Data: e.Data, SurfaceOp: e.SurfaceOp,
	}
	if e.SourceEventSeqs != nil {
		wire.SourceEventSeqs = &e.SourceEventSeqs
	}
	return json.Marshal(wire)
}

// UnmarshalJSON reads the envelope, distinguishing an absent
// sourceEventSeqs from a present empty array.
func (e *Event) UnmarshalJSON(data []byte) error {
	var wire struct {
		Type            string          `json:"type"`
		Seq             int64           `json:"seq"`
		Time            int64           `json:"time"`
		Data            json.RawMessage `json:"data"`
		SurfaceOp       *SurfaceOp      `json:"surfaceOp"`
		SourceEventSeqs *[]int64        `json:"sourceEventSeqs"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	e.Type, e.Seq, e.Time, e.Data, e.SurfaceOp = wire.Type, wire.Seq, wire.Time, wire.Data, wire.SurfaceOp
	if wire.SourceEventSeqs != nil {
		e.SourceEventSeqs = *wire.SourceEventSeqs
		if e.SourceEventSeqs == nil {
			e.SourceEventSeqs = []int64{}
		}
	} else {
		e.SourceEventSeqs = nil
	}
	return nil
}

// DecodeUserMessage reads a user/message payload.
func DecodeUserMessage(e Event) (llm.Message, error) {
	return decodePayload[llm.Message](e, EventUserMessage)
}

// DecodeAssistantMessage reads an assistant/message payload.
func DecodeAssistantMessage(e Event) (AssistantMessageData, error) {
	return decodePayload[AssistantMessageData](e, EventAssistantMsg)
}

// DecodeToolResult reads a tool/result payload.
func DecodeToolResult(e Event) (ToolResultData, error) {
	return decodePayload[ToolResultData](e, EventToolResult)
}

// AssistantMessageData is the assistant/message payload: the assembled
// message for one step, with its usage when the adapter reported token
// accounting. Interrupted marks a cancelled mid-stream finalization of the
// delivered prefix.
type AssistantMessageData struct {
	Turn    int64           `json:"turn"`
	Step    int64           `json:"step"`
	Message llm.Message     `json:"message"`
	Usage   *llm.TokenUsage `json:"usage,omitempty"`
	// Interrupted is true only on the cancelled-prefix finalization marker.
	Interrupted bool `json:"interrupted,omitempty"`
}

// ToolCallData is the tool/call payload: the raw arguments JSON string
// exactly as the model produced it, unparsed.
type ToolCallData struct {
	Turn      int64          `json:"turn"`
	Step      int64          `json:"step"`
	CallID    llm.ToolCallID `json:"callId"`
	Name      string         `json:"name"`
	Arguments string         `json:"arguments"`
}

// ToolResultData is the tool/result payload. Meta is opaque to the core but
// MUST be JSON-serializable: the durable log reproduces the identical card
// on replay.
type ToolResultData struct {
	Turn    int64       `json:"turn"`
	Step    int64       `json:"step"`
	Message llm.Message `json:"message"`
	// Error is the optional internal failure identity.
	Error *ToolResultError `json:"error,omitempty"`
	// Meta is the optional tool-private presentation payload.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// ToolResultError names an internal tool failure.
type ToolResultError struct {
	Name string `json:"name"`
	Code string `json:"code"`
}

// RequestHeaderData is the request/header payload.
type RequestHeaderData struct {
	Header EpochHeader `json:"header"`
	Reason string      `json:"reason"`
	// StartsSeries: a changed header also begins a distinct model-message
	// series.
	StartsSeries bool `json:"startsSeries,omitempty"`
}

func decodePayload[T any](e Event, want string) (T, error) {
	var zero T
	if e.Type != want {
		return zero, fmt.Errorf("session: event seq %d is %q, not %q", e.Seq, e.Type, want)
	}
	var decoded T
	if err := json.Unmarshal(e.Data, &decoded); err != nil {
		return zero, fmt.Errorf("session: event seq %d (%s) payload: %w", e.Seq, e.Type, err)
	}
	return decoded, nil
}
