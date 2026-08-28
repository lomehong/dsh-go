package timecontext

import (
	"context"
	"strings"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// newLoopAgent builds one live agent whose session appends through the
// surface validator.
func newLoopAgent(t *testing.T, id string) (*agent.Agent, *session.Session) {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil, &session.SessionHeader{ID: session.SessionID(id), CWD: "D:\\tmp"})
	if err != nil {
		t.Fatalf("NewDetached: %v", err)
	}
	inbox, err := agent.NewInbox(sess, nil)
	if err != nil {
		t.Fatalf("NewInbox: %v", err)
	}
	registry := agent.NewAgentRegistry(nil, nil)
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Options: agent.AgentOptions{}, Session: sess, Inbox: inbox}, registry.Events())
	detach, err := registry.Enter(built, nil)
	if err != nil {
		t.Fatalf("Enter: %v", err)
	}
	t.Cleanup(detach)
	return built, sess
}

func rpcMessage(id string, zone string) llm.Message {
	return llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}},
		llm.MessageSource{Kind: llm.SourceUser, RPCID: id, ClientTimeZone: zone})
}

func interval(ms int64) *int64 { return &ms }

func TestValidateRefreshInterval(t *testing.T) {
	if err := ValidateRefreshInterval(nil); err != nil {
		t.Fatalf("nil interval must be legal: %v", err)
	}
	if err := ValidateRefreshInterval(interval(0)); err != nil {
		t.Fatalf("zero interval must be legal: %v", err)
	}
	err := ValidateRefreshInterval(interval(-1))
	if err == nil || !strings.Contains(err.Error(), "time-context: refreshIntervalMs must be a non-negative safe integer, got -1") {
		t.Fatalf("err = %v, want the verbatim negative rejection", err)
	}
}

