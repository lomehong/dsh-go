package local_test

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dshgo/attachment"
	"dshgo/attachment/local"
)

// pngBytes encodes one solid RGBA PNG of the given size.
func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 128, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, picture); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buffer.Bytes()
}

// pngBytesWithText embeds one validly CRC'd tEXt metadata chunk into an
// otherwise clean PNG, forcing the normalization re-encode path.
func pngBytesWithText(t *testing.T, width, height int) []byte {
	t.Helper()
	clean := pngBytes(t, width, height)
	payload := []byte("Comment\x00synthetic metadata")
	body := append([]byte("tEXt"), payload...)
	crc := crc32.ChecksumIEEE(body)
	annotated := append([]byte{}, clean[:8]...)
	annotated = binary.BigEndian.AppendUint32(annotated, uint32(len(payload)))
	annotated = append(annotated, body...)
	annotated = binary.BigEndian.AppendUint32(annotated, crc)
	annotated = append(annotated, clean[8:]...)
	return annotated
}

func jpegBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			picture.Set(x, y, color.RGBA{R: uint8(x * 8), G: uint8(y * 8), B: 64, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, picture, nil); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buffer.Bytes()
}

func gifBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := image.NewPaletted(image.Rect(0, 0, width, height), color.Palette{color.RGBA{G: 255, A: 255}, color.RGBA{B: 255, A: 255}})
	var buffer bytes.Buffer
	if err := gif.Encode(&buffer, picture, nil); err != nil {
		t.Fatalf("gif encode: %v", err)
	}
	return buffer.Bytes()
}

func testStore(t *testing.T, mutate func(*local.Config)) *local.AttachmentStore {
	t.Helper()
	config := local.Config{DSHHome: t.TempDir()}
	if mutate != nil {
		mutate(&config)
	}
	return local.New(config)
}

