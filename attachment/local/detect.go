// Package local ports packages/attachment-local: the durable local
// attachment backend rooted below DSH_HOME, content-addressed by sha256.
//
// Go adaptation: raster work uses the standard library instead of sharp.
// PNG, JPEG, and GIF fully decode and re-encode; WebP is admitted through a
// header probe (media type, dimensions, animation, alpha) but cannot be
// re-encoded, so an out-of-budget WebP fails normalization instead of being
// transcoded. EXIF orientation is not applied — oriented JPEGs carry
// metadata and therefore always re-encode orientation-free.
package local

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	"dshgo/attachment"
)

// detected carries the intrinsic facts admission and normalization share.
type detected struct {
	mediaType string
	width     int
	height    int
	animated  bool
	// carriesMetadata reports whether the bytes carry descriptive
	// metadata, a color profile, or orientation.
	carriesMetadata bool
	depth           string
	space           string
	hasAlpha        bool
}

// pngSignature is the canonical PNG file signature.
var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// sniff returns the wire media type from the leading bytes, or "" when the
// container is unrecognized.
func sniff(data []byte) string {
	switch {
	case len(data) >= 12 && bytes.Equal(data[:8], pngSignature):
		return attachment.MediaPNG
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return attachment.MediaJPEG
	case len(data) >= 6 && (string(data[:6]) == "GIF87a" || string(data[:6]) == "GIF89a"):
		return attachment.MediaGIF
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return attachment.MediaWebP
	default:
		return ""
	}
}

// detectImage fully decodes a supported raster and returns its intrinsic
// metadata, applying the intrinsic-dimension admission limits.
func detectImage(data []byte, maxPixels, maxDimension int) (detected, error) {
	mediaType := sniff(data)
	if mediaType == "" {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	var facts detected
	var err error
	switch mediaType {
	case attachment.MediaGIF:
		facts, err = detectGIF(data)
	case attachment.MediaWebP:
		facts, err = detectWebP(data)
	default:
		facts, err = detectStdlib(data, mediaType)
	}
	if err != nil {
		return detected{}, err
	}
	if maxPixels > 0 && facts.width*facts.height > maxPixels {
		return detected{}, attachment.NewAttachmentError("Image exceeds the configured decoded-pixel limit.", attachment.CodeImageTooManyPixels)
	}
	if maxDimension > 0 && max(facts.width, facts.height) > maxDimension {
		return detected{}, attachment.NewAttachmentError("Image exceeds the configured per-side pixel limit.", attachment.CodeImageDimTooLarge)
	}
	// Full decode: a truncated raster fails admission here rather than at
	// model-request time.
	if mediaType != attachment.MediaWebP {
		if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
			return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
		}
	}
	return facts, nil
}

