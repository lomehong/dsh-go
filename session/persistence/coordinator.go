// Backend-agnostic session write-path orchestration: buffering,
// serialization, adoption, repair, and disposal sequencing shared by
// first-party backends. Port of coordinator.ts. The official cordis
// listener wiring (`ctx.on('session/created'|'session/event'|'session/
// flush'|'session/disposed')`) maps onto the exported AttachLive/
// NotifySessionEvent/FlushSession/RetireSession/Dispose methods; the boot
// phase registers those as cordis effects in the same reverse-dispose
// order the official installWritePath uses.
package persistence

import (
	"fmt"
	"sync"

	"dshgo/session"
)

// Re-exported defaults (official coordinator constants).
const (
	// DefaultPreparedSessionCacheSize is the default number of detached
	// session preparations retained by a coordinator.
	DefaultPreparedSessionCacheSize = 5
	// DefaultWriteBatchMaxDelay is the default maximum intentional wait
	// before a live session batch starts writing.
	DefaultWriteBatchMaxDelayMs = 200
	// MaxWriteBatchDelayMs is the largest write batching delay accepted by
	// the scheduler (Node MAX_TIMER_DELAY_MS parity).
	MaxWriteBatchDelayMs = 1<<31 - 1
)

// CoordinatorOptions is the coordinator policy supplied by a concrete
// persistence backend.
type CoordinatorOptions struct {
	// PreparedSessionCacheSize is the maximum completed unpublished
	// preparations retained for reuse.
	PreparedSessionCacheSize int
	// WriteBatchMaxDelayMs is the maximum intentional batching wait after
	// an idle live queue receives work.
	WriteBatchMaxDelayMs int64
}

// Inspection is an immutable logical session view: validated metadata plus
// the contiguous logical event log.
type Inspection struct {
	Meta   session.SessionHeader `json:"meta"`
	Events []session.Event       `json:"events"`
}

// sessionState is the per-session write state in the coordinator's
// in-memory bookkeeping.
type sessionState struct {
	meta session.SessionHeader
	// cursor is the next seq the backend expects to append (the stored log
	// length).
	cursor int64
	// materialized reports whether lazy creation has produced a durable
	// artifact.
	materialized bool
	// owner is the live Session this state was bound to via the write path,
	// if any; a non-nil owner lets a second, unrelated session on the same
	// id be rejected as a collision instead of silently no-opped.
	owner *session.Session
}

// liveSessionState is one live session's initialization and bounded
// write-behind controller. A placeholder state additionally carries the
// resolved controller in initLive, published strictly before initDone is
// closed, so every waiter reads the same result without re-reading the map
// (a retirement may remove the entry concurrently).
type liveSessionState struct {
	initDone chan struct{}
	initLive *liveSessionState
	initErr  error
	writes   *SessionWriteBehind
}

// retirement is one disposed lifecycle's final drain.
type retirement struct {
	done chan struct{}
	err  error
}

// Logger observes background failures without failing producers.
type Logger interface {
	Warn(message string)
}

// Sessions is the coordinator's view of the live session registry (the
// official `ctx.sessions` consumer seam). A nil registry builds
// unpublished sessions directly for preparation.
type Sessions interface {
	// Get resolves one live session by id.
	Get(id session.SessionID) (*session.Session, bool)
	// List enumerates live sessions (HMR re-seed).
	List() []*session.Session
	// Prepare builds the exact unpublished Session for resume.
	Prepare(id session.SessionID, seed []session.Event, meta session.SessionHeader) (*session.Session, error)
}

// HeaderMaterializer is the optional backend hook behind
// EnsureMaterialized: durably create an empty header-only session artifact.
type HeaderMaterializer interface {
	MaterializeHeader(meta session.SessionHeader) error
}

// Coordinator owns the backend-agnostic session write-path orchestration.
// All per-id operations are serialized (a per-id lock) so concurrent
// flushes and a flush racing a load never interleave storage writes.
type Coordinator struct {
	backend  Backend
	sessions Sessions
	logger   Logger
	preps    *preparations

	writeBatchMaxDelayMs int64

	mu          sync.Mutex
	states      map[session.SessionID]*sessionState
	live        map[*session.Session]*liveSessionState
	retirements map[session.SessionID]*retirement
	chains      map[session.SessionID]*sync.Mutex
}

// NewCoordinator validates the policy and builds one coordinator. The
// caller wires the write-path methods into its lifecycle events AFTER this
// returns (official: the disposer registers before the listeners).
func NewCoordinator(backend Backend, sessions Sessions, logger Logger, options CoordinatorOptions) (*Coordinator, error) {
	if options.PreparedSessionCacheSize <= 0 {
		options.PreparedSessionCacheSize = DefaultPreparedSessionCacheSize
	}
	if options.WriteBatchMaxDelayMs == 0 {
		options.WriteBatchMaxDelayMs = DefaultWriteBatchMaxDelayMs
	}
	if options.WriteBatchMaxDelayMs < 1 || options.WriteBatchMaxDelayMs > MaxWriteBatchDelayMs {
		return nil, fmt.Errorf("writeBatchMaxDelayMs must be an integer between 1 and %d", MaxWriteBatchDelayMs)
	}
	return &Coordinator{
		backend:              backend,
		sessions:             sessions,
		logger:               logger,
		preps:                newPreparations(options.PreparedSessionCacheSize),
		writeBatchMaxDelayMs: options.WriteBatchMaxDelayMs,
		states:               map[session.SessionID]*sessionState{},
		live:                 map[*session.Session]*liveSessionState{},
		retirements:          map[session.SessionID]*retirement{},
		chains:               map[session.SessionID]*sync.Mutex{},
	}, nil
}

