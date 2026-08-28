// Bounded sharing and exclusive reservation of unpublished Sessions.
// Faithful port of preparations.ts (observeQueuedAbort folds away: Go
// cancellation rides the caller's context through the coordinator's
// serialization, and shared work here is never cancelled).
package persistence

import (
	"fmt"
	"sync"

	"dshgo/session"
)

// preparedSource is one validated cold source and the exact unpublished
// Session built from it.
type preparedSource struct {
	inspection    Inspection
	sess          *session.Session
	revision      Revision
	sessionLength int64
	tornMarker    any
	closers       []session.Event
}

// committedPreparation is the exclusive reservation's payload: the source
// and its committed persistence state.
type committedPreparation struct {
	source *preparedSource
	state  *sessionState
}

// reservation is one exclusively held prepared source and its committed
// persistence state.
type reservation struct {
	entry  *preparationEntry
	source *preparedSource
	state  *sessionState
}

// preparationPhase is one entry's position in the share/commit/reserve
// lifecycle.
type preparationPhase int

const (
	phaseLoading preparationPhase = iota
	phaseReady
	phaseCommitting
	phaseReserved
)

// preparationEntry is the per-id shared state machine.
type preparationEntry struct {
	id      session.SessionID
	phase   preparationPhase
	source  *preparedSource
	reserve *reservation
	pins    int

	// loaded closes when the cold load attempt settled (success or error).
	loaded chan struct{}
	// loadErr is the cold load failure, valid after loaded closes.
	loadErr error
	// settled closes on makeReady/remove, waking reservation waiters.
	settled chan struct{}
}

// preparations is per-coordinator cold-read sharing, exclusive reservation,
// and ready-entry LRU.
type preparations struct {
	mu       sync.Mutex
	entries  map[session.SessionID]*preparationEntry
	order    []*preparationEntry // LRU: least-recently-touched first
	capacity int
}

func newPreparations(capacity int) *preparations {
	return &preparations{entries: map[session.SessionID]*preparationEntry{}, capacity: capacity}
}

// has reports whether this pool currently knows about an unpublished
// identity.
func (p *preparations) has(id session.SessionID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[id] != nil
}

// inspect observes one prepared source, sharing an in-flight read for the
// same id.
func (p *preparations) inspect(id session.SessionID, load func() (*preparedSource, error)) (*preparedSource, error) {
	entry, wait := p.entryFor(id, load)
	<-wait
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.loadErr != nil {
		return nil, entry.loadErr
	}
	if p.entries[id] == entry && entry.phase == phaseReady {
		p.touchLocked(entry)
	}
	return entry.source, nil
}

// borrow pins one ready entry against LRU eviction for the caller's
// observation. The returned unpin releases the pin.
func (p *preparations) borrow(id session.SessionID, load func() (*preparedSource, error)) (source *preparedSource, unpin func(), err error) {
	entry, wait := p.entryFor(id, load)
	p.mu.Lock()
	pinned := p.entries[id] == entry
	if pinned {
		entry.pins++
	}
	p.mu.Unlock()
	<-wait
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry.loadErr != nil {
		if pinned && p.entries[id] == entry {
			entry.pins--
			if entry.phase == phaseReady {
				p.touchLocked(entry)
			}
		}
		return nil, nil, entry.loadErr
	}
	if p.entries[id] != entry {
		return entry.source, func() {}, nil
	}
	if entry.phase == phaseReady {
		p.touchLocked(entry)
	}
	released := false
	return entry.source, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if released {
			return
		}
		released = true
		if p.entries[id] != entry {
			return
		}
		entry.pins--
		if entry.phase == phaseReady {
			p.touchLocked(entry)
		}
	}, nil
}

// reserve takes one ready source after committing its pending durable
// repair. It returns nil (without error) when the entry was invalidated or
// the commit declined.
func (p *preparations) reserve(
	id session.SessionID,
	load func() (*preparedSource, error),
	commit func(source *preparedSource) (*committedPreparation, error),
) (*reservation, error) {
	entry, wait := p.entryFor(id, load)
	<-wait
	p.mu.Lock()
	// A failed cold load rejects the reservation (official: awaiting
	// entry.result throws), so callers surface the error instead of
	// retrying a permanently failing read forever.
	if entry.loadErr != nil {
		p.mu.Unlock()
		return nil, entry.loadErr
	}
	for p.entries[id] == entry && entry.phase != phaseReady {
		settled := entry.settled
		if settled == nil {
			p.mu.Unlock()
			return nil, fmt.Errorf("session %q preparation lost its reservation waiter", id)
		}
		p.mu.Unlock()
		<-settled
		p.mu.Lock()
	}
	if p.entries[id] != entry {
		p.mu.Unlock()
		return nil, nil
	}
	source := entry.source
	entry.phase = phaseCommitting
	entry.settled = make(chan struct{})
	p.mu.Unlock()

	committed, commitErr := commit(source)
	p.mu.Lock()
	defer p.mu.Unlock()
	if commitErr != nil {
		p.removeLocked(entry)
		return nil, commitErr
	}
	if committed == nil {
		p.removeLocked(entry)
		return nil, nil
	}
	entry.source = committed.source
	if p.entries[id] != entry {
		return nil, nil
	}
	res := &reservation{entry: entry, source: committed.source, state: committed.state}
	entry.phase = phaseReserved
	entry.reserve = res
	return res, nil
}

