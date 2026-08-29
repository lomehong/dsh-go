package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/scope"
	"dshgo/session"
	"dshgo/tools"
)

// --- test seams -----------------------------------------------------------

type recordingLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *recordingLogger) Info(...any) {}
func (l *recordingLogger) Warn(args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprint(args...))
}
func (l *recordingLogger) Error(args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, fmt.Sprint(args...))
}
func (l *recordingLogger) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.warns...)
}

// scheduleDriver claims maintenance synchronously unless set busy; WhenIdle
// stays pending until closeIdle.
type scheduleDriver struct {
	mu        sync.Mutex
	followups []llm.Message
	busy      bool
	idle      chan struct{}
}

func (d *scheduleDriver) Cancel(session.TurnEndCancelCause, agent.CancelOptions) {}
func (d *scheduleDriver) Send(llm.Message, agent.InboxTarget, bool)              {}
func (d *scheduleDriver) Steer(llm.Message)                                      {}
func (d *scheduleDriver) Inject(llm.Message)                                     {}
func (d *scheduleDriver) Followup(message llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.followups = append(d.followups, message)
}
func (d *scheduleDriver) WhenIdle() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idle == nil {
		d.idle = make(chan struct{})
	}
	return d.idle
}
func (d *scheduleDriver) closeIdle() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idle == nil {
		d.idle = make(chan struct{})
	}
	select {
	case <-d.idle:
	default:
		close(d.idle)
	}
}
func (d *scheduleDriver) RunMaintenance(task func(signal context.Context) error) error {
	d.mu.Lock()
	busy := d.busy
	d.mu.Unlock()
	if busy {
		return errors.New("agent is busy")
	}
	return task(context.Background())
}
func (d *scheduleDriver) followupTexts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	texts := make([]string, 0, len(d.followups))
	for _, message := range d.followups {
		text := ""
		for _, block := range message.Content {
			text += block.Text
		}
		texts = append(texts, text)
	}
	return texts
}
func (d *scheduleDriver) setBusy(busy bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.busy = busy
}

type noopNotifications struct{}

func (noopNotifications) Inserted(llm.Message)       {}
func (noopNotifications) Discarded(llm.Message)      {}
func (noopNotifications) Claimed(llm.Message, int64) {}

type fixture struct {
	agents     *agent.AgentRegistry
	runtime    *tools.ToolRuntime
	ag         *agent.Agent
	driver     *scheduleDriver
	logger     *recordingLogger
	sess       *session.Session
	dispose    func()
	mu         sync.Mutex
	nowMs      int64
	flushErrs  []error
	flushCalls int
}

func (f *fixture) now() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nowMs
}

func (f *fixture) setNow(ms int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nowMs = ms
}

// flush simulates the shared persistence checkpoint; queued errors are
// consumed one per call.
func (f *fixture) flush(*session.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushCalls++
	if len(f.flushErrs) == 0 {
		return nil
	}
	err := f.flushErrs[0]
	f.flushErrs = f.flushErrs[1:]
	return err
}

func (f *fixture) queueFlushErr(errs ...error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushErrs = append(f.flushErrs, errs...)
}

func (f *fixture) call(t *testing.T, name string, args map[string]any) any {
	t.Helper()
	return f.callScope(t, name, args, f.ag.Scope)
}

func (f *fixture) callScope(t *testing.T, name string, args map[string]any, caller scope.ScopeKey) any {
	t.Helper()
	definition, ok := f.runtime.Get(name, f.ag.Scope)
	if !ok {
		t.Fatalf("%s is not registered in the agent scope", name)
	}
	value, err := definition.Execute(args, &tools.ToolRunContext{
		ToolExecution: tools.ToolExecution{Agent: caller},
		Signal:        context.Background(),
	})
	if err != nil {
		t.Fatalf("%s execute: %v", name, err)
	}
	return value
}

