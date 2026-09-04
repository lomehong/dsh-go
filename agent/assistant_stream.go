package agent

import "dshgo/llm"

// AssistantStreamFrame is one transient live frame (official
// AssistantStreamFrame in runtime-types.ts): a sealed start/chunk/end union
// published on the scoped `agent/assistant-stream` event. Durable
// settlements own replay; these frames are process-local presentation.
type AssistantStreamFrame struct {
	Type      string `json:"type"`
	AttemptID llm.LlmAttemptId `json:"attemptId"`
	// Revision is monotone within one attached Agent lifecycle; a
	// replacement Agent restarts at 1.
	Revision int64 `json:"revision"`
	Turn     int64 `json:"turn"`
	Step     int64 `json:"step"`
	// Chunk frames: dense zero-based position and the stream timestamp the
	// durable embedded stream reuses.
	Index int64           `json:"index,omitempty"`
	Time  int64           `json:"time,omitempty"`
	Chunk *llm.StreamChunk `json:"chunk,omitempty"`
	// End frames: the durable settlement committed before this notification,
	// or live abandonment without one.
	Outcome *AssistantStreamOutcome `json:"outcome,omitempty"`
}

// AssistantStreamOutcome is the end frame's settlement record.
type AssistantStreamOutcome struct {
	Kind       string `json:"kind"` // committed | abandoned
	EventType  string `json:"eventType,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
}

