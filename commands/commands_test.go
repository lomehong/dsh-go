package commands

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dshgo/cordis"
	"dshgo/scope"
	"dshgo/session"
)

func newSession(t *testing.T, id string) *session.Session {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	return sess
}

func okHandler(Invocation) (CommandResult, error) {
	return CommandResult{Kind: ResultSuccess}, nil
}

func TestParseCommand(t *testing.T) {
	parsed, ok := ParseCommand("/plan")
	if !ok || parsed.Name != "plan" || parsed.RawInput != "" {
		t.Fatalf("parsed = %+v %v", parsed, ok)
	}
	parsed, ok = ParseCommand("/plan  extra ")
	if !ok || parsed.Name != "plan" || parsed.RawInput != "  extra " {
		t.Fatalf("rawInput = %+v %v, want verbatim separator whitespace", parsed, ok)
	}
	parsed, ok = ParseCommand("/plan\nnext")
	if !ok || parsed.RawInput != "\nnext" {
		t.Fatalf("newline rawInput = %+v %v", parsed, ok)
	}
	// The name run must end at the line end or whitespace: `/plan-x` is the
	// single command `plan-x`, but `/plan$x` is not a command.
	if parsed, ok = ParseCommand("/plan-x"); !ok || parsed.Name != "plan-x" {
		t.Fatalf("hyphen name = %+v %v", parsed, ok)
	}
	if _, ok = ParseCommand("/plan$x"); ok {
		t.Fatal("a non-boundary character must reject the line")
	}
	// A separator whitespace splits the name even mid-word: `/pl an` is the
	// command `pl` with rawInput " an".
	if parsed, ok = ParseCommand("/pl an"); !ok || parsed.Name != "pl" || parsed.RawInput != " an" {
		t.Fatalf("split = %+v %v", parsed, ok)
	}
	for _, line := range range2() {
		if _, ok = ParseCommand(line); ok {
			t.Fatalf("%q must not parse as a command", line)
		}
	}
}

func range2() []string {
	return []string{"", "/", "plan", "/UPPER", "/1x", "  /plan", "/pl-an?x"}
}

func TestRegisterNormalizesLoud(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, CommandDefinition{Name: "Bad", Description: "d", Handler: okHandler}); err == nil ||
		!strings.Contains(err.Error(), `command name "Bad" must match`) {
		t.Fatalf("err = %v, want the name rejection", err)
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "ok", Description: "", Handler: okHandler}); err == nil ||
		!strings.Contains(err.Error(), `command "ok" description must be a string`) {
		t.Fatalf("err = %v, want the description rejection", err)
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "ok", Description: "   ", Handler: okHandler}); err == nil ||
		!strings.Contains(err.Error(), `command "ok" description must not be empty`) {
		t.Fatalf("err = %v, want the blank-description rejection", err)
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "ok", Description: "d", Handler: nil}); err == nil ||
		!strings.Contains(err.Error(), `command "ok" handler must be a function`) {
		t.Fatalf("err = %v, want the handler rejection", err)
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "ok", Description: "d", Handler: okHandler,
		Input: &CommandInputDescriptor{Hint: ""}}); err == nil ||
		!strings.Contains(err.Error(), `command "ok" input hint must be a string`) {
		t.Fatalf("err = %v, want the hint rejection", err)
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "ok", Description: "d", Handler: okHandler,
		Input: &CommandInputDescriptor{Hint: "  "}}); err == nil ||
		!strings.Contains(err.Error(), `command "ok" input hint must not be empty`) {
		t.Fatalf("err = %v, want the blank-hint rejection", err)
	}
}