// probeImage parses a supported raster's header and returns its intrinsic
// metadata without decoding pixels. Digest-verified reads use this:
// admission already proved these exact bytes decode completely.
func probeImage(data []byte) (detected, error) {
	mediaType := sniff(data)
	if mediaType == "" {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	switch mediaType {
	case attachment.MediaGIF:
		return detectGIF(data)
	case attachment.MediaWebP:
		return detectWebP(data)
	default:
		return probeStdlib(data, mediaType)
	}
}

// detectStdlib decodes PNG/JPEG metadata through the standard library.
func detectStdlib(data []byte, mediaType string) (detected, error) {
	facts, err := probeStdlib(data, mediaType)
	if err != nil {
		return detected{}, err
	}
	config, err := decodeConfig(bytes.NewReader(data), mediaType)
	if err != nil {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	facts.width, facts.height = config.Width, config.Height
	facts.hasAlpha = mediaHasAlpha(config, mediaType)
	if mediaType == attachment.MediaPNG {
		facts.carriesMetadata = pngCarriesMetadata(data)
	}
	if mediaType == attachment.MediaJPEG {
		facts.carriesMetadata = jpegCarriesMetadata(data)
	}
	return facts, nil
}

// probeStdlib derives PNG/JPEG header facts without a full raster decode.
func probeStdlib(data []byte, mediaType string) (detected, error) {
	config, err := decodeConfig(bytes.NewReader(data), mediaType)
	if err != nil {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	facts := detected{
		mediaType: mediaType,
		width:     config.Width,
		height:    config.Height,
		depth:     "uchar",
		space:     "srgb",
	}
	facts.hasAlpha = mediaHasAlpha(config, mediaType)
	if mediaType == attachment.MediaPNG {
		facts.carriesMetadata = pngCarriesMetadata(data)
	}
	if mediaType == attachment.MediaJPEG {
		facts.carriesMetadata = jpegCarriesMetadata(data)
	}
	return facts, nil
}

func decodeConfig(reader io.Reader, mediaType string) (image.Config, error) {
	switch mediaType {
	case attachment.MediaPNG:
		return png.DecodeConfig(reader)
	case attachment.MediaJPEG:
		return jpeg.DecodeConfig(reader)
	default:
		return image.Config{}, fmt.Errorf("unsupported media type %q", mediaType)
	}
}

func mediaHasAlpha(config image.Config, mediaType string) bool {
	if mediaType == attachment.MediaJPEG {
		return false
	}
	// PNG with palette transparency or an alpha channel; the stdlib config
	// only exposes the color model, so treat palettes as potentially
	// transparent (conservative for normalization decisions).
	return config.ColorModel != nil
}

// pngCarriesMetadata scans the chunk stream for retained metadata chunks.
func pngCarriesMetadata(data []byte) bool {
	offset := 8
	metadataChunks := map[string]bool{
		"eXIf": true, "iCCP": true, "tEXt": true, "zTXt": true, "iTXT": true,
		"acTL": true,
	}
	for offset+8 <= len(data) {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		name := string(data[offset+4 : offset+8])
		if metadataChunks[name] {
			return true
		}
		if name == "IEND" {
			return false
		}
		offset += 12 + length
	}
	return false
}

// jpegCarriesMetadata scans the segment stream for EXIF/XMP/ICC payloads.
func jpegCarriesMetadata(data []byte) bool {
	offset := 2
	for offset+4 <= len(data) && data[offset] == 0xFF {
		marker := data[offset+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			offset += 2
			continue
		}
		if offset+4 > len(data) {
			return false
		}
		segmentLength := int(binary.BigEndian.Uint16(data[offset+2 : offset+4]))
		if marker == 0xE1 && offset+10 <= len(data) && string(data[offset+4:offset+10]) == "Exif\x00\x00" {
			return true
		}
		if marker == 0xE2 && offset+15 <= len(data) && string(data[offset+4:offset+15]) == "ICC_PROFILE\x00" {
			return true
		}
		if marker == 0xDA {
			return false
		}
		offset += 2 + segmentLength
	}
	return false
}

// detectGIF parses the GIF block stream for frame count.
func detectGIF(data []byte) (detected, error) {
	decoded, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	first := decoded.Image[0]
	return detected{
		mediaType: attachment.MediaGIF,
		width:     decoded.Config.Width,
		height:    decoded.Config.Height,
		animated:  len(decoded.Image) > 1,
		depth:     "uchar",
		space:     "srgb",
		hasAlpha:  hasPaletteAlpha(first),
	}, nil
}

func hasPaletteAlpha(palette image.Image) bool {
	paletted, ok := palette.(*image.Paletted)
	if !ok {
		return false
	}
	for _, color := range paletted.Palette {
		if _, _, _, alpha := color.RGBA(); alpha < 0xFFFF {
			return true
		}
	}
	return false
}

// detectWebP parses the RIFF container header: media type, canvas
// dimensions, animation, and alpha flags.
func detectWebP(data []byte) (detected, error) {
	facts := detected{
		mediaType: attachment.MediaWebP,
		depth:     "uchar",
		space:     "srgb",
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8 ":
		// Lossy: the frame header carries the dimensions at offset 26.
		if len(data) < 30 {
			return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
		}
		facts.width = int(binary.LittleEndian.Uint16(data[26:28]) & 0x3FFF)
		facts.height = int(binary.LittleEndian.Uint16(data[28:30]) & 0x3FFF)
	case "VP8L":
		if len(data) < 25 {
			return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		facts.width = int(bits&0x3FFF) + 1
		facts.height = int((bits>>14)&0x3FFF) + 1
		facts.hasAlpha = bits>>28&0x1 == 0x1
	case "VP8X":
		flags := data[20]
		facts.animated = flags&0x02 != 0
		facts.hasAlpha = flags&0x10 != 0
		facts.width = int(binary.LittleEndian.Uint32(append([]byte{0}, data[24:27]...)))
		facts.height = int(binary.LittleEndian.Uint32(append([]byte{0}, data[27:30]...))) + 1
		facts.width++
	default:
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	if facts.width <= 0 || facts.height <= 0 {
		return detected{}, attachment.NewAttachmentError("Unsupported or malformed image data.", attachment.CodeInvalidImage)
	}
	return facts, nil
}
