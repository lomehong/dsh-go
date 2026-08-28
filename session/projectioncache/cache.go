// Package projectioncache ports the persisted projection cache: durable
// checkpoints of every projection unit's state, one record per session.
// Reads and writes share one coherent state (the store is authoritative for
// durability; the write path lands durability first, then the dirty state
// resets), and the cache is a fold shortcut, never an authority: a row is
// possibly stale (its seq says how stale) but never wrong, so every
// throttled write path is fail-soft and a version mismatch discards the row
// instead of migrating it. Port of
// packages/session/session-projection-cache/src/index.ts at tag
// dsh-v0.1.2-alpha.1.
package projectioncache

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"dshgo/cordis"
	"dshgo/session"
	"dshgo/session/projection"
)

// Identity is the stored-log identity a record is bound to: the immutable
// header fields that distinguish one session lifecycle from another under
// the same id. A session id names a slot, not a lifecycle.
type Identity struct {
	CreatedAt int64  `json:"createdAt"`
	CWD       string `json:"cwd,omitempty"`
}

// Record is one session's stored checkpoint: its log identity plus the
// checkpoint rows. The whole record is replaced on every write
// (whole-value discipline).
type Record struct {
	Identity Identity
	Rows     projection.Checkpoint
}

// Store is the durable seam (the storage-domain KvTable role): one record
// per session id, replaced whole. Implementations must be safe for
// concurrent use.
type Store interface {
	Get(id session.SessionID) (*Record, bool)
	Put(id session.SessionID, record *Record) error
	Close() error
}

// Sessions is the live-session seam the durability barrier reads through.
type Sessions interface {
	// Get resolves the live session currently bound to the id.
	Get(id session.SessionID) (*session.Session, bool)
	// Flush drains the session's persistence write-behind to the backend.
	Flush(sess *session.Session) error
}

// Logger receives fail-soft write warnings.
type Logger interface {
	Warn(args ...any)
}

// Config carries the two deployment-varying throttle choices; the three
// mandatory write points (creation, turn/end, disposal) are policy and
// always fire.
type Config struct {
	// WriteEveryEvents is the committed-event count that forces a durable
	// checkpoint between mandatory points.
	WriteEveryEvents int
	// WriteIntervalMs is the longest time a dirty checkpoint may stay
	// unwritten between mandatory points.
	WriteIntervalMs int64
}

type dirtyState struct {
	pending int
	timer   *time.Timer
}

// Service checkpoints live sessions on a throttled write-behind plus the
// three mandatory points, and serves cached rows for a session header.
type Service struct {
	mu        sync.Mutex
	store     Store
	registry  *projection.Registry
	sessions  Sessions
	logger    Logger
	config    Config
	dirty     map[*session.Session]*dirtyState
	writeNow  func() // test hook; nil = time.Now-driven timer path
	closeOnce sync.Once
	closeErr  error
}

// New builds the cache service over a store, registry, and session seam.
func New(store Store, registry *projection.Registry, sessions Sessions, logger Logger, config Config) (*Service, error) {
	if config.WriteEveryEvents < 1 {
		return nil, fmt.Errorf("session projection cache: WriteEveryEvents must be a positive integer, got %d", config.WriteEveryEvents)
	}
	if config.WriteIntervalMs < 1 {
		return nil, fmt.Errorf("session projection cache: WriteIntervalMs must be a positive integer, got %d", config.WriteIntervalMs)
	}
	if store == nil || registry == nil {
		return nil, fmt.Errorf("session projection cache: store and registry are required")
	}
	if logger == nil {
		logger = discardLogger{}
	}
	return &Service{
		store:    store,
		registry: registry,
		sessions: sessions,
		logger:   logger,
		config:   config,
		dirty:    map[*session.Session]*dirtyState{},
	}, nil
}

type discardLogger struct{}

func (discardLogger) Warn(...any) {}

