// Package agentloop is the concrete agent driver plugin: it drives one session
// through turn and step boundaries over queued turns and step-boundary input,
// creates scoped ReactLoopAgents, publishes them through the agent registry,
// and owns their ordered teardown.
//
// Port of packages/core/agent-loop/src. Go adaptations:
//
//   - The JS per-agent `Scope`/`Context` pair is the ported agent.Scope
//     (dsh-scope key chain) plus agent.Ctx (*cordis.Context child); the driver
//     registers itself as the Agent's Driver instead of implementing the Agent
//     interface on a second class.
//   - AbortSignal becomes a context.Context carrying a WithCancelCause
//     cancellation; `signal.reason` reads back through context.Cause.
//   - Driver turns run on one goroutine per wake; session appends all happen on
//     that goroutine, so the JS single-threaded ordering holds structurally.
//   - PreparedAgent teardown is memoized with sync.Once instead of a memoized
//     promise; the cordis fiber lifecycle is the caller-owned context disposal.
//   - `markAgentLoopRequest` is a dev-mode attribution marker in the source; the
//     Go request is a value, so it has no counterpart.
//   - Per-agent `provider`/`model`/`cwd` prompt variables register at the
//     agent's own scope (the source registers globally and reads the
//     per-subject agent from the assembly context; the Go AssembleContext
//     carries only the scope, so the closures capture their agent instead).
package agentloop

// DefaultMaxParallelToolCalls is the default maximum in-flight parallel-safe
// calls per agent step.
const DefaultMaxParallelToolCalls = 10
