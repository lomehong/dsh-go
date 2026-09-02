// Contract tests for the inbox projection: replay, claims, cancellation,
// replacement, duplicate rejection, and seed exclusion.
package agent

import (
	"slices"
	"testing"

	"dshgo/llm"
	"dshgo/session"
)

func inboxMessage(id string) llm.Message {
	return llm.Message{ID: llm.MessageID(id), Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "m:" + id}}}
}

func newEmptySession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, nil, 0)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return sess
}

func mustTurnStart(t *testing.T, s *session.Session, turn int64) {
	t.Helper()
	if _, err := s.Append(session.EventTurnStart, session.TurnStartData{Turn: turn}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
}

func mustTurnEnd(t *testing.T, s *session.Session, turn int64, kind string) {
	t.Helper()
	if _, err := s.Append(session.EventTurnEnd, session.TurnEndData{Turn: turn, Reason: session.TurnEndReason{Kind: kind}}, nil); err != nil {
		t.Fatalf("turn/end: %v", err)
	}
}

func mustStepStart(t *testing.T, s *session.Session, turn, step int64) {
	t.Helper()
	if _, err := s.Append(session.EventStepStart, session.StepStartData{Turn: turn, Step: step}, nil); err != nil {
		t.Fatalf("step/start: %v", err)
	}
}

func TestInboxAppendAndClaim(t *testing.T) {
	sess := newEmptySession(t, "inbox-1")
	notifications := &recordingNotifications{}
	inbox, err := NewInbox(sess, notifications)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := inbox.Append(InboxNextStep, inboxMessage("s1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !inbox.HasPending() {
		t.Fatal("inbox must be pending")
	}

	claimed, err := inbox.Claim(InboxNextTurn, 7)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(claimed) != 2 || claimed[0].ID != "s1" || claimed[1].ID != "m1" {
		t.Fatalf("claimed = %+v", claimed)
	}
	if inbox.HasPending() {
		t.Fatal("claim must drain the inbox")
	}
	if !slices.Equal(notifications.events, []string{"inserted:m1", "inserted:s1", "claimed:s1", "claimed:m1"}) {
		t.Fatalf("notifications = %v", notifications.events)
	}

	// The durable splices: one insertion each, then two pure deletions with
	// removedCount and no canceled outcome.
	var splices []InboxSplicedData
	for _, event := range sess.Events() {
		if event.Type == EventAgentInboxSpliced {
			data, err := DecodeInboxSpliced(event)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			splices = append(splices, data)
		}
	}
	if len(splices) != 4 {
		t.Fatalf("splices = %+v", splices)
	}
	if splices[0].Outcome != "" || splices[0].RemovedCount != nil {
		t.Fatalf("insert splice = %+v", splices[0])
	}
	if splices[2].Outcome != "" || *splices[2].RemovedCount != 1 || splices[2].Start != 0 {
		t.Fatalf("claim splice = %+v", splices[2])
	}
}

func TestInboxClearCancelsStepBeforeTurn(t *testing.T) {
	sess := newEmptySession(t, "inbox-2")
	notifications := &recordingNotifications{}
	inbox, _ := NewInbox(sess, notifications)
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := inbox.Append(InboxNextStep, inboxMessage("s1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	notifications.events = nil
	if err := inbox.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if inbox.HasPending() {
		t.Fatal("clear must empty both lists")
	}
	// next-step clears first, and cancellations publish discarded.
	if len(notifications.events) != 2 || notifications.events[0] != "discarded:s1" || notifications.events[1] != "discarded:m1" {
		t.Fatalf("notifications = %v", notifications.events)
	}
	var splices []InboxSplicedData
	for _, event := range sess.Events() {
		if event.Type == EventAgentInboxSpliced {
			data, _ := DecodeInboxSpliced(event)
			splices = append(splices, data)
		}
	}
	if len(splices) != 4 {
		t.Fatalf("splices = %d", len(splices))
	}
	if splices[2].Target != InboxNextStep || splices[2].Outcome != InboxOutcomeCanceled {
		t.Fatalf("step clear = %+v", splices[2])
	}
	if splices[3].Target != InboxNextTurn || splices[3].Outcome != InboxOutcomeCanceled {
		t.Fatalf("turn clear = %+v", splices[3])
	}
}

func TestInboxReplaceAndRemove(t *testing.T) {
	sess := newEmptySession(t, "inbox-3")
	notifications := &recordingNotifications{}
	inbox, _ := NewInbox(sess, notifications)
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	replaced, err := inbox.Replace("m1", inboxMessage("m2"))
	if err != nil || !replaced {
		t.Fatalf("Replace = %v, %v", replaced, err)
	}
	if len(inbox.NextTurn()) != 1 || inbox.NextTurn()[0].ID != "m2" {
		t.Fatalf("nextTurn = %+v", inbox.NextTurn())
	}
	if !slices.Equal(notifications.events, []string{"inserted:m1", "discarded:m1", "inserted:m2"}) {
		t.Fatalf("notifications = %v", notifications.events)
	}

	removed, err := inbox.Remove("m2")
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	removed, err = inbox.Remove("m2")
	if err != nil || removed {
		t.Fatalf("re-Remove = %v, %v", removed, err)
	}
}

func TestInboxDuplicateIdentityRejected(t *testing.T) {
	sess := newEmptySession(t, "inbox-4")
	inbox, _ := NewInbox(sess, &recordingNotifications{})
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := inbox.Append(InboxNextTurn, inboxMessage("m1")); err == nil ||
		err.Error() != `message "m1" is already pending` {
		t.Fatalf("dup err = %v", err)
	}
	// Across lists too.
	if err := inbox.Append(InboxNextStep, inboxMessage("m1")); err == nil ||
		err.Error() != `message "m1" is already pending` {
		t.Fatalf("cross-list dup err = %v", err)
	}
	// Replacing with another pending identity is rejected as well.
	if err := inbox.Append(InboxNextTurn, inboxMessage("m2")); err != nil {
		t.Fatalf("Append m2: %v", err)
	}
	if _, err := inbox.Replace("m2", inboxMessage("m1")); err == nil ||
		err.Error() != `message "m1" is already pending` {
		t.Fatalf("replace-dup err = %v", err)
	}
}

func TestInboxReplayFromPersistedLog(t *testing.T) {
	first := newEmptySession(t, "inbox-5")
	recorder := &recordingNotifications{}
	live, _ := NewInbox(first, recorder)
	if err := live.Append(InboxNextTurn, inboxMessage("keep")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := live.Append(InboxNextStep, inboxMessage("gone")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := live.Remove("gone"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	replayNotifications := &recordingNotifications{}
	restored, err := NewInbox(first, replayNotifications)
	if err != nil {
		t.Fatalf("replay NewInbox: %v", err)
	}
	if len(restored.NextTurn()) != 1 || restored.NextTurn()[0].ID != "keep" {
		t.Fatalf("nextTurn = %+v", restored.NextTurn())
	}
	if len(restored.NextStep()) != 0 {
		t.Fatalf("nextStep = %+v", restored.NextStep())
	}
	// Replay does not republish notifications.
	if len(replayNotifications.events) != 0 {
		t.Fatalf("replay published = %v", replayNotifications.events)
	}
}

func TestInboxSeedBoundaryExcludesParentPendingWork(t *testing.T) {
	parent := newEmptySession(t, "inbox-6")
	seedInbox, _ := NewInbox(parent, &recordingNotifications{})
	if err := seedInbox.Append(InboxNextTurn, inboxMessage("parents")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	seed := parent.Events()
	seedLength := int64(len(seed))
	child, err := session.NewDetached("inbox-6-child", seed, &session.SessionHeader{ID: "inbox-6-child", IsSeeded: true, InheritedEventCount: session.SessionLogOffset(seedLength)}, session.SessionLogOffset(seedLength))
	if err != nil {
		t.Fatalf("child session: %v", err)
	}
	childInbox, err := NewInbox(child, &recordingNotifications{})
	if err != nil {
		t.Fatalf("child inbox: %v", err)
	}
	if childInbox.HasPending() {
		t.Fatalf("pending work must not survive a fork: %+v", childInbox.NextTurn())
	}
}

func TestInboxCorruptPersistedSpliceFailsLoud(t *testing.T) {
	sess := newEmptySession(t, "inbox-7")
	mustTurnStart(t, sess, 1)
	// Forge an out-of-range persisted splice (coordinates the live clamp can
	// never produce; the projection must reject it at replay).
	removed := int64(1)
	if _, err := sess.Append(EventAgentInboxSpliced, InboxSplicedData{Target: InboxNextTurn, Start: 5, RemovedCount: &removed}, nil); err != nil {
		t.Fatalf("forge append: %v", err)
	}
	_, err := NewInbox(sess, &recordingNotifications{})
	if err == nil || err.Error() != "invalid persisted inbox splice at session seq 1: invalid inbox splice" {
		t.Fatalf("replay err = %v", err)
	}
}

func TestInboxSpliceValidationLive(t *testing.T) {
	sess := newEmptySession(t, "inbox-8")
	inbox, _ := NewInbox(sess, &recordingNotifications{})
	// Live splices clamp out-of-range coordinates to a no-op (JS splice
	// semantics); the validation rejects only unclampable persisted events.
	if _, err := inbox.Splice(InboxNextTurn, 1, 0, nil); err != nil {
		t.Fatalf("clamped splice = %v", err)
	}
	eventsBefore := len(sess.Events())
	if _, err := inbox.Splice(InboxNextTurn, 0, 0, nil); err != nil {
		t.Fatalf("no-op splice: %v", err)
	}
	if len(sess.Events()) != eventsBefore {
		t.Fatal("no-op splice must not append")
	}
}
