// Package schedule ports @deepseek-ai/dsh-schedule: agent-scoped durable
// one-shot and fixed-rate reminders over the session event log. The owning
// session's versioned schedule/change stream is the only durable state;
// timers, idle waiters, and tool values are disposable projections.
//
// Go adaptations, each documented at its site: ctx.sessions.flush maps to
// an optional FlushSession config func (a composition without the
// persistence coordinator has a trivially complete in-memory checkpoint);
// WeakMap transaction tails map to a keyed map guarded by a mutex; the
// invariants companion has no Go runtime (fold validation covers the same
// stream at every read boundary); Intl timezone resolution maps to
// time.LoadLocation, which does not alias-canonicalize zone names (the
// requested IANA name is kept; instant math is identical).
package schedule

// ScheduleId is a stable reminder identity that is unique and never reused
// within one session. The branded TS type is a plain string at the Go wire
// boundary.
type ScheduleId = string

// ScheduleRecord is the v1 durable reminder record union.
type ScheduleRecord interface {
	isScheduleRecord()
	// recordId reads the stable session-local id.
	recordId() ScheduleId
}

// AfterScheduleRecord is a durable one-shot reminder created from a
// positive delay.
type AfterScheduleRecord struct {
	ID           ScheduleId `json:"id"`
	Kind         string     `json:"kind"`
	Prompt       string     `json:"prompt"`
	AfterSeconds int64      `json:"afterSeconds"`
	ScheduledAt  string     `json:"scheduledAt"`
}

func (*AfterScheduleRecord) isScheduleRecord() {}

// AtScheduleRecord is a durable one-shot reminder created from an absolute
// instant.
type AtScheduleRecord struct {
	ID          ScheduleId `json:"id"`
	Kind        string     `json:"kind"`
	Prompt      string     `json:"prompt"`
	ScheduledAt string     `json:"scheduledAt"`
}

func (*AtScheduleRecord) isScheduleRecord() {}

// EveryScheduleRecord is a durable fixed-rate reminder whose next target
// remains creation-anchor-aligned.
type EveryScheduleRecord struct {
	ID           ScheduleId `json:"id"`
	Kind         string     `json:"kind"`
	Prompt       string     `json:"prompt"`
	EverySeconds int64      `json:"everySeconds"`
	ScheduledAt  string     `json:"scheduledAt"`
}

func (*EveryScheduleRecord) isScheduleRecord() {}

// OneShotScheduleRecord is a record variant that terminates on an id-only
// dispatch.
type OneShotScheduleRecord interface {
	ScheduleRecord
	isOneShotScheduleRecord()
}

func (*AfterScheduleRecord) isOneShotScheduleRecord() {}
func (*AtScheduleRecord) isOneShotScheduleRecord()    {}

// LocalAtInput is the structured local-calendar input accepted by
// schedule_create.
type LocalAtInput struct {
	Date     string `json:"date"`
	Time     string `json:"time"`
	TimeZone string `json:"time_zone"`
}

// ScheduleCreateChange creates one durable reminder record.
type ScheduleCreateChange struct {
	Version   int            `json:"version"`
	Operation string         `json:"operation"`
	Schedule  ScheduleRecord `json:"schedule"`
}

// ScheduleDeleteChange deletes one currently active reminder.
type ScheduleDeleteChange struct {
	Version   int        `json:"version"`
	Operation string     `json:"operation"`
	ID        ScheduleId `json:"id"`
}

// OneShotScheduleDispatchChange records that one active one-shot reminder
// entered the durable dispatch history.
type OneShotScheduleDispatchChange struct {
	Version   int        `json:"version"`
	Operation string     `json:"operation"`
	ID        ScheduleId `json:"id"`
}

// EveryScheduleDispatchChange records one fixed-rate decision and advances
// directly past missed occurrences.
type EveryScheduleDispatchChange struct {
	Version    int        `json:"version"`
	Operation  string     `json:"operation"`
	ID         ScheduleId `json:"id"`
	AcceptedAt string     `json:"acceptedAt"`
}

// ScheduleChange is the strict version-1 durable Schedule mutation union.
type ScheduleChange interface {
	isScheduleChange()
}

func (*ScheduleCreateChange) isScheduleChange()          {}
func (*ScheduleDeleteChange) isScheduleChange()          {}
func (*OneShotScheduleDispatchChange) isScheduleChange() {}
func (*EveryScheduleDispatchChange) isScheduleChange()   {}

// ScheduleState is the current delivery timing derived from the durable
// record and wall clock.
type ScheduleState string

// The v1 delivery-timing states.
const (
	StateScheduled ScheduleState = "scheduled"
	StateOverdue   ScheduleState = "overdue"
)

// ScheduleDeliveryMode is the fixed v1 delivery boundary: the original
// session must be live.
const DeliveryModeSessionLocal = "session-local"

// ScheduleView is the complete model-facing view of one active reminder.
type ScheduleView struct {
	// Exactly one record variant is inlined; consumers discriminate on
	// Kind.
	ID           ScheduleId    `json:"id"`
	Kind         string        `json:"kind"`
	Prompt       string        `json:"prompt"`
	AfterSeconds int64         `json:"afterSeconds,omitempty"`
	EverySeconds int64         `json:"everySeconds,omitempty"`
	ScheduledAt  string        `json:"scheduledAt"`
	State        ScheduleState `json:"state"`
	DeliveryMode string        `json:"deliveryMode"`
}
