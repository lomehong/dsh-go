package local

import (
	"path/filepath"

	"dshgo/attachment"
)

// Default admission and normalization limits (official defaults).
const (
	// DefaultMaxImageBytes: oversized sources are refused, not shrunk.
	DefaultMaxImageBytes = 20 * 1024 * 1024
	// DefaultMaxImagesPerMessage: maximum images in one prompt.
	DefaultMaxImagesPerMessage = 20
	// DefaultMaxMessageImageBytes: maximum aggregate image bytes in one
	// prompt.
	DefaultMaxMessageImageBytes = 200 * 1024 * 1024
	// DefaultMaxImagePixels: maximum intrinsic pixels for one submitted
	// image.
	DefaultMaxImagePixels = 64_000_000
	// DefaultMaxImageDimension: default per-side pixel cap for one
	// submitted image.
	DefaultMaxImageDimension = 8192
	// DefaultNormalizedImageMaxPixels is the total-pixel budget of the
	// stored normalized image. A larger source is admitted and downscaled
	// proportionally, so admission bounds what rides every later model
	// request without refusing ordinary large sources.
	DefaultNormalizedImageMaxPixels = 2048 * 2048
	// DefaultNormalizedImageMaxDimension is the long-edge cap of the
	// stored normalized image, applied after the total-pixel budget.
	DefaultNormalizedImageMaxDimension = 8192
	// DefaultNormalizedImageMaxBytes is the encoded-byte target for one
	// stored normalized image.
	DefaultNormalizedImageMaxBytes = 4 * 1024 * 1024
)

// Config is the local attachment backend configuration.
type Config struct {
	// DSHHome is the explicit harness home root.
	DSHHome                     string
	MaxImageBytes               int
	MaxImagesPerMessage         int
	MaxMessageImageBytes        int
	MaxImagePixels              int
	MaxImageDimension           int
	NormalizedImageMaxPixels    int
	NormalizedImageMaxDimension int
	NormalizedImageMaxBytes     int
}

// AttachmentStore is the persistent content-addressed local attachment
// store, rooted at `<DSH_HOME>/attachments/v1`.
type AttachmentStore struct {
	root                string
	limits              attachment.ImageAttachmentLimits
	normalizationPolicy NormalizationPolicy
}

// New resolves the store configuration and roots the backend.
func New(config Config) *AttachmentStore {
	maxImageBytes := orDefault(config.MaxImageBytes, DefaultMaxImageBytes)
	maxImagesPerMessage := orDefault(config.MaxImagesPerMessage, DefaultMaxImagesPerMessage)
	maxMessageImageBytes := orDefault(config.MaxMessageImageBytes, DefaultMaxMessageImageBytes)
	maxImagePixels := orDefault(config.MaxImagePixels, DefaultMaxImagePixels)
	maxImageDimension := orDefault(config.MaxImageDimension, DefaultMaxImageDimension)
	store := &AttachmentStore{
		root: filepath.Join(config.DSHHome, "attachments", "v1"),
		limits: attachment.ImageAttachmentLimits{
			MaxImageBytes:        maxImageBytes,
			MaxImagesPerMessage:  maxImagesPerMessage,
			MaxMessageImageBytes: maxMessageImageBytes,
			MaxImagePixels:       maxImagePixels,
			MaxImageDimension:    maxImageDimension,
			MediaTypes:           []string{attachment.MediaPNG, attachment.MediaJPEG, attachment.MediaWebP, attachment.MediaGIF},
		},
		normalizationPolicy: NormalizationPolicy{
			MaxPixels:    orDefault(config.NormalizedImageMaxPixels, DefaultNormalizedImageMaxPixels),
			MaxDimension: orDefault(config.NormalizedImageMaxDimension, DefaultNormalizedImageMaxDimension),
			MaxBytes:     orDefault(config.NormalizedImageMaxBytes, DefaultNormalizedImageMaxBytes),
		},
	}
	return store
}

func orDefault(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// ImageLimits is the deployment-resolved image policy.
func (s *AttachmentStore) ImageLimits() attachment.ImageAttachmentLimits {
	return s.limits
}

// Root is the absolute versioned storage root.
func (s *AttachmentStore) Root() string { return s.root }

// NormalizationPolicy is the resolved provider-independent policy.
func (s *AttachmentStore) NormalizationPolicy() NormalizationPolicy { return s.normalizationPolicy }

// ValidateImage runs the full admission policy for one image without
// touching storage, including normalization: a batch whose members all
// validate cannot later be refused by the normalized byte cap during
// publication.
func (s *AttachmentStore) ValidateImage(input attachment.SaveImageAttachment) error {
	_, err := prepareImageFile(input, s.limits, s.normalizationPolicy)
	return err
}

// SaveImages validates and durably commits one ordered image batch: batch
// policy first, then every member prepared before any publication.
func (s *AttachmentStore) SaveImages(inputs []attachment.SaveImageAttachment) ([]attachment.ImageAttachmentRef, error) {
	if err := attachment.ValidateImageBatch(s.limits, inputs); err != nil {
		return nil, err
	}
	prepared := make([]preparedImageFile, 0, len(inputs))
	for _, input := range inputs {
		item, err := prepareImageFile(input, s.limits, s.normalizationPolicy)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, item)
	}
	refs := make([]attachment.ImageAttachmentRef, 0, len(prepared))
	for _, item := range prepared {
		ref, err := commitPreparedImageFile(s.root, item)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// SaveImage validates and durably commits one image.
func (s *AttachmentStore) SaveImage(input attachment.SaveImageAttachment) (attachment.ImageAttachmentRef, error) {
	prepared, err := prepareImageFile(input, s.limits, s.normalizationPolicy)
	if err != nil {
		return attachment.ImageAttachmentRef{}, err
	}
	return commitPreparedImageFile(s.root, prepared)
}

// ReadImage reads and verifies one content-addressed image.
func (s *AttachmentStore) ReadImage(ref attachment.ImageAttachmentRef) (attachment.StoredImageAttachment, error) {
	return readImageFile(s.root, ref)
}

// ImageHostPath locates the provider-owned normalized object in the host
// filesystem.
func (s *AttachmentStore) ImageHostPath(ref attachment.ImageAttachmentRef) (string, bool, error) {
	path, err := NormalizedImagePath(s.root, ref)
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// ReadImageRequest generates or reads one deterministic model-request
// version from the stored normalized image.
func (s *AttachmentStore) ReadImageRequest(ref attachment.ImageAttachmentRef, policy attachment.ImageRequestPolicy) (attachment.RequestImageAttachment, error) {
	return attachment.RequestImageAttachment{}, attachment.NewAttachmentError(
		"The mounted attachment provider cannot derive model-request images.",
		attachment.CodeProjectionUnsupported,
	)
}