func TestFormatDurationAndTimestamp(t *testing.T) {
	if got := formatDuration(0); got != "0s" {
		t.Fatalf("formatDuration(0) = %s", got)
	}
	if got := formatDuration(90061000); got != "1d 1h 1m 1s" {
		t.Fatalf("formatDuration(90061000ms) = %s", got)
	}
	if got := formatDuration(3661000); got != "1h 1m 1s" {
		t.Fatalf("formatDuration(3661000ms) = %s", got)
	}
	if got := formatDuration(65000); got != "1m 5s" {
		t.Fatalf("formatDuration(65000ms) = %s", got)
	}
	loc, err := CreateTimestampFormatter("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	got := FormatTimestamp(1767225600000, loc, "Asia/Shanghai")
	if !strings.Contains(got, "+08:00[Asia/Shanghai]") {
		t.Fatalf("timestamp = %s, want the +08:00[Asia/Shanghai] shape", got)
	}
	utc := FormatTimestamp(1767225600000, time.UTC, "UTC")
	if !strings.Contains(utc, "+00:00[UTC]") {
		t.Fatalf("utc timestamp = %s, want +00:00[UTC]", utc)
	}
	if _, err := CreateTimestampFormatter("Not/AZone"); err == nil {
		t.Fatal("an invalid IANA zone must fail")
	}
}

func TestDeriveBrowserZoneContext(t *testing.T) {
	missing, err := DeriveBrowserTimeZoneContext(nil)
	if err != nil || missing.Kind != "missing" {
		t.Fatalf("derive = %+v err = %v", missing, err)
	}
	resolved, err := DeriveBrowserTimeZoneContext([]llm.Message{
		rpcMessage("r2", "Asia/Shanghai"),
		{Source: llm.MessageSource{Kind: llm.SourcePlugin, Plugin: Name}},
		{Source: llm.MessageSource{Kind: llm.SourceUser}},
	})
	if err != nil || resolved.Kind != "resolved" || resolved.TimeZone != "Asia/Shanghai" {
		t.Fatalf("derive = %+v err = %v (non-rpc user messages carry no zone)", resolved, err)
	}
	resolvedUTC, err := DeriveBrowserTimeZoneContext([]llm.Message{rpcMessage("r1", "UTC")})
	if err != nil || resolvedUTC.Kind != "resolved" || resolvedUTC.TimeZone != "UTC" {
		t.Fatalf("derive = %+v err = %v", resolvedUTC, err)
	}
	mixed, err := DeriveBrowserTimeZoneContext([]llm.Message{
		rpcMessage("r1", "Europe/Paris"),
		rpcMessage("r2", "Asia/Shanghai"),
	})
	if err != nil || mixed.Kind != "mixed" || strings.Join(mixed.TimeZones, ",") != "Asia/Shanghai,Europe/Paris" {
		t.Fatalf("derive = %+v err = %v", mixed, err)
	}
	dup, err := DeriveBrowserTimeZoneContext([]llm.Message{rpcMessage("r1", "UTC"), rpcMessage("r2", "UTC")})
	if err != nil || dup.Kind != "resolved" {
		t.Fatalf("derive = %+v err = %v", dup, err)
	}
	for _, zone := range []string{"asia/shanghai", "Not/A Zone!", "Mars/Olympus"} {
		if _, err := DeriveBrowserTimeZoneContext([]llm.Message{rpcMessage("r1", zone)}); err == nil {
			t.Fatalf("zone %q must be rejected", zone)
		}
	}
	if got := RenderBrowserTimeZoneContext(BrowserTimeZoneContext{Kind: "resolved", TimeZone: "UTC"}); got != "Browser time zone for this request: UTC. Interpret otherwise-unqualified dates and times in this zone." {
		t.Fatalf("resolved render = %q", got)
	}
	if got := RenderBrowserTimeZoneContext(BrowserTimeZoneContext{Kind: "mixed", TimeZones: []string{"A", "B"}}); got != `Browser time zone for this request: mixed ["A","B"]. Ask the user to clarify otherwise-unqualified dates and times.` {
		t.Fatalf("mixed render = %q", got)
	}
	if got := RenderBrowserTimeZoneContext(BrowserTimeZoneContext{Kind: "missing"}); got != "Browser time zone for this request: unavailable. Ask the user to clarify otherwise-unqualified dates and times." {
		t.Fatalf("missing render = %q", got)
	}
}

func runPreStep(registry *agent.AgentRegistry, agentObj *agent.Agent, proposed []llm.Message, turn int64, step int64) agent.PreStepDecision {
	return registry.Events().Waterfall(agent.EventPreStep, nil, agent.PreStepPayload{
		Agent: agentObj, Messages: proposed, Turn: turn, Step: step, Signal: context.Background(),
	}, func(payload any) any { return agent.PreStepEnter(proposed) }).(agent.PreStepDecision)
}

func TestPreStepInjectsDurableReadings(t *testing.T) {
	agentObj, sess := newLoopAgent(t, "tz-agent")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(agentObj, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	undo, err := Register(cordis.NewRoot(cordis.Discard{}), registry, Config{TimeZone: "Asia/Shanghai"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(undo)

	proposed := []llm.Message{rpcMessage("r1", "Asia/Shanghai")}
	decision := runPreStep(registry, agentObj, proposed, 1, 1)
	if decision.Kind != "enter" || len(decision.Messages) != 2 {
		t.Fatalf("decision = %+v, want enter with the reading appended", decision)
	}
	reading := decision.Messages[1]
	if reading.Source.Kind != llm.SourcePlugin || reading.Source.Plugin != Name || reading.Source.Form != llm.FormSnapshot {
		t.Fatalf("reading source = %+v", reading.Source)
	}
	if len(reading.Source.Sections) != 1 || reading.Source.Sections[0].Name != Name || reading.Source.Sections[0].Text != reading.Content[0].Text {
		t.Fatalf("reading sections = %+v", reading.Source.Sections)
	}
	text := reading.Content[0].Text
	for _, want := range []string{
		"Time sampled while preparing turn 1, step 1: ",
		"[Asia/Shanghai]",
		"Browser time zone for this request: Asia/Shanghai. Interpret otherwise-unqualified dates and times in this zone.",
		"Elapsed since the preceding model-visible message: unavailable.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("reading text %q missing %q", text, want)
		}
	}

	// The loop admits the proposed messages: append them so later scans see
	// the reading as an in-turn durable injection.
	if _, err := sess.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("turn/start: %v", err)
	}
	injectedTime := int64(0)
	for _, message := range decision.Messages {
		event, err := sess.Append(session.EventUserMessage, message, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: "append"}})
		if err != nil {
			t.Fatalf("append reading: %v", err)
		}
		if message.Source.Plugin == Name {
			injectedTime = event.Time
		}
	}
	if injectedTime == 0 {
		t.Fatal("the reading must be a logged user/message")
	}

	// With a 60s gate — replacing the gateless listener, one config per
	// plugin instance — the immediate next step is suppressed.
	undo()
	undo2, err := Register(cordis.NewRoot(cordis.Discard{}), registry, Config{TimeZone: "Asia/Shanghai", RefreshIntervalMs: interval(60000)})
	if err != nil {
		t.Fatalf("Register 2: %v", err)
	}
	t.Cleanup(undo2)
	decision2 := runPreStep(registry, agentObj, nil, 1, 2)
	if len(decision2.Messages) != 0 {
		t.Fatalf("refresh gate must suppress, got %d appends", len(decision2.Messages))
	}

	// Advancing the clock past the gate reopens injection; the in-turn
	// elapsed baseline is the previous plugin reading.
	nowMillis = func() int64 { return injectedTime + 61000 }
	t.Cleanup(func() { nowMillis = func() int64 { return time.Now().UnixMilli() } })
	decision3 := runPreStep(registry, agentObj, nil, 1, 3)
	if len(decision3.Messages) != 1 {
		t.Fatalf("decision = %+v, want the reading re-injected", decision3)
	}
	if !strings.Contains(decision3.Messages[0].Content[0].Text, "Elapsed since the preceding step context: 1m 1s.") {
		t.Fatalf("reading text = %q, want the in-turn elapsed baseline", decision3.Messages[0].Content[0].Text)
	}
}

