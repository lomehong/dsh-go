// Package sessiontelemetry ports packages/session/session-telemetry: the
// Service Definition owning the CAPTURE side of session-event reporting —
// which records exist (the chunk projection), what they carry (the logical
// record), when they are captured (adoption, the per-append firehose,
// lifecycle forwarding), and live versus on-demand canonical-log capture.
// Everything downstream of emit (batching, retry, queueing, loss policy)
// is the reporting SDK's territory and is deliberately not modelled here.
package sessiontelemetry

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/session"
)

// Severity of a telemetry record, pre-mapped at capture so a receiver can
// alert with zero configuration: error for events whose own outcome flag
// says so (the tool-result block's isError, turn/end error reasons) and for
// agent-error operational records. Captured events otherwise default to
// info; warn remains available to record policies and backends.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// Record is one logical record handed to a backend — the capture contract's
// whole outbound vocabulary. Ledger records mirror session-log events
// one-to-one; operational records (channel ops) carry the two signals with
// no log home (agent-error, shutdown) and deliberately omit event-seq-style
// identity so they can never be mistaken for ledger rows.
type Record struct {
	// Channel is ledger (session-log mirror) or ops (operational signal);
	// backends keep the two under separate instrumentation scopes.
	Channel string
	// Time is Unix epoch milliseconds — the source event's append time for
	// ledger records, the emission time for ops records.
	Time int64
	// Severity is the pre-mapped alerting severity.
	Severity Severity
	// Attributes are the minimal identity attributes.
	Attributes map[string]any
	// Body is the complete payload: a deep copy of the session event's data
	// for ledger records, or the op payload for ops records. Never mutated
	// after handoff.
	Body any
}

// Sink is the minimum backend contract the coordinator requires.
type Sink interface {
	// Emit hands one record to the backend's pipeline. MUST be a
	// non-blocking enqueue — the coordinator calls this synchronously from
	// the session/event hot path. Errors thrown here are contained by the
	// coordinator and logged; they never reach the loop.
	Emit(record Record)
	// Flush is the optional hint that a turn ended (fire-and-forget).
	Flush()
	// Shutdown forwards fiber disposal to the SDK: flush queued records and
	// reach quiescence. A failure is logged as a warning and never fails
	// application teardown.
	Shutdown() error
}

// Backend is the loadable form of the sink contract: one implementation per
// context. A backend composes a Coordinator in its constructor to install
// the capture side.
type Backend interface {
	Sink
	// Sharing is the deployment-selected session-sharing policy disclosed
	// to human-facing acknowledgement surfaces.
	Sharing() string
}

// SharingStatus is the disclosure vocabulary for SessionTelemetryBackend.
type SharingStatus string

const (
	SharingFull         SharingStatus = "full"
	SharingFeedbackOnly SharingStatus = "feedback-only"
	SharingDisabled     SharingStatus = "disabled"
)

// Capture is whether capture follows live events or reads the canonical log
// only when requested.
type Capture string

const (
	CaptureLive     Capture = "live"
	CaptureOnDemand Capture = "on-demand"
)

// Coordinator installs the telemetry capture side onto a context for one
// backend. Live capture subscribes to the session store lifecycle hooks plus
// the agent/error relay; on-demand capture reads the canonical log only when
// requested. Every synchronous handler is self-contained so a failing
// backend can never starve other subscribers or touch the agent loop.
type Coordinator struct {
	store   *session.Store
	agents  *agent.AgentRegistry
	logger  cordis.Logger
	backend Sink

	// adopted sessions keyed by pointer; double-adoption is a no-op.
	mu      sync.Mutex
	adopted map[*session.Session]struct{}
	// handoff cursor: per session, the highest seq handed to a backend.
	cursor map[*session.Session]int64
	// chunkSeen: per session, the turn:step keys whose first chunk shipped.
	chunkSeen map[*session.Session]map[string]struct{}

	// recordWaterfall applies the session-telemetry/record redaction rules
	// (none mounted = pass-through). Called on the capture hot path inside
	// containment.
	recordWaterfall func(Record) Record
}

