// Disposable live timer projection for one exact root agent.
package schedule

import (
	"context"
	"fmt"
	"sync"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// MAX_TIMER_DELAY_MS is the largest delay a single timer segment represents
// without clamping.
const MAX_TIMER_DELAY_MS = int64(2_147_483_647)

// everyDue carries one complete fixed-rate batch entry.
type everyDue struct {
	record       *EveryScheduleRecord
	occurrenceAt string
}

// dueDecision selects one due one-shot, one complete fixed-rate batch, or
// the next wake.
type dueDecision struct {
	kind       string // "one-shot" | "every" | "wait"
	oneShot    OneShotScheduleRecord
	reminders  []everyDue
	acceptedAt string
	target     int64 // "wait" only; zero means none
}

func decideDue(folded *FoldedSchedules, now int64) (dueDecision, error) {
	type indexedRecord struct {
		record ScheduleRecord
		index  int
	}
	indexed := make([]indexedRecord, 0, len(folded.Active))
	for index, record := range folded.Active {
		indexed = append(indexed, indexedRecord{record: record, index: index})
	}
	targetOf := func(record ScheduleRecord) int64 {
		var scheduledAt string
		switch typed := record.(type) {
		case *AfterScheduleRecord:
			scheduledAt = typed.ScheduledAt
		case *AtScheduleRecord:
			scheduledAt = typed.ScheduledAt
		case *EveryScheduleRecord:
			scheduledAt = typed.ScheduledAt
		}
		epoch, err := parseInstantMs(scheduledAt)
		if err != nil {
			return 0
		}
		return epoch
	}
	byTargetThenCreate := func(left, right indexedRecord) bool {
		leftTarget, rightTarget := targetOf(left.record), targetOf(right.record)
		if leftTarget != rightTarget {
			return leftTarget < rightTarget
		}
		return left.index < right.index
	}

	oneShots := []indexedRecord{}
	for _, entry := range indexed {
		if _, isEvery := entry.record.(*EveryScheduleRecord); isEvery {
			continue
		}
		if targetOf(entry.record) <= now {
			oneShots = append(oneShots, entry)
		}
	}
	if len(oneShots) > 0 {
		sortByTarget(oneShots, byTargetThenCreate)
		return dueDecision{kind: "one-shot", oneShot: oneShots[0].record.(OneShotScheduleRecord)}, nil
	}

	dues := []indexedRecord{}
	for _, entry := range indexed {
		if every, isEvery := entry.record.(*EveryScheduleRecord); isEvery && targetOf(every) <= now {
			dues = append(dues, entry)
		}
	}
	if len(dues) > 0 {
		sortByTarget(dues, byTargetThenCreate)
		decision := dueDecision{kind: "every", acceptedAt: isoMilli(now)}
		for _, entry := range dues {
			record := entry.record.(*EveryScheduleRecord)
			occurrence, err := ResolveEveryOccurrence(record, now)
			if err != nil {
				return dueDecision{}, err
			}
			decision.reminders = append(decision.reminders, everyDue{record: record, occurrenceAt: occurrence.OccurrenceAt})
		}
		return decision, nil
	}

	target := int64(0)
	for _, entry := range indexed {
		candidate := targetOf(entry.record)
		if candidate > now && (target == 0 || candidate < target) {
			target = candidate
		}
	}
	return dueDecision{kind: "wait", target: target}, nil
}

// sortByTarget orders entries by target then create order.
func sortByTarget[T any](values []T, less func(T, T) bool) {
	for outer := 1; outer < len(values); outer++ {
		for inner := outer; inner > 0 && less(values[inner], values[inner-1]); inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}

// ScheduleRuntime is one process-local, disposable projection of an exact
// agent's durable schedules. All mutable state is guarded by mu; the run
// goroutine drains coalesced triggers serially.
type ScheduleRuntime struct {
	agents *agent.AgentRegistry
	ag     *agent.Agent
	// flush checkpoints the session's live prefix (the official shared
	// persistence barrier); nil means the composition has no persistence
	// coordinator and the checkpoint is trivially complete.
	flush  func(*session.Session) error
	logger cordis.Logger
	nowFn  func() int64

	mu            sync.Mutex
	timer         *time.Timer
	requested     bool
	stopping      bool
	faulted       bool
	runActive     bool
	idleListening bool
	stopCh        chan struct{}
	settled       sync.WaitGroup
}

// NewScheduleRuntime constructs an inactive runtime; Start begins the first
// preflight.
func NewScheduleRuntime(agents *agent.AgentRegistry, ag *agent.Agent, flush func(*session.Session) error, logger cordis.Logger, nowFn func() int64) *ScheduleRuntime {
	if logger == nil {
		logger = cordis.Discard{}
	}
	if nowFn == nil {
		nowFn = func() int64 { return time.Now().UnixMilli() }
	}
	return &ScheduleRuntime{
		agents: agents,
		ag:     ag,
		flush:  flush,
		logger: logger,
		nowFn:  nowFn,
		stopCh: make(chan struct{}),
	}
}

// Start begins the initial durability preflight and timer derivation.
func (r *ScheduleRuntime) Start() { r.RequestDrive() }

// RequestDrive recomputes the live projection after a committed mutation or
// idle transition.
func (r *ScheduleRuntime) RequestDrive() {
	r.mu.Lock()
	if r.stopping || r.faulted {
		r.mu.Unlock()
		return
	}
	r.clearTimerLocked()
	r.requested = true
	if r.runActive {
		r.mu.Unlock()
		return
	}
	r.runActive = true
	r.settled.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.settled.Done()
		r.runRequested()
	}()
}

// runRequested drains coalesced triggers serially.
func (r *ScheduleRuntime) runRequested() {
	for {
		r.mu.Lock()
		if !r.requested || r.stopping || r.faulted {
			r.runActive = false
			restart := r.requested && !r.stopping && !r.faulted
			r.requested = false
			r.mu.Unlock()
			if restart {
				r.RequestDrive()
			}
			return
		}
		r.requested = false
		r.mu.Unlock()
		_, _ = RunScheduleTransaction(r.ag, func() (struct{}, error) {
			r.driveOnce()
			return struct{}{}, nil
		})
	}
}

// Dispose stops future work, cancels timers, and awaits every outstanding
// runtime goroutine.
func (r *ScheduleRuntime) Dispose() {
	r.mu.Lock()
	if r.stopping {
		r.mu.Unlock()
		r.settled.Wait()
		return
	}
	r.stopping = true
	r.requested = false
	r.clearTimerLocked()
	close(r.stopCh)
	r.mu.Unlock()
	r.settled.Wait()
}

// isLive reports whether this exact root lifecycle remains authoritative.
func (r *ScheduleRuntime) isLive() bool {
	if r.agents.Get(r.ag.ID) != r.ag {
		return false
	}
	for _, root := range r.agents.Roots() {
		if root == r.ag {
			return true
		}
	}
	return false
}

// isRunnable reports whether this runtime may start or continue Schedule
// work.
func (r *ScheduleRuntime) isRunnable() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.stopping && r.isLive()
}

