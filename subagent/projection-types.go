package subagent

// Subagent projection vocabulary (official projection-types.ts). The units
// fold `subagent/descriptor` and turn-timing events into durable per-session
// values; the listing reads identity, the control surface reads timing.

// SubagentTimingProjection is the durable active-turn timing for one
// descriptor-backed child session (official `subagentTiming` unit value).
type SubagentTimingProjection struct {
	// SettledMs accumulates completed turns after the child's own descriptor.
	SettledMs int64 `json:"settledMs"`
	// Active bounds the currently open turn, when one has not reached
	// turn/end.
	Active *SubagentTimingActive `json:"active,omitempty"`
}

// SubagentTimingActive bounds one open turn.
type SubagentTimingActive struct {
	// Since is the start of the open turn.
	Since int64 `json:"since"`
	// Through is the latest event time folded into this projection cut.
	Through int64 `json:"through"`
}

// Subagent identity lifecycle modes.
const (
	SubagentModeOneShot           = "one-shot"
	SubagentModeContinual         = "continuable"
	SubagentActivityLive          = "running"
	SubagentActivityCold          = "inactive"
	SubagentDiagnosticCorrupt     = "corrupt"
	SubagentDiagnosticUnsupported = "unsupported"
	SubagentDiagnosticUnavailable = "unavailable"
)

// SubagentIdentityProjection is the durable identity of one descriptor-backed
// subagent session (official `subagent` unit value): lifecycle mode plus
// creation label, folded last-wins from `subagent/descriptor` events. Label
// strength follows the descriptor schema — a continuable child always carries
// one, a one-shot child may omit it.
type SubagentIdentityProjection struct {
	// Mode discriminates the descriptor lifecycle.
	Mode string `json:"mode"`
	// Label is the durable creation label; always set for continuable
	// children, optional for one-shot.
	Label *string `json:"label,omitempty"`
	// Seq is the sequence of the `subagent/descriptor` event this identity
	// was folded from. Seq >= header.SeedLength proves the identity comes
	// from the child's OWN log suffix — where a descriptor is immutable once
	// appended — and not from a fork seed's replayed ancestor descriptor.
	Seq int64 `json:"seq"`
}

// SubagentProjectionValues carries the served values of one projection
// snapshot cut. A nil Subagent merges the official `null` (registered
// no-value sentinel) and `undefined` (dropped at a JSON boundary): both are
// no value for every consumer.
type SubagentProjectionValues struct {
	Subagent *SubagentIdentityProjection `json:"subagent"`
}