// NewCoordinator builds and installs the capture side for live mode. The
// waterfall applies the session-telemetry/record redaction rules (none
// mounted = pass-through).
func NewCoordinator(store *session.Store, agents *agent.AgentRegistry, logger cordis.Logger, backend Sink, waterfall func(Record) Record) *Coordinator {
	return newCoordinator(store, agents, logger, backend, waterfall, CaptureLive)
}

// NewOnDemandCoordinator builds a coordinator that registers none of the
// continuous listeners; CaptureSession reads the canonical log explicitly
// and never creates operational records.
func NewOnDemandCoordinator(store *session.Store, logger cordis.Logger, backend Sink, waterfall func(Record) Record) *Coordinator {
	return newCoordinator(store, nil, logger, backend, waterfall, CaptureOnDemand)
}

// newCoordinator implements both capture modes.
func newCoordinator(store *session.Store, agents *agent.AgentRegistry, logger cordis.Logger, backend Sink, waterfall func(Record) Record, capture Capture) *Coordinator {
	if logger == nil {
		logger = cordis.Discard{}
	}
	if waterfall == nil {
		waterfall = func(record Record) Record { return record }
	}
	c := &Coordinator{
		store:           store,
		agents:          agents,
		logger:          logger,
		backend:         backend,
		adopted:         map[*session.Session]struct{}{},
		cursor:          map[*session.Session]int64{},
		chunkSeen:       map[*session.Session]map[string]struct{}{},
		recordWaterfall: waterfall,
	}
	if capture != CaptureLive {
		return c
	}
	store.OnCreated(func(sess *session.Session) error {
		c.adopt(sess)
		return nil
	})
	store.OnDisposed(func(sess *session.Session) {
		c.contain(func() {
			if !c.retire(sess) {
				return
			}
			c.deliver(sess, projectedRecord{record: c.redact(shutdownRecord(sess))})
		})
	})
	store.OnEvent(func(sess *session.Session, event session.Event) {
		c.contain(func() { c.captureEvent(sess, event) })
	})
	store.OnFlush(func(sess *session.Session) error {
		c.contain(func() { c.hintFlush(sess) })
		return nil
	})
	if agents != nil {
		_ = agents.Events().OnEmit(agent.EventAgentError, nil, func(payload any) error {
			if typed, ok := payload.(agent.AgentErrorPayload); ok {
				c.contain(func() { c.relayAgentError(typed) })
			}
			return nil
		})
	}
	for _, id := range store.List() {
		if sess := store.Get(id); sess != nil {
			c.adopt(sess)
		}
	}
	return c
}

// retire drops one session from the adopted set; false when not adopted.
func (c *Coordinator) retire(sess *session.Session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.adopted[sess]; !ok {
		return false
	}
	delete(c.adopted, sess)
	return true
}

// adopt replays the session's log THROUGH the projection from the handoff
// cursor, then relies on the firehose. A second adoption is a no-op.
func (c *Coordinator) adopt(sess *session.Session) {
	c.mu.Lock()
	if _, ok := c.adopted[sess]; ok {
		c.mu.Unlock()
		return
	}
	c.adopted[sess] = struct{}{}
	c.mu.Unlock()
	c.CaptureSession(sess, -1)
}

// CaptureSession projects and hands over the canonical session-log suffix
// after the handoff cursor, optionally stopping at an inclusive sequence
// boundary. Redaction runs during this call, so an on-demand caller retains
// no copied records before requesting capture. Backend and policy failures
// remain contained per event. Exported for the on-demand capture mode.
func (c *Coordinator) CaptureSession(sess *session.Session, throughSeq int64) {
	c.mu.Lock()
	cursor, ok := c.cursor[sess]
	if !ok {
		// Constructor seeds never publish on the firehose; their content
		// already left the process under another identity (resume/fork).
		cursor = firstLiveSeq(sess) - 1
	}
	c.mu.Unlock()
	for _, event := range sess.Events() {
		if throughSeq >= 0 && event.Seq > throughSeq {
			break
		}
		c.contain(func() {
			if event.Seq <= cursor {
				c.track(sess, event)
			} else {
				c.captureEvent(sess, event)
			}
		})
	}
}

