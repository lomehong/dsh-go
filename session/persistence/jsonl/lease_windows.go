//go:build windows

package jsonl

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"dshgo/session"
)

// Windows half of the kernel write lease: a named kernel semaphore derived
// from the lock path — never a file lock or handle, so readers, searches,
// and directory removal proceed freely while the lock is held. A sharing
// style contention (the semaphore already at its count) maps to
// AlreadyOwnedError; the kernel releases the semaphore's last handle on any
// process death.

const (
	waitObject0        = 0
	waitTimeout        = 0x00000102
	errorAlreadyExists = 183
)

type heldKernelLock struct {
	close func()
}

// kernel32 semaphore bindings (x/sys exposes WaitForSingleObject only).
var (
	modkernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procCreateSemaphoreW = modkernel32.NewProc("CreateSemaphoreW")
	procReleaseSemaphore = modkernel32.NewProc("ReleaseSemaphore")
)

func createSemaphoreW(attrs *windows.SecurityAttributes, initial, maximum int32, name *uint16) (windows.Handle, error) {
	r1, _, callErr := procCreateSemaphoreW.Call(
		uintptr(unsafe.Pointer(attrs)),
		uintptr(initial),
		uintptr(maximum),
		uintptr(unsafe.Pointer(name)),
	)
	if r1 == 0 {
		// A named semaphore that already exists returns a valid handle with
		// ERROR_ALREADY_EXISTS — fine: acquiring waits on its count below.
		if errno, ok := callErr.(windows.Errno); ok && errno == errorAlreadyExists {
			return windows.Handle(r1), nil
		}
		return 0, callErr
	}
	return windows.Handle(r1), nil
}

func releaseSemaphore(handle windows.Handle, count int32) error {
	r1, _, callErr := procReleaseSemaphore.Call(uintptr(handle), uintptr(count), 0)
	if r1 == 0 {
		return callErr
	}
	return nil
}

func ensureDir(dir string) error {
	// Owner-only like the materialized directories; an existing directory
	// is fine.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return nil
}

func semaphoreName(path string) string {
	absolute, err := absoluteNormalized(path)
	if err != nil {
		absolute = path
	}
	sum := sha256.Sum256([]byte(strings.ToLower(absolute)))
	return `Local\dsh-session-` + hex.EncodeToString(sum[:16])
}

func acquireKernelLock(path, dir string, id session.SessionID) (heldKernelLock, error) {
	name, err := windows.UTF16PtrFromString(semaphoreName(path))
	if err != nil {
		return heldKernelLock{}, err
	}
	// CreateSemaphoreW reports ERROR_ALREADY_EXISTS when the name exists —
	// that is fine: acquiring waits on its count below.
	handle, err := createSemaphoreW(nil, 1, 1, name)
	if err != nil {
		return heldKernelLock{}, err
	}
	if handle == 0 {
		return heldKernelLock{}, fmt.Errorf("session %q lease could not create its kernel semaphore", id)
	}
	event, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		windows.CloseHandle(handle)
		return heldKernelLock{}, err
	}
	if errors.Is(err, windows.Errno(waitTimeout)) || event == uint32(waitTimeout) {
		windows.CloseHandle(handle)
		return heldKernelLock{}, alreadyOwned(id)
	}
	if event != uint32(waitObject0) {
		windows.CloseHandle(handle)
		return heldKernelLock{}, fmt.Errorf("session %q lease wait returned %#x", id, event)
	}
	return heldKernelLock{close: func() {
		// Release then close: closing the last handle releases the
		// semaphore entirely (kernel-arbitrated on process death too).
		_ = releaseSemaphore(handle, 1)
		_ = windows.CloseHandle(handle)
	}}, nil
}

func absoluteNormalized(path string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	// Long-path prefixed form keeps the semaphore name stable across cwd and
	// drive-letter casing; the name itself hashes a normalized lower-case
	// spelling so every spelling of one path contends on one semaphore.
	buffer := make([]uint16, 4096)
	length, err := windows.GetFullPathName(pointer, uint32(len(buffer)), &buffer[0], nil)
	if length == 0 || err != nil || int(length) >= len(buffer) {
		return "", errors.New("session lease: cannot absolutize the lock path")
	}
	return windows.UTF16ToString(buffer[:length]), nil
}