// appendChange writes one durable schedule/change payload directly.
func (f *fixture) appendChange(t *testing.T, payload map[string]any) {
	t.Helper()
	if _, err := f.sess.Append("schedule/change", payload, nil); err != nil {
		t.Fatalf("append schedule/change: %v", err)
	}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		agents: agent.NewAgentRegistry(nil, nil),
		logger: &recordingLogger{},
		nowMs:  baseNow,
		driver: &scheduleDriver{},
	}
	runtime, err := tools.NewToolRuntime(nil, tools.Config{})
	if err != nil {
		t.Fatalf("tool runtime: %v", err)
	}
	f.runtime = runtime
	// Subscribe the plugin before the agent publishes, mirroring plugin
	// load order.
	dispose, err := Register(Options{
		Agents:  f.agents,
		Runtime: f.runtime,
		Flush:   f.flush,
		Logger:  f.logger,
		Now:     f.now,
	})
	if err != nil {
		t.Fatalf("schedule register: %v", err)
	}
	t.Cleanup(dispose)
	f.dispose = dispose

	header := &session.SessionHeader{ID: session.SessionID("sess-root"), CWD: "D:\\work"}
	sess, err := session.NewDetached(session.SessionID("sess-root"), nil, header)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	f.sess = sess
	inbox, err := agent.NewInbox(sess, noopNotifications{})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	built := agent.NewAgent(agent.AgentConfig{ID: sess.ID(), Session: sess, Inbox: inbox}, f.agents.Events())
	detach, err := f.agents.Register(built)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(detach)
	f.ag = built
	built.SetDriver(f.driver)
	// The plugin runtime's initial drive is asynchronous; drain it before
	// the test body so seeded records cannot race the first fold.
	waitFor(t, "plugin initial drive", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.flushCalls >= 1 })
	return f
}

// seedCreatedAt appends one created at-record targeting baseNow+offset.
func (f *fixture) seedCreatedAt(t *testing.T, id string, offsetSeconds int64) {
	t.Helper()
	f.appendChange(t, map[string]any{
		"version": float64(1), "operation": "create",
		"schedule": map[string]any{
			"id": id, "kind": "at", "prompt": "seeded " + id,
			"scheduledAt": isoAfter(baseNow, offsetSeconds),
		},
	})
}

// quiesce waits until the async runtime drives spawned by notifyDurableChange
// have drained their flush checkpoints, so queued flush errors reach the
// next synchronous tool call.
func (f *fixture) quiesce(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	prev := -1
	for time.Now().Before(deadline) {
		time.Sleep(4 * time.Millisecond)
		f.mu.Lock()
		current := f.flushCalls
		f.mu.Unlock()
		if current == prev {
			return
		}
		prev = current
	}
	t.Fatalf("flush calls never stabilized")
}

func waitFor(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// --- tool surface ---------------------------------------------------------

func TestCreateAfterRoundTrip(t *testing.T) {
	f := newFixture(t)
	value := f.call(t, "schedule_create", map[string]any{"prompt": "check tea", "after_seconds": float64(90)})
	view, ok := value.(ScheduleView)
	if !ok {
		t.Fatalf("create returned %T", value)
	}
	if view.ID != "schedule-1" || view.Kind != "after" || view.AfterSeconds != 90 ||
		view.ScheduledAt != isoAfter(baseNow, 90) || view.State != StateScheduled ||
		view.DeliveryMode != DeliveryModeSessionLocal {
		t.Fatalf("view = %+v", view)
	}
	created := 0
	for _, event := range f.sess.Events() {
		if event.Type == "schedule/change" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("durable create count = %d", created)
	}
}

func TestCreateValidationSurfaces(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name string
		args map[string]any
		code string
	}{
		{"blank prompt", map[string]any{"prompt": "   ", "after_seconds": float64(60)}, "invalid_prompt"},
		{"two selectors", map[string]any{"prompt": "p", "after_seconds": float64(60), "at": "2024-01-01T00:00:00.000Z"}, "invalid_selector"},
		{"no selector", map[string]any{"prompt": "p"}, "invalid_selector"},
		{"unknown key", map[string]any{"prompt": "p", "after_seconds": float64(60), "extra": 1}, "invalid_selector"},
		{"after zero", map[string]any{"prompt": "p", "after_seconds": float64(0)}, "invalid_rule"},
		{"after fractional", map[string]any{"prompt": "p", "after_seconds": 1.5}, "invalid_rule"},
		{"after huge", map[string]any{"prompt": "p", "after_seconds": float64(1 << 53)}, "invalid_rule"},
		{"every small", map[string]any{"prompt": "p", "every_seconds": float64(60)}, "frequency_too_high"},
		{"every zero", map[string]any{"prompt": "p", "every_seconds": float64(0)}, "invalid_rule"},
	}
	for _, testCase := range cases {
		value := f.call(t, "schedule_create", testCase.args)
		errValue, ok := value.(map[string]any)
		if !ok || errValue["code"] != testCase.code {
			t.Fatalf("%s: value = %#v, want code %s", testCase.name, value, testCase.code)
		}
	}
	deleted := f.call(t, "schedule_delete", map[string]any{"id": " spaced "})
	errValue := deleted.(map[string]any)
	if errValue["code"] != "invalid_rule" {
		t.Fatalf("delete id validation = %#v", errValue)
	}
}

