package commandfeedback

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/commands"
	"dshgo/cordis"
	"dshgo/identity"
	"dshgo/llm"
	"dshgo/session"
)

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

func newInvocationSession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\work"}, 0)
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	if _, err := agent.NewInbox(sess, noopNotifications{}); err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	return sess
}

func TestRecordFeedbackNormalizesAndRejectsEmpty(t *testing.T) {
	sess := newInvocationSession(t, "fb-1")
	if err := RecordFeedback(sess, "  great session  "); err != nil {
		t.Fatalf("record: %v", err)
	}
	events := sess.Events()
	var last session.Event
	for _, event := range events {
		if event.Type == EventFeedbackRecord {
			last = event
		}
	}
	if last.Data == nil || !strings.Contains(string(last.Data), `"great session"`) {
		t.Fatalf("payload: %+v", last)
	}
	if err := RecordFeedback(sess, "   "); err == nil || err.Error() != "feedback text must not be empty" {
		t.Fatalf("empty: %v", err)
	}
}

func TestRegisterExecutesFeedbackCommand(t *testing.T) {
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	calls := 0
	undo, err := Register(runtime, Options{
		Getenv: func(string) string { return "" },
		SharingDisclosure: func() string {
			calls++
			return "Session sharing is disabled."
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	definition, ok := runtime.Find(nil, "feedback")
	if !ok {
		t.Fatal("feedback command not registered")
	}
	if definition.Description != "record feedback about this session" {
		t.Fatalf("description: %q", definition.Description)
	}
	sess := newInvocationSession(t, "fb-2")
	result, err := definition.Handler(commands.Invocation{Session: sess, RawInput: "  love it  "})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.Kind != commands.ResultSuccess || !strings.Contains(result.Text, "Feedback recorded for session fb-2") {
		t.Fatalf("result: %+v", result)
	}
	if !strings.Contains(result.Text, "Anonymous user: ") || !strings.Contains(result.Text, "Session sharing is disabled.") {
		t.Fatalf("disclosure text: %q", result.Text)
	}
	if calls != 1 {
		t.Fatalf("disclosure calls: %d", calls)
	}
	// recordInput: false — the authoritative feedback/record event owns the
	// payload, so command/run must not duplicate it.
	if definition.RecordInput == nil || *definition.RecordInput {
		t.Fatal("recordInput must default off")
	}
}

func TestRegisterUsageErrorLeavesNoEvent(t *testing.T) {
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	undo, err := Register(runtime, Options{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	definition, _ := runtime.Find(nil, "feedback")
	sess := newInvocationSession(t, "fb-3")
	result, err := definition.Handler(commands.Invocation{Session: sess, RawInput: "   "})
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.Kind != commands.ResultError || !strings.Contains(result.Text, "Feedback text is required.") || !strings.Contains(result.Text, Usage) {
		t.Fatalf("result: %+v", result)
	}
	for _, event := range sess.Events() {
		if event.Type == EventFeedbackRecord {
			t.Fatal("empty feedback recorded an event")
		}
	}
}

var _ = identity.AnonymousUserIDFileName