// clearTimerLocked cancels the currently armed timer, if any.
func (r *ScheduleRuntime) clearTimerLocked() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
}

// arm locks in one bounded timer segment; every wake rechecks the wall
// clock.
func (r *ScheduleRuntime) arm(target int64, now int64) {
	delay := target - now
	if delay > MAX_TIMER_DELAY_MS {
		delay = MAX_TIMER_DELAY_MS
	}
	if delay < 0 {
		delay = 0
	}
	r.mu.Lock()
	r.clearTimerLocked()
	r.timer = time.AfterFunc(time.Duration(delay)*time.Millisecond, func() {
		r.mu.Lock()
		r.timer = nil
		r.mu.Unlock()
		r.RequestDrive()
	})
	r.mu.Unlock()
}

// waitForIdle awaits one public idle boundary without holding admission or
// creating a retry timer.
func (r *ScheduleRuntime) waitForIdle() {
	r.mu.Lock()
	if r.idleListening || r.stopping {
		r.mu.Unlock()
		return
	}
	r.idleListening = true
	r.settled.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.settled.Done()
		select {
		case <-r.ag.Driver().WhenIdle():
			r.mu.Lock()
			r.idleListening = false
			r.mu.Unlock()
			r.RequestDrive()
		case <-r.stopCh:
			r.mu.Lock()
			r.idleListening = false
			r.mu.Unlock()
		}
	}()
}

// readFolded folds the current exact runtime suffix and contains a corrupt
// durable stream.
func (r *ScheduleRuntime) readFolded() *FoldedSchedules {
	seedLength := int64(0)
	if r.ag.Session.Header().IsSeeded {
		seedLength = int64(r.ag.Session.Header().InheritedEventCount)
	}
	folded, err := FoldScheduleEvents(r.ag.Session.Events(), seedLength)
	if err != nil {
		r.mu.Lock()
		r.faulted = true
		r.mu.Unlock()
		detail := err.Error()
		if logErr, ok := err.(*ScheduleLogError); ok {
			detail = logErr.message
		}
		r.logger.Warn(fmt.Sprintf("schedule: corrupt schedule log for agent %q: %s", r.ag.ID, detail))
		return nil
	}
	return folded
}