// --- per-id serialization -------------------------------------------------

// serialize runs op after any in-flight operation for the same session id,
// so writes for one session never interleave. Errors do not poison the
// chain (each operation takes the mutex fresh). Serialized public methods
// must NOT call each other (deadlock); they call the unserialized *Core
// helpers instead. The per-id mutex lives for the coordinator's lifetime:
// one entry per distinct id is bounded by the id set.
func (c *Coordinator) serialize(id session.SessionID, op func() error) error {
	c.mu.Lock()
	mu := c.chains[id]
	if mu == nil {
		mu = &sync.Mutex{}
		c.chains[id] = mu
	}
	c.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	return op()
}

// --- public API (the backend's service methods delegate here) -------------

// Create registers detached session metadata for lazy creation on the
// first append; duplicate tracked or persisted ids reject.
func (c *Coordinator) Create(meta session.SessionHeader) error {
	return c.serialize(meta.ID, func() error { return c.createCore(meta) })
}

func (c *Coordinator) createCore(meta session.SessionHeader) error {
	// Do NOT clobber an existing session: the SessionID IS the identity.
	if c.states[meta.ID] != nil || c.preps.has(meta.ID) {
		return fmt.Errorf("session %q already exists in this backend", meta.ID)
	}
	// A persisted artifact under this id (in ANY scope) blocks creation:
	// load/resume identify a session by id alone, so a second artifact
	// would make resume nondeterministic.
	stored, err := c.backend.LoadStored(meta.ID)
	if err != nil {
		return err
	}
	if stored != nil {
		return fmt.Errorf("session %q already has a persisted log on disk; load/resume it instead of creating", meta.ID)
	}
	// Pure lazy: record intent only. No artifact until the first append.
	c.states[meta.ID] = &sessionState{meta: meta}
	return nil
}

// EnsureMaterialized materializes one exact live session without inventing
// a session event.
func (c *Coordinator) EnsureMaterialized(sess *session.Session) error {
	if err := c.FlushSession(sess); err != nil {
		return err
	}
	return c.serialize(sess.ID(), func() error {
		state := c.states[sess.ID()]
		if state == nil {
			return fmt.Errorf("session %q is not registered for persistence", sess.ID())
		}
		if state.materialized {
			return nil
		}
		materializer, ok := c.backend.(HeaderMaterializer)
		if !ok {
			return fmt.Errorf("session persistence backend cannot materialize an empty session")
		}
		if err := materializer.MaterializeHeader(state.meta); err != nil {
			return err
		}
		state.materialized = true
		c.preps.invalidate(sess.ID())
		return nil
	})
}

// Append durably persists a batch of events, honoring the append-only and
// contiguous-seq contracts. The batch is deep-copied here, in one
// traversal, before the op waits behind the per-session chain: the checked
// value is exactly the value persisted.
func (c *Coordinator) Append(id session.SessionID, events []session.Event) error {
	batch := make([]session.Event, len(events))
	for i, event := range events {
		batch[i] = session.DeepCopyEvent(event)
	}
	return c.serialize(id, func() error { return c.appendCore(id, batch) })
}

func (c *Coordinator) appendCore(id session.SessionID, events []session.Event) error {
	// Every append route converges here: the public service, live
	// write-behind drains, and seed/suffix adoption. Legacy-shape rejection
	// stays at this shared boundary. The unknown-type guard is deliberately
	// read-side only: an append-time refusal would stall a live session's
	// durability mid-flight, which costs more than a loud refusal at the
	// log's next load.
	if err := assertSupportedEvents(events, id); err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	if err := c.preps.assertWritable(id); err != nil {
		return err
	}
	state := c.states[id]
	if state == nil {
		adopted, err := c.adopt(id)
		if err != nil {
			return err
		}
		state = adopted
	}
	// Contiguity: each event's seq must continue the stored log.
	for i, event := range events {
		if event.Seq != state.cursor+int64(i) {
			return fmt.Errorf("append seq mismatch for %q: expected %d at index %d, got %d", id, state.cursor+int64(i), i, event.Seq)
		}
	}
	if err := c.backend.AppendBatch(state.meta, events, state.materialized); err != nil {
		return err
	}
	// The durable write is the transaction: mark materialized + advance the
	// cursor as soon as it commits (uniform across backends).
	state.materialized = true
	state.cursor += int64(len(events))
	c.preps.invalidate(id)
	return nil
}

// Prepare reserves the exact unpublished Session used by resume.
// Revision retries converge once the durable log remains unchanged for one
// read/check round trip. Release on the returned preparation publishes or
// rolls back the reservation.
func (c *Coordinator) Prepare(id session.SessionID) (*Preparation, error) {
	for {
		if err := c.waitForRetirement(id); err != nil {
			return nil, err
		}
		if _, isLive := c.liveByID(id); isLive {
			return nil, fmt.Errorf("cannot prepare session %q while it is live", id)
		}
		res, err := c.preps.reserve(
			id,
			func() (*preparedSource, error) { return c.serializedPrepareCore(id) },
			func(source *preparedSource) (*committedPreparation, error) { return c.serializedCommitPrepared(source) },
		)
		if err != nil {
			return nil, err
		}
		if res == nil {
			continue
		}
		if _, isLive := c.liveByID(id); isLive {
			c.preps.release(res, false)
			return nil, fmt.Errorf("cannot prepare session %q while it is live", id)
		}
		prep := &Preparation{
			Session:    res.source.sess,
			Inspection: res.source.inspection,
			Revision:   res.source.revision,
			TornMarker: res.source.tornMarker,
			Closers:    res.source.closers,
			res:        res,
			c:          c,
		}
		return prep, nil
	}
}