// reservationFor returns the exact reservation for Session publication,
// rejecting aliases.
func (p *preparations) reservationFor(sess *session.Session) (*reservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[sess.ID()]
	if entry == nil {
		return nil, nil
	}
	if entry.phase == phaseReserved && entry.source != nil && entry.source.sess == sess && entry.reserve != nil {
		return entry.reserve, nil
	}
	return nil, fmt.Errorf("cannot publish session %q: persisted state already owns this identity", sess.ID())
}

// attach consumes a reservation after its exact Session has attached.
func (p *preparations) attach(res *reservation) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := res.entry
	if p.entries[entry.id] != entry || entry.reserve != res {
		return fmt.Errorf("session %q preparation is no longer reserved", entry.id)
	}
	p.removeLocked(entry)
	return nil
}

// discard consumes a reservation whose caller only needs the committed
// inspection.
func (p *preparations) discard(res *reservation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := res.entry
	if p.entries[entry.id] != entry || entry.reserve != res {
		return
	}
	p.removeLocked(entry)
}

// release returns a reusable unpublished reservation to the ready LRU.
func (p *preparations) release(res *reservation, reusable bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := res.entry
	if p.entries[entry.id] != entry || entry.reserve != res || entry.phase != phaseReserved {
		return
	}
	if !reusable {
		p.removeLocked(entry)
		return
	}
	entry.reserve = nil
	p.makeReadyLocked(entry)
}

// invalidate discards a prepared view after the durable log changes.
func (p *preparations) invalidate(id session.SessionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry := p.entries[id]; entry != nil {
		p.removeLocked(entry)
	}
}

// discardReady discards an exact stale ready source without disturbing an
// exclusive owner.
func (p *preparations) discardReady(id session.SessionID, expected *preparedSource) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[id]
	if entry == nil || entry.source != expected {
		return "missing"
	}
	if entry.phase != phaseReady {
		return "retained"
	}
	p.removeLocked(entry)
	return "discarded"
}

// assertWritable rejects writes while an unpublished Session exclusively
// reserves the id.
func (p *preparations) assertWritable(id session.SessionID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch p.entries[id].phaseIfPresent() {
	case phaseCommitting, phaseReserved:
		return fmt.Errorf("cannot append session %q while its persisted preparation is reserved", id)
	}
	return nil
}

// takeReady removes a completed entry for an already-serialized append
// adoption.
func (p *preparations) takeReady(id session.SessionID) *preparedSource {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[id]
	if entry == nil || entry.phase != phaseReady || entry.source == nil {
		return nil
	}
	p.removeLocked(entry)
	return entry.source
}

// phaseIfPresent is a nil-safe phase read for assertWritable.
func (entry *preparationEntry) phaseIfPresent() preparationPhase {
	if entry == nil {
		return phaseLoading
	}
	return entry.phase
}

// entryFor returns the existing entry or installs one and runs the cold
// load on the caller's goroutine. It returns the entry and a channel that
// closes when the load attempt settled.
func (p *preparations) entryFor(id session.SessionID, load func() (*preparedSource, error)) (*preparationEntry, chan struct{}) {
	p.mu.Lock()
	if entry := p.entries[id]; entry != nil {
		p.mu.Unlock()
		return entry, entry.loaded
	}
	entry := &preparationEntry{id: id, phase: phaseLoading, loaded: make(chan struct{})}
	p.entries[id] = entry
	p.order = append(p.order, entry)
	p.mu.Unlock()

	source, err := func() (src *preparedSource, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("session %q preparation panicked: %v", id, r)
			}
		}()
		return load()
	}()
	p.mu.Lock()
	defer p.mu.Unlock()
	// Deliver the completed load even if invalidate raced the entry away:
	// waiters fall back to this value (official `entry.source ?? loaded`).
	entry.source = source
	if err != nil {
		p.removeLocked(entry)
		entry.loadErr = err
		close(entry.loaded)
		return entry, entry.loaded
	}
	if p.entries[id] == entry {
		p.makeReadyLocked(entry)
	}
	close(entry.loaded)
	return entry, entry.loaded
}

// makeReadyLocked transitions an entry to ready and wakes reservation
// waiters.
func (p *preparations) makeReadyLocked(entry *preparationEntry) {
	if p.entries[entry.id] != entry {
		return
	}
	entry.phase = phaseReady
	if entry.settled != nil {
		close(entry.settled)
		entry.settled = nil
	}
	p.touchLocked(entry)
}

// removeLocked drops an entry and wakes reservation waiters.
func (p *preparations) removeLocked(entry *preparationEntry) {
	if p.entries[entry.id] != entry {
		return
	}
	delete(p.entries, entry.id)
	for i, candidate := range p.order {
		if candidate == entry {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	if entry.settled != nil {
		close(entry.settled)
		entry.settled = nil
	}
}

// touchLocked refreshes an entry's LRU position and evicts the oldest
// ready unpinned entry when ready entries exceed capacity.
func (p *preparations) touchLocked(entry *preparationEntry) {
	for i, candidate := range p.order {
		if candidate == entry {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
	p.order = append(p.order, entry)
	readyCount := 0
	for _, candidate := range p.order {
		if candidate.phase == phaseReady {
			readyCount++
		}
	}
	if readyCount <= p.capacity {
		return
	}
	for _, candidate := range p.order {
		if candidate.phase != phaseReady || candidate.pins > 0 {
			continue
		}
		p.removeLocked(candidate)
		return
	}
}
