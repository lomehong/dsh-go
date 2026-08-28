// How one agent log accounts for the work it consumed. Port of
// packages/core/agent/src/consumed-work.ts.
package agent

import (
	"encoding/json"

	"dshgo/session"
)

// ConsumedWork is how one agent log accounts for the work it consumed.
type ConsumedWork struct {
	// End is the latest closed turn that accounts for consumed work: one
	// that entered a model step, or one that claimed inbox input and then
	// failed, was stopped, or was rejected. Nil when no turn closed over
	// any work.
	End *session.Event
	// DroppedUnrun reports whether accepted work was cancelled out of the
	// inbox, unrun, after that turn. This is the only account of input a
	// cancellation took before any turn could open over it — no turn/end
	// describes it.
	DroppedUnrun bool
}

// accountsForClaim reports whether a turn that consumed input but never
// reached a step ends in a way that accounts for that input. Only a
// completed end does not: it had nothing left to run once its claim was
// rewritten away. A blocked end is that input's ending too — the pre-step
// rejection that produced it discarded the claimed messages, so the work it
// took will never run. The merge-extensible default is true: an unnameable
// ending over consumed input must not read as success.
func accountsForClaim(kind string) bool {
	return kind != session.TurnEndCompleted
}

// FoldConsumedWork folds one agent log, or an owned suffix of one, into its
// account of consumed work. Single pass, and every input is the log itself:
// no caller has to sample live state before cancelling, so a cancellation
// issued by anyone — the owner's teardown, an ancestor's interrupt, an
// unloading plugin — reads the same. Malformed payloads cannot occur in a
// well-formed log (append validates) and are skipped.
func FoldConsumedWork(events []session.Event) ConsumedWork {
	stepped := map[int64]bool{}
	claimed := map[int64]bool{}
	var open *int64
	var end *session.Event
	droppedUnrun := false
	for index := range events {
		event := &events[index]
		switch event.Type {
		case session.EventTurnStart:
			var data session.TurnStartData
			if json.Unmarshal(event.Data, &data) == nil {
				turn := data.Turn
				open = &turn
			}
		case session.EventStepStart:
			var data session.StepStartData
			if json.Unmarshal(event.Data, &data) == nil {
				stepped[data.Turn] = true
			}
		case EventAgentInboxSpliced:
			data, err := DecodeInboxSpliced(*event)
			if err != nil {
				continue
			}
			if data.RemovedCount == nil {
				continue
			}
			// A replacement keeps the work pending under a new identity,
			// so only a cancellation that leaves nothing behind drops it.
			if data.Outcome == InboxOutcomeCanceled {
				if len(data.Inserted) == 0 {
					droppedUnrun = true
				}
			} else if open != nil {
				// Claims are the loop's own step-boundary reads, always
				// inside a turn.
				claimed[*open] = true
			}
		case session.EventTurnEnd:
			var data session.TurnEndData
			if json.Unmarshal(event.Data, &data) != nil {
				continue
			}
			open = nil
			wasStepped := stepped[data.Turn]
			delete(stepped, data.Turn)
			wasClaimed := claimed[data.Turn]
			delete(claimed, data.Turn)
			if wasStepped || (wasClaimed && accountsForClaim(data.Reason.Kind)) {
				eventCopy := *event
				end = &eventCopy
				// Anything dropped before this turn closed is what its own
				// ending reports; only a later drop is still unaccounted
				// for.
				droppedUnrun = false
			}
		}
	}
	return ConsumedWork{End: end, DroppedUnrun: droppedUnrun}
}
