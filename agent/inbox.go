// Incremental projection of durable agent inbox events. Port of
// packages/core/agent/src/inbox.ts.
package agent

import (
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/llm"
	"dshgo/session"
)

// EventAgentInboxSpliced is one normalized mutation of an agent's durable
// pending-message lists. Live dispatch precedes projection mutation, so
// synchronous observers may read the pre-splice inbox to recover the removed
// messages.
const EventAgentInboxSpliced = "agent/inbox/spliced"

// InboxOutcomeCanceled marks a splice whose removals are cancellations, not
// claims.
const InboxOutcomeCanceled = "canceled"

// RegisterEvents extends the session vocabulary with this package's event
// types; the assembly layer (boot) calls it for the static build.
func RegisterEvents() {
	// The agent package owns this vocabulary member: a build that does not
	// know the type refuses the log (fail-closed).
	session.EnsureEventTypes(EventAgentInboxSpliced)
}

// InboxSplicedData is the agent/inbox/spliced payload.
type InboxSplicedData struct {
	Target InboxTarget `json:"target"`
	Start  int64       `json:"start"`
	// RemovedCount is present exactly when the splice removed messages.
	RemovedCount *int64        `json:"removedCount,omitempty"`
	Inserted     []llm.Message `json:"inserted"`
	Outcome      string        `json:"outcome,omitempty"`
}

// DecodeInboxSpliced reads one agent/inbox/spliced payload.
func DecodeInboxSpliced(event session.Event) (InboxSplicedData, error) {
	var data InboxSplicedData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return InboxSplicedData{}, err
	}
	return data, nil
}

// InboxNotifications are the live notifications committed by inbox mutations.
type InboxNotifications interface {
	// Inserted publishes one inserted message.
	Inserted(message llm.Message)
	// Discarded publishes one discarded message.
	Discarded(message llm.Message)
	// Claimed publishes one claimed message inside its owning turn.
	Claimed(message llm.Message, turn int64)
}

// Inbox is a replay-once projection that incrementally consumes later inbox
// splices. The mutex serializes every read-validate-commit-apply cycle: Go
// hook handlers deliver pending input from other goroutines while the driver
// claims it, so each splice commits atomically against the lists current at
// its slot. Live notifications dispatch after the unlock.
type Inbox struct {
	mu            sync.Mutex
	session       *session.Session
	notifications InboxNotifications
	nextTurn      []llm.Message
	nextStep      []llm.Message
	// pendingNotifications queues the live dispatches committed while the
	// lock is held; the caller drains strictly after releasing it. Guarded
	// by mu.
	pendingNotifications []func()
}

// NewInbox builds the projection and replays the session's owned log (past
// the seed boundary). Malformed persisted splices fail loud with the seq
// attribution.
func NewInbox(s *session.Session, notifications InboxNotifications) (*Inbox, error) {
	inbox := &Inbox{session: s, notifications: notifications}
	events := s.Events()
	start := 0
	if seedLength := s.Header().SeedLength; seedLength != nil {
		start = int(*seedLength)
	}
	for _, event := range events[start:] {
		if event.Type != EventAgentInboxSpliced {
			continue
		}
		splice, err := DecodeInboxSpliced(event)
		if err != nil {
			return nil, fmt.Errorf("invalid persisted inbox splice at session seq %d: %w", event.Seq, err)
		}
		if err := inbox.apply(splice); err != nil {
			return nil, fmt.Errorf("invalid persisted inbox splice at session seq %d: %w", event.Seq, err)
		}
	}
	return inbox, nil
}

// NextTurn returns a snapshot of the prompts awaiting individual turns.
func (i *Inbox) NextTurn() []llm.Message {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]llm.Message(nil), i.nextTurn...)
}

// NextStep returns a snapshot of the input awaiting the next step boundary.
func (i *Inbox) NextStep() []llm.Message {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]llm.Message(nil), i.nextStep...)
}

// HasPending reports whether either pending-message list contains work.
func (i *Inbox) HasPending() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.nextTurn) > 0 || len(i.nextStep) > 0
}

// Clear durably cancels all pending input, clearing next-step before
// next-turn.
func (i *Inbox) Clear() error {
	i.mu.Lock()
	if _, err := i.mutateLocked(InboxNextStep, 0, int64(len(i.nextStep)), nil, true); err != nil {
		i.mu.Unlock()
		return err
	}
	_, err := i.mutateLocked(InboxNextTurn, 0, int64(len(i.nextTurn)), nil, true)
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	return err
}

