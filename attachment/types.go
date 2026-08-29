// Package attachment ports packages/attachment: the durable attachment
// storage seam (`ctx.attachments`) shared by upload admission, model-request
// projection, and the session log's image references.
package attachment

// ImageMediaType names the raster formats accepted by the version-one
// attachment path.
const (
	MediaPNG  = "image/png"
	MediaJPEG = "image/jpeg"
	MediaWebP = "image/webp"
	MediaGIF  = "image/gif"
)

// ImageAttachmentRef is a durable, serializable reference to one immutable
// normalized image.
type ImageAttachmentRef struct {
	// AttachmentID is an opaque storage identifier — never a filesystem
	// path or bearer URL.
	AttachmentID string `json:"attachmentId"`
	// MediaType is verified from the stored bytes.
	MediaType string `json:"mediaType"`
	// Bytes is the exact encoded byte length.
	Bytes int `json:"bytes"`
	// Width is the intrinsic encoded width in pixels.
	Width int `json:"width"`
	// Height is the intrinsic encoded height in pixels.
	Height int `json:"height"`
	// Name is an optional display name stripped of local path information.
	Name string `json:"name,omitempty"`
	// OriginalDimensions records the input dimensions after EXIF
	// orientation and before normalization scaling; present only when
	// normalization reduced the image.
	OriginalDimensions *ImageDimensions `json:"originalDimensions,omitempty"`
}

// ImageDimensions is one width/height pair.
type ImageDimensions struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

// ImageAttachmentLimits is the deployment-resolved limit set used by
// authoritative and fast-path validation.
type ImageAttachmentLimits struct {
	MaxImageBytes        int
	MaxImagesPerMessage  int
	MaxMessageImageBytes int
	MaxImagePixels       int
	// MaxImageDimension is the maximum intrinsic width and the maximum
	// intrinsic height in pixels for one image.
	MaxImageDimension int
	// MediaTypes lists the accepted raster formats.
	MediaTypes []string
}

// EncodedImageAttachment is one base64-encoded image upload accompanying a
// wire request.
type EncodedImageAttachment struct {
	// MediaType is declared by the caller and verified against the decoded
	// bytes during admission.
	MediaType string `json:"mediaType"`
	// Data is the canonical base64 encoding of the image bytes.
	Data string `json:"data"`
	// Name is an optional display name; it is never interpreted as a path.
	Name string `json:"name,omitempty"`
}

// SaveImageAttachment requests validation and durable commit of one image.
type SaveImageAttachment struct {
	Data []byte
	// MediaType is caller-declared and checked against fully decoded bytes.
	MediaType string
	// Name is an optional display name; it is never interpreted as a path.
	Name string
}

// StoredImageAttachment returns stored image bytes after reference and
// digest verification.
type StoredImageAttachment struct {
	Ref  ImageAttachmentRef
	Data []byte
}

// ImageRequestPolicy is the deterministic request-image policy selected by
// one exact model route.
type ImageRequestPolicy struct {
	// MaxPixels is the maximum width-times-height after aspect-preserving
	// projection.
	MaxPixels int
	// MaxBytes is the encoded-byte target before base64 expansion or
	// Files-API upload; the smallest quality-ladder output is kept when no
	// quality fits.
	MaxBytes int
}

// RequestImageAttachment is the cached request version derived from one
// provider-independent normalized attachment.
type RequestImageAttachment struct {
	// VariantID is the cache and upload-index key over the attachment id,
	// policy, and fixed encoder parameters.
	VariantID string
	// Attachment is the durable normalized attachment this request version
	// derives from.
	Attachment ImageAttachmentRef
	// Data is the encoded request bytes.
	Data []byte
	// MediaType is the encoded request format.
	MediaType string
	Bytes     int
	Width     int
	Height    int
	// Depth and Space are provider-compatible facts proven after request
	// encoding.
	Depth string
	Space string
	// HasAlpha reports whether the encoded request version retains an
	// alpha channel.
	HasAlpha bool
}
