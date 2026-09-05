// JSONL PersistenceBackend adapter: wires the file store into the shared
// persistence coordinator. Port of the official index.ts backend class:
// side-effect-free location hints, non-mutating torn-tail reporting (the
// coordinator owns the repair decision), stat-identity revisions, and
// atomic first-batch materialization.
package jsonl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"dshgo/session"
	"dshgo/session/persistence"
)

// BackendName is the backend's name in dispose-failure aggregates.
const BackendName = "jsonl"

// Backend implements persistence.Backend over one Store root.
type Backend struct {
	Store *Store
	// writeLeases holds one kernel write lock per session directory for the
	// life of this backend's write handles: a second process's append
	// refuses with AlreadyOwnedError while the holder lives (official
	// SessionWriteLease; readers never touch the lock).
	leaseMu     sync.Mutex
	writeLeases map[string]*SessionWriteLease
}

// NewBackend builds the file backend over a session root directory.
func NewBackend(root string, compression Compression) *Backend {
	return &Backend{Store: &Store{Root: root, Compression: compression}}
}

// acquireLease takes (or reuses) this backend's kernel lock for one session
// directory.
func (b *Backend) acquireLease(cwd, id session.SessionID) error {
	dir := SessionDir(b.Store.Root, cwd, string(id))
	b.leaseMu.Lock()
	defer b.leaseMu.Unlock()
	if b.writeLeases == nil {
		b.writeLeases = map[string]*SessionWriteLease{}
	}
	if _, held := b.writeLeases[dir]; held {
		return nil
	}
	lease, err := AcquireWriteLease(dir, id)
	if err != nil {
		return err
	}
	b.writeLeases[dir] = lease
	return nil
}

// releaseLeases drops every held kernel lock (backend dispose boundary).
func (b *Backend) releaseLeases() {
	b.leaseMu.Lock()
	defer b.leaseMu.Unlock()
	for _, lease := range b.writeLeases {
		lease.Release()
	}
	b.writeLeases = map[string]*SessionWriteLease{}
}

// Name is the human-readable backend name.
func (b *Backend) Name() string { return BackendName }

// findByID scans every project scope for the id's artifact (the
// coordinator passes ids alone; load/resume identify a session by id
// across all stored scopes).
func (b *Backend) findByID(id session.SessionID) (string, error) {
	entries, err := os.ReadDir(b.Store.Root)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(b.Store.Root, project.Name())
		sessions, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, dir := range sessions {
			if !dir.IsDir() {
				continue
			}
			path, _, found := SelectGenerationLog(filepath.Join(projectDir, dir.Name()), b.Store.suffix())
			if !found {
				continue
			}
			header, ok, metaErr := readFirstLineHeader(path)
			if metaErr != nil {
				// A format refusal for THIS id must surface unwrapped with
				// the raw-log location: the user must see "upgrade the
				// harness", never "not found" or "corrupt".
				var unsupported *SessionFormatUnsupportedError
				if errors.As(metaErr, &unsupported) && unsupported.ID == id {
					return "", &persistence.FormatUnsupportedError{
						Message:  fmt.Sprintf("%s (raw log: %s)", unsupported.Error(), path),
						Location: &persistence.Location{Path: path},
					}
				}
				continue
			}
			if ok && header.ID == id {
				return path, nil
			}
		}
	}
	return "", nil
}

// readFirstLineHeader parses only the header record of a log.
func readFirstLineHeader(path string) (session.SessionHeader, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return session.SessionHeader{}, false, nil
	}
	defer file.Close()
	buffer := make([]byte, 0, 4096)
	one := make([]byte, 1)
	for {
		n, readErr := file.Read(one)
		if n > 0 {
			if one[0] == 0x0A {
				break
			}
			buffer = append(buffer, one[0])
			if len(buffer) > 1<<20 {
				return session.SessionHeader{}, false, errors.New("session header line exceeds 1 MiB")
			}
		}
		if readErr != nil {
			// A header-less fragment is not a session artifact.
			return session.SessionHeader{}, false, nil
		}
	}
	return ParseHeaderMeta(string(buffer))
}

// fileRevision is the source-qualified revision shared by full and
// lightweight reads. Dev/inode are omitted: Windows exposes no portable
// file identity, so size+mtime carries the change token (documented
// deviation from the official dev:ino:size:mtimeNs:ctimeNs tuple).
func fileRevision(info os.FileInfo) persistence.Revision {
	return persistence.Revision(fmt.Sprintf("jsonl:%d:%d", info.Size(), info.ModTime().UnixNano()))
}

// statByID resolves the artifact and its stat snapshot in one step.
func (b *Backend) statByID(id session.SessionID) (string, os.FileInfo, error) {
	path, err := b.findByID(id)
	if err != nil || path == "" {
		return "", nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, nil
		}
		return "", nil, err
	}
	return path, info, nil
}

