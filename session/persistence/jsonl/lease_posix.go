//go:build !windows

package jsonl

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"dshgo/session"
)

// POSIX half of the kernel write lease: a non-blocking flock(2) on the lock
// file, with an inode recheck — a lock names an inode, not a path, so after
// locking the holder verifies the locked inode is still the file at the lock
// path and retries otherwise; an unlinked-and-recreated lock file carries a
// fresh inode, and a lock on the orphaned one proves nothing.

type heldKernelLock struct {
	close func()
}

func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return nil
}

func acquireKernelLock(path, dir string, id session.SessionID) (heldKernelLock, error) {
	// Bounded retry: locking an inode a releasing creator just unlinked (or
	// a recreated path) re-opens the fresh file; steady state needs one pass.
	for attempt := 0; attempt < 3; attempt++ {
		handle, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return heldKernelLock{}, err
		}
		keep := func(closeErr error) (heldKernelLock, error) {
			_ = handle.Close()
			if closeErr != nil {
				return heldKernelLock{}, closeErr
			}
			return heldKernelLock{}, errors.New("session lease: unavailable")
		}
		if err := syscall.Flock(int(handle.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EACCES) {
				_ = handle.Close()
				return heldKernelLock{}, alreadyOwned(id)
			}
			return keep(err)
		}
		heldInfo, err := handle.Stat()
		if err != nil {
			return keep(err)
		}
		current, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return keep(err)
		}
		if current != nil && os.SameFile(heldInfo, current) {
			return heldKernelLock{close: func() { _ = handle.Close() }}, nil
		}
		// The locked inode is no longer the file at the lock path: start
		// over against whatever now stands there.
		_ = handle.Close()
	}
	return heldKernelLock{}, fmt.Errorf("session %q lease could not stabilize its lock file", id)
}
