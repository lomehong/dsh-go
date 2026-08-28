// Package identity ports packages/identity/anonymous-user-id: the
// per-harness-home anonymous user id shared by telemetry and feedback.
//
// The id is a random UUID persisted as a bare line in `.anonymous-user-id`
// inside the harness home, and never derived from the hostname, network
// address, git remote, or any other identifying source. It is scoped to the
// harness home, not the machine: every process sharing one `$DSH_HOME`
// reports the same id, and deleting the file mints a fresh identity on the
// next launch.
//
// Reads and writes are synchronous so boot-time and command consumers can
// use one API. The result is memoized per resolved file path: one process
// touches the disk once, and a file deleted mid-run keeps the process's id
// until the next launch.
package identity

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"dshgo/homepaths"
)

// AnonymousUserID is a harness-home-scoped anonymous user id (random UUID
// v4).
type AnonymousUserID = string

// AnonymousUserIDFileName is the file inside the harness home storing the
// id: a bare UUID line, no wrapper format.
const AnonymousUserIDFileName = ".anonymous-user-id"

// uuidPattern matches the persisted id shape.
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Options carries the ambient hooks for locating and generating the id;
// every field has a default.
type Options struct {
	// Getenv consults the environment for `DSH_HOME`; nil means os.Getenv.
	Getenv homepaths.Getenv
	// RandomUUID generates the id; nil means the crypto/rand v4 source
	// (test hook).
	RandomUUID func() string
}

// memo is the process-lifetime memo keyed by resolved file path, so distinct
// test homes never share an id.
var memo sync.Map

// GetOrCreateAnonymousUserID returns the harness home's anonymous user id,
// creating and persisting one on first use. A concurrent first launch is
// settled by an exclusive-create write: the loser rereads the winner's id.
// (A reread landing in the winner's narrow create-to-write window can still
// yield two per-process ids for that run; the next launch converges on the
// persisted one.) Persistence is best-effort — a write failure (read-only
// home) still returns a usable id for the current run so feedback and
// telemetry are never blocked.
func GetOrCreateAnonymousUserID(options Options) AnonymousUserID {
	file := filepath.Join(homepaths.ResolveDshHome("", options.Getenv), AnonymousUserIDFileName)
	if cached, ok := memo.Load(file); ok {
		return cached.(AnonymousUserID)
	}

	id := readPersistedID(file)
	if id == "" {
		generate := options.RandomUUID
		if generate == nil {
			generate = RandomUUID
		}
		created := generate()
		if persisted := persistID(file, created); persisted != "" {
			id = persisted
		} else {
			// Best-effort persistence: keep the fresh id in memory even when
			// the home is unwritable, so this run still reports a consistent
			// id.
			id = created
		}
	}
	memo.Store(file, id)
	return id
}

// RandomUUID generates one lowercase UUID v4 (the official crypto.randomUUID
// source).
func RandomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// readPersistedID reads a valid persisted id from the file, or "" when
// absent or corrupt.
func readPersistedID(file string) AnonymousUserID {
	raw, err := os.ReadFile(file)
	if err != nil {
		// Absent or unreadable: the caller mints and persists a fresh id.
		return ""
	}
	value := strings.TrimSpace(string(raw))
	if uuidPattern.MatchString(value) {
		return value
	}
	return ""
}

// persistID writes the id through the official ladder: exclusive create
// first; on refusal reread (adopting a concurrent winner), then a
// last-writer overwrite for a corrupt leftover. It returns the id that is
// durably on disk, or "" when nothing could be persisted.
func persistID(file, created string) AnonymousUserID {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return ""
	}
	if err := writeExclusive(file, created+"\n"); err == nil {
		return created
	}
	// A wx refusal covers both a concurrent winner and a pre-existing
	// corrupt file: the reread adopts a valid winner, and an invalid reread
	// falls through to the overwrite path. Other failures land there too,
	// accepted best-effort below.
	if adopted := readPersistedID(file); adopted != "" {
		return adopted
	}
	if err := os.WriteFile(file, []byte(created+"\n"), 0o644); err != nil {
		return ""
	}
	return created
}

// writeExclusive creates the file only when absent (the `wx` flag role).
func writeExclusive(file, content string) error {
	handle, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := handle.WriteString(content)
	closeErr := handle.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