// Claim removes and returns the complete batch proposed for one step,
// publishing each claimed message. The durable splices are pure deletions.
// Target InboxNextTurn also consumes one queued turn. Internal: the agent
// loop's step-boundary operation, not a plugin extension point.
func (i *Inbox) Claim(target InboxTarget, turn int64) ([]llm.Message, error) {
	i.mu.Lock()
	claimed, err := i.mutateLocked(InboxNextStep, 0, int64(len(i.nextStep)), nil, false)
	if err == nil && target == InboxNextTurn {
		var queued []llm.Message
		queued, err = i.mutateLocked(InboxNextTurn, 0, 1, nil, false)
		claimed = append(claimed, queued...)
	}
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	if err != nil {
		return nil, err
	}
	for _, message := range claimed {
		i.notifications.Claimed(message, turn)
	}
	return claimed, nil
}

// Append appends one message to a pending list and durably records the
// insertion.
func (i *Inbox) Append(target InboxTarget, message llm.Message) error {
	i.mu.Lock()
	_, err := i.mutateLocked(target, int64(len(i.list(target))), 0, []llm.Message{message}, true)
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	return err
}

// Prepend prepends one message to a pending list and durably records the
// insertion.
func (i *Inbox) Prepend(target InboxTarget, message llm.Message) error {
	_, err := i.Splice(target, 0, 0, []llm.Message{message})
	return err
}

