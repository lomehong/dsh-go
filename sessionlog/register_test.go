package sessionlog

import (
	"context"
	"encoding/json"
	"testing"

	"dshgo/llm/deepseek"
	"dshgo/session"
)

func TestRegisterDeepseekFieldPreparesAndAccepts(t *testing.T) {
	store := session.NewStore(nil)
	sess := newSession(t, "ext-1")
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("one"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("two"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.Enter(sess); err != nil {
		t.Fatalf("enter: %v", err)
	}
	registry := deepseek.NewExtensionRegistry()
	detach, err := RegisterDeepseekField(registry, store, NewFolder())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()

	// The prepared field carries the whole log bracketed by -1/-1.
	facts := deepseek.RequestFacts{
		Body:      map[string]any{"model": "deepseek-v4-pro", "stream": true},
		SessionID: "ext-1",
		Signal:    context.Background(),
	}
	prepared, err := registry.Prepare(context.Background(), facts)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	raw, err := json.Marshal(prepared.Fields[SessionLogField])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var contribution Contribution
	if err := json.Unmarshal(raw, &contribution); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if contribution.Version != 1 || contribution.AfterSeq != -1 || contribution.ThroughSeq != 1 || len(contribution.Events) != 2 {
		t.Fatalf("contribution = %+v", contribution)
	}
	if contribution.Session.ID != session.SessionID("ext-1") {
		t.Fatalf("header = %+v", contribution.Session)
	}
	// Provider mutation of the facts body must not leak.
	facts.Body["hacked"] = true
	prepared2, err := registry.Prepare(context.Background(), facts)
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if _, leaked := prepared2.Fields["hacked"]; leaked {
		t.Fatal("facts mutation leaked into prepared fields")
	}

	// The registry-level merge collides only with base fields the
	// provider did not own; dsh_session_log merges cleanly.
	if err := prepared.Accept(); err != nil {
		t.Fatalf("accept: %v", err)
	}
	// Acceptance recorded the watermark: the next request resumes after
	// the last event.
	prepared3, err := registry.Prepare(context.Background(), deepseek.RequestFacts{SessionID: "ext-1", Signal: context.Background()})
	if err != nil {
		t.Fatalf("prepare 3: %v", err)
	}
	raw3, _ := json.Marshal(prepared3.Fields[SessionLogField])
	var next Contribution
	if err := json.Unmarshal(raw3, &next); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if next.AfterSeq != 1 || len(next.Events) != 1 || next.Events[0].Type != EventTypeDeliveryAccepted {
		t.Fatalf("post-accept contribution = %+v", next)
	}
}

func TestRegisterDeepseekFieldDeclinesWithoutRequestContext(t *testing.T) {
	store := session.NewStore(nil)
	sess := newSession(t, "ext-2")
	if _, err := sess.Append(session.EventUserMessage, llmUserMessage("m"), &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := store.Enter(sess); err != nil {
		t.Fatalf("enter: %v", err)
	}
	registry := deepseek.NewExtensionRegistry()
	detach, err := RegisterDeepseekField(registry, store, NewFolder())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer detach()

	// No session id on the request: no field.
	prepared, err := registry.Prepare(context.Background(), deepseek.RequestFacts{Signal: context.Background()})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, present := prepared.Fields[SessionLogField]; present {
		t.Fatal("field prepared without a session id")
	}
	// Unknown session id: no field.
	prepared, err = registry.Prepare(context.Background(), deepseek.RequestFacts{SessionID: "ghost", Signal: context.Background()})
	if err != nil {
		t.Fatalf("prepare 2: %v", err)
	}
	if _, present := prepared.Fields[SessionLogField]; present {
		t.Fatal("field prepared for an unknown session")
	}
	// The provider still declines after the field is released.
	detach()
	prepared, err = registry.Prepare(context.Background(), deepseek.RequestFacts{SessionID: "ext-2", Signal: context.Background()})
	if err != nil {
		t.Fatalf("prepare 3: %v", err)
	}
	if _, present := prepared.Fields[SessionLogField]; present {
		t.Fatal("released field still prepares")
	}

}