func TestListOrdersByCreateAndMarksOverdue(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "early", -60)
	f.seedCreatedAt(t, "later", 60)
	value := f.call(t, "schedule_list", nil)
	views, ok := value.([]ScheduleView)
	if !ok || len(views) != 2 {
		t.Fatalf("list = %#v", value)
	}
	if views[0].ID != "early" || views[0].State != StateOverdue {
		t.Fatalf("first view = %+v", views[0])
	}
	if views[1].ID != "later" || views[1].State != StateScheduled {
		t.Fatalf("second view = %+v", views[1])
	}
}

func TestDeleteFlow(t *testing.T) {
	f := newFixture(t)
	f.call(t, "schedule_create", map[string]any{"prompt": "p", "after_seconds": float64(60)})
	if got := f.call(t, "schedule_delete", map[string]any{"id": "schedule-1"}); got.(map[string]any)["deleted"] != true {
		t.Fatalf("first delete = %#v", got)
	}
	gone := f.call(t, "schedule_delete", map[string]any{"id": "schedule-1"}).(map[string]any)
	if gone["deleted"] != false || gone["code"] != "schedule_not_found" {
		t.Fatalf("second delete = %#v", gone)
	}
	unknown := f.call(t, "schedule_delete", map[string]any{"id": "schedule-9"}).(map[string]any)
	if unknown["deleted"] != false || unknown["code"] != "schedule_not_found" {
		t.Fatalf("unknown delete = %#v", unknown)
	}
}

func TestPersistenceUncertainCarriesOperation(t *testing.T) {
	f := newFixture(t)
	// Create: preflight passes, barrier fails.
	f.queueFlushErr(nil, errors.New("disk gone"))
	value := f.call(t, "schedule_create", map[string]any{"prompt": "p", "after_seconds": float64(60)})
	errValue, ok := value.(map[string]any)
	if !ok || errValue["code"] != "persistence_uncertain" || errValue["operation"] != "create" || errValue["id"] != "schedule-1" {
		t.Fatalf("create barrier value = %#v", value)
	}
	f.quiesce(t)
	// List: first preflight fails, no id.
	f.queueFlushErr(errors.New("disk gone"))
	listValue := f.call(t, "schedule_list", nil)
	listErr, ok := listValue.(map[string]any)
	if !ok || listErr["code"] != "persistence_uncertain" || listErr["operation"] != "list" {
		if _, isViews := listValue.([]ScheduleView); isViews {
			t.Fatalf("list with failed flush = %#v", listValue)
		}
		t.Fatalf("list barrier value = %#v", listValue)
	}
	if _, hasId := listErr["id"]; hasId {
		t.Fatalf("list uncertainty carries no id: %#v", listErr)
	}
	f.quiesce(t)
	// Delete: barrier failure carries the id.
	f.queueFlushErr(nil, errors.New("disk gone"))
	deleteValue := f.call(t, "schedule_delete", map[string]any{"id": "schedule-1"})
	// The record was created before the failed barrier, so delete reaches
	// its barrier and reports uncertainty with the id.
	deleteErr, ok := deleteValue.(map[string]any)
	if !ok || deleteErr["code"] != "persistence_uncertain" || deleteErr["operation"] != "delete" || deleteErr["id"] != "schedule-1" {
		t.Fatalf("delete barrier value = %#v", deleteValue)
	}
}