// decide contains an invalid wall-clock decision without permanently
// faulting this runtime.
func (r *ScheduleRuntime) decide(folded *FoldedSchedules, now int64) *dueDecision {
	decision, err := decideDue(folded, now)
	if err != nil {
		r.logger.Warn(fmt.Sprintf("schedule: fixed-rate decision failed for agent %q: %s", r.ag.ID, err.Error()))
		return nil
	}
	return &decision
}

// driveOnce prefights, folds, arms, or dispatches the next one-shot or
// fixed-rate batch.
func (r *ScheduleRuntime) driveOnce() {
	r.mu.Lock()
	r.clearTimerLocked()
	r.mu.Unlock()
	if !r.isRunnable() {
		return
	}
	if r.flush != nil {
		if err := r.flush(r.ag.Session); err != nil {
			if r.isLive() {
				r.logger.Warn(fmt.Sprintf("schedule: preflight failed for agent %q: %s", r.ag.ID, err.Error()))
			}
			return
		}
	}
	if !r.isRunnable() {
		return
	}

	folded := r.readFolded()
	if folded == nil {
		return
	}
	wakeNow := r.nowFn()
	wakeDecision := r.decide(folded, wakeNow)
	if wakeDecision == nil {
		return
	}
	if wakeDecision.kind == "wait" {
		if wakeDecision.target != 0 {
			r.arm(wakeDecision.target, wakeNow)
		}
		return
	}

	// Claim the idle maintenance phase; RunMaintenance fails synchronously
	// only while another agent activity owns it.
	claimed := false
	maintenanceErr := r.ag.Driver().RunMaintenance(func(signal context.Context) error {
		claimed = r.runMaintenanceStep()
		return nil
	})
	if maintenanceErr != nil {
		if r.isLive() {
			r.waitForIdle()
		}
		return
	}
	if !claimed {
		return
	}
	if r.flush != nil {
		if err := r.flush(r.ag.Session); err != nil {
			if r.isLive() {
				r.logger.Warn(fmt.Sprintf("schedule: dispatch barrier failed for agent %q: %s", r.ag.ID, err.Error()))
			}
			return
		}
	}
	if r.isRunnable() {
		r.RequestDrive()
	}
}

// runMaintenanceStep rechecks the wall clock and exact live owner inside
// the claimed maintenance phase, frames and queues the reminder, then
// appends the dispatch records. A synchronous framing or enqueue failure
// appends no dispatch; a later model failure does not roll one back.
func (r *ScheduleRuntime) runMaintenanceStep() bool {
	if !r.isRunnable() {
		return false
	}
	claimedFolded := r.readFolded()
	if claimedFolded == nil {
		return false
	}
	decisionNow := r.nowFn()
	decision := r.decide(claimedFolded, decisionNow)
	if decision == nil {
		return false
	}
	if decision.kind == "wait" {
		if decision.target != 0 {
			r.arm(decision.target, decisionNow)
		}
		return false
	}
	var text string
	if decision.kind == "one-shot" {
		text = RenderReminderFraming(decision.oneShot)
	} else {
		text = RenderEveryReminderBatchFraming(everyReminders(decision.reminders))
	}
	message := llm.NewUserMessage(
		[]llm.ContentBlock{{Type: llm.BlockText, Text: text}},
		llm.MessageSource{Kind: llm.SourcePlugin, Plugin: "schedule"},
	)
	r.ag.Driver().Followup(message)
	fail := func(err error) {
		r.mu.Lock()
		r.faulted = true
		r.clearTimerLocked()
		r.mu.Unlock()
		r.logger.Warn(fmt.Sprintf("schedule: dispatch append failed for agent %q: %s", r.ag.ID, err.Error()))
	}
	if decision.kind == "one-shot" {
		if err := r.appendDispatch(decision.oneShot.recordId(), ""); err != nil {
			fail(err)
			return false
		}
	} else {
		for _, reminder := range decision.reminders {
			if err := r.appendDispatch(reminder.record.ID, decision.acceptedAt); err != nil {
				fail(err)
				return false
			}
		}
	}
	return true
}

// appendDispatch appends one durable dispatch record.
func (r *ScheduleRuntime) appendDispatch(id ScheduleId, acceptedAt string) error {
	payload := map[string]any{"version": SCHEDULE_CHANGE_VERSION, "operation": "dispatch", "id": id}
	if acceptedAt != "" {
		payload["acceptedAt"] = acceptedAt
	}
	_, err := r.ag.Session.Append("schedule/change", payload, nil)
	return err
}

// everyReminders adapts the internal due entries to the framing input.
func everyReminders(dues []everyDue) []EveryReminder {
	out := make([]EveryReminder, 0, len(dues))
	for _, due := range dues {
		out = append(out, EveryReminder{Record: due.record, OccurrenceAt: due.occurrenceAt})
	}
	return out
}