func TestListSortedAndScopedShadowing(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	for _, name := range []string{"zeta", "alpha"} {
		if _, err := runtime.Register(nil, CommandDefinition{Name: name, Description: "global " + name, Handler: okHandler}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "global plan", Handler: okHandler}); err != nil {
		t.Fatalf("register plan: %v", err)
	}
	scopeKey := scope.NewScopeKey(nil)
	if _, err := runtime.Register(scopeKey, CommandDefinition{Name: "plan", Description: "shadow plan", Handler: okHandler}); err != nil {
		t.Fatalf("register shadow: %v", err)
	}

	got := runtime.List(scopeKey)
	if len(got) != 3 || got[0].Name != "alpha" || got[1].Name != "plan" || got[2].Name != "zeta" {
		t.Fatalf("list = %+v, want name-sorted", got)
	}
	if got[1].Description != "shadow plan" {
		t.Fatalf("shadow = %+v, want the scoped shadow", got[1])
	}
	// Another scope sees the global.
	other := runtime.List(scope.NewScopeKey(nil))
	if other[1].Description != "global plan" {
		t.Fatalf("other = %+v, want the global", other[1])
	}
	// Find resolves the shadow for the owning scope.
	if definition, ok := runtime.Find(scopeKey, "plan"); !ok || definition.Description != "shadow plan" {
		t.Fatalf("find = %+v %v", definition, ok)
	}
	// Nil scope resolves only globals.
	if definition, ok := runtime.Find(nil, "plan"); !ok || definition.Description != "global plan" {
		t.Fatalf("global find = %+v %v", definition, ok)
	}
}

func TestRegisterDuplicateDiagnostics(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler})
	if err == nil || !strings.Contains(err.Error(), `command "plan" is already registered (for a per-agent variant, mount a command-injected plugin under that agent's `+"`agent.ctx`"+`)`) {
		t.Fatalf("err = %v, want the global duplicate diagnostic", err)
	}
	scopeKey := scope.NewScopeKey(nil)
	if _, err := runtime.Register(scopeKey, CommandDefinition{Name: "plan", Description: "scoped", Handler: okHandler}); err != nil {
		t.Fatalf("scoped register: %v", err)
	}
	_, err = runtime.Register(scopeKey, CommandDefinition{Name: "plan", Description: "scoped2", Handler: okHandler})
	if err == nil || !strings.Contains(err.Error(), `command "plan" is already registered in this scope`) {
		t.Fatalf("err = %v, want the scoped duplicate diagnostic", err)
	}
}