// Preparation is the owned result of a cold resume preparation. Release
// consumes the reservation: reusable=true returns the unpublished Session
// to the coordinator's ready cache for a later AttachLive claim.
type Preparation struct {
	Session    *session.Session
	Inspection Inspection
	Revision   Revision
	TornMarker any
	Closers    []session.Event

	res *reservation
	c   *Coordinator
}

// Release consumes the reservation after publication or rollback. The
// source stays reusable for resume only while it remains unpublished and
// complete (official release formula).
func (p *Preparation) Release(published bool) {
	reusable := !published &&
		p.res.state.owner == nil &&
		int64(len(p.res.source.inspection.Events)) == p.res.source.sessionLength
	p.c.preps.release(p.res, reusable)
}

// Load commits recovery and returns its immutable logical view without
// publication.
func (c *Coordinator) Load(id session.SessionID) (Inspection, error) {
	for {
		if err := c.waitForRetirement(id); err != nil {
			return Inspection{}, err
		}
		if live, isLive := c.liveByID(id); isLive {
			return c.loadLiveSnapshot(live)
		}
		res, err := c.preps.reserve(
			id,
			func() (*preparedSource, error) { return c.serializedPrepareCore(id) },
			func(source *preparedSource) (*committedPreparation, error) { return c.serializedCommitPrepared(source) },
		)
		if err != nil {
			return Inspection{}, err
		}
		if res == nil {
			continue
		}
		if attached, isLive := c.liveByID(id); isLive {
			c.preps.discard(res)
			return c.loadLiveSnapshot(attached)
		}
		c.preps.discard(res)
		return res.source.inspection, nil
	}
}

// Inspect reads a logical session without publishing it or committing
// recovery. A stale ready source is reloaded; a source already committing
// or reserved for resume stays exclusive, and inspection may borrow its
// immutable view.
func (c *Coordinator) Inspect(id session.SessionID) (Inspection, error) {
	for {
		if c.hasRetirement(id) {
			if err := c.waitForRetirement(id); err != nil {
				return Inspection{}, err
			}
		}
		if live, isLive := c.liveByID(id); isLive {
			return inspectLive(live), nil
		}
		source, err := c.preps.inspect(
			id,
			func() (*preparedSource, error) { return c.serializedPrepareCore(id) },
		)
		if err != nil {
			if attached, isLive := c.liveByID(id); isLive {
				return inspectLive(attached), nil
			}
			return Inspection{}, err
		}
		if attached, isLive := c.liveByID(id); isLive {
			return inspectLive(attached), nil
		}
		current, err := c.serializedIsPreparedSourceCurrent(source)
		if err != nil {
			if attached, isLive := c.liveByID(id); isLive {
				return inspectLive(attached), nil
			}
			return Inspection{}, err
		}
		if _, isLive := c.liveByID(id); isLive {
			return inspectLive(c.mustLiveByID(id)), nil
		}
		if current {
			return source.inspection, nil
		}
		if c.preps.discardReady(id, source) == "retained" {
			return source.inspection, nil
		}
	}
}

// ReadFrom reads the stored events from fromSeq onward, detached and
// non-mutating. A seek-capable backend reads only the suffix; every other
// backend reads its stored prefix and skips forward here.
func (c *Coordinator) ReadFrom(id session.SessionID, fromSeq int64) (Inspection, error) {
	if fromSeq < 0 {
		return Inspection{}, fmt.Errorf("readFrom fromSeq must be a non-negative safe integer, got %d", fromSeq)
	}
	if err := c.waitForRetirement(id); err != nil {
		return Inspection{}, err
	}
	var inspection Inspection
	err := c.serialize(id, func() error {
		if suffixReader, ok := c.backend.(SuffixReader); ok {
			suffix, err := suffixReader.ReadStoredFrom(id, fromSeq)
			if err != nil {
				return err
			}
			if suffix == nil {
				return &NotFoundError{SessionID: id}
			}
			if err := c.assertStoredId(id, suffix.Meta); err != nil {
				return err
			}
			if err := c.assertVersion(suffix.Meta); err != nil {
				return err
			}
			for _, event := range suffix.Events {
				if needsLegacyPrefix(event) {
					whole, err := c.readStoredPrefix(id)
					if err != nil {
						return err
					}
					inspection = Inspection{Meta: whole.Meta, Events: filterFromSeq(whole.Events, fromSeq)}
					return nil
				}
			}
			events, err := normalizeStoredEvents(suffix.Events, id)
			if err != nil {
				return err
			}
			if err := c.assertEventsSupported(suffix.Meta, events); err != nil {
				return err
			}
			inspection = Inspection{Meta: session.DeepCopyHeader(suffix.Meta), Events: events}
			return nil
		}
		whole, err := c.readStoredPrefix(id)
		if err != nil {
			return err
		}
		// Sequential fallback: contiguous seqs from 0 make the suffix an
		// index slice.
		inspection = Inspection{Meta: whole.Meta, Events: sliceFromSeq(whole.Events, fromSeq)}
		return nil
	})
	if err != nil {
		return Inspection{}, err
	}
	return inspection, nil
}

// readStoredPrefix reads one detached physical prefix without logical
// recovery or caching.
func (c *Coordinator) readStoredPrefix(id session.SessionID) (Inspection, error) {
	stored, err := c.backend.LoadStored(id)
	if err != nil {
		return Inspection{}, err
	}
	if stored == nil {
		return Inspection{}, &NotFoundError{SessionID: id}
	}
	if err := c.assertStoredId(id, stored.Meta); err != nil {
		return Inspection{}, err
	}
	if err := c.assertVersion(stored.Meta); err != nil {
		return Inspection{}, err
	}
	events, err := normalizeStoredEvents(stored.Events, id)
	if err != nil {
		return Inspection{}, err
	}
	if err := c.assertEventsSupported(stored.Meta, events); err != nil {
		return Inspection{}, err
	}
	return Inspection{Meta: session.DeepCopyHeader(stored.Meta), Events: events}, nil
}

