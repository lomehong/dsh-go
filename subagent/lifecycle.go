package subagent

import (
	"encoding/json"
	"fmt"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/session"
)

// ActivationTerminal is how one Activation's residency epoch ended, as both
// the terminal lifecycle edge and the manager's own parent delivery report
// it.
type ActivationTerminal struct {
	// StopReason is why this epoch's last ordinary turn ended. Teardown
	// failure overrides the epoch's own outcome.
	StopReason StopReason
	// Output is the epoch's final assistant content; nil when it produced
	// none. Teardown failure withholds it: an answer this harness could not
	// durably release is not a result.
	Output []llm.ContentBlock
}

// ActivationObserver is the lifecycle observer for one Activation's
// residency epoch, so continuable children emit the same start/end pair as
// one-shot runs. Observers see the same vocabulary as a one-shot run, so a
// child's start and settlement remain observable without exposing whether
// the manager materialized, woke, or cold-resumed it. Creation failure
// before residency emits no lifecycle edge — the manager simply never calls
// Start/Settle for a non-resident epoch. Package-private: the continuation
// manager is the only consumer, and its call ordering (start → capture →
// terminal/settle) is an in-package contract rather than a published
// extension point.
type ActivationObserver struct {
	runtime  *SubagentRuntime
	identity SubagentRunInfo
	parent   *agent.Agent

	// boundary is the child log length at Start: a cold resume replays
	// earlier turns, so this epoch's telemetry must come from the suffix it
	// actually produced — never the whole session, which would report a
	// previous epoch's answer when this one opened no turn.
	boundary int
	// captured is assigned by Capture, which the disposal path always runs
	// before Settle; a resident epoch therefore always has its facts by then.
	captured ActivationTerminal
}

// Start records the epoch's log boundary and publishes the start edge once
// the epoch is resident.
func (o *ActivationObserver) Start(child *agent.Agent) {
	if child != nil {
		o.boundary = len(child.Session.Events())
	}
	o.runtime.emit(EventSubagentStart, o.parent, o.identity)
}

// Capture snapshots the child-dependent terminal facts from the epoch's own
// log suffix while the child is still registered, because handle disposal
// unregisters it and consumers resolve it to read the child's own log and
// scope.
func (o *ActivationObserver) Capture(child *agent.Agent) {
	if child == nil {
		return
	}
	events := child.Session.Events()
	if o.boundary <= len(events) {
		events = events[o.boundary:]
	} else {
		events = nil
	}
	o.captured = ActivationTerminal{
		StopReason: EpochStopReason(events),
		Output:     FinalAssistantOutput(events),
	}
}

// Terminal resolves the terminal facts Settle will publish, without
// publishing them. The manager's parent delivery must run before the
// ownership release that lets the parent settle, which is earlier than the
// terminal edge; both therefore read one computation instead of restating
// the failure rule. A teardown failure overrides the epoch's own outcome and
// withholds its output.
func (o *ActivationObserver) Terminal(failure error) ActivationTerminal {
	if failure != nil {
		return ActivationTerminal{StopReason: StopError}
	}
	return o.captured
}

// Settle publishes the terminal edge exactly once, pairing Start, after the
// disposal outcome is known.
func (o *ActivationObserver) Settle(failure error) {
	terminal := o.Terminal(failure)
	end := SubagentRunEndInfo{
		SubagentRunInfo: o.identity,
		StopReason:      terminal.StopReason,
	}
	if len(terminal.Output) > 0 {
		end.LastAssistantMessage = terminal.Output
	}
	o.runtime.emit(EventSubagentEnd, o.parent, end)
}

// createActivationObserver builds the observer for one continuable
// Activation's residency epoch. The run id is minted once so start and end
// pair by it, exactly like a one-shot run.
func createActivationObserver(runtime *SubagentRuntime, provider string, childID session.SessionID, parent *agent.Agent) *ActivationObserver {
	return &ActivationObserver{
		runtime: runtime,
		identity: SubagentRunInfo{
			RunID:    newSubagentRunID(),
			Provider: provider,
			ID:       childID,
			Local:    true,
		},
		parent:   parent,
		captured: ActivationTerminal{StopReason: StopCompleted},
	}
}

// EpochStopReason is why this child's epoch ended, for the terminal lifecycle
// edge and the manager's own parent delivery. The child's own log is
// authoritative: teardown succeeding says nothing about whether the model
// errored, hit its token ceiling, or was cancelled, so deriving the reason
// from disposal would report failed work as completed.
//
// FoldConsumedWork supplies both halves the raw turn sequence cannot: which
// turn accounts for the work this epoch consumed, and whether accepted work
// was cancelled after it without any turn opening over it. A recorded
// failure still wins over a cancellation — stopping a child that had already
// failed does not turn its failure into a cancellation.
func EpochStopReason(events []session.Event) StopReason {
	work := agent.FoldConsumedWork(events)
	if work.End == nil {
		// A clean ending and no accounting turn at all share one rule: the
		// epoch finished what it was given unless a cancelled queue says
		// otherwise.
		if work.DroppedUnrun {
			return StopAborted
		}
		return StopCompleted
	}
	var endData session.TurnEndData
	if err := json.Unmarshal(work.End.Data, &endData); err != nil {
		return StopError
	}
	switch endData.Reason.Kind {
	case session.TurnEndMaxTokens:
		return StopMaxTokens
	case session.TurnEndAborted, session.TurnEndInterrupted:
		return StopAborted
	case session.TurnEndError:
		return StopError
	case session.TurnEndBlocked:
		// A pre-step rejection — a hook deny, a policy plugin — discarded
		// input this epoch had claimed: the work was declined, not done.
		return StopRefusal
	case session.TurnEndCompleted:
		if work.DroppedUnrun {
			return StopAborted
		}
		return StopCompleted
	default:
		// TurnEndReason is merge-extensible, so this arm needs a backend
		// that adds a variant; treating an unnameable reason as success
		// would report failed work as completed.
		return StopError
	}
}

// renderThrown renders one listener failure for the containment log without
// letting coercion escape.
func renderThrown(value any) string {
	if err, ok := value.(error); ok {
		return fmt.Sprintf("%T: %s", err, err.Error())
	}
	return fmt.Sprintf("%v", value)
}
