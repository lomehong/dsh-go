// The session-reference row: canonical session mentions in direct user
// messages snapshot the referenced session's surface into the pre-step
// decision (the r129 deferred deep wiring).
package boot

import (
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/cordis/loader"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionreference"
)

// testLog surfaces composition warnings through the test output.
type testLog struct{ t *testing.T }

func (l testLog) Info(args ...any)  { l.t.Logf("info: %v", args) }
func (l testLog) Warn(args ...any)  { l.t.Logf("warn: %v", args) }
func (l testLog) Error(args ...any) { l.t.Logf("error: %v", args) }

func TestSessionReferenceRowRewritesMentions(t *testing.T) {
	home := t.TempDir()
	root := cordis.NewRoot(cordis.Discard{})
	entries := []loader.Entry{}
	for _, name := range []string{
		"typert-registry", "session", "session-projection", "session-persistence-jsonl",
		"agent", "session-query", "session-reference",
	} {
		entries = append(entries, loader.Entry{ID: name, Name: "@deepseek-ai/dsh-" + name})
	}
	app, err := Assemble(root, entries, NewCatalog(CatalogDeps{Logger: testLog{t}, Home: home}))
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	defer func() {
		if err := app.Shutdown(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	}()

	store := root.Get(ServiceSessions).(*session.Store)
	agents := root.Get(ServiceAgents).(*agent.AgentRegistry)
	t.Logf("after assemble: pre-step listeners = %d", agents.Events().WaterfallListenerCount("agent/pre-step"))

	// The referenced session: one completed conversation turn.
	referenced, err := store.Create("session-ref-target", session.CreateOptions{
		HeaderMetadata: session.SessionHeader{Version: session.SESSION_FORMAT_VERSION},
	})
	if err != nil {
		t.Fatalf("create referenced: %v", err)
	}
	if _, err := referenced.Append(session.EventUserMessage, llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "the launch is on Friday"}},
		llm.MessageSource{Kind: llm.SourceUser},
	), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
		t.Fatalf("referenced content: %v", err)
	}

	// The referencing agent with one live completed turn.
	turning, err := store.Create("session-ref-src", session.CreateOptions{
		HeaderMetadata: session.SessionHeader{Version: session.SESSION_FORMAT_VERSION},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	live := agent.NewAgent(agent.AgentConfig{
		ID: "session-ref-src", Session: turning, Ctx: root.Child(),
	}, agents.Events())
	if _, err := agents.Register(live); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	// A direct user message carrying the canonical mention.
	mention := sessionreference.FormatSessionReferenceMention(
		sessionreference.Input{SessionID: "session-ref-target", Label: "launch notes"},
	) + " summarize the referenced conversation"
	claim := llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: mention}},
		llm.MessageSource{Kind: llm.SourceUser})

	// Direct resolver probe: the exact-read seam observes the referenced
	// session and prepares the snapshot context.
	resolver := root.Get("sessionReferences").(*sessionreference.Resolver)
	mentionOnly := sessionreference.FormatSessionReferenceMention(
		sessionreference.Input{SessionID: "session-ref-target", Label: "launch notes"},
	) + " summarize the referenced conversation"
	preparedDirect, directErr := resolver.PrepareDirectMessages("session-ref-src", []llm.Message{
		llm.NewUserMessage([]llm.ContentBlock{{Type: llm.BlockText, Text: mentionOnly}},
			llm.MessageSource{Kind: llm.SourceUser}),
	})
	if directErr != nil {
		t.Fatalf("direct prepare: %v", directErr)
	}
	if len(preparedDirect) != 2 {
		t.Fatalf("direct prepare = %d messages", len(preparedDirect))
	}

	// A probe listener proves the dispatch reaches global pre-step listeners
	// and reports what the row listener produced.
	probe := false
	probeLen := -1
	undoProbe := agents.Events().PreStep().On(nil, func(payload agent.PreStepPayload, next func(agent.PreStepPayload) agent.PreStepDecision) agent.PreStepDecision {
		probe = true
		probeLen = len(payload.Messages)
		return next(payload)
	})
	defer undoProbe()

	t.Logf("dispatch bus = %p", agents.Events())
	t.Logf("dispatch bus = %p, pre-step listeners = %d", agents.Events(), agents.Events().WaterfallListenerCount("agent/pre-step"))
	decision := agents.Events().PreStep().Dispatch(live.Scope, agent.PreStepPayload{
		Agent:    live,
		Messages: []llm.Message{claim},
		Turn:     1,
		Step:     1,
	}, func(payload agent.PreStepPayload) agent.PreStepDecision {
		return agent.PreStepEnter(payload.Messages)
	})
	if decision.Kind != "enter" {
		t.Fatalf("decision = %+v", decision)
	}
	if !probe {
		t.Fatal("the global pre-step probe never fired")
	}
	t.Logf("probe saw %d messages", probeLen)
	if len(decision.Messages) != 2 {
		t.Fatalf("messages = %d, want the cleaned message plus the snapshot context", len(decision.Messages))
	}
	context := decision.Messages[1]
	if !strings.Contains(context.Content[0].Text, "Referenced sessions") {
		t.Fatalf("context = %q", context.Content[0].Text)
	}
	if !strings.Contains(context.Content[0].Text, "the launch is on Friday") {
		t.Fatalf("context = %q, want the referenced surface content", context.Content[0].Text)
	}
	if strings.Contains(decision.Messages[0].Content[0].Text, "session-ref-target") {
		t.Fatalf("cleaned content still carries the mention: %q", decision.Messages[0].Content[0].Text)
	}
}
