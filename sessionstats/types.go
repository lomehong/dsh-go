// Package sessionstats ports packages/session/session-stats: the
// `sessionStats` projection unit, a pure fold of step boundaries, stream
// chunks, tool pairs, and assembled assistant messages into whole-log
// counts and wall times.
package sessionstats

// ProjectionKey is the unit's registered projection key.
const ProjectionKey = "sessionStats"

// Projection is the whole-log conversation figures (the wire view),
// independent of how much history a client has paged in. Counts and wall
// times all fold from the complete durable log; every field is 0 until its
// first contributing event lands. Field names mirror the client window fold
// so an assembly without this unit can fall back to it wholesale.
type Projection struct {
	// Turns counts distinct turns carrying at least one closed step
	// (step/end); rejected or empty turns are uncounted.
	Turns int64 `json:"turns"`
	// Steps counts closed step/end events — completed, failed, and
	// cancelled steps alike.
	Steps int64 `json:"steps"`
	// LlmMs sums model wall time (step/start → assistant/message) over
	// steps that assembled a message.
	LlmMs int64 `json:"llmMs"`
	// ToolMs sums tool wall time over tool/call → tool/result pairs
	// matched by callId.
	ToolMs int64 `json:"toolMs"`
	// TtftMs sums first-token latency (step/start → first non-empty delta
	// chunk) over TtftSteps.
	TtftMs int64 `json:"ttftMs"`
	// TtftSteps counts steps carrying a recorded first token.
	TtftSteps int64 `json:"ttftSteps"`
	// DecodeMs sums decode wall time (first token → assistant/message) over
	// steps that also report output tokens.
	DecodeMs int64 `json:"decodeMs"`
	// DecodeTokens sums provider output tokens over the same decode-timed
	// steps.
	DecodeTokens int64 `json:"decodeTokens"`
}
