package local

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"dshgo/attachment"
)

// idPattern matches the durable attachment-id form `sha256:<64 hex>`.
var idPattern = regexp.MustCompile(`^sha256:([a-f0-9]{64})$`)

// digestHex hashes one payload with sha256.
func digestHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// displayName strips local path information from a submitted display name.
// Both separator styles are stripped by hand: a POSIX host treats `\` as an
// ordinary character, so filepath.Base would keep a Windows client's full
// local path and leak it into the reference and the session log.
func displayName(value string) string {
	if value == "" {
		return ""
	}
	slash := strings.LastIndexByte(value, '/')
	backslash := strings.LastIndexByte(value, '\\')
	leaf := value[max(slash, backslash)+1:]
	var clean strings.Builder
	for _, r := range leaf {
		if r <= 0x1F || r == 0x7F {
			continue
		}
		clean.WriteRune(r)
	}
	cleaned := strings.TrimSpace(clean.String())
	if len(cleaned) > 255 {
		cleaned = cleaned[:255]
	}
	return cleaned
}

// ensureReference validates one durable reference and returns its sha256
// digest.
func ensureReference(ref attachment.ImageAttachmentRef) (string, error) {
	match := idPattern.FindStringSubmatch(ref.AttachmentID)
	if match == nil {
		return "", attachment.NewAttachmentError("Attachment reference is invalid.", attachment.CodeInvalidAttachmentRef)
	}
	return match[1], nil
}

// NormalizedImagePath derives the absolute immutable-object path for one
// normalized attachment: a provider-local path without reading the object.
func NormalizedImagePath(root string, ref attachment.ImageAttachmentRef) (string, error) {
	sha256, err := ensureReference(ref)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "objects", sha256[:2], sha256), nil
}

// preparedImageFile is a fully prepared normalized object, verified before
// any batch member is persisted.
type preparedImageFile struct {
	data []byte
	ref  attachment.ImageAttachmentRef
}

// prepareImageFile decodes, normalizes, and verifies one submitted image
// without touching storage.
func prepareImageFile(input attachment.SaveImageAttachment, limits attachment.ImageAttachmentLimits, policy NormalizationPolicy) (preparedImageFile, error) {
	if len(input.Data) > limits.MaxImageBytes {
		return preparedImageFile{}, attachment.NewAttachmentError("Image exceeds the configured byte limit.", attachment.CodeImageTooLarge)
	}
	detectedFacts, err := detectImage(input.Data, limits.MaxImagePixels, limits.MaxImageDimension)
	if err != nil {
		return preparedImageFile{}, err
	}
	if detectedFacts.mediaType != input.MediaType {
		return preparedImageFile{}, attachment.NewAttachmentError("Declared image type does not match its bytes.", attachment.CodeImageTypeMismatch)
	}
	normalized, err := normalizeImage(input.Data, detectedFacts, policy)
	if err != nil {
		return preparedImageFile{}, err
	}
	sha256 := digestHex(normalized.data)
	name := displayName(input.Name)
	downscaled := detectedFacts.width != normalized.width || detectedFacts.height != normalized.height
	ref := attachment.ImageAttachmentRef{
		AttachmentID: "sha256:" + sha256,
		MediaType:    normalized.mediaType,
		Width:        normalized.width,
		Height:       normalized.height,
		Bytes:        len(normalized.data),
		Name:         name,
	}
	if downscaled {
		ref.OriginalDimensions = &attachment.ImageDimensions{Width: detectedFacts.width, Height: detectedFacts.height}
	}
	return preparedImageFile{data: normalized.data, ref: ref}, nil
}

// commitPreparedImageFile publishes one already verified normalized image
// below a versioned attachment root. Publication is content-addressed:
// a concurrent save of identical bytes deduplicates through the link race,
// and the target is verified against the reference digest either way.
func commitPreparedImageFile(root string, prepared preparedImageFile) (attachment.ImageAttachmentRef, error) {
	sha256, err := ensureReference(prepared.ref)
	if err != nil {
		return attachment.ImageAttachmentRef{}, err
	}
	if digestHex(prepared.data) != sha256 || len(prepared.data) != prepared.ref.Bytes {
		return attachment.ImageAttachmentRef{}, attachment.NewAttachmentError("Prepared attachment bytes do not match their reference.", attachment.CodeAttachmentCorrupt)
	}
	bucket := filepath.Join(root, "objects", sha256[:2])
	staging := filepath.Join(root, "tmp")
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	random, err := randomHex(16)
	if err != nil {
		return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	temporary := filepath.Join(staging, random)
	target, err := NormalizedImagePath(root, prepared.ref)
	if err != nil {
		return attachment.ImageAttachmentRef{}, err
	}
	cleanup := func() {
		_ = os.Remove(temporary)
	}
	if err := writeFileSync(temporary, prepared.data); err != nil {
		cleanup()
		return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	if err := os.Link(temporary, target); err != nil {
		if !errors.Is(err, os.ErrExist) {
			cleanup()
			return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
		}
		// Dedup race: verify the existing object matches the reference.
		existing, readErr := os.ReadFile(target)
		if readErr != nil || digestHex(existing) != sha256 {
			cleanup()
			return attachment.ImageAttachmentRef{}, attachment.NewAttachmentError("Stored attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
		}
	}
	if err := os.Remove(temporary); err != nil {
		cleanup()
		return attachment.ImageAttachmentRef{}, attachment.WrappedAttachmentError("Unable to persist image attachment.", attachment.CodeAttachmentWriteFailed, err)
	}
	// The target remains the sole link for a new object; read-only mode
	// keeps published objects immutable on both dedup paths.
	_ = os.Chmod(target, 0o400)
	return prepared.ref, nil
}

// writeFileSync writes one file and syncs its contents before returning.
func writeFileSync(path string, data []byte) error {
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := handle.Write(data); err != nil {
		_ = handle.Close()
		return err
	}
	if err := handle.Sync(); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

// readImageFile reads and verifies one content-addressed image. The digest
// proves these are the exact bytes admission fully decoded, so the read path
// only re-derives the header fields.
func readImageFile(root string, ref attachment.ImageAttachmentRef) (attachment.StoredImageAttachment, error) {
	sha256, err := ensureReference(ref)
	if err != nil {
		return attachment.StoredImageAttachment{}, err
	}
	path, err := NormalizedImagePath(root, ref)
	if err != nil {
		return attachment.StoredImageAttachment{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return attachment.StoredImageAttachment{}, attachment.NewAttachmentError("Attachment object is missing.", attachment.CodeAttachmentNotFound)
		}
		return attachment.StoredImageAttachment{}, attachment.WrappedAttachmentError("Unable to read image attachment.", attachment.CodeAttachmentReadFailed, err)
	}
	if digestHex(data) != sha256 {
		return attachment.StoredImageAttachment{}, attachment.NewAttachmentError("Stored attachment failed integrity verification.", attachment.CodeAttachmentCorrupt)
	}
	metadata, err := probeImage(data)
	if err != nil {
		return attachment.StoredImageAttachment{}, err
	}
	if metadata.mediaType != ref.MediaType || len(data) != ref.Bytes || metadata.width != ref.Width || metadata.height != ref.Height {
		return attachment.StoredImageAttachment{}, attachment.NewAttachmentError("Stored attachment metadata does not match its reference.", attachment.CodeAttachmentCorrupt)
	}
	return attachment.StoredImageAttachment{Ref: ref, Data: data}, nil
}

// randomHex returns hex-encoded cryptographic randomness.
func randomHex(bytesCount int) (string, error) {
	buffer := make([]byte, bytesCount)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("random source failed: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}