func TestRejectAndPoisonedZoneShortCircuit(t *testing.T) {
	agentObj, _ := newLoopAgent(t, "tz-reject")
	registry := agent.NewAgentRegistry(nil, nil)
	if _, err := registry.Enter(agentObj, nil); err != nil {
		t.Fatalf("Enter: %v", err)
	}
	undo, err := Register(cordis.NewRoot(cordis.Discard{}), registry, Config{TimeZone: "UTC"})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(undo)
	decision := runPreStep(registry, agentObj, nil, 2, 1)
	_ = decision
	rejected := registry.Events().Waterfall(agent.EventPreStep, nil, agent.PreStepPayload{
		Agent: agentObj, Messages: nil, Turn: 2, Step: 1, Signal: context.Background(),
	}, func(payload any) any { return agent.PreStepReject() }).(agent.PreStepDecision)
	if rejected.Kind != "reject" {
		t.Fatalf("decision = %+v, want reject passthrough", rejected)
	}
	poisoned := []llm.Message{llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}},
		llm.MessageSource{Kind: llm.SourceUser, RPCID: "r1", ClientTimeZone: "Mars/Olympus"})}
	decision2 := registry.Events().Waterfall(agent.EventPreStep, nil, agent.PreStepPayload{
		Agent: agentObj, Messages: poisoned, Turn: 2, Step: 1, Signal: context.Background(),
	}, func(payload any) any { return agent.PreStepEnter(poisoned) }).(agent.PreStepDecision)
	if decision2.Kind != "reject" {
		t.Fatalf("decision = %+v, want the poisoned-zone step rejection", decision2)
	}
}

func TestRegisterValidationFailsLoud(t *testing.T) {
	if _, err := Register(cordis.NewRoot(cordis.Discard{}), agent.NewAgentRegistry(nil, nil), Config{TimeZone: "UTC", RefreshIntervalMs: interval(-5)}); err == nil ||
		!strings.Contains(err.Error(), "refreshIntervalMs must be a non-negative safe integer") {
		t.Fatalf("err = %v, want the interval rejection", err)
	}
	if _, err := Register(cordis.NewRoot(cordis.Discard{}), agent.NewAgentRegistry(nil, nil), Config{TimeZone: "Not/AZone"}); err == nil ||
		!strings.Contains(err.Error(), `time-context: invalid IANA timeZone "Not/AZone"`) {
		t.Fatalf("err = %v, want the zone rejection", err)
	}
}
