package compactionbasic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dshgo/commands"
	"dshgo/compaction"
	"dshgo/cordis"
	"dshgo/session"
)

// newCompactSession builds a detached session for lifecycle records.
func newCompactSession(t *testing.T, id string) *session.Session {
	t.Helper()
	header := session.SessionHeader{Version: 0, ID: session.SessionID(id)}
	sess, err := session.NewDetached(id, nil, &header, 0)
	if err != nil {
		t.Fatalf("detached: %v", err)
	}
	return sess
}

func TestCompactCommandOutcomes(t *testing.T) {
	summarySeq := int64(41)
	starts := 0
	seenCommandID := commands.CommandID("")
	var startOverride func(context.Context) (*compaction.Result, error)
	starter := func(invocation commands.Invocation, signal context.Context) (*compaction.Result, error) {
		starts++
		seenCommandID = invocation.CommandID
		if startOverride != nil {
			return startOverride(signal)
		}
		return &compaction.Result{SummarySeq: 41, ShadowedSeqs: []int64{2, 3, 40}, ShadowedTokenCount: 900}, nil
	}
	_ = seenCommandID
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	undo, err := RegisterCompactCommand(runtime, starter)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer undo()
	sess := newCompactSession(t, "compact-1")

	// Success with a summary pointer.
	execution, err := runtime.Execute(context.Background(), nil, sess, "/compact", nil)
	if err != nil || execution == nil {
		t.Fatalf("execute = %v %v", execution, err)
	}
	if execution.Result.Text != "Compacted 3 history items (~900 tokens)." || execution.Result.SourceEventSeq == nil || *execution.Result.SourceEventSeq != 41 {
		t.Fatalf("result = %+v", execution.Result)
	}
	if seenCommandID != execution.CommandID {
		t.Fatalf("starter command id = %s, want %s", seenCommandID, execution.CommandID)
	}

	// Arguments are refused with the usage line.
	if execution, err = runtime.Execute(context.Background(), nil, sess, "/compact later", nil); err != nil || execution == nil ||
		execution.Result.Text != compactUsage {
		t.Fatalf("usage = %v %v %+v", execution, err, executionResult(execution))
	}

	// No compactable history: success with the fixed text.
	startOverride = func(context.Context) (*compaction.Result, error) { return nil, nil }
	if execution, err = runtime.Execute(context.Background(), nil, sess, "/compact", nil); err != nil || execution == nil ||
		execution.Result.Text != "No compactable history yet." {
		t.Fatalf("empty = %v %v %+v", execution, err, executionResult(execution))
	}

	// Every expected capability failure converts to its human-only text.
	texts := map[ManualCompactionKind]string{
		ManualBusy:        "Compaction is unavailable because this process has an active compaction, or the agent is not idle.",
		ManualCancelled:   "Compaction cancelled.",
		ManualChanged:     "The history selected for compaction changed before it could be replaced. The conversation is unchanged; the attempt is recorded in the session log.",
		ManualSummary:     "Compaction could not produce a useful summary. The conversation is unchanged; the attempt is recorded in the session log.",
		ManualCommit:      "Compaction did not finish cleanly; some session history may have changed. Inspect the current session state before retrying.",
		ManualPersistence: "Compaction finished, but the session could not be saved.",
	}
	for kind, want := range texts {
		kind := kind
		startOverride = func(context.Context) (*compaction.Result, error) {
			return nil, &ManualCompactionError{Kind: kind, Message: "m"}
		}
		execution, err = runtime.Execute(context.Background(), nil, sess, "/compact", nil)
		if err != nil || execution == nil || execution.Result.Text != want || execution.Result.Kind != commands.ResultError {
			t.Fatalf("%s = %v %v %+v", kind, execution, err, executionResult(execution))
		}
	}

	// An unexpected failure is not swallowed: it propagates loud.
	startOverride = func(context.Context) (*compaction.Result, error) { return nil, errors.New("provider exploded") }
	if _, err = runtime.Execute(context.Background(), nil, sess, "/compact", nil); err == nil ||
		!strings.Contains(err.Error(), "provider exploded") {
		t.Fatalf("unexpected failure = %v", err)
	}

	// Every settled run so far rode the lifecycle pairing (success + empty
	// + six expected failures + the unexpected one).
	if starts != 9 {
		t.Fatalf("starter calls = %d", starts)
	}

	// The dispatching request's cancellation reads as compaction cancelled.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	startOverride = func(signal context.Context) (*compaction.Result, error) { return nil, signal.Err() }
	execution, err = runtime.Execute(cancelled, nil, sess, "/compact", nil)
	if execution != nil {
		t.Fatalf("pre-cancelled execute = %+v", execution)
	}
	if err == nil {
		t.Fatal("pre-cancelled request must fail loud (the executor settles thrown)")
	}

	_ = summarySeq
}

// executionResult renders a possibly-nil execution for failure messages.
func executionResult(execution *commands.CommandExecution) any {
	if execution == nil {
		return nil
	}
	return execution.Result
}

func TestCompactCommandUndoDrainsInFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	starter := func(_ commands.Invocation, signal context.Context) (*compaction.Result, error) {
		close(started)
		<-release
		return nil, nil
	}
	runtime := commands.NewCommandRuntime(cordis.Discard{})
	undo, err := RegisterCompactCommand(runtime, starter)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newCompactSession(t, "compact-2")
	done := make(chan error, 1)
	go func() {
		_, execErr := runtime.Execute(context.Background(), nil, sess, "/compact", nil)
		done <- execErr
	}()
	<-started
	drained := make(chan struct{})
	go func() {
		undo()
		close(drained)
	}()
	select {
	case <-drained:
		t.Fatal("undo returned while a handler was still in flight")
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("execute: %v", err)
	}
	<-drained
}
