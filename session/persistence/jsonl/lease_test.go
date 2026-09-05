package jsonl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"dshgo/session"
)

func TestWriteLeaseExcludesSecondAcquirer(t *testing.T) {
	dir := t.TempDir()
	id := session.SessionID("leased")
	first, err := AcquireWriteLease(dir, id)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := AcquireWriteLease(dir, id); err == nil {
		t.Fatal("a second acquirer must refuse while the first holds the lock")
	} else {
		if !asAlreadyOwned(err, probeTarget) {
			t.Fatalf("contention must surface AlreadyOwned, got %T", err)
		}
	}
	// Release lets the successor in.
	first.Release()
	second, err := AcquireWriteLease(dir, id)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.Release()
	second.Release() // idempotent
	// The POSIX lock file survives release (stable inode for later lockers).
	if _, statErr := statLeaseFile(filepath.Join(dir, LEASE_FILENAME)); statErr != nil && leaseFileExpected() {
		t.Fatalf("lock file must survive release: %v", statErr)
	}
}

func TestBackendAppendRefusesForeignHolder(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	// A foreign process holds the directory's write lease.
	dir := SessionDir(root, cwd, "contended")
	foreign, err := AcquireWriteLease(dir, "contended")
	if err != nil {
		t.Fatalf("foreign acquire: %v", err)
	}
	defer foreign.Release()

	backend := NewBackend(root, CompressionNone)
	defer backend.Close()
	header := session.SessionHeader{
		Version: session.SESSION_FORMAT_VERSION, ID: "contended", CreatedAt: 7, CWD: cwd,
	}
	if err := backend.AppendBatch(header, nil, true); err == nil {
		t.Fatal("an append under a foreign lease must refuse")
	} else if !asAlreadyOwned(err, probeTarget) {
		t.Fatalf("refusal must surface AlreadyOwned, got %T: %v", err, err)
	}

	// After the foreign holder releases, the append proceeds.
	foreign.Release()
	if err := backend.AppendBatch(header, nil, true); err != nil {
		t.Fatalf("append after the lease frees: %v", err)
	}
}

// asAlreadyOwned reports whether the error chain carries the stable
// ownership-contention message (persistence.AlreadyOwnedError).
func asAlreadyOwned(err error, _ *int) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if strings.HasSuffix(err.Error(), leaseOwnedMessage) {
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var probeTarget *int

const leaseOwnedMessage = "is already owned by an active write handle"

func statLeaseFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func leaseFileExpected() bool { return runtime.GOOS != "windows" }