func filterFromSeq(events []session.Event, fromSeq int64) []session.Event {
	out := make([]session.Event, 0, len(events))
	for _, event := range events {
		if event.Seq >= fromSeq {
			out = append(out, event)
		}
	}
	return out
}

// sliceFromSeq indexes a contiguous-from-0 log; out-of-range yields empty.
func sliceFromSeq(events []session.Event, fromSeq int64) []session.Event {
	if fromSeq >= int64(len(events)) {
		return nil
	}
	if fromSeq <= 0 {
		return events
	}
	return events[fromSeq:]
}

// List passes through to the backend (a direct read needing no
// coordinator state).
func (c *Coordinator) List() ([]session.SessionHeader, error) { return c.backend.List() }

// ListSnapshots passes through to the backend.
func (c *Coordinator) ListSnapshots() ([]Snapshot, error) { return c.backend.ListSnapshots() }

// BorrowedSession is a borrowed exact Session source returned from a cold
// materialization or a concurrent live owner. Release pins/unpins the
// prepared source against the ready cache.
type BorrowedSession struct {
	// Source is "prepared" (a reusable unpublished Session is pinned until
	// Release) or "live" (a live Session won source resolution).
	Source          string
	Inspection      Inspection
	Revision        Revision
	PreparedSession *session.Session

	release func()
}

// Release ends the observation.
func (bs *BorrowedSession) Release() {
	if bs.release != nil {
		bs.release()
		bs.release = nil
	}
}

// BorrowSession borrows one exact logical view while pinning its reusable
// prepared Session.
func (c *Coordinator) BorrowSession(id session.SessionID) (*BorrowedSession, error) {
	for {
		if c.hasRetirement(id) {
			if err := c.waitForRetirement(id); err != nil {
				return nil, err
			}
		}
		if live, isLive := c.liveByID(id); isLive {
			return &BorrowedSession{Source: "live", Inspection: inspectLive(live)}, nil
		}
		source, unpin, err := c.preps.borrow(
			id,
			func() (*preparedSource, error) { return c.serializedPrepareCore(id) },
		)
		if err != nil {
			if attached, isLive := c.liveByID(id); isLive {
				return &BorrowedSession{Source: "live", Inspection: inspectLive(attached)}, nil
			}
			return nil, err
		}
		if attached, isLive := c.liveByID(id); isLive {
			unpin()
			return &BorrowedSession{Source: "live", Inspection: inspectLive(attached)}, nil
		}
		current, err := c.serializedIsPreparedSourceCurrent(source)
		if err != nil {
			unpin()
			if attached, isLive := c.liveByID(id); isLive {
				return &BorrowedSession{Source: "live", Inspection: inspectLive(attached)}, nil
			}
			return nil, err
		}
		if _, isLive := c.liveByID(id); isLive {
			unpin()
			return &BorrowedSession{Source: "live", Inspection: inspectLive(c.mustLiveByID(id))}, nil
		}
		if current || c.preps.discardReady(id, source) == "retained" {
			return &BorrowedSession{
				Source:          "prepared",
				Inspection:      source.inspection,
				Revision:        source.revision,
				PreparedSession: source.sess,
				release:         unpin,
			}, nil
		}
		unpin()
	}
}

// --- cold preparation + commit --------------------------------------------

// serializedPrepareCore wraps prepareCore in the id's serialization chain.
func (c *Coordinator) serializedPrepareCore(id session.SessionID) (*preparedSource, error) {
	var source *preparedSource
	err := c.serialize(id, func() error {
		var innerErr error
		source, innerErr = c.prepareCore(id)
		return innerErr
	})
	return source, err
}

// readPrepareCore reads, repairs in memory, validates, and freezes one
// cold source once.
func (c *Coordinator) prepareCore(id session.SessionID) (*preparedSource, error) {
	stored, err := c.backend.LoadStored(id)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, &NotFoundError{SessionID: id}
	}
	source, err := c.buildPreparedSource(id, stored)
	if err != nil {
		// An unsupported format is a refusal over an intact log, not
		// damage — surface it unwrapped so callers can point at the raw
		// artifact.
		var unsupported *FormatUnsupportedError
		if asFormatUnsupported(err, &unsupported) {
			return nil, unsupported
		}
		return nil, &CorruptionError{
			Message: fmt.Sprintf("stored session %q failed validation: %v", id, err),
			Cause:   err,
		}
	}
	return source, nil
}

func (c *Coordinator) buildPreparedSource(id session.SessionID, stored *StoredPrefix) (*preparedSource, error) {
	meta := stored.Meta
	if err := c.assertStoredId(id, meta); err != nil {
		return nil, err
	}
	if err := c.assertVersion(meta); err != nil {
		return nil, err
	}
	storedEvents, err := normalizeStoredEvents(stored.Events, id)
	if err != nil {
		return nil, err
	}
	if err := c.assertEventsSupported(meta, storedEvents); err != nil {
		return nil, err
	}
	// Preserve complete interrupted events and synthesize only missing
	// closers.
	closers := session.InterruptedTurnClosers(storedEvents)
	balanced := append(append([]session.Event{}, storedEvents...), closers...)
	var sess *session.Session
	var buildErr error
	if c.sessions != nil {
		sess, buildErr = c.sessions.Prepare(id, balanced, meta)
	} else {
		sess, buildErr = session.NewRestored(id, balanced, meta)
	}
	if buildErr != nil {
		return nil, buildErr
	}
	return &preparedSource{
		inspection:    Inspection{Meta: sess.Header(), Events: balanced},
		sess:          sess,
		revision:      stored.Revision,
		sessionLength: int64(len(sess.Events())),
		tornMarker:    stored.TornMarker,
		closers:       closers,
	}, nil
}

