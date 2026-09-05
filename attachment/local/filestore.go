package local

import (
	cryptosha256 "crypto/sha256"
	"crypto/subtle"
	"hash"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dshgo/attachment"
)

// Verbatim content-addressed local file storage. Port of
// packages/attachment/attachment-local/src/file-store.ts (0.1.3-alpha.1):
// files are stored byte-for-byte with no normalization; the digest names a
// directory so the sanitized display name stays the stored leaf name, giving
// models and users a path that ends in the real filename.

var fileIDPattern = regexp.MustCompile(`^sha256:([a-f0-9]{64})$`)

var windowsDeviceName = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

func isWindowsDeviceName(name string) bool {
	dot := strings.Index(name, ".")
	stem := name
	if dot >= 0 {
		stem = name[:dot]
	}
	stem = strings.TrimRight(stem, ". ")
	return windowsDeviceName.MatchString(stem)
}

// utf8Prefix truncates to at most maxBytes without splitting a rune.
func utf8Prefix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	bytes := 0
	index := 0
	for runeIndex, character := range value {
		size := len(string(character))
		if bytes+size > maxBytes {
			return value[:index]
		}
		bytes += size
		index = runeIndex + size
	}
	return value
}

// FileLeafName sanitizes one caller display name into a safe stored leaf
// name. Both separator styles are stripped by hand: a POSIX host treats `\`
// as an ordinary character, so a Windows client's full local path would leak
// into the reference and the session log. Characters Windows refuses in file
// names become `_` so one reference stays valid on every supported host.
func FileLeafName(value string) string {
	if value == "" {
		return "file"
	}
	leafStart := strings.LastIndexAny(value, `/\`) + 1
	leaf := value[leafStart:]
	var clean strings.Builder
	for _, character := range leaf {
		if character <= 0x1f || character == 0x7f {
			continue
		}
		clean.WriteRune(character)
	}
	result := clean.String()
	for _, refused := range []string{"<", ">", ":", "\"", "|", "?", "*"} {
		result = strings.ReplaceAll(result, refused, "_")
	}
	result = strings.TrimSpace(result)
	result = strings.TrimRight(result, ". ")
	if isWindowsDeviceName(result) {
		result = "_" + result
	}
	result = strings.TrimRight(utf8Prefix(result, 255), ". ")
	if result == "" || result == "." || result == ".." {
		return "file"
	}
	return result
}

// ensureFileReference validates one durable file reference and returns its
// raw digest.
func ensureFileReference(ref attachment.FileAttachmentRef) (string, error) {
	match := fileIDPattern.FindStringSubmatch(ref.AttachmentID)
	if match == nil || ref.Name != FileLeafName(ref.Name) {
		return "", attachment.NewAttachmentError("File attachment reference is invalid.", attachment.CodeInvalidAttachmentRef)
	}
	return match[1], nil
}

// StoredFilePath derives the absolute immutable-object path for one stored
// file without reading the object.
func StoredFilePath(root string, ref attachment.FileAttachmentRef) (string, error) {
	sha256, err := ensureFileReference(ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "files", sha256[:2], sha256, ref.Name), nil
}

// storedFileObjectPath is the canonical object path shared by every display
// name for one digest.
func storedFileObjectPath(root, sha256 string) string {
	return filepath.Join(root, "file-objects", sha256[:2], sha256)
}

// SaveFileVerbatim commits one file byte-for-byte below the versioned
// attachment root.
func SaveFileVerbatim(root string, input attachment.SaveFileAttachment) (attachment.FileAttachmentRef, error) {
	sum := cryptosha256.Sum256(input.Data)
	sha256 := hex.EncodeToString(sum[:])
	ref := attachment.FileAttachmentRef{
		AttachmentID: "sha256:" + sha256,
		Name:         FileLeafName(input.Name),
		Bytes:        len(input.Data),
	}
	if err := publishFileObject(root, sha256, input.Data); err != nil {
		return attachment.FileAttachmentRef{}, err
	}
	if err := publishFileAlias(root, sha256, ref); err != nil {
		return attachment.FileAttachmentRef{}, err
	}
	return ref, nil
}

// SaveFileStreamVerbatim commits one file byte-for-byte from bounded chunks;
// the stream never retains the whole sequence in memory.
func SaveFileStreamVerbatim(root string, input attachment.SaveFileStreamAttachment) (attachment.FileAttachmentRef, error) {
	name := FileLeafName(input.Name)
	var hash hash.Hash = cryptosha256.New()
	object, err := stagedObjectPath(root)
	if err != nil {
		return attachment.FileAttachmentRef{}, err
	}
	defer os.Remove(object)
	handle, err := os.OpenFile(object, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	bytes := 0
	buffer := make([]byte, 1<<16)
	for {
		read, readErr := input.Data.Read(buffer)
		if read > 0 {
			if _, writeErr := handle.Write(buffer[:read]); writeErr != nil {
				handle.Close()
				return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, writeErr)
			}
			hash.Write(buffer[:read])
			bytes += read
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			handle.Close()
			return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, readErr)
		}
	}
	if err := handle.Sync(); err != nil {
		handle.Close()
		return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := handle.Close(); err != nil {
		return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	sha256 := hex.EncodeToString(hash.Sum(nil))
	finalPath := storedFileObjectPath(root, sha256)
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if linkErr := os.Link(object, finalPath); linkErr != nil {
		if !errors.Is(linkErr, os.ErrExist) {
			return attachment.FileAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, linkErr)
		}
	}
	_ = os.Chmod(finalPath, 0o400)
	ref := attachment.FileAttachmentRef{
		AttachmentID: "sha256:" + sha256,
		Name:         name,
		Bytes:        bytes,
	}
	if err := publishFileAlias(root, sha256, ref); err != nil {
		return attachment.FileAttachmentRef{}, err
	}
	return ref, nil
}

// publishFileObject writes the canonical object once; a dedup race verifies
// the existing object matches the digest.
func publishFileObject(root, sha256 string, data []byte) error {
	bucket := filepath.Join(root, "file-objects", sha256[:2])
	staging := filepath.Join(root, "tmp")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	random, err := randomHex(16)
	if err != nil {
		return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	temporary := filepath.Join(staging, random)
	target := storedFileObjectPath(root, sha256)
	if err := writeFileSync(temporary, data); err != nil {
		_ = os.Remove(temporary)
		return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := os.Link(temporary, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			_ = os.Remove(temporary)
			return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
		}
		existing, readErr := os.ReadFile(target)
		if readErr != nil || sha256 != hex.EncodeToString(func() []byte { sum := cryptosha256.Sum256(existing); return sum[:] }()) {
			_ = os.Remove(temporary)
			return attachment.NewAttachmentError("Stored file attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
		}
	}
	_ = os.Remove(temporary)
	_ = os.Chmod(target, 0o400)
	return nil
}

// publishFileAlias hard-links the display-name path to the canonical object;
// an existing alias is accepted (different names share one object).
func publishFileAlias(root, sha256 string, ref attachment.FileAttachmentRef) error {
	object := storedFileObjectPath(root, sha256)
	alias, err := StoredFilePath(root, ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := os.Link(object, alias); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
		}
		// An existing alias must resolve to the same object.
		existing, readErr := os.ReadFile(alias)
		if readErr == nil {
			sum := cryptosha256.Sum256(existing)
			if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(sha256)) != 1 {
				return attachment.NewAttachmentError("Stored file attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
			}
		}
	}
	return nil
}

// ReadFileStreamVerbatim reads one stored file through an io.ReadCloser and
// verifies its byte count and digest at close time. The caller owns Close.
func ReadFileStreamVerbatim(root string, ref attachment.FileAttachmentRef) (io.ReadCloser, error) {
	sha256, err := ensureFileReference(ref)
	if err != nil {
		return nil, err
	}
	path, err := StoredFilePath(root, ref)
	if err != nil {
		return nil, err
	}
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, attachment.NewAttachmentError("File attachment object is missing.", attachment.CodeAttachmentNotFound)
		}
		return nil, attachment.WrappedAttachmentError("Unable to read file attachment.", attachment.CodeAttachmentReadFailed, err)
	}
	return &verifyingReader{handle: handle, hash: cryptosha256.New(), want: sha256, bytes: ref.Bytes}, nil
}

type verifyingReader struct {
	handle *os.File
	hash   hash.Hash
	want   string
	bytes  int
	seen   int
	done   bool
}

func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.handle.Read(p)
	if n > 0 {
		r.hash.Write(p[:n])
		r.seen += n
	}
	if errors.Is(err, io.EOF) && !r.done {
		r.done = true
		r.handle.Close()
		if r.seen != r.bytes || hex.EncodeToString(r.hash.Sum(nil)) != r.want {
			return n, attachment.NewAttachmentError("Stored file attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
		}
	}
	return n, err
}

func (r *verifyingReader) Close() error {
	if !r.done {
		r.done = true
		r.handle.Close()
	}
	return nil
}

// stagedObjectPath reserves one staging slot for a streaming write.
func stagedObjectPath(root string) (string, error) {
	staging := filepath.Join(root, "tmp")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	random, err := randomHex(16)
	if err != nil {
		return "", attachment.WrappedAttachmentError("Unable to persist file attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	return filepath.Join(staging, random), nil
}

var _ attachment.FileStore = (*AttachmentStore)(nil)

// SaveFile implements attachment.FileStore.
func (s *AttachmentStore) SaveFile(input attachment.SaveFileAttachment) (attachment.FileAttachmentRef, error) {
	return SaveFileVerbatim(s.Root(), input)
}

// SaveFileStream implements attachment.FileStore.
func (s *AttachmentStore) SaveFileStream(input attachment.SaveFileStreamAttachment) (attachment.FileAttachmentRef, error) {
	return SaveFileStreamVerbatim(s.Root(), input)
}

// FileHostPath implements attachment.FileStore.
func (s *AttachmentStore) FileHostPath(ref attachment.FileAttachmentRef) (string, bool, error) {
	path, err := StoredFilePath(s.Root(), ref)
	if err != nil {
		return "", false, err
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return "", false, attachment.NewAttachmentError("File attachment object is missing.", attachment.CodeAttachmentNotFound)
		}
		return "", false, attachment.WrappedAttachmentError("Unable to read file attachment.", attachment.CodeAttachmentReadFailed, statErr)
	}
	if !info.Mode().IsRegular() {
		return "", false, attachment.NewAttachmentError("Stored file attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
	}
	return path, true, nil
}
