package jsonl

import (
	"path/filepath"

	"dshgo/session"
	"dshgo/session/persistence"
)

// Cross-process write-ownership lock for one session's artifact directory,
// held for the whole life of a write handle. The arbiter is the kernel:
// POSIX takes a non-blocking flock(2) on `session.lock` beside the log, and
// Windows holds a named kernel semaphore derived from that path — never a
// file lock or handle, so readers, searches, and directory removal proceed
// freely while the lock is held. Contention maps to
// persistence.AlreadyOwnedError; the kernel releases the lock when the
// holder's descriptor or last object handle closes, including on any process
// death, so a crashed holder never blocks a successor. A live but wedged
// holder keeps the lock until its process exits: there is deliberately no
// expiry that could expropriate a stalled writer whose resumed appends would
// tear the log. Readers never touch the lock. Port of
// packages/session/session-persistence-jsonl/src/lease.ts (0.1.3-alpha.1).

// LEASE_FILENAME is the kernel lock file's base name inside a session
// directory (POSIX only; Windows has no lock file at all).
const LEASE_FILENAME = "session.lock"

// leasePath is one session directory's lock file path.
func leasePath(dir string) string {
	return filepath.Join(dir, LEASE_FILENAME)
}

// SessionWriteLease is one held kernel write lock. Constructed only by
// AcquireWriteLease; Release closes the descriptor or handle, which is what
// releases the lock.
type SessionWriteLease struct {
	held heldKernelLock
}

// AcquireWriteLease acquires the session directory's kernel write lock,
// creating the directory when absent.
func AcquireWriteLease(dir string, id session.SessionID) (*SessionWriteLease, error) {
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	held, err := acquireKernelLock(leasePath(dir), dir, id)
	if err != nil {
		return nil, err
	}
	return &SessionWriteLease{held: held}, nil
}

// Release releases the kernel lock by closing its descriptor or handle. The
// POSIX lock file is never removed: every acquired lock belongs to a
// materialized or materializing session, and keeping the file preserves the
// stable inode later lockers verify against. Idempotent.
func (l *SessionWriteLease) Release() {
	if l == nil || l.held.close == nil {
		return
	}
	l.held.close()
	l.held.close = nil
}

// leaseOwnedText is the stable contention fragment (official
// SessionAlreadyOwnedError message).
const leaseOwnedText = "is already owned by an active write handle"

// alreadyOwned renders the contention refusal.
func alreadyOwned(id session.SessionID) error {
	return &persistence.AlreadyOwnedError{SessionID: id}
}