// serializedCommitPrepared wraps commitPrepared in the id's serialization
// chain.
func (c *Coordinator) serializedCommitPrepared(source *preparedSource) (*committedPreparation, error) {
	var committed *committedPreparation
	err := c.serialize(source.inspection.Meta.ID, func() error {
		var innerErr error
		committed, innerErr = c.commitPrepared(source)
		return innerErr
	})
	return committed, err
}

// commitPrepared commits one prepared repair and establishes its
// ownerless durable cursor. committed == nil means the durable revision
// moved and the caller must retry from a fresh read.
func (c *Coordinator) commitPrepared(source *preparedSource) (*committedPreparation, error) {
	id := source.inspection.Meta.ID
	cursor := int64(len(source.inspection.Events))
	existing := c.states[id]
	if existing != nil && existing.owner != nil {
		return nil, fmt.Errorf("session %q already has a live persistence owner", id)
	}
	current, err := c.isPreparedSourceCurrent(source)
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, nil
	}
	if source.tornMarker != nil || len(source.closers) > 0 {
		if err := c.backend.CommitRepair(source.inspection.Meta, source.tornMarker, source.closers); err != nil {
			return nil, err
		}
		// The repair changed the durable revision. Reload the exact
		// committed graph instead of associating the old in-memory view
		// with a newer revision.
		return nil, nil
	}
	state := existing
	if state == nil {
		state = &sessionState{meta: source.inspection.Meta, cursor: cursor, materialized: true}
	}
	state.meta = source.inspection.Meta
	state.cursor = cursor
	state.materialized = true
	c.states[id] = state
	return &committedPreparation{source: source, state: state}, nil
}

// isPreparedSourceCurrent reports whether one cached source still names
// the current durable log revision.
func (c *Coordinator) isPreparedSourceCurrent(source *preparedSource) (bool, error) {
	revision, err := c.backend.ReadStoredRevision(source.inspection.Meta.ID)
	if err != nil {
		return false, err
	}
	return revision == source.revision, nil
}

func (c *Coordinator) serializedIsPreparedSourceCurrent(source *preparedSource) (bool, error) {
	var current bool
	err := c.serialize(source.inspection.Meta.ID, func() error {
		var innerErr error
		current, innerErr = c.isPreparedSourceCurrent(source)
		return innerErr
	})
	return current, err
}

// adopt builds a state for a session discovered in storage but not yet in
// memory. It runs inside the id's serialization chain.
func (c *Coordinator) adopt(id session.SessionID) (*sessionState, error) {
	for {
		source := c.preps.takeReady(id)
		if source == nil {
			var err error
			source, err = c.prepareCore(id)
			if err != nil {
				return nil, err
			}
		}
		committed, err := c.commitPrepared(source)
		if err != nil {
			return nil, err
		}
		if committed != nil {
			return committed.state, nil
		}
	}
}

// --- validation helpers -----------------------------------------------------

func (c *Coordinator) assertVersion(meta session.SessionHeader) error {
	if meta.Version == session.SESSION_FORMAT_VERSION {
		return nil
	}
	return c.unsupported(meta, SessionFormatVersionRefusal(meta.ID, meta.Version))
}

// assertEventsSupported refuses a log containing an event type this build
// does not know, unless the event carries the envelope's ignorable marker:
// an unrecognized ignorable event is purely informational and its loss
// cannot affect reconstruction, so the reader may skip it; an unrecognized
// required event could change how the rest of the log is interpreted and is
// refused (fail-closed). Runs on NORMALIZED events, after legacy shapes this
// build still reads were upgraded and the unreadable ones kept their
// specific diagnostics.
func (c *Coordinator) assertEventsSupported(meta session.SessionHeader, events []session.Event) error {
	for _, event := range events {
		if session.KnownEventType(event.Type) {
			continue
		}
		if event.Ignorable {
			continue
		}
		return c.unsupported(meta, fmt.Sprintf("session %q contains event type %q (seq %d) unknown to this harness; refusing to interpret the log — it was likely written by a newer harness", meta.ID, event.Type, event.Seq))
	}
	return nil
}

// unsupported builds a format refusal that points at the raw artifact when
// the backend has one.
func (c *Coordinator) unsupported(meta session.SessionHeader, reason string) error {
	var location *Location
	if locator, ok := c.backend.(ArtifactLocator); ok {
		location = locator.Locate(meta)
	}
	if location != nil {
		reason = fmt.Sprintf("%s (raw log: %s)", reason, location.Path)
	}
	return &FormatUnsupportedError{Message: reason, Location: location}
}

// assertStoredId rejects backend metadata that is not bound to the
// requested session id.
func (c *Coordinator) assertStoredId(id session.SessionID, meta session.SessionHeader) error {
	if meta.ID != id {
		return fmt.Errorf("stored session identity mismatch: requested %q, header contains %q", id, meta.ID)
	}
	return nil
}

// asFormatUnsupported is a type assertion helper keeping errors.As-free
// (the error is always produced unwrapped by this package).
func asFormatUnsupported(err error, target **FormatUnsupportedError) bool {
	if e, ok := err.(*FormatUnsupportedError); ok {
		*target = e
		return true
	}
	return false
}

