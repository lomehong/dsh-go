// Package atomicwrite ports packages/util/atomic-write: the shared atomic
// file replacement with Windows transient-interference retry. The rename
// over the target is the atomic replace; on Windows a reader holding the
// destination briefly makes the rename fail with access/sharing errors, so
// the complete temp file stays the rename source while we retry a bounded
// interval before giving up.
package atomicwrite

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"time"
)

// The Windows error codes behind the upstream EACCES/EBUSY/EPERM set.
// Numbers are defined on every platform (syscall.Errno compiles portably);
// the runtime.GOOS guard in isTransientWindowsRenameError is what makes the
// check Windows-only.
const (
	errAccessDenied     syscall.Errno = 5  // ERROR_ACCESS_DENIED
	errSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION
	errLockViolation    syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// Retry cadence for transient Windows rename interference (upstream:
// WINDOWS_RENAME_RETRY_INITIAL_MS/_MAX_MS/_LIMIT).
const (
	initialDelay = 20 * time.Millisecond
	maxDelay     = 200 * time.Millisecond
	maxRetries   = 8
)

// RenameReplacing renames temp over filename — the final step of an atomic
// replacement whose temp file the caller already wrote completely. Windows
// replacement retries transient EACCES/EBUSY/EPERM-style failures for a
// bounded interval (the complete temp file stays the rename source
// throughout); on any remaining failure the error is rethrown and removing
// the temp file stays the caller's cleanup.
func RenameReplacing(temp, filename string) error {
	delay := initialDelay
	for retries := 0; ; retries++ {
		err := os.Rename(temp, filename)
		if err == nil {
			return nil
		}
		if !isTransientWindowsRenameError(err) || retries >= maxRetries {
			return err
		}
		time.Sleep(delay)
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

// isTransientWindowsRenameError reports whether Windows reported temporary
// interference with an atomic replacement: access denied, sharing
// violation, or lock violation.
func isTransientWindowsRenameError(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	errno, ok := linkErr.Err.(syscall.Errno)
	if !ok {
		return false
	}
	switch errno {
	case errAccessDenied, errSharingViolation, errLockViolation:
		return true
	}
	return false
}
