package local

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"

	"dshgo/attachment"
)

// NormalizationPolicy is the deployment-resolved policy for the persisted
// normalized attachment.
type NormalizationPolicy struct {
	// MaxPixels is the total-pixel budget; larger sources downscale
	// proportionally.
	MaxPixels int
	// MaxDimension is the long-edge cap in pixels applied after the
	// total-pixel budget.
	MaxDimension int
	// MaxBytes is the encoded-byte target for the quality ladder; the
	// smallest ladder output is kept when no quality fits.
	MaxBytes int
}

// normalizedImage carries the normalized bytes beside the facts recorded by
// a durable reference.
type normalizedImage struct {
	data      []byte
	mediaType string
	width     int
	height    int
}

// encodingLadder lists the deterministic JPEG quality steps tried in order.
var encodingLadder = []int{85, 70, 50, 30}

// canPassThroughNormalization reports whether the source can pass through
// byte-identically: clean, single-frame, 8-bit sRGB, and inside every
// normalization limit. GIF always re-encodes; WebP passthrough requires the
// source to already fit (it cannot be transcoded).
func canPassThroughNormalization(facts detected, bytesCount int, policy NormalizationPolicy) bool {
	if facts.mediaType == attachment.MediaWebP {
		return bytesCount <= policy.MaxBytes
	}
	return facts.mediaType != attachment.MediaGIF &&
		!facts.animated &&
		!facts.carriesMetadata &&
		facts.depth == "uchar" &&
		facts.space == "srgb" &&
		bytesCount <= policy.MaxBytes &&
		facts.width*facts.height <= policy.MaxPixels &&
		max(facts.width, facts.height) <= policy.MaxDimension
}

// initialDimensions applies the total-pixel budget, then the long-edge cap,
// without changing aspect ratio.
func initialDimensions(facts detected, policy NormalizationPolicy) (int, int) {
	budgeted := attachment.RequestImageDimensions(facts.width, facts.height, policy.MaxPixels)
	longEdge := max(budgeted.Width, budgeted.Height)
	if longEdge <= policy.MaxDimension {
		return budgeted.Width, budgeted.Height
	}
	scale := float64(policy.MaxDimension) / float64(longEdge)
	return max(1, int(float64(budgeted.Width)*scale)), max(1, int(float64(budgeted.Height)*scale))
}

// normalizeImage produces the persisted provider-independent normalized
// version of one fully decoded source. The source passes through only when
// already clean and inside every limit. Re-encoding never removes
// transparency. When every ladder quality exceeds the byte target, the
// smallest ladder output is kept; provider byte caps stay enforced at the
// route that transmits the bytes.
func normalizeImage(data []byte, facts detected, policy NormalizationPolicy) (normalizedImage, error) {
	if canPassThroughNormalization(facts, len(data), policy) {
		return normalizedImage{data: data, mediaType: facts.mediaType, width: facts.width, height: facts.height}, nil
	}
	width, height := initialDimensions(facts, policy)
	var encoded normalizedImage
	var err error
	switch facts.mediaType {
	case attachment.MediaWebP:
		// WebP cannot be transcoded with the standard library; a
		// passthrough-ineligible WebP fails here instead of drifting
		// from the normalization contract.
		return normalizedImage{}, attachment.NewAttachmentError(
			"The image/webp could not be converted to the normalized 8-bit sRGB form.",
			attachment.CodeAttachmentWriteFailed,
		)
	case attachment.MediaGIF:
		// Animated or oversized GIFs normalize to their first frame.
		encoded, err = normalizeGIF(data, width, height, policy)
	case attachment.MediaJPEG, attachment.MediaPNG:
		encoded, err = normalizeRaster(data, facts, width, height, policy)
	default:
		err = fmt.Errorf("unsupported media type %q", facts.mediaType)
	}
	if err != nil {
		var attachmentErr *attachment.AttachmentError
		if asAttachmentError(err, &attachmentErr) {
			return normalizedImage{}, err
		}
		return normalizedImage{}, attachment.WrappedAttachmentError(
			fmt.Sprintf("The %s could not be converted to the normalized 8-bit sRGB form.", facts.mediaType),
			attachment.CodeAttachmentWriteFailed, err,
		)
	}
	return verifyNormalizedImage(encoded)
}

// normalizeRaster decodes PNG/JPEG pixels, projects them into the target
// size, and re-encodes through the deterministic ladder.
func normalizeRaster(data []byte, facts detected, width, height int, policy NormalizationPolicy) (normalizedImage, error) {
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return normalizedImage{}, err
	}
	projected := projectImage(source, width, height)
	if facts.hasAlpha {
		var buffer bytes.Buffer
		if err := png.Encode(&buffer, projected); err != nil {
			return normalizedImage{}, err
		}
		return normalizedImage{data: buffer.Bytes(), mediaType: attachment.MediaPNG, width: width, height: height}, nil
	}
	// Opaque sources encode through the JPEG quality ladder; when no
	// quality fits the target, the smallest ladder output is kept.
	var smallest normalizedImage
	for index, quality := range encodingLadder {
		var buffer bytes.Buffer
		if err := jpeg.Encode(&buffer, projected, &jpeg.Options{Quality: quality}); err != nil {
			return normalizedImage{}, err
		}
		candidate := normalizedImage{data: buffer.Bytes(), mediaType: attachment.MediaJPEG, width: width, height: height}
		if index == 0 || len(candidate.data) < len(smallest.data) {
			smallest = candidate
		}
		if len(candidate.data) <= policy.MaxBytes {
			return candidate, nil
		}
	}
	return smallest, nil
}

// normalizeGIF projects the first frame of a GIF into the normalized form.
func normalizeGIF(data []byte, width, height int, policy NormalizationPolicy) (normalizedImage, error) {
	decoded, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return normalizedImage{}, err
	}
	projected := projectImage(decoded.Image[0], width, height)
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, projected); err != nil {
		return normalizedImage{}, err
	}
	return normalizedImage{data: buffer.Bytes(), mediaType: attachment.MediaPNG, width: width, height: height}, nil
}

// projectImage resamples one source into the exact target size with
// nearest-neighbor sampling. Small sources are never enlarged.
func projectImage(source image.Image, width, height int) draw.Image {
	bounds := source.Bounds()
	targetWidth, targetHeight := width, height
	if bounds.Dx() < targetWidth && bounds.Dy() < targetHeight {
		targetWidth, targetHeight = bounds.Dx(), bounds.Dy()
	}
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/targetHeight
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/targetWidth
			target.Set(x, y, source.At(sourceX, sourceY))
		}
	}
	return target
}

// verifyNormalizedImage asserts that a normalized output is a single-frame
// image with matching facts.
func verifyNormalizedImage(image normalizedImage) (normalizedImage, error) {
	facts, err := probeImage(image.data)
	if err != nil {
		return normalizedImage{}, err
	}
	if facts.mediaType != image.mediaType || facts.width != image.width || facts.height != image.height || facts.animated {
		return normalizedImage{}, attachment.NewAttachmentError(
			"Image normalization did not produce a single-frame 8-bit sRGB image with matching metadata.",
			attachment.CodeAttachmentWriteFailed,
		)
	}
	return image, nil
}

func asAttachmentError(err error, target **attachment.AttachmentError) bool {
	for err != nil {
		if typed, ok := err.(*attachment.AttachmentError); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
