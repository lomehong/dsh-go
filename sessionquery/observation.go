package sessionquery

import (
	"context"
	"errors"

	"dshgo/session"
	"dshgo/session/persistence"
	"dshgo/session/projection"
)

// ProjectionSource is the optional projection seam for observations:
// Snapshot mirrors a live session's registry state; HydratePrepared folds
// one prepared log. Both are optional capabilities the composition wires
// (nil = the projection registry is not mounted).
type ProjectionSource interface {
	// SnapshotLive computes every registered projection for a live session.
	SnapshotLive(sess *session.Session) (projection.Snapshot, bool)
	// HydratePrepared folds one prepared session's exact log.
	HydratePrepared(sess *session.Session, meta session.SessionHeader, events []session.Event) (projection.Snapshot, bool, error)
}

// SessionObservationOptions carry cancellation and projection selection for
// one observation read.
type SessionObservationOptions struct {
	// ProjectionMode selects all-or-none projection computation; empty
	// defaults to all.
	ProjectionMode string
}

// ProjectionModeAll computes every projection (the default).
const ProjectionModeAll = "all"

// ProjectionModeNone leaves projection state untouched.
const ProjectionModeNone = "none"

// SessionObservation is one exact immutable Session cut retained for the
// caller's read lifetime. Retain duplicates the lease for another owner;
// Release must be called once per lease. Events are detached copies.
type SessionObservation struct {
	// Source is "live" (an attached Session) or "prepared" (a retained
	// preparation).
	Source string
	// Header is the immutable Session identity metadata.
	Header session.SessionHeader
	// Events are the immutable contiguous events at Cursor.
	Events []session.Event
	// Cursor is the last observed event seq, -1 for an empty log.
	Cursor int64
	// Revision is the durable source revision for a cold prepared
	// observation.
	Revision *persistence.Revision
	// Projections is the exact projection baseline at Cursor, when a
	// registry is mounted and projections were requested.
	Projections *projection.Snapshot

	lease *observationLease
}

// Retain returns an independently releasable lease over the same
// observation. Use after Release fails the retain.
func (o *SessionObservation) Retain() (*SessionObservation, error) {
	return o.lease.retain(o)
}

// Release ends one lease over the observation. When the last lease ends, a
// prepared source is unpinned.
func (o *SessionObservation) Release() {
	o.lease.release()
}

type observationLease struct {
	detach func()
	refs   int
}

func (l *observationLease) retain(template *SessionObservation) (*SessionObservation, error) {
	if l.refs == 0 {
		return nil, queryError(CodeAborted, "session observation %q is released", template.Header.ID)
	}
	l.refs++
	copied := *template
	copied.lease = l
	return &copied, nil
}

func (l *observationLease) release() {
	if l.refs == 0 {
		return
	}
	l.refs--
	if l.refs == 0 && l.detach != nil {
		l.detach()
	}
}

// SessionObservationReader builds point observations without a corpus
// listing preflight.
type SessionObservationReader struct {
	sessions    Sessions
	persistence *persistence.Coordinator
	projections ProjectionSource
}

// NewSessionObservationReader builds a reader over the given registry and
// optional persistence/projection seams.
func NewSessionObservationReader(sessions Sessions, persistence *persistence.Coordinator, projections ProjectionSource) *SessionObservationReader {
	return &SessionObservationReader{
		sessions:    sessions,
		persistence: persistence,
		projections: projections,
	}
}

// Read observes one live-preferred Session and retains a cold preparation
// until disposal. A live read snapshots without querying persistence; a
// persisted read re-checks the live registry after borrowing and keeps
// retrying while the persistence race reports a live win.
func (r *SessionObservationReader) Read(ctx context.Context, sessionID session.SessionID, options SessionObservationOptions) (*SessionObservation, error) {
	projectionMode := options.ProjectionMode
	if projectionMode == "" {
		projectionMode = ProjectionModeAll
	}
	for {
		if err := ctxEnsureLive(ctx); err != nil {
			return nil, err
		}
		if live, ok := r.sessions.Get(sessionID); ok {
			return liveObservation(live, r, projectionMode), nil
		}
		if r.persistence == nil {
			return nil, sessionNotFound(sessionID)
		}
		borrowed, err := r.persistence.BorrowSession(sessionID)
		if err != nil {
			notFound := &persistence.NotFoundError{}
			if errors.As(err, &notFound) {
				return nil, queryErrorCause(CodeSessionNotFound, err, "session %q not found", sessionID)
			}
			corrupt := &persistence.CorruptionError{}
			if errors.As(err, &corrupt) {
				return nil, queryErrorCause(CodeCorruptSession, err, "stored session %q is corrupt: %v", sessionID, err)
			}
			return nil, queryErrorCause(CodePersistenceFailed, err, "failed to observe session %q: %v", sessionID, err)
		}
		if err := ctxEnsureLive(ctx); err != nil {
			borrowed.Release()
			return nil, err
		}
		if borrowed.Inspection.Meta.ID != sessionID {
			borrowed.Release()
			return nil, queryError(CodeSourceConflict, "session persistence returned %q for %q", borrowed.Inspection.Meta.ID, sessionID)
		}
		if attached, ok := r.sessions.Get(sessionID); ok {
			observation := liveObservation(attached, r, projectionMode)
			borrowed.Release()
			return observation, nil
		}
		if borrowed.Source == "live" {
			// The live Session disappeared between persistence's race check
			// and this read; retry against its now-cold durable identity.
			borrowed.Release()
			continue
		}
		observation := &SessionObservation{
			Source: "prepared",
			Header: borrowed.Inspection.Meta,
			Events: borrowed.Inspection.Events,
			Cursor: lastSeq(borrowed.Inspection.Events),
			lease: &observationLease{
				refs:   1,
				detach: borrowed.Release,
			},
		}
		if borrowed.Revision != "" {
			revision := borrowed.Revision
			observation.Revision = &revision
		}
		if projectionMode != ProjectionModeNone && r.projections != nil {
			snapshot, _, err := r.projections.HydratePrepared(borrowed.PreparedSession, borrowed.Inspection.Meta, borrowed.Inspection.Events)
			if err != nil {
				borrowed.Release()
				return nil, queryErrorCause(CodeCorruptSession, err, "failed to project session %q: %v", sessionID, err)
			}
			observation.Projections = &snapshot
		}
		return observation, nil
	}
}

func liveObservation(live *session.Session, reader *SessionObservationReader, projectionMode string) *SessionObservation {
	events := append([]session.Event(nil), live.Events()...)
	observation := &SessionObservation{
		Source: "live",
		Header: live.Header(),
		Events: events,
		Cursor: lastSeq(events),
		lease:  &observationLease{refs: 1},
	}
	if projectionMode != ProjectionModeNone && reader.projections != nil {
		if snapshot, ok := reader.projections.SnapshotLive(live); ok {
			observation.Projections = &snapshot
		}
	}
	return observation
}

func lastSeq(events []session.Event) int64 {
	if len(events) == 0 {
		return -1
	}
	return events[len(events)-1].Seq
}