// Attach installs the write-behind listeners on a cordis context; the
// returned disposer removes them and clears pending timers.
func (s *Service) Attach(ctx *cordis.Context) func() {
	disposers := []cordis.Disposer{
		ctx.On("session/event", func(value any, next func(any) any) any {
			if payload, ok := value.(*projection.SessionEventPayload); ok {
				s.onEvent(payload.Session, payload.Event)
			}
			return next(value)
		}),
		ctx.On("session/created", func(value any, next func(any) any) any {
			if payload, ok := value.(*projection.SessionCreatedPayload); ok {
				s.FlushSoft(payload.Session, "create")
			}
			return next(value)
		}),
		ctx.On("session/disposed", func(value any, next func(any) any) any {
			if payload, ok := value.(*projection.SessionDisposedPayload); ok {
				s.FlushSoft(payload.Session, "detach")
				s.markClean(payload.Session)
				s.mu.Lock()
				delete(s.dirty, payload.Session)
				s.mu.Unlock()
			}
			return next(value)
		}),
	}
	return func() {
		for _, dispose := range disposers {
			dispose()
		}
		s.mu.Lock()
		for _, state := range s.dirty {
			if state.timer != nil {
				state.timer.Stop()
			}
		}
		s.dirty = map[*session.Session]*dirtyState{}
		s.mu.Unlock()
	}
}

// onEvent advances the dirty counter; turn/end is a mandatory point, and
// count/interval throttle the in-turn stream.
func (s *Service) onEvent(sess *session.Session, event session.Event) {
	if event.Type == "turn/end" {
		s.FlushSoft(sess, "turn/end")
		return
	}
	s.mu.Lock()
	state := s.dirty[sess]
	if state == nil {
		state = &dirtyState{}
		s.dirty[sess] = state
	}
	state.pending++
	if state.pending >= s.config.WriteEveryEvents {
		s.mu.Unlock()
		s.FlushSoft(sess, "count threshold")
		return
	}
	if state.timer == nil {
		interval := time.Duration(s.config.WriteIntervalMs) * time.Millisecond
		state.timer = time.AfterFunc(interval, func() {
			s.FlushSoft(sess, "interval")
		})
	}
	s.mu.Unlock()
}

// Write durably checkpoints one live session now. NOT fail-soft — callers
// on the fail-soft paths contain it. The registry cut is snapshotted first,
// then the session's record is replaced after the persistence flush, so a
// crash can leave the cache behind the log (longer tail replay) but never
// ahead of it (phantom values folded from events no stored log contains).
func (s *Service) Write(sess *session.Session) error {
	rows, err := s.registry.Checkpoint(sess)
	if err != nil {
		return err
	}
	s.markClean(sess)
	// Durability barrier: the checkpoint cut was taken above, so flushing
	// after it guarantees every event inside the cut is durably logged
	// before the cache row lands. At detach the store entry is already
	// gone; persistence's own retirement drain covers that path and any
	// residual overreach is caught by the cold read's anchored floor.
	if s.sessions != nil {
		if live, ok := s.sessions.Get(sess.ID()); ok && live == sess {
			if err := s.sessions.Flush(sess); err != nil {
				return err
			}
		}
	}
	return s.put(sess.ID(), identityOf(sess.Header()), rows)
}

// FlushSoft is one fail-soft durable checkpoint: failures log a warning and
// the cache self-heals on the next write.
func (s *Service) FlushSoft(sess *session.Session, trigger string) {
	if err := s.Write(sess); err != nil {
		s.logger.Warn(fmt.Sprintf("session projection cache: %s write for %q failed (cache stays stale): %v", trigger, sess.ID(), err))
	}
}

// CachedSnapshot is the zero-I/O listing read: whole values viewed straight
// from the stored rows (version-matching keys only), never from an
// unrelated log (the caller's header is the identity witness). ok=false
// when no usable row exists for this lifecycle.
func (s *Service) CachedSnapshot(meta session.SessionHeader, keys ...string) (projection.Snapshot, bool) {
	record, ok := s.recordFor(meta.ID, identityOf(meta))
	if !ok {
		return projection.Snapshot{}, false
	}
	values := s.registry.ViewCheckpoint(record.Rows, keys...)
	if len(values) == 0 {
		return projection.Snapshot{}, false
	}
	// ONE cut: the lowest served watermark is the seq every value is at
	// least current as of (under-claiming is safe; over-claiming would let
	// a stale value outrank pushes).
	var asOfSeq int64
	first := true
	for key, row := range record.Rows {
		if _, served := values[key]; !served {
			continue
		}
		if first || row.Seq < asOfSeq {
			asOfSeq = row.Seq
			first = false
		}
	}
	return projection.Snapshot{AsOfSeq: asOfSeq, Values: values}, true
}