// Replace replaces one pending message in place, possibly changing its
// identity. A successful replacement publishes the old message as discarded
// and the new message as inserted. Reports whether the message was still
// pending. Locate and splice share one lock hold: coordinates from a
// released locate could go stale under a concurrent splice and delete the
// wrong message.
func (i *Inbox) Replace(messageID llm.MessageID, newMessage llm.Message) (bool, error) {
	i.mu.Lock()
	target, index, ok := i.locateLocked(messageID)
	if !ok {
		i.mu.Unlock()
		return false, nil
	}
	_, err := i.mutateLocked(target, int64(index), 1, []llm.Message{newMessage}, true)
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Remove removes one pending message and durably records its cancellation.
// Reports whether the message was still pending. Locate and splice share
// one lock hold: coordinates from a released locate could go stale under a
// concurrent splice and delete the wrong message.
func (i *Inbox) Remove(messageID llm.MessageID) (bool, error) {
	i.mu.Lock()
	target, index, ok := i.locateLocked(messageID)
	if !ok {
		i.mu.Unlock()
		return false, nil
	}
	_, err := i.mutateLocked(target, int64(index), 1, nil, true)
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Splice applies standard splice semantics and durably records the
// normalized result. The durable event commits before the live projection
// mutates, so synchronous session/event observers see the pre-splice lists
// and can reconstruct the removed messages from the normalized coordinates.
// Returns the messages removed by the splice.
func (i *Inbox) Splice(target InboxTarget, start int64, deleteCount int64, inserted []llm.Message) ([]llm.Message, error) {
	i.mu.Lock()
	removed, err := i.mutateLocked(target, start, deleteCount, inserted, true)
	pending := i.drainNotificationsLocked()
	i.mu.Unlock()
	for _, notify := range pending {
		notify()
	}
	return removed, err
}

// drainNotificationsLocked hands the queued live dispatches to the caller;
// it dispatches them strictly after releasing the lock. Caller holds i.mu.
func (i *Inbox) drainNotificationsLocked() []func() {
	pending := i.pendingNotifications
	i.pendingNotifications = nil
	return pending
}

func (i *Inbox) list(target InboxTarget) []llm.Message {
	if target == InboxNextTurn {
		return i.nextTurn
	}
	return i.nextStep
}

func (i *Inbox) setList(target InboxTarget, list []llm.Message) {
	if target == InboxNextTurn {
		i.nextTurn = list
		return
	}
	i.nextStep = list
}

// locateLocked finds one pending identity across both owned lists,
// next-turn first. Caller holds i.mu, so the returned coordinates are
// consumed before the lock is released.
func (i *Inbox) locateLocked(messageID llm.MessageID) (InboxTarget, int, bool) {
	for _, target := range []InboxTarget{InboxNextTurn, InboxNextStep} {
		for index, message := range i.list(target) {
			if message.ID == messageID {
				return target, index, true
			}
		}
	}
	return "", 0, false
}

// mutateLocked commits one normalized mutation; live notifications dispatch
// after the caller releases the lock, so a notification observer may freely
// read or append the inbox. Caller holds i.mu.
func (i *Inbox) mutateLocked(target InboxTarget, start int64, deleteCount int64, inserted []llm.Message, discardRemoved bool) ([]llm.Message, error) {
	inbox := i.list(target)
	actualStart := start
	if actualStart < 0 {
		if fromEnd := int64(len(inbox)) + actualStart; fromEnd > 0 {
			actualStart = fromEnd
		} else {
			actualStart = 0
		}
	} else if actualStart > int64(len(inbox)) {
		actualStart = int64(len(inbox))
	}
	actualDeleteCount := deleteCount
	if actualDeleteCount < 0 {
		actualDeleteCount = 0
	}
	if remaining := int64(len(inbox)) - actualStart; actualDeleteCount > remaining {
		actualDeleteCount = remaining
	}
	if actualDeleteCount == 0 && len(inserted) == 0 {
		return nil, nil
	}
	outcome := ""
	if discardRemoved && actualDeleteCount > 0 {
		outcome = InboxOutcomeCanceled
	}
	splice := InboxSplicedData{Target: target, Start: actualStart, Inserted: inserted, Outcome: outcome}
	if actualDeleteCount > 0 {
		removed := actualDeleteCount
		splice.RemovedCount = &removed
	}
	if err := i.validate(splice); err != nil {
		return nil, err
	}
	event, err := i.session.Append(EventAgentInboxSpliced, splice, nil)
	if err != nil {
		return nil, err
	}
	// Re-read the stored payload: the event's normalized wire form is what
	// later replays apply, and it is the source of record for the insert.
	stored, err := DecodeInboxSpliced(event)
	if err != nil {
		return nil, fmt.Errorf("invalid inbox splice: %w", err)
	}
	removedMessages := append([]llm.Message{}, inbox[actualStart:actualStart+actualDeleteCount]...)
	updated := make([]llm.Message, 0, int64(len(inbox))-actualDeleteCount+int64(len(stored.Inserted)))
	updated = append(updated, inbox[:actualStart]...)
	updated = append(updated, stored.Inserted...)
	updated = append(updated, inbox[actualStart+actualDeleteCount:]...)
	i.setList(target, updated)
	notify := func() {
		if discardRemoved {
			for _, message := range removedMessages {
				i.notifications.Discarded(message)
			}
		}
		for _, message := range stored.Inserted {
			i.notifications.Inserted(message)
		}
	}
	i.pendingNotifications = append(i.pendingNotifications, notify)
	return removedMessages, nil
}

// apply applies one normalized durable splice to the projection.
func (i *Inbox) apply(splice InboxSplicedData) error {
	if err := i.validate(splice); err != nil {
		return err
	}
	removedCount := int64(0)
	if splice.RemovedCount != nil {
		removedCount = *splice.RemovedCount
	}
	inbox := i.list(splice.Target)
	updated := make([]llm.Message, 0, int64(len(inbox))-removedCount+int64(len(splice.Inserted)))
	updated = append(updated, inbox[:splice.Start]...)
	updated = append(updated, splice.Inserted...)
	updated = append(updated, inbox[splice.Start+removedCount:]...)
	i.setList(splice.Target, updated)
	return nil
}

// validate checks one normalized splice against the current projection:
// coordinates in range and no duplicated pending identity across both lists.
func (i *Inbox) validate(splice InboxSplicedData) error {
	removedCount := int64(0)
	if splice.RemovedCount != nil {
		removedCount = *splice.RemovedCount
	}
	inbox := i.list(splice.Target)
	if splice.Start < 0 || splice.Start > int64(len(inbox)) || removedCount < 0 || splice.Start+removedCount > int64(len(inbox)) {
		return fmt.Errorf("invalid inbox splice")
	}
	candidate := make([]llm.Message, 0, int64(len(inbox))-removedCount+int64(len(splice.Inserted)))
	candidate = append(candidate, inbox[:splice.Start]...)
	candidate = append(candidate, splice.Inserted...)
	candidate = append(candidate, inbox[splice.Start+removedCount:]...)
	var ids map[string]bool
	check := func(message llm.Message) error {
		if ids[message.ID] {
			return fmt.Errorf("message %q is already pending", message.ID)
		}
		ids[message.ID] = true
		return nil
	}
	other := i.list(InboxNextTurn)
	if splice.Target == InboxNextTurn {
		ids = make(map[string]bool, len(candidate)+len(i.nextStep))
		for _, message := range candidate {
			if err := check(message); err != nil {
				return err
			}
		}
		other = i.nextStep
	} else {
		ids = make(map[string]bool, len(candidate)+len(i.nextTurn))
		for _, message := range i.nextTurn {
			if err := check(message); err != nil {
				return err
			}
		}
		other = candidate
	}
	for _, message := range other {
		if err := check(message); err != nil {
			return err
		}
	}
	return nil
}
