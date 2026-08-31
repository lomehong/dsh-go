// Package storagejson ports packages/storage/storage-json: the JSON storage
// backend — one human-readable document per unit under a configured root, a
// whole-unit file (single layout) or one document per record (per-record
// layout), published by atomic rewrite.
package storagejson

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"dshgo/atomicwrite"
)

// WriteAtomic durably replaces path with data: a same-directory temp file
// (fsynced, exclusive 0600) renamed over the target. Rename is an atomic
// replace on POSIX and on Windows (MoveFileExW with
// MOVEFILE_REPLACE_EXISTING), and replacement is the intended semantic —
// unlike the session-log backend's no-clobber protocol, a unit file has
// exactly one writer per process and last-write-wins is correct. On POSIX
// the parent directory is fsynced after the rename so the new entry is
// crash-durable.
func WriteAtomic(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+randomName()+".tmp")
	file, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := atomicwrite.RenameReplacing(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows rejects read handles on directories.
		return nil
	}
	return fsyncDirectory(filepath.Dir(path))
}

// fsyncDirectory fsyncs a POSIX directory so a just-renamed entry is
// crash-durable.
func fsyncDirectory(path string) error {
	handle, err := os.Open(path)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// randomName is a random hex token for temp-file names.
func randomName() string {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%x", os.Getpid())
	}
	return hex.EncodeToString(raw)
}