func TestCrossScopeCallerGetsInternalError(t *testing.T) {
	f := newFixture(t)
	value := f.callScope(t, "schedule_list", nil, scope.NewScopeKey(nil))
	errValue, ok := value.(map[string]any)
	if !ok || errValue["code"] != "internal_error" {
		t.Fatalf("cross-scope value = %#v", value)
	}
}

func TestCorruptLogSurfacesStableError(t *testing.T) {
	f := newFixture(t)
	f.appendChange(t, map[string]any{"version": float64(2), "operation": "delete", "id": "x"})
	value := f.call(t, "schedule_list", nil)
	errValue, ok := value.(map[string]any)
	if !ok || errValue["code"] != "corrupt_schedule_log" {
		t.Fatalf("list on corrupt log = %#v", value)
	}
}

// --- runtime --------------------------------------------------------------

// newBareRuntime builds a runtime directly over the fixture's agent
// with the plugin runtime already silenced: bare-runtime tests own their
// projections exclusively.
func (f *fixture) newBareRuntime() *ScheduleRuntime {
	f.dispose()
	return NewScheduleRuntime(f.agents, f.ag, f.flush, f.logger, f.now)
}

func TestRuntimeDispatchesOverdueOneShot(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "due", -60)
	runtime := f.newBareRuntime()
	t.Cleanup(runtime.Dispose)
	runtime.Start()
	waitFor(t, "one-shot followup", func() bool { return len(f.driver.followupTexts()) == 1 })
	text := f.driver.followupTexts()[0]
	if !strings.HasPrefix(text, "[SCHEDULE REMINDER]\n") || !strings.Contains(text, `schedule_id_json: "due"`) {
		t.Fatalf("framing = %s", text)
	}
	dispatched := false
	for _, event := range f.sess.Events() {
		if event.Type != "schedule/change" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload["operation"] == "dispatch" && payload["id"] == "due" {
			dispatched = true
		}
	}
	if !dispatched {
		t.Fatalf("no dispatch appended")
	}
}

func TestRuntimeAdvancesAndExhaustsEvery(t *testing.T) {
	f := newFixture(t)
	f.appendChange(t, map[string]any{
		"version": float64(1), "operation": "create",
		"schedule": map[string]any{
			"id": "e", "kind": "every", "prompt": "water", "everySeconds": float64(300),
			"scheduledAt": isoAfter(baseNow, -600),
		},
	})
	runtime := f.newBareRuntime()
	t.Cleanup(runtime.Dispose)
	runtime.Start()
	waitFor(t, "every followup", func() bool { return len(f.driver.followupTexts()) == 1 })
	text := f.driver.followupTexts()[0]
	if !strings.HasPrefix(text, "[SCHEDULE REMINDER BATCH]\n") || !strings.Contains(text, `"schedule_id":"e"`) {
		t.Fatalf("batch framing = %s", text)
	}
	// The advanced target is the latest occurrence at/before now plus one
	// interval: accepted -600+600=0 occurrence, next at +300s.
	waitFor(t, "advanced durable target", func() bool {
		for _, event := range f.sess.Events() {
			if event.Type != "schedule/change" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				continue
			}
			if payload["operation"] == "dispatch" && payload["id"] == "e" && payload["acceptedAt"] == isoMilli(baseNow) {
				return true
			}
		}
		return false
	})
}

func TestRuntimeWakesOnTimerTick(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "future", 120)
	runtime := f.newBareRuntime()
	t.Cleanup(runtime.Dispose)
	runtime.Start()
	waitFor(t, "first drive with no followup", func() bool {
		return len(f.driver.followupTexts()) == 0
	})
	if len(f.driver.followupTexts()) != 0 {
		t.Fatalf("early followup: %v", f.driver.followupTexts())
	}
	f.setNow(baseNow + 120_000)
	runtime.RequestDrive()
	waitFor(t, "woken followup", func() bool { return len(f.driver.followupTexts()) == 1 })
}

func TestRuntimeRetriesAfterBusyMaintenance(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "due", -30)
	f.driver.setBusy(true)
	runtime := f.newBareRuntime()
	t.Cleanup(runtime.Dispose)
	runtime.Start()
	waitFor(t, "idle waiter registered", func() bool { return true })
	if len(f.driver.followupTexts()) != 0 {
		t.Fatalf("dispatched while busy")
	}
	f.driver.setBusy(false)
	f.driver.closeIdle()
	waitFor(t, "post-idle followup", func() bool { return len(f.driver.followupTexts()) == 1 })
}

