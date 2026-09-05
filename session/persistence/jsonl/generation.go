package jsonl

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"dshgo/session"
	"dshgo/sessionformat"
	"dshgo/sessionformatcatalog"
)

// Ensure-current: a stored Session predating the current format generation
// migrates on body read. The exact source path, bytes, and inode remain; the
// migration composes every required adjacent edge in memory, validates and
// syncs a same-directory temporary stage for only the final target, rechecks
// the source fingerprint, publishes the previously absent target without
// overwrite, and reopens it through current validation. Port of the released
// Session migration publication lifecycle (official tag dsh-v0.1.3-alpha.1).

// formatCatalog is the build-static codec + migration catalog (constructed
// lazily; compilation fails loud on the first use, never silently degrades).
var formatCatalog sessionformat.Catalog

func catalogForLoad() (sessionformat.Catalog, error) {
	if formatCatalog != nil {
		return formatCatalog, nil
	}
	catalog, err := sessionformatcatalog.New()
	if err != nil {
		return nil, fmt.Errorf("session format catalog: %w", err)
	}
	formatCatalog = catalog
	return catalog, nil
}

// classifyStoredLog reads only a log's header line and reports its stored
// format generation.
func classifyStoredLog(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
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
				return 0, errors.New("session header line exceeds 1 MiB")
			}
		}
		if readErr != nil {
			return 0, errors.New("empty or header-less session log")
		}
	}
	parsed, err := parseLineValue(bytes.TrimSuffix(buffer, []byte("\n")))
	if err != nil {
		return 0, errors.New("corrupt session log: header line is not valid JSON")
	}
	versionRaw, ok := parsed["version"]
	if !ok {
		return 0, errors.New("corrupt session log: first line is not a session header")
	}
	number, ok := versionRaw.(json.Number)
	if !ok {
		return 0, errors.New("corrupt session log: first line is not a session header")
	}
	version, err := number.Int64()
	if err != nil {
		return 0, errors.New("corrupt session log: header version must be an integer")
	}
	return version, nil
}

// EnsureCurrent returns the path to read for one stored generation: the
// source itself when current, or the freshly published successor after
// migrating a supported historical body. The second result reports whether
// a successor was published.
func (st *Store) EnsureCurrent(sourcePath string) (string, bool, error) {
	storedVersion, err := classifyStoredLog(sourcePath)
	if err != nil {
		return "", false, err
	}
	if storedVersion == session.SESSION_FORMAT_VERSION {
		return sourcePath, false, nil
	}
	if storedVersion > session.SESSION_FORMAT_VERSION {
		id := sessionIDOf(sourcePath)
		return "", false, &SessionFormatUnsupportedError{ID: id, Version: storedVersion}
	}
	catalog, err := catalogForLoad()
	if err != nil {
		return "", false, err
	}

	// One stable source snapshot drives the whole migration.
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", false, err
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		return "", false, err
	}
	fingerprint := fileFingerprint(sourceInfo)

	headerEnd := bytes.IndexByte(source, 0x0A)
	if headerEnd == -1 {
		return "", false, errors.New("empty or header-less session log")
	}
	var rows []json.RawMessage
	rowScanner := bytes.Split(source[headerEnd+1:], []byte("\n"))
	for _, row := range rowScanner {
		if len(row) == 0 {
			continue
		}
		rows = append(rows, json.RawMessage(row))
	}
	artifact, err := catalog.DecodeRecoverableArtifact(source[:headerEnd], rows)
	if err != nil {
		return "", false, err
	}
	migrated, err := catalog.Migrate(artifact)
	if err != nil {
		return "", false, err
	}
	encoded, err := catalog.EncodeCurrent(migrated)
	if err != nil {
		return "", false, err
	}

	dir := filepath.Dir(sourcePath)
	targetPath := filepath.Join(dir, GenerationBasename(session.SESSION_FORMAT_VERSION)+LogSuffix(st.suffix()))
	var target bytes.Buffer
	target.Write(encoded.Header)
	target.WriteByte('\n')
	for _, row := range encoded.Rows {
		target.Write(row)
		target.WriteByte('\n')
	}
	payload := target.Bytes()

	// A pre-existing target is accepted only as a regular current-format
	// file with exactly the expected bytes (a racing winner).
	if existing, statErr := os.Stat(targetPath); statErr == nil {
		if !existing.Mode().IsRegular() {
			return "", false, fmt.Errorf("session successor %s exists and is not a regular file", targetPath)
		}
		existingBytes, readErr := os.ReadFile(targetPath)
		if readErr != nil {
			return "", false, readErr
		}
		if !bytes.Equal(existingBytes, payload) {
			return "", false, fmt.Errorf("session successor %s exists with different bytes; refusing to overwrite committed evidence", targetPath)
		}
		return targetPath, false, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", false, statErr
	}

	// The source must not have changed under the migration.
	freshInfo, err := os.Stat(sourcePath)
	if err != nil {
		return "", false, err
	}
	if fileFingerprint(freshInfo) != fingerprint {
		return "", false, fmt.Errorf("session source %s changed during migration; restarting", sourcePath)
	}

	if err := publishNoOverwrite(dir, targetPath, payload); err != nil {
		return "", false, err
	}
	return targetPath, true, nil
}

// publishNoOverwrite stages one same-directory temp file (write + sync) and
// publishes it under the target name without ever overwriting: the hard
// link fails when the target exists. A filesystem without hard links falls
// back to an exclusive create. The namespace sync is best-effort (no
// portable directory fsync on Windows).
func publishNoOverwrite(dir, target string, payload []byte) error {
	stage, err := stagedPath(dir)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		file.Close()
		os.Remove(stage)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(stage)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(stage)
		return err
	}
	linkErr := os.Link(stage, target)
	if linkErr == nil {
		os.Remove(stage)
		return syncDirBestEffort(dir)
	}
	if !errors.Is(linkErr, os.ErrExist) {
		// Any non-ErrExist link failure (unsupported filesystem, permission)
		// falls back to an exclusive direct create, which carries the same
		// no-overwrite guarantee via O_EXCL.
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			os.Remove(stage)
			return err
		}
		if _, err := file.Write(payload); err != nil {
			file.Close()
			os.Remove(stage)
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			os.Remove(stage)
			return err
		}
		if err := file.Close(); err != nil {
			os.Remove(stage)
			return err
		}
		os.Remove(stage)
		return syncDirBestEffort(dir)
	}
	// A racing winner committed the target: accept only byte-identical.
	existing, readErr := os.ReadFile(target)
	os.Remove(stage)
	if readErr != nil {
		return readErr
	}
	if !bytes.Equal(existing, payload) {
		return fmt.Errorf("session successor %s exists with different bytes; refusing to overwrite committed evidence", target)
	}
	return nil
}

func stagedPath(dir string) (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return filepath.Join(dir, "session.migrating."+hex.EncodeToString(random)+".tmp"), nil
}

func syncDirBestEffort(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer handle.Close()
	_ = handle.Sync()
	return nil
}

func fileFingerprint(info os.FileInfo) string {
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

// sessionIDOf best-effort extracts the id for refusal diagnostics.
func sessionIDOf(path string) string {
	base := filepath.Base(filepath.Dir(path))
	return base
}