// inspectLive borrows one immutable view from an already-live Session.
func inspectLive(sess *session.Session) Inspection {
	return Inspection{Meta: sess.Header(), Events: sess.Events()}
}

// loadLiveSnapshot returns one durable immutable view of an already-live
// Session.
func (c *Coordinator) loadLiveSnapshot(sess *session.Session) (Inspection, error) {
	events := sess.Events()
	if err := c.FlushSession(sess); err != nil {
		return Inspection{}, err
	}
	c.mu.Lock()
	state := c.states[sess.ID()]
	c.mu.Unlock()
	if state == nil {
		return Inspection{}, fmt.Errorf("session %q lost persistence state during load", sess.ID())
	}
	if len(events) == 0 && !state.materialized {
		return Inspection{}, fmt.Errorf("session %q not found", sess.ID())
	}
	if len(session.InterruptedTurnClosers(events)) > 0 {
		return Inspection{}, fmt.Errorf("cannot load session %q while its live turn is open; use the live Session or wait for the turn to close", sess.ID())
	}
	return Inspection{Meta: state.meta, Events: events}, nil
}

// --- live registry + retirement helpers -------------------------------------

func (c *Coordinator) liveByID(id session.SessionID) (*session.Session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sess := range c.live {
		if sess.ID() == id {
			return sess, true
		}
	}
	return nil, false
}

func (c *Coordinator) mustLiveByID(id session.SessionID) *session.Session {
	sess, _ := c.liveByID(id)
	return sess
}

func (c *Coordinator) hasRetirement(id session.SessionID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retirements[id] != nil
}

// waitForRetirement awaits one retiring lifecycle.
func (c *Coordinator) waitForRetirement(id session.SessionID) error {
	c.mu.Lock()
	r := c.retirements[id]
	c.mu.Unlock()
	if r == nil {
		return nil
	}
	<-r.done
	return r.err
}

// --- write path (session/event → flush drain) --------------------------------

// AttachLive returns the one lifecycle controller for a live session,
// creating it if needed (official initFor, driven by session/created, the
// per-event path, and HMR re-seed). The placeholder installs under the map
// lock, so racing attaches share one initialization: exactly one caller
// runs the prepared-bind / seed path, every other caller returns the same
// controller once its initialization settles.
func (c *Coordinator) AttachLive(sess *session.Session) *liveSessionState {
	c.mu.Lock()
	if existing := c.live[sess]; existing != nil {
		c.mu.Unlock()
		// A placeholder carries no write-behind controller: await the
		// initialization and take its published result.
		if existing.writes == nil {
			<-existing.initDone
			return existing.initLive
		}
		return existing
	}
	placeholder := &liveSessionState{initDone: make(chan struct{})}
	c.live[sess] = placeholder
	c.mu.Unlock()

	go func() {
		live := c.initLiveState(sess)
		c.mu.Lock()
		c.live[sess] = live
		c.mu.Unlock()
		placeholder.initLive = live
		close(placeholder.initDone)
	}()
	<-placeholder.initDone
	return placeholder.initLive
}

// initLiveState runs the one-shot live-state initialization: a completed
// prepare() whose exact Session is being published binds here instead of
// re-reading storage (official reservationFor path); otherwise the session
// seeds from its own stable snapshot (backends only serialize it).
func (c *Coordinator) initLiveState(sess *session.Session) *liveSessionState {
	if res, err := c.preps.reservationFor(sess); err == nil && res != nil {
		live, attachErr := c.attachPrepared(sess, res)
		if attachErr == nil {
			return live
		}
		if c.logger != nil {
			c.logger.Warn(fmt.Sprintf("%s: session %q preparation attach failed: %v", c.backend.Name(), sess.ID(), attachErr))
		}
	}

	seed := sess.Events()
	live := &liveSessionState{initDone: make(chan struct{})}
	live.writes = c.createWriteBehind(sess, live)
	go func() {
		live.initErr = c.serialize(sess.ID(), func() error { return c.onCreated(sess, seed) })
		close(live.initDone)
	}()
	return live
}

// NotifySessionEvent keeps a persistence-owned copy of each frozen event
// and starts its bounded window.
func (c *Coordinator) NotifySessionEvent(sess *session.Session, event session.Event) {
	live := c.AttachLive(sess)
	live.writes.Enqueue(event)
}

// FlushSession is the immediate durability barrier for buffered writes.
func (c *Coordinator) FlushSession(sess *session.Session) error {
	live := c.AttachLive(sess)
	live.writes.CancelAutomaticWait()
	<-live.initDone
	if live.initErr != nil {
		// Admission is closed during retirement/teardown, but an ordinary
		// flush may have raced one last enqueue while initialization was
		// pending.
		live.writes.CancelAutomaticWait()
		return live.initErr
	}
	return live.writes.Flush()
}