// LoadStored reads a stored prefix without mutating anything. A torn tail
// is reported through the torn marker (the truncate-to offset) so the
// coordinator's commitRepair owns the truncation; the official backend
// stores recovered torn-frame events alongside, which only the zstd
// variant can produce (deferred with the zstd codec).
func (b *Backend) LoadStored(id session.SessionID) (*persistence.StoredPrefix, error) {
	path, info, err := b.statByID(id)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	// A stored Session predating the current generation migrates before the
	// body read returns: the source stays byte-identical, and the successor
	// (published without overwrite) becomes the read target. A newer stored
	// generation refuses with the upgrade direction.
	path, _, err = b.Store.EnsureCurrent(path)
	if err != nil {
		var unsupported *SessionFormatUnsupportedError
		if errors.As(err, &unsupported) {
			return nil, &persistence.FormatUnsupportedError{
				Message:  fmt.Sprintf("%s (raw log: %s)", unsupported.Error(), path),
				Location: &persistence.Location{Path: path},
			}
		}
		return nil, err
	}
	info, err = os.Stat(path)
	if err != nil {
		return nil, err
	}
	buffer, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	scan, err := ScanLog(buffer)
	if err != nil {
		return nil, err
	}
	prefix := &persistence.StoredPrefix{
		Meta:     scan.Meta,
		Events:   scan.Events,
		Revision: fileRevision(info),
	}
	if scan.CommittedBytes < int64(len(buffer)) {
		prefix.TornMarker = scan.CommittedBytes
	}
	return prefix, nil
}

// ReadStoredRevision reads one stored log's change token without loading
// its events; an empty revision means the identity is absent.
func (b *Backend) ReadStoredRevision(id session.SessionID) (persistence.Revision, error) {
	_, info, err := b.statByID(id)
	if err != nil || info == nil {
		return "", err
	}
	return fileRevision(info), nil
}

// AppendBatch durably appends one contiguous batch; when the artifact does
// not exist yet, the header and the first batch commit in one write. The
// write lease is acquired for an existing artifact at write-open and, for a
// created session, right before its first materializing write — an
// unmaterialized session has no filesystem footprint to lock ahead of.
func (b *Backend) AppendBatch(meta session.SessionHeader, events []session.Event, materialized bool) error {
	if !materialized {
		if err := b.acquireLease(meta.CWD, meta.ID); err != nil {
			return err
		}
		return b.Store.Create(meta, events)
	}
	if err := b.acquireLease(meta.CWD, meta.ID); err != nil {
		return err
	}
	return b.Store.Append(meta.CWD, string(meta.ID), 0, events)
}

// CommitRepair makes a crash repair durable: truncate the torn tail (iff
// the marker is present) and append closers (iff any). Two durable steps,
// not atomic — a file backend may truncate-then-append.
func (b *Backend) CommitRepair(meta session.SessionHeader, tornMarker any, closers []session.Event) error {
	if err := b.acquireLease(meta.CWD, meta.ID); err != nil {
		return err
	}
	path := b.Store.PathOf(meta.CWD, string(meta.ID))
	if tornMarker != nil {
		truncateTo, ok := tornMarker.(int64)
		if !ok {
			return fmt.Errorf("jsonl torn marker must be a truncate offset, got %T", tornMarker)
		}
		if err := os.Truncate(path, truncateTo); err != nil {
			return err
		}
	}
	return b.Store.Append(meta.CWD, string(meta.ID), 0, closers)
}

// List enumerates every stored session's metadata.
func (b *Backend) List() ([]session.SessionHeader, error) { return b.Store.List() }

// ListSnapshots lists metadata with cheap per-log change tokens.
func (b *Backend) ListSnapshots() ([]persistence.Snapshot, error) {
	entries, err := os.ReadDir(b.Store.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []persistence.Snapshot
	for _, project := range entries {
		if !project.IsDir() {
			continue
		}
		projectDir := filepath.Join(b.Store.Root, project.Name())
		sessions, err := os.ReadDir(projectDir)
		if err != nil {
			continue
		}
		for _, dir := range sessions {
			if !dir.IsDir() {
				continue
			}
			path, _, found := SelectGenerationLog(filepath.Join(projectDir, dir.Name()), b.Store.suffix())
			if !found {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			header, ok, metaErr := readFirstLineHeader(path)
			if metaErr != nil {
				return nil, metaErr
			}
			if ok {
				out = append(out, persistence.Snapshot{Header: header, Revision: fileRevision(info)})
			}
		}
	}
	return out, nil
}

// Close releases backend resources: every held kernel write lock.
func (b *Backend) Close() error {
	b.releaseLeases()
	return nil
}

// Locate points refusal diagnostics at the raw log.
func (b *Backend) Locate(meta session.SessionHeader) *persistence.Location {
	path := b.Store.PathOf(meta.CWD, string(meta.ID))
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return &persistence.Location{Path: path}
}

// ReadStoredRaw returns the artifact's own text, verbatim.
func (b *Backend) ReadStoredRaw(id session.SessionID) (persistence.RawArtifact, error) {
	path, _, err := b.statByID(id)
	if err != nil {
		return persistence.RawArtifact{}, err
	}
	if path == "" {
		return persistence.RawArtifact{}, &persistence.NotFoundError{SessionID: id}
	}
	buffer, err := os.ReadFile(path)
	if err != nil {
		return persistence.RawArtifact{}, err
	}
	content := string(buffer)
	meta, err := ScanLog(buffer)
	if err != nil {
		return persistence.RawArtifact{}, err
	}
	return persistence.RawArtifact{
		Meta:     meta.Meta,
		Filename: strings.TrimSuffix(filepath.Base(path), LogSuffix(b.Store.suffix())),
		Content:  content,
	}, nil
}