func TestSaveAndReadRoundTrip(t *testing.T) {
	store := testStore(t, nil)
	source := pngBytes(t, 16, 16)
	ref, err := store.SaveImage(attachment.SaveImageAttachment{Data: source, MediaType: attachment.MediaPNG, Name: "shot.png"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !strings.HasPrefix(ref.AttachmentID, "sha256:") || len(ref.AttachmentID) != len("sha256:")+64 {
		t.Fatalf("id = %q", ref.AttachmentID)
	}
	if ref.MediaType != attachment.MediaPNG || ref.Bytes != len(source) || ref.Width != 16 || ref.Height != 16 || ref.Name != "shot.png" {
		t.Fatalf("ref = %+v", ref)
	}
	// The normalized object exists at its content address.
	path, ok, err := store.ImageHostPath(ref)
	if err != nil || !ok {
		t.Fatalf("host path = %q %v %v", path, ok, err)
	}
	if filepath.Base(filepath.Dir(path)) != ref.AttachmentID[len("sha256:"):][:2] {
		t.Fatalf("bucket = %q", filepath.Dir(path))
	}
	stored, err := store.ReadImage(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(stored.Data, source) {
		t.Fatal("round trip changed the bytes")
	}
}

func TestSaveRejectsRefusals(t *testing.T) {
	store := testStore(t, func(config *local.Config) { config.MaxImageBytes = 64 })
	if _, err := store.SaveImage(attachment.SaveImageAttachment{Data: pngBytes(t, 32, 32), MediaType: attachment.MediaPNG}); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeImageTooLarge {
		t.Fatalf("oversize = %v", err)
	}
	// Declared type must match the decoded bytes (default byte budget).
	roomy := testStore(t, nil)
	if _, err := roomy.SaveImage(attachment.SaveImageAttachment{Data: jpegBytes(t, 8, 8), MediaType: attachment.MediaPNG}); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeImageTypeMismatch {
		t.Fatalf("mismatch = %v", err)
	}
	// Truncated bytes fail the full-decode admission.
	truncated := pngBytes(t, 8, 8)[:20]
	if _, err := roomy.SaveImage(attachment.SaveImageAttachment{Data: truncated, MediaType: attachment.MediaPNG}); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeInvalidImage {
		t.Fatalf("truncated = %v", err)
	}
}

func TestReadVerifiesIntegrity(t *testing.T) {
	store := testStore(t, nil)
	ref, err := store.SaveImage(attachment.SaveImageAttachment{Data: pngBytes(t, 8, 8), MediaType: attachment.MediaPNG})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	// A missing object is a not-found fault.
	missing := ref
	missing.AttachmentID = "sha256:" + strings.Repeat("ab", 32)
	if _, err := store.ReadImage(missing); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeAttachmentNotFound {
		t.Fatalf("missing = %v", err)
	}
	// A tampered object fails digest verification. The published object is
	// read-only; the test lifts that to tamper with it.
	path, _, _ := store.ImageHostPath(ref)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := store.ReadImage(ref); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeAttachmentCorrupt {
		t.Fatalf("tampered = %v", err)
	}
	// A malformed reference is invalid.
	if _, err := store.ReadImage(attachment.ImageAttachmentRef{AttachmentID: "../escape"}); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeInvalidAttachmentRef {
		t.Fatalf("invalid ref = %v", err)
	}
}

func TestIdenticalBytesDeduplicate(t *testing.T) {
	store := testStore(t, nil)
	source := pngBytes(t, 12, 12)
	first, err := store.SaveImage(attachment.SaveImageAttachment{Data: source, MediaType: attachment.MediaPNG})
	if err != nil {
		t.Fatalf("first save: %v", err)
	}
	second, err := store.SaveImage(attachment.SaveImageAttachment{Data: source, MediaType: attachment.MediaPNG})
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if first.AttachmentID != second.AttachmentID {
		t.Fatalf("ids diverged: %q vs %q", first.AttachmentID, second.AttachmentID)
	}
}

func TestNormalizationReencodesMetadataAndOversize(t *testing.T) {
	// A tiny pixel budget forces downscaling; the annotated source forces
	// the re-encode path, and the tEXt chunk must not survive.
	store := testStore(t, func(config *local.Config) {
		config.NormalizedImageMaxPixels = 64
		config.NormalizedImageMaxDimension = 8
	})
	ref, err := store.SaveImage(attachment.SaveImageAttachment{Data: pngBytesWithText(t, 64, 64), MediaType: attachment.MediaPNG})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if ref.Width*ref.Height > 64 || ref.Width > 8 || ref.Height > 8 {
		t.Fatalf("normalized = %dx%d", ref.Width, ref.Height)
	}
	if ref.OriginalDimensions == nil || ref.OriginalDimensions.Width != 64 || ref.OriginalDimensions.Height != 64 {
		t.Fatalf("original = %+v", ref.OriginalDimensions)
	}
	stored, err := store.ReadImage(ref)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Normalized output is a clean PNG whose chunk stream carries no tEXt.
	if bytes.Contains(stored.Data, []byte("tEXt")) {
		t.Fatal("metadata survived normalization")
	}
}

func TestGIFNormalizesToFirstFramePNG(t *testing.T) {
	store := testStore(t, nil)
	ref, err := store.SaveImage(attachment.SaveImageAttachment{Data: gifBytes(t, 8, 8), MediaType: attachment.MediaGIF})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if ref.MediaType != attachment.MediaPNG {
		t.Fatalf("normalized media type = %q", ref.MediaType)
	}
}

func TestWebPHeaderProbeAdmits(t *testing.T) {
	store := testStore(t, nil)
	// Minimal VP8L WebP: RIFF/WEBP container with a 4x2 lossless frame
	// header. Admitted through the header probe; the bytes cannot be
	// transcoded, but a fitting WebP passes through byte-identically.
	webp := []byte{
		'R', 'I', 'F', 'F', 0x0E, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P',
		'V', 'P', '8', 'L', 0x02, 0x00, 0x00, 0x00,
		0x2F, 0x00, 0x00, 0x00, 0x00,
	}
	ref, err := store.SaveImage(attachment.SaveImageAttachment{Data: webp, MediaType: attachment.MediaWebP})
	if err != nil {
		t.Fatalf("save webp: %v", err)
	}
	if ref.MediaType != attachment.MediaWebP || ref.Width != 1 || ref.Height != 1 {
		t.Fatalf("ref = %+v", ref)
	}
}

func TestBatchFailureStartsNoWrites(t *testing.T) {
	store := testStore(t, func(config *local.Config) { config.MaxImagesPerMessage = 1 })
	_, err := store.SaveImages([]attachment.SaveImageAttachment{
		{Data: pngBytes(t, 8, 8), MediaType: attachment.MediaPNG},
		{Data: pngBytes(t, 8, 8), MediaType: attachment.MediaPNG},
	})
	if err == nil || err.(*attachment.AttachmentError).Code != attachment.CodeTooManyImages {
		t.Fatalf("batch = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), "objects"))
	if err == nil && len(entries) != 0 {
		t.Fatalf("failed batch wrote %d objects", len(entries))
	}
	// An ordered valid batch commits every member in order (its own store:
	// the refusal store caps batches at one).
	ordered := testStore(t, nil)
	refs, err := ordered.SaveImages([]attachment.SaveImageAttachment{
		{Data: pngBytes(t, 8, 8), MediaType: attachment.MediaPNG},
		{Data: gifBytes(t, 8, 8), MediaType: attachment.MediaGIF},
		{Data: jpegBytes(t, 8, 8), MediaType: attachment.MediaJPEG},
	})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(refs) != 3 || refs[0].MediaType != attachment.MediaPNG || refs[1].MediaType != attachment.MediaPNG || refs[2].MediaType != attachment.MediaJPEG {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestValidateImageTouchesNoStorage(t *testing.T) {
	store := testStore(t, nil)
	if err := store.ValidateImage(attachment.SaveImageAttachment{Data: pngBytes(t, 8, 8), MediaType: attachment.MediaPNG}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "objects")); !os.IsNotExist(err) {
		t.Fatalf("validation wrote storage: %v", err)
	}
}

func TestReadImageRequestUnsupported(t *testing.T) {
	store := testStore(t, nil)
	_, err := store.ReadImageRequest(attachment.ImageAttachmentRef{}, attachment.ImageRequestPolicy{})
	if err == nil || err.(*attachment.AttachmentError).Code != attachment.CodeProjectionUnsupported {
		t.Fatalf("request = %v", err)
	}
}