// RetireSession observes session disposal; retirement contains its own
// failure and logs it.
func (c *Coordinator) RetireSession(sess *session.Session) {
	c.mu.Lock()
	if _, tracked := c.live[sess]; !tracked {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	go func() {
		err := c.retireCore(sess)
		r := &retirement{done: make(chan struct{}), err: err}
		c.mu.Lock()
		c.retirements[sess.ID()] = r
		c.mu.Unlock()
		close(r.done)
		c.mu.Lock()
		if c.retirements[sess.ID()] == r {
			delete(c.retirements, sess.ID())
		}
		c.mu.Unlock()
		if err != nil && c.logger != nil {
			c.logger.Warn(fmt.Sprintf("%s: session %q retirement failed: %v", c.backend.Name(), sess.ID(), err))
		}
	}()
}

// retireCore drains and releases state owned by one exact disposed Session
// lifecycle.
func (c *Coordinator) retireCore(sess *session.Session) error {
	if err := c.FlushSession(sess); err != nil {
		return err
	}
	id := sess.ID()
	return c.serialize(id, func() error {
		c.mu.Lock()
		delete(c.live, sess)
		if state := c.states[id]; state != nil && state.owner == sess {
			delete(c.states, id)
		}
		c.mu.Unlock()
		return nil
	})
}

// attachPrepared binds one exact prepared Session and persists only its
// unpublished suffix.
func (c *Coordinator) attachPrepared(sess *session.Session, res *reservation) (*liveSessionState, error) {
	source, state := res.source, res.state
	if source.sess != sess || state.owner != nil ||
		state.cursor != int64(len(source.inspection.Events)) ||
		sess.FirstLiveSeq() != state.cursor {
		return nil, fmt.Errorf("session %q preparation no longer matches its persistence state", sess.ID())
	}
	suffix := make([]session.Event, 0)
	for _, event := range sess.Events()[state.cursor:] {
		suffix = append(suffix, session.DeepCopyEvent(event))
	}
	if err := c.preps.attach(res); err != nil {
		return nil, err
	}
	state.owner = sess
	live := &liveSessionState{initDone: make(chan struct{})}
	live.writes = c.createWriteBehind(sess, live)
	if len(suffix) > 0 {
		go func() {
			live.initErr = c.serialize(sess.ID(), func() error { return c.appendCore(sess.ID(), suffix) })
			close(live.initDone)
		}()
	} else {
		close(live.initDone)
	}
	return live, nil
}

// seedMatchesPersisted reports whether a live session's seed reproduces
// the first cursor persisted events. A cursor of 0 (nothing persisted yet)
// trivially matches. Used when a live session claims ownerless state left
// by a prior Load/Create.
func (c *Coordinator) seedMatchesPersisted(id session.SessionID, seed []session.Event, cursor int64) (bool, error) {
	if cursor == 0 {
		return true, nil
	}
	stored, err := c.backend.LoadStored(id)
	if err != nil {
		return false, err
	}
	if stored == nil {
		return false, nil
	}
	if err := c.assertStoredId(id, stored.Meta); err != nil {
		return false, err
	}
	events, err := normalizeStoredEvents(stored.Events, id)
	if err != nil {
		return false, err
	}
	if cursor > int64(len(events)) {
		return false, nil
	}
	return seedCoversPrefix(seed, events[:cursor]), nil
}

// seedCoversPrefix reports whether a live session seed reproduces a
// persisted prefix exactly.
func seedCoversPrefix(seed []session.Event, prefix []session.Event) bool {
	if len(prefix) > len(seed) {
		return false
	}
	for i, event := range prefix {
		seedRaw, err := seedJSON(seed[i])
		if err != nil || seedRaw != eventJSON(event) {
			return false
		}
	}
	return true
}

// onCreated syncs the backend's in-memory state to a live Session
// (session/created path).
//
// Cases, by whether this backend tracks the id and whether an artifact
// exists:
//  1. Already tracked → no-op (or claim ownerless state if the seed
//     matches, or reclaim a truly-abandoned id, else reject as a collision).
//  2. Not tracked, an artifact EXISTS in the same scope and is a
//     seq-aligned PREFIX of the live events → ADOPT it, persisting any
//     live suffix.
//  3. Not tracked, an artifact EXISTS elsewhere or is NOT a prefix →
//     REJECT (collision).
//  4. Not tracked and NO artifact → a genuinely new session: register meta
//     (lazy) and persist its seed once.
func (c *Coordinator) onCreated(sess *session.Session, seed []session.Event) error {
	id := sess.ID()
	c.mu.Lock()
	tracked := c.states[id]
	c.mu.Unlock()
	if tracked != nil {
		// case 1: already tracked.
		if tracked.owner == sess {
			return nil
		}
		if tracked.owner == nil {
			// Ownerless state from the public Create/Load API. The FIRST
			// live session claims it — but ONLY if both the cwd scope and
			// the seed match. A same-id ownerless artifact at a different
			// cwd is a collision, not a claim; the seed guard then ensures
			// the live events reproduce the persisted prefix.
			if tracked.meta.CWD != sess.Header().CWD {
				return fmt.Errorf("session %q is already persisted at a different cwd (persisted: %s, live: %s) (id collision)", id, tracked.meta.CWD, sess.Header().CWD)
			}
			matches, err := c.seedMatchesPersisted(id, seed, tracked.cursor)
			if err != nil {
				return err
			}
			if !matches {
				return fmt.Errorf("session %q is already persisted with %d event(s) that do not match this live session (id collision)", id, tracked.cursor)
			}
			tracked.owner = sess
			// Persist the seed SUFFIX beyond the persisted prefix.
			// Constructor seed events never emit session/event, so the
			// buffer never sees them.
			suffix := append([]session.Event{}, seed[tracked.cursor:]...)
			if len(suffix) > 0 {
				return c.appendCore(id, suffix)
			}
			return nil
		}
		ownerLive := c.liveByPointer(tracked.owner)
		if !tracked.materialized && (ownerLive == nil || !ownerLive.writes.HasWork()) {
			c.mu.Lock()
			delete(c.states, id)
			c.mu.Unlock()
		} else {
			return fmt.Errorf("session %q is already bound to a different live session in this backend (id collision)", id)
		}
	}

	// case 2/3: resolve the id once across storage, then let adoption
	// reject a cwd mismatch before repair or state publication.
	stored, err := c.backend.LoadStored(id)
	if err != nil {
		return err
	}
	if stored != nil {
		// Do NOT route through cold preparation: that crash-repairs open
		// turns as interrupted, which is wrong while the live Session is
		// still the authority and may append the real step/turn end later.
		return c.adoptLivePrefix(sess, seed, stored)
	}

	// case 4: a genuinely new session.
	meta := session.DeepCopyHeader(sess.Header())
	if err := c.createCore(meta); err != nil {
		return err
	}
	// Bind this state to the live session so a later DIFFERENT session
	// reusing the id is detected as a collision (case 1) rather than
	// silently no-opped.
	c.mu.Lock()
	if created := c.states[id]; created != nil {
		created.owner = sess
	}
	c.mu.Unlock()
	if len(seed) > 0 {
		return c.appendCore(id, seed)
	}
	return nil
}

// adoptLivePrefix adopts a stored prefix as a live session's history
// (reload): verify the seed covers the stored prefix, truncate any torn
// tail (NOT the open turn — the live Session is still the authority), bind
// ownership, and persist the live suffix that was ahead of the stored
// prefix.
func (c *Coordinator) adoptLivePrefix(sess *session.Session, seed []session.Event, stored *StoredPrefix) error {
	meta := stored.Meta
	id := sess.ID()
	if err := c.assertStoredId(id, meta); err != nil {
		return err
	}
	if meta.CWD != sess.Header().CWD {
		return fmt.Errorf("session %q is already persisted at a different cwd (persisted: %s, live: %s) (id collision)", id, meta.CWD, sess.Header().CWD)
	}
	if err := c.assertVersion(meta); err != nil {
		return err
	}
	storedEvents, err := normalizeStoredEvents(stored.Events, id)
	if err != nil {
		return err
	}
	if err := c.assertEventsSupported(meta, storedEvents); err != nil {
		return err
	}
	if !seedCoversPrefix(seed, storedEvents) {
		return fmt.Errorf("session %q already has a persisted log on disk that does not match this live session (id collision)", id)
	}
	// Truncate-only repair (no closers): the open turn is NOT closed here.
	if stored.TornMarker != nil {
		if err := c.backend.CommitRepair(meta, stored.TornMarker, nil); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.states[id] = &sessionState{
		meta:         session.DeepCopyHeader(meta),
		cursor:       int64(len(storedEvents)),
		materialized: true,
		owner:        sess,
	}
	c.mu.Unlock()
	suffix := append([]session.Event{}, seed[len(storedEvents):]...)
	if len(suffix) > 0 {
		return c.appendCore(id, suffix)
	}
	return nil
}

func (c *Coordinator) liveByPointer(sess *session.Session) *liveSessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[sess]
}

// createWriteBehind builds one write controller around initialization and
// id serialization.
func (c *Coordinator) createWriteBehind(sess *session.Session, live *liveSessionState) *SessionWriteBehind {
	return NewSessionWriteBehind(WriteBehindOptions{
		MaxDelay: msToDuration(c.writeBatchMaxDelayMs),
		Write: func(batch []session.Event) error {
			<-live.initDone
			if live.initErr != nil {
				return live.initErr
			}
			return c.serialize(sess.ID(), func() error {
				return c.appendLiveBatch(sess.ID(), batch)
			})
		},
		ReportBackgroundFailure: func(err error) {
			if c.logger != nil {
				c.logger.Warn(fmt.Sprintf("%s: background write for session %q failed (buffered events retained): %v", c.backend.Name(), sess.ID(), err))
			}
		},
	})
}

// appendLiveBatch appends one controller-owned prefix after filtering
// events initialization already stored.
func (c *Coordinator) appendLiveBatch(id session.SessionID, batch []session.Event) error {
	c.mu.Lock()
	state := c.states[id]
	var cursor int64
	if state != nil {
		cursor = state.cursor
	}
	c.mu.Unlock()
	fresh := make([]session.Event, 0, len(batch))
	for _, event := range batch {
		if event.Seq >= cursor {
			fresh = append(fresh, event)
		}
	}
	return c.appendCore(id, fresh)
}

// Dispose drains every live session to quiescence and closes the backend.
// The caller must have stopped event admission first (official: the
// disposer registers before the listeners, so cordis tears event admission
// down before this drain).
func (c *Coordinator) Dispose() error {
	c.mu.Lock()
	live := make([]*session.Session, 0, len(c.live))
	for sess := range c.live {
		live = append(live, sess)
	}
	c.mu.Unlock()
	var errs []error
	for _, sess := range live {
		if err := c.FlushSession(sess); err != nil {
			errs = append(errs, err)
		}
	}
	// Wait for every in-flight serialized operation to settle.
	for {
		c.mu.Lock()
		ids := make([]session.SessionID, 0, len(c.chains))
		for id := range c.chains {
			ids = append(ids, id)
		}
		c.mu.Unlock()
		if len(ids) == 0 {
			break
		}
		busy := false
		for _, id := range ids {
			c.mu.Lock()
			mu := c.chains[id]
			c.mu.Unlock()
			if mu != nil && !mu.TryLock() {
				busy = true
				continue
			}
			if mu != nil {
				mu.Unlock()
			}
		}
		if !busy {
			break
		}
	}
	closeErr := c.backend.Close()
	if len(errs) > 0 {
		return &DisposeError{Backend: c.backend.Name(), Errors: errs}
	}
	return closeErr
}

// DisposeError aggregates drain failures during disposal; a backend Close
// failure only replaces it when the drain succeeded.
type DisposeError struct {
	Backend string
	Errors  []error
}

func (e *DisposeError) Error() string {
	message := fmt.Sprintf("%s dispose failed:", e.Backend)
	for _, err := range e.Errors {
		message += " " + err.Error() + ";"
	}
	return message
}

// Unwrap exposes every drain failure.
func (e *DisposeError) Unwrap() []error { return e.Errors }