func TestExecuteLogsLifecycleAndSettles(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	seq := int64(41)
	if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-lifecycle")
	execution, err := runtime.Execute(context.Background(), nil, sess, "/plan  keep going", nil)
	if err != nil || execution == nil {
		t.Fatalf("execute = %v %v", execution, err)
	}
	if execution.Result.Kind != ResultSuccess {
		t.Fatalf("result = %+v", execution.Result)
	}

	events := sess.Events()
	if len(events) != 2 || events[0].Type != EventCommandRun || events[1].Type != EventCommandDone {
		t.Fatalf("events = %+v", events)
	}
	var run CommandRunData
	if err := json.Unmarshal(events[0].Data, &run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.CommandID != execution.CommandID || run.Name != "plan" || run.Args == nil || *run.Args != "  keep going" {
		t.Fatalf("run = %+v, want the verbatim rawInput", run)
	}
	if run.Source.Kind != "user" {
		t.Fatalf("source = %+v, want user", run.Source)
	}
	var done CommandDoneData
	if err := json.Unmarshal(events[1].Data, &done); err != nil {
		t.Fatalf("done: %v", err)
	}
	if done.CommandID != execution.CommandID || done.Kind != ResultSuccess || done.Text != nil {
		t.Fatalf("done = %+v", done)
	}

	// A success result can point at the richer authoritative event.
	if _, err := runtime.Register(nil, CommandDefinition{Name: "rich", Description: "d", Handler: func(Invocation) (CommandResult, error) {
		return CommandResult{Kind: ResultSuccess, SourceEventSeq: &seq}, nil
	}}); err != nil {
		t.Fatalf("register rich: %v", err)
	}
	execution, err = runtime.Execute(context.Background(), nil, sess, "/rich", nil)
	if err != nil || execution == nil {
		t.Fatalf("rich execute = %v %v", execution, err)
	}
	events = sess.Events()
	var richDone CommandDoneData
	if err := json.Unmarshal(events[len(events)-1].Data, &richDone); err != nil {
		t.Fatalf("rich done: %v", err)
	}
	if richDone.SourceEventSeq == nil || *richDone.SourceEventSeq != 41 {
		t.Fatalf("rich done = %+v", richDone)
	}
}

func TestExecuteAdmissionMissesLogNothing(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-miss")
	execution, err := runtime.Execute(context.Background(), nil, sess, "not a command", nil)
	if execution != nil || err != nil {
		t.Fatalf("non-command = %v %v", execution, err)
	}
	execution, err = runtime.Execute(context.Background(), nil, sess, "/unknown", nil)
	if execution != nil || err != nil {
		t.Fatalf("unknown = %v %v", execution, err)
	}
	if len(sess.Events()) != 0 {
		t.Fatal("admission misses must log nothing")
	}
}

func TestExecuteRecordInputFalseOmitsArgs(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	off := false
	if _, err := runtime.Register(nil, CommandDefinition{Name: "quiet", Description: "d", RecordInput: &off, Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-quiet")
	if _, err := runtime.Execute(context.Background(), nil, sess, "/quiet payload", nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	var run CommandRunData
	if err := json.Unmarshal(sess.Events()[0].Data, &run); err != nil {
		t.Fatalf("run: %v", err)
	}
	if run.Args != nil {
		t.Fatalf("args = %v, want absent", *run.Args)
	}
}

func TestExecuteHandlerErrorSettlesErrorLoud(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	if _, err := runtime.Register(nil, CommandDefinition{Name: "boom", Description: "d", Handler: func(Invocation) (CommandResult, error) {
		return CommandResult{}, context.DeadlineExceeded
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-boom")
	_, err := runtime.Execute(context.Background(), nil, sess, "/boom", nil)
	if err == nil {
		t.Fatal("the handler error must propagate")
	}
	events := sess.Events()
	if len(events) != 2 || events[1].Type != EventCommandDone {
		t.Fatalf("events = %+v", events)
	}
	var done CommandDoneData
	if err := json.Unmarshal(events[1].Data, &done); err != nil {
		t.Fatalf("done: %v", err)
	}
	if done.Kind != ResultError || done.Text == nil || *done.Text != "context deadline exceeded" {
		t.Fatalf("done = %+v", done)
	}
}

func TestExecuteImageAdmission(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	// A command that does not declare images rejects them before the handler.
	if _, err := runtime.Register(nil, CommandDefinition{Name: "textonly", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-images")
	execution, err := runtime.Execute(context.Background(), nil, sess, "/textonly", []any{"img"})
	if err != nil || execution == nil {
		t.Fatalf("execute = %v %v", execution, err)
	}
	if execution.Result.Kind != ResultError || execution.Result.Text != "/textonly does not accept image attachments" {
		t.Fatalf("result = %+v", execution.Result)
	}

	// No composed attachment store: unavailable, nothing durable.
	if _, err := runtime.Register(nil, CommandDefinition{Name: "visual", Description: "d", Handler: okHandler,
		Input: &CommandInputDescriptor{Hint: "describe", Images: true}}); err != nil {
		t.Fatalf("register visual: %v", err)
	}
	execution, err = runtime.Execute(context.Background(), nil, sess, "/visual", []any{"img"})
	if err != nil || execution == nil {
		t.Fatalf("visual = %v %v", execution, err)
	}
	if execution.Result.Text != "/visual: image attachments are unavailable because no attachment store is composed" {
		t.Fatalf("result = %+v", execution.Result)
	}

	// With the store seam, the handler receives the admitted attachments.
	runtime.SetImageAdmitter(func(images []any) ([]ImageAttachment, error) {
		refs := make([]ImageAttachment, 0, len(images))
		for range images {
			refs = append(refs, ImageAttachment{Reference: "stored"})
		}
		return refs, nil
	})
	var seen []ImageAttachment
	if _, err := runtime.Register(nil, CommandDefinition{Name: "useimg", Description: "d", Handler: func(invocation Invocation) (CommandResult, error) {
		seen = invocation.Attachments
		return CommandResult{Kind: ResultSuccess}, nil
	}, Input: &CommandInputDescriptor{Hint: "describe", Images: true}}); err != nil {
		t.Fatalf("register useimg: %v", err)
	}
	if _, err := runtime.Execute(context.Background(), nil, sess, "/useimg", []any{"a", "b"}); err != nil {
		t.Fatalf("useimg: %v", err)
	}
	if len(seen) != 2 || seen[0].Reference != "stored" {
		t.Fatalf("attachments = %+v", seen)
	}
	// Each settled image rejection appended run+done; the admitted one too.
	if len(sess.Events()) != 6 {
		t.Fatalf("events = %d, want 6", len(sess.Events()))
	}
}

func TestExecuteCancelledBeforeHandler(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	entered := false
	if _, err := runtime.Register(nil, CommandDefinition{Name: "slow", Description: "d", Handler: func(Invocation) (CommandResult, error) {
		entered = true
		return CommandResult{Kind: ResultSuccess}, nil
	}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	sess := newSession(t, "cmd-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.Execute(ctx, nil, sess, "/slow", nil); err == nil {
		t.Fatal("the cancelled request must fail the execution")
	}
	if entered {
		t.Fatal("the handler must not run after cancellation")
	}
	// The abort is thrown before admission: nothing entered a handler, so
	// nothing is logged.
	if len(sess.Events()) != 0 {
		t.Fatalf("events = %+v, want none", sess.Events())
	}
}

func TestMintedIDsNeverRepeatAcrossInstances(t *testing.T) {
	seen := map[CommandID]bool{}
	for _, pair := range []struct {
		id string
	}{{"a"}, {"b"}} {
		runtime := NewCommandRuntime(cordis.Discard{})
		if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler}); err != nil {
			t.Fatalf("register: %v", err)
		}
		for i := 0; i < 2; i++ {
			sess := newSession(t, "cmd-mint-"+pair.id)
			execution, err := runtime.Execute(context.Background(), nil, sess, "/plan", nil)
			if err != nil || execution == nil {
				t.Fatalf("execute = %v %v", execution, err)
			}
			if seen[execution.CommandID] {
				t.Fatalf("id %s repeated", execution.CommandID)
			}
			seen[execution.CommandID] = true
			if !strings.HasPrefix(string(execution.CommandID), "cmd-") {
				t.Fatalf("id %s lacks the cmd- prefix", execution.CommandID)
			}
		}
	}
}

func TestChangeNotificationsAreContained(t *testing.T) {
	runtime := NewCommandRuntime(cordis.Discard{})
	changes := 0
	undo := runtime.OnChange(func() { changes++ })
	throwing := runtime.OnChange(func() { panic("listener blew up") })
	if _, err := runtime.Register(nil, CommandDefinition{Name: "plan", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if changes != 1 {
		t.Fatalf("changes = %d, want 1", changes)
	}
	throwing()
	if _, err := runtime.Register(nil, CommandDefinition{Name: "other", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register other: %v", err)
	}
	if changes != 2 {
		t.Fatalf("changes = %d, want 2 (the throwing listener is contained)", changes)
	}
	undo()
	undo2 := runtime.OnChange(func() { changes++ })
	undo2()
	if _, err := runtime.Register(nil, CommandDefinition{Name: "third", Description: "d", Handler: okHandler}); err != nil {
		t.Fatalf("register third: %v", err)
	}
	if changes != 2 {
		t.Fatalf("changes = %d, want 2 after undo", changes)
	}
}