// HydratePrepared seeds projection cells for an already-prepared session
// without another persistence read: matching rows seed, the supplied exact
// log advances every unit to the observation cut. No checkpoint is written
// because the logical observation may contain recovery events not yet
// durable.
func (s *Service) HydratePrepared(sess *session.Session, meta session.SessionHeader, events []session.Event) (projection.Snapshot, error) {
	var rows projection.Checkpoint
	if record, ok := s.recordFor(meta.ID, identityOf(meta)); ok {
		rows = record.Rows
	}
	snapshot, err := s.registry.Hydrate(sess, rows, events, 0)
	if err != nil {
		// Cached rows are disposable derived data. Retry from the exact log
		// so a stale schema cannot make a valid session unreadable.
		return s.registry.Hydrate(sess, nil, events, 0)
	}
	return snapshot, nil
}

// ColdSnapshot reads one session's projections from its complete log,
// seeding from identity-checked rows; the refreshed checkpoint is written
// back fail-soft and fire-and-forget, so the first cold read creates the
// row and later ones seed from it.
func (s *Service) ColdSnapshot(meta session.SessionHeader, events []session.Event) (projection.Snapshot, error) {
	var rows projection.Checkpoint
	if record, ok := s.recordFor(meta.ID, identityOf(meta)); ok {
		rows = record.Rows
	}
	restored, err := s.registry.Restore(rows, events, 0, meta)
	if err != nil {
		return projection.Snapshot{}, err
	}
	go func() {
		if err := s.put(meta.ID, identityOf(meta), restored.Checkpoint); err != nil {
			s.logger.Warn(fmt.Sprintf("session projection cache: cold-read write-back for %q failed (cache stays stale): %v", meta.ID, err))
		}
	}()
	return restored.Snapshot, nil
}

// Close releases the store; safe to call once.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		for _, state := range s.dirty {
			if state.timer != nil {
				state.timer.Stop()
			}
		}
		s.dirty = map[*session.Session]*dirtyState{}
		s.mu.Unlock()
		s.closeErr = s.store.Close()
	})
	return s.closeErr
}

// recordFor reads the stored record only when its bound log identity
// matches expected (synchronous from the store's coherent state).
func (s *Service) recordFor(id session.SessionID, expected Identity) (*Record, bool) {
	record, ok := s.store.Get(id)
	if !ok {
		return nil, false
	}
	if record.Identity.CreatedAt != expected.CreatedAt || record.Identity.CWD != expected.CWD {
		return nil, false
	}
	return record, true
}

// put replaces one session's stored record. The checkpoint rows are already
// detached JSON (the registry checkpointed through a JSON round-trip), so a
// non-serializable state failed loud before reaching the cache.
func (s *Service) put(id session.SessionID, identity Identity, rows projection.Checkpoint) error {
	if rows == nil {
		rows = projection.Checkpoint{}
	}
	return s.store.Put(id, &Record{Identity: identity, Rows: rows})
}

func (s *Service) markClean(sess *session.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.dirty[sess]
	if state == nil {
		return
	}
	state.pending = 0
	if state.timer != nil {
		state.timer.Stop()
		state.timer = nil
	}
}

// identityOf projects a header onto the identity fields a record binds to.
func identityOf(header session.SessionHeader) Identity {
	return Identity{CreatedAt: header.CreatedAt, CWD: header.CWD}
}

// MarshalRecord serializes a record for durable media; exported for store
// implementations and tests.
func MarshalRecord(record *Record) ([]byte, error) {
	return json.Marshal(record)
}

// UnmarshalRecord parses a stored record document.
func UnmarshalRecord(raw []byte) (*Record, error) {
	var decoded struct {
		Identity Identity                  `json:"identity"`
		Rows     map[string]projection.Row `json:"rows"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	rows := projection.Checkpoint(decoded.Rows)
	if rows == nil {
		rows = projection.Checkpoint{}
	}
	return &Record{Identity: decoded.Identity, Rows: rows}, nil
}