// firstLiveSeq is the session's first non-seed event seq (or 0).
func firstLiveSeq(sess *session.Session) int64 {
	if sess.Header().SeedLength != nil {
		return *sess.Header().SeedLength
	}
	return 0
}

// track feeds the chunk projection without handing off — the cursor half of
// re-adoption.
func (c *Coordinator) track(sess *session.Session, event session.Event) {
	if event.Type == session.EventAssistantChunk {
		key := fmt.Sprintf("%d:%d", chunkTurn(event), chunkStep(event))
		c.seen(sess)[key] = struct{}{}
	}
}

// captureEvent projects, redacts, and hands one event to the backend.
func (c *Coordinator) captureEvent(sess *session.Session, event session.Event) {
	if event.Type == session.EventAssistantChunk {
		key := fmt.Sprintf("%d:%d", chunkTurn(event), chunkStep(event))
		seen := c.seen(sess)
		// Fixed chunk projection: only the first chunk of each (turn, step)
		// ships — the stream-started signal; content is byte-complete in the
		// step's assembled assistant/message. Dropped chunks do not advance
		// the cursor, so re-adoption re-drops them deterministically.
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
	}
	c.deliver(sess, projectedRecord{
		record: c.redact(Record{
			Channel:    "ledger",
			Time:       event.Time,
			Severity:   severityOf(event),
			Attributes: identityOf(sess, event),
			Body:       deepClone(event.Data),
		}),
		seq: event.Seq,
	})
}

// redact runs the record waterfall at capture time; a throwing rule
// withholds the record instead of reaching the loop (fail-closed).
func (c *Coordinator) redact(record Record) Record {
	return c.recordWaterfall(record)
}

// deliver hands one redacted record to the backend, then advances its
// ledger cursor.
func (c *Coordinator) deliver(sess *session.Session, pending projectedRecord) {
	c.backend.Emit(pending.record)
	if pending.seq >= 0 {
		c.mu.Lock()
		c.cursor[sess] = pending.seq
		c.mu.Unlock()
	}
}

// hintFlush forwards the turn-end boundary to the backend's optional flush
// hint.
func (c *Coordinator) hintFlush(sess *session.Session) {
	c.mu.Lock()
	_, adopted := c.adopted[sess]
	c.mu.Unlock()
	if adopted {
		c.backend.Flush()
	}
}

// relayAgentError relays one agent/error bus emission as an agent-error
// operational record.
func (c *Coordinator) relayAgentError(payload agent.AgentErrorPayload) {
	sess := payload.Agent.Session
	c.deliver(sess, projectedRecord{
		record: c.redact(Record{
			Channel:  "ops",
			Time:     time.Now().UnixMilli(),
			Severity: SeverityError,
			Attributes: map[string]any{
				"telemetry.op": "agent-error",
				"session.id":   string(sess.ID()),
				"agent.id":     payload.Agent.ID,
				"error.name":   errorName(payload.Error),
				"turn":         payload.Turn,
				"step":         payload.Step,
			},
			Body: errorDetail(payload.Error),
		}),
	})
}

// seen lazily creates the per-session first-chunk tracking set.
func (c *Coordinator) seen(sess *session.Session) map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	set, ok := c.chunkSeen[sess]
	if !ok {
		set = map[string]struct{}{}
		c.chunkSeen[sess] = set
	}
	return set
}