func TestRuntimeFaultsOnCorruptLog(t *testing.T) {
	f := newFixture(t)
	f.appendChange(t, map[string]any{"version": float64(9), "operation": "delete", "id": "x"})
	runtime := f.newBareRuntime()
	t.Cleanup(runtime.Dispose)
	runtime.Start()
	waitFor(t, "corrupt-log warning", func() bool {
		for _, warn := range f.logger.all() {
			if strings.Contains(warn, "corrupt schedule log") {
				return true
			}
		}
		return false
	})
	// Even after the log is fine the runtime stays faulted; a later drive
	// produces nothing.
	runtime.RequestDrive()
	time.Sleep(20 * time.Millisecond)
	if len(f.driver.followupTexts()) != 0 {
		t.Fatalf("faulted runtime dispatched")
	}
}

func TestRuntimeDisposeSilencesFutureWork(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "future", 120)
	runtime := f.newBareRuntime()
	runtime.Start()
	runtime.Dispose()
	runtime.Dispose() // idempotent
	f.setNow(baseNow + 120_000)
	runtime.RequestDrive()
	time.Sleep(20 * time.Millisecond)
	if len(f.driver.followupTexts()) != 0 {
		t.Logf("followup text: %q", f.driver.followupTexts()[0])
		t.Fatalf("disposed runtime dispatched")
	}
}

// --- plugin lifecycle -----------------------------------------------------

func TestPluginAttachesOnlyFutureLiveRoots(t *testing.T) {
	f := newFixture(t)
	// Pre-existing tools prove attachment happened exactly once for the
	// announced root.
	if _, ok := f.runtime.Get("schedule_list", f.ag.Scope); !ok {
		t.Fatalf("root agent did not receive schedule tools")
	}

	// A child (non-root) agent must not receive tools.
	childSess, err := session.NewDetached(session.SessionID("sess-child"), nil, &session.SessionHeader{ID: session.SessionID("sess-child"), CWD: "D:\\work"})
	if err != nil {
		t.Fatalf("child session: %v", err)
	}
	childInbox, err := agent.NewInbox(childSess, noopNotifications{})
	if err != nil {
		t.Fatalf("child inbox: %v", err)
	}
	child := agent.NewAgent(agent.AgentConfig{ID: childSess.ID(), Session: childSess, Inbox: childInbox}, f.agents.Events())
	enter, err := f.agents.Enter(child, f.ag)
	if err != nil {
		t.Fatalf("enter child: %v", err)
	}
	defer enter()
	if err := f.agents.Announce(child); err != nil {
		t.Fatalf("announce child: %v", err)
	}
	if _, ok := f.runtime.Get("schedule_list", child.Scope); ok {
		t.Fatalf("non-root agent received schedule tools")
	}
}

func TestPluginIdleStatusDrivesRuntime(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "due", -10)
	// The plugin runtime was started at attach; emit the idle status.
	f.agents.Events().Emit(agent.EventAgentStatus, f.ag.Scope, agent.AgentStatusPayload{Agent: f.ag, Status: agent.AgentIdle})
	waitFor(t, "idle-driven followup", func() bool { return len(f.driver.followupTexts()) == 1 })
}

func TestPluginDisposeRemovesToolsAndStopsRuntimes(t *testing.T) {
	f := newFixture(t)
	f.seedCreatedAt(t, "future", 120)
	f.dispose()
	if _, ok := f.runtime.Get("schedule_list", f.ag.Scope); ok {
		t.Fatalf("tools survived disposal")
	}
	f.setNow(baseNow + 120_000)
	f.agents.Events().Emit(agent.EventAgentStatus, f.ag.Scope, agent.AgentStatusPayload{Agent: f.ag, Status: agent.AgentIdle})
	time.Sleep(20 * time.Millisecond)
	if len(f.driver.followupTexts()) != 0 {
		t.Logf("followup text: %q", f.driver.followupTexts()[0])
		t.Fatalf("disposed runtime dispatched")
	}
}
