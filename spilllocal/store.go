// Package spilllocal ports @deepseek-ai/dsh-spill-local: the
// host-filesystem implementation of the spill storage seam. Persists a
// tool's oversized text to a private, session-scoped file (traversal-safe
// naming and an exclusive owner-only write) and returns a path locator plus
// local read/grep retrieval guidance. After activation it runs one
// best-effort startup sweep that reclaims spill files older than
// CleanupPeriodDays.
package spilllocal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dshgo/spill"
)

// spillSessionID aliases the seam's session id type.
type spillSessionID = spill.SessionID

// DefaultRootPrefix is shared by default-root creation and startup discovery.
const DefaultRootPrefix = "dsh-spill-"

// defaultRootSuffixChars is the alphabet of the fixed 6-character suffix this
// port appends when creating a default root, so discovery's exact-shape regex
// keeps matching backend-created roots.
const defaultRootSuffixChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// defaultRoot is the lazily-created private per-process spill root.
var defaultRoot string

// PrivateRoot returns the lazily-created private per-process spill root.
func PrivateRoot() (string, error) {
	if defaultRoot != "" {
		return defaultRoot, nil
	}
	root, err := makeTempRoot(os.TempDir())
	if err != nil {
		return "", err
	}
	defaultRoot = root
	return defaultRoot, nil
}

// makeTempRoot creates one `<prefix><6 alnum>` directory under base,
// mirroring the exact mkdtemp shape discovery matches.
func makeTempRoot(base string) (string, error) {
	for attempt := 0; attempt < 64; attempt++ {
		suffix := make([]byte, 6)
		buf := make([]byte, 6)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("spill-local: entropy unavailable for a default spill root: %w", err)
		}
		for i, b := range buf {
			suffix[i] = defaultRootSuffixChars[int(b)%len(defaultRootSuffixChars)]
		}
		path := filepath.Join(base, DefaultRootPrefix+string(suffix))
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return path, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", err
	}
	return "", errors.New("spill-local: could not allocate a unique default spill root")
}

// encodeSegment encodes an arbitrary string as one safe path segment,
// injectively over all Go strings. A session id / suggested name is
// untrusted input, so this neutralizes `../`, absolute paths, NUL, and
// separators before any filesystem use. Each rune is kept literal
// (`[A-Za-z0-9._-]`, minus `~`) or escaped as `~XXXX`; `~` is itself
// escaped, so the mapping is reversible and distinct inputs never collide.
// The whole-segment tokens `.`/`..` are escaped so they can never traverse.
// An empty string encodes to `~` (never an empty segment).
func encodeSegment(raw string) string {
	if raw == "" {
		return "~"
	}
	if raw == "." {
		return "~002E"
	}
	if raw == ".." {
		return "~002E~002E"
	}
	out := make([]byte, 0, len(raw))
	for _, r := range raw {
		if r != '~' && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			out = append(out, byte(r))
			continue
		}
		out = append(out, []byte(fmt.Sprintf("~%04X", r))...)
	}
	return string(out)
}

// sessionDirName derives the stable session-scoped directory name under a
// spill root: `session-` plus the first 12 hex characters of
// sha256(sessionID).
func sessionDirName(sessionID spillSessionID) string {
	sum := sha256.Sum256([]byte(sessionID))
	return "session-" + hex.EncodeToString(sum[:])[:12]
}

// SessionDir derives the stable session-scoped directory under a spill root.
func SessionDir(root string, sessionID spillSessionID) string {
	return filepath.Join(root, sessionDirName(sessionID))
}

// SaveTextOptions are the inputs needed to save a local spill file.
type SaveTextOptions struct {
	// Root is the spill root.
	Root string
	// SessionID is the owning session id.
	SessionID spillSessionID
	// SuggestedName is the caller-suggested filename.
	SuggestedName string
	// Content is the full text to persist.
	Content string
}

// SavedText is a written spill file.
type SavedText struct {
	// Path is the absolute saved path.
	Path string
	// Bytes is the UTF-8 content length.
	Bytes int
}

// SaveTextFile writes text to a fresh 0600 file below its private session
// directory, returning the saved path and UTF-8 byte length.
func SaveTextFile(options SaveTextOptions) (SavedText, error) {
	dir := SessionDir(options.Root, options.SessionID)
	path := filepath.Join(dir, randomHex(6)+"-"+encodeSegment(options.SuggestedName))
	for {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return SavedText{}, err
		}
		handle, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			defer handle.Close()
			if _, err := handle.WriteString(options.Content); err != nil {
				return SavedText{}, err
			}
			return SavedText{Path: path, Bytes: len(options.Content)}, nil
		}
		// Requires another process to remove the directory between
		// MkdirAll and OpenFile, or an external permission/IO race.
		if os.IsNotExist(err) {
			continue
		}
		return SavedText{}, err
	}
}

// randomHex returns n random bytes as 16 lowercase-hex per 8... exactly 2*n
// hex characters, mirroring randomBytes(n).toString('hex').
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// A spill file needs an unpredictable name; entropy loss is a hard
		// backend failure surfaced to SaveText's caller.
		panic(fmt.Sprintf("spill-local: entropy unavailable: %v", err))
	}
	return hex.EncodeToString(buf)
}