// contain runs one capture-side step with its exception contained.
func (c *Coordinator) contain(step func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.logger.Warn(fmt.Sprintf("telemetry: capture step failed: %v", recovered))
		}
	}()
	step()
}

// Shutdown captures shutdown markers for live-adopted sessions, then awaits
// the backend's shutdown; a failure there warns instead of throwing.
func (c *Coordinator) Shutdown() {
	c.mu.Lock()
	sessions := make([]*session.Session, 0, len(c.adopted))
	for sess := range c.adopted {
		sessions = append(sessions, sess)
	}
	c.mu.Unlock()
	for _, sess := range sessions {
		c.contain(func() { c.deliver(sess, projectedRecord{record: c.redact(shutdownRecord(sess))}) })
	}
	if err := c.backend.Shutdown(); err != nil {
		c.logger.Warn(fmt.Sprintf("telemetry: backend shutdown failed: %v", err))
	}
}

// projectedRecord is one record ready for backend handoff with its optional
// ledger cursor.
type projectedRecord struct {
	record Record
	seq    int64 // -1 = no cursor advance
}

// shutdownRecord builds the per-session clean-exit marker.
func shutdownRecord(sess *session.Session) Record {
	return Record{
		Channel:  "ops",
		Time:     time.Now().UnixMilli(),
		Severity: SeverityInfo,
		Attributes: map[string]any{
			"telemetry.op": "shutdown",
			"session.id":   string(sess.ID()),
		},
		Body: map[string]any{"op": "shutdown"},
	}
}

// severityOf maps an event's own outcome flag to the pre-baked severity.
func severityOf(event session.Event) Severity {
	switch event.Type {
	case session.EventToolResult:
		var data session.ToolResultData
		if err := json.Unmarshal(event.Data, &data); err == nil {
			for _, block := range data.Message.Content {
				if block.IsError {
					return SeverityError
				}
			}
		}
		return SeverityInfo
	case session.EventTurnEnd:
		var data session.TurnEndData
		if err := json.Unmarshal(event.Data, &data); err == nil {
			if data.Reason.Kind == "error" {
				return SeverityError
			}
		}
		return SeverityInfo
	default:
		// Merge-extensible fall-through: event types this coordinator does
		// not depend on pass through as info.
		return SeverityInfo
	}
}

// errorDetail normalizes the arbitrary thrown value into the stable
// operational-record shape.
func errorDetail(err error) map[string]any {
	if err == nil {
		return map[string]any{"name": "unknown", "message": "unknown"}
	}
	return map[string]any{"name": errorName(err), "message": err.Error()}
}

func errorName(err error) string {
	if named, ok := err.(interface{ Name() string }); ok {
		return named.Name()
	}
	return "Error"
}

// identityOf builds the minimal identity attributes.
func identityOf(sess *session.Session, event session.Event) map[string]any {
	attributes := map[string]any{
		"session.id": string(sess.ID()),
		"event.type": event.Type,
		"event.seq":  event.Seq,
	}
	header := sess.Header()
	if header.CWD != "" {
		attributes["session.cwd"] = header.CWD
	}
	if header.ParentSession != "" {
		attributes["session.parent_id"] = string(header.ParentSession)
	}
	if header.SeedLength != nil {
		attributes["session.seed_length"] = *header.SeedLength
	}
	return attributes
}

// deepClone makes an independent copy of a JSON-serializable value.
func deepClone(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return value
	}
	return decoded
}

// chunkTurn and chunkStep read the assistant/chunk payload's coordinates.
func chunkTurn(event session.Event) int64 {
	var data struct {
		Turn int64 `json:"turn"`
	}
	if err := json.Unmarshal(event.Data, &data); err == nil {
		return data.Turn
	}
	return 0
}

func chunkStep(event session.Event) int64 {
	var data struct {
		Step int64 `json:"step"`
	}
	if err := json.Unmarshal(event.Data, &data); err == nil {
		return data.Step
	}
	return 0
}
