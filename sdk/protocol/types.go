// Package protocol ports packages/sdk/protocol: the shared wire protocol
// for the DSH SDK runtime — the newline-delimited JSON-RPC transport plus
// the named request, result, and notification types both wire ends speak.
// The runtime server serves this protocol; SDK clients drive it.
package protocol

import (
	"encoding/json"

	"dshgo/llm"
)

// ServerName is the wire-stable server identity returned by initialization.
const ServerName = "deepseek-harness-sdk-runtime"

// Client-to-server request methods.
const (
	MethodInitialize    = "initialize"
	MethodSessionPrompt = "session/prompt"
	MethodShutdown      = "shutdown"
)

// Server-to-client notification methods.
const (
	NotifySessionEvent     = "session.event"
	NotifySessionStatus    = "session.status"
	NotifySubagentStarted  = "subagent.started"
	NotifySubagentFinished = "subagent.finished"
)

// Session statuses on the wire.
const (
	StatusIdle    = "idle"
	StatusRunning = "running"
)

// SdkRunStatus is the deployment-mapped SDK outcome: ok for an accepted
// result, error otherwise.
type SdkRunStatus string

// Run outcome values.
const (
	RunStatusOk    SdkRunStatus = "ok"
	RunStatusError SdkRunStatus = "error"
)

// InitializeParams are the parameters for the process-wide SDK handshake.
type InitializeParams struct {
	// Cwd is the working directory recorded on every SDK-created session's
	// header.
	Cwd string `json:"cwd"`
	// Provider is the provider route every SDK-created agent runs on.
	Provider string `json:"provider"`
	// Model is the model name every SDK-created agent runs on (the server
	// may mount a fallback adapter).
	Model string `json:"model"`
	// ReasoningEffort is the optional adapter-owned reasoning effort for the
	// selected provider/model route.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	// MaxTokens is the optional positive output-token cap inherited by
	// SDK-created agents and their in-process descendants.
	MaxTokens int64 `json:"maxTokens,omitempty"`
}

// ServerIdentity is the wire-stable server identity.
type ServerIdentity struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is the wire-stable server identity returned by
// initialization.
type InitializeResult struct {
	ServerInfo ServerIdentity `json:"serverInfo"`
}

// SessionPromptParams is one user turn on one SDK session.
type SessionPromptParams struct {
	// SessionID is the SDK-side session id; an unknown id lazily creates the
	// agent+session pair.
	SessionID string `json:"sessionId"`
	// ContentBlocks are the prompt content blocks, sent verbatim as the user
	// message. Each element decodes as an llm content block or an
	// SdkEncodedImageBlock awaiting admission.
	ContentBlocks []json.RawMessage `json:"contentBlocks"`
}

// SdkEncodedImageBlock is inline raster input admitted into the runtime's
// durable attachment store.
type SdkEncodedImageBlock struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// ImageMimeTypes are the declared raster MIME types verified during
// admission.
var ImageMimeTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// SessionPromptResult is the durable enqueue receipt for one prompt.
type SessionPromptResult struct {
	// MessageID is the identity of the queued user message.
	MessageID string `json:"messageId"`
}

// SessionEventNotification is the `session.event` payload: one session-log
// event, streamed as it is recorded.
type SessionEventNotification struct {
	// SessionID is the session the event belongs to (every session in the
	// runtime, not only SDK-created ones).
	SessionID string `json:"sessionId"`
	// Event is the full session-log event envelope.
	Event json.RawMessage `json:"event"`
}

// SessionStatusNotification is the whole-agent lifecycle state for one
// session.
type SessionStatusNotification struct {
	// SessionID is the session whose live agent changed status.
	SessionID string `json:"sessionId"`
	// Status is the whole-agent state after the transition.
	Status string `json:"status"`
}

// SubagentStartedNotification is the `subagent.started` payload: an
// in-runtime child session was created.
type SubagentStartedNotification struct {
	// ParentSessionID is the delegating session.
	ParentSessionID string `json:"parentSessionId"`
	// ChildSessionID is the new child session.
	ChildSessionID string `json:"childSessionId"`
}

// SubagentFinishedNotification is the `subagent.finished` payload: an
// in-process subagent run ended (remote runs are not reported).
type SubagentFinishedNotification struct {
	// Provider is the subagent provider name that ran the child.
	Provider string `json:"provider"`
	// AgentID is the child agent's id (equals ChildSessionID for local
	// runs).
	AgentID string `json:"agentId"`
	// ParentSessionID is the delegating session.
	ParentSessionID string `json:"parentSessionId"`
	// ChildSessionID is the child session.
	ChildSessionID string `json:"childSessionId"`
	// Status is the deployment-mapped run outcome.
	Status SdkRunStatus `json:"status"`
	// StopReason is the provider-reported stop reason.
	StopReason string `json:"stopReason"`
	// LastAssistantMessage is the child's selected assistant output; absent
	// when the child produced none.
	LastAssistantMessage []llm.ContentBlock `json:"lastAssistantMessage,omitempty"`
}
