package subagent

import (
	"errors"

	"dshgo/agent"
)

// maxSafeInteger is 2^53: beyond it, integers lose exactness in the wire
// JSON encoders and can no longer represent an exact delegation depth.
const maxSafeInteger = int64(1) << 53

func isSafeInteger(value int64) bool {
	return value >= -maxSafeInteger && value <= maxSafeInteger
}

// DelegationDepthOf reads an agent's delegation depth, treating absence as
// top-level depth zero. The persisted session header is authoritative and
// monotone: runtime AgentOptions.SubagentDepth may DEEPEN the count but can
// never lower it — a resumed child arrives with fresh options, and counting
// it from zero would let it delegate as if it were top-level.
func DelegationDepthOf(a *agent.Agent) (int64, error) {
	runtime := int64(0)
	if a.Options.SubagentDepth != nil {
		runtime = *a.Options.SubagentDepth
		// The source also rejects Object.is(runtime, -0); Go's int64 cannot
		// encode IEEE negative zero, so the plain value space is exhaustive.
		if runtime < 0 || !isSafeInteger(runtime) {
			return 0, errors.New("agent subagentDepth must be a non-negative safe integer")
		}
	}
	// The header value was validated at the session boundary (creation and
	// persistence load both construct through the store).
	header := int64(0)
	if depth := a.Session.Header().DelegationDepth; depth != nil {
		header = *depth
	}
	if runtime > header {
		return runtime, nil
	}
	return header, nil
}

// AssertSubagentMaxDepth rejects a recursion cap that cannot represent an
// exact delegation depth.
func AssertSubagentMaxDepth(maxDepth *int64) error {
	if maxDepth == nil {
		return nil
	}
	if *maxDepth < 0 || !isSafeInteger(*maxDepth) {
		return errors.New("subagent maxDepth must be a non-negative safe integer")
	}
	return nil
}
