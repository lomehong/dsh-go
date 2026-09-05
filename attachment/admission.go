package attachment

import (
	"encoding/base64"
	"fmt"

	"dshgo/llm"
)

// Store is the immutable binary attachment service. Implementations validate
// bytes before publishing a reference.
type Store interface {
	// ImageLimits is the deployment-resolved image policy used by
	// authoritative and fast-path validation.
	ImageLimits() ImageAttachmentLimits
	// ValidateImage validates one image without persisting it. Batch
	// callers validate every member before saving any member.
	ValidateImage(input SaveImageAttachment) error
	// SaveImages validates and durably commits one ordered image batch:
	// validation failures start no writes; storage failures return no
	// partial references.
	SaveImages(inputs []SaveImageAttachment) ([]ImageAttachmentRef, error)
	// SaveImage validates and durably commits one image before its owning
	// session event is appended.
	SaveImage(input SaveImageAttachment) (ImageAttachmentRef, error)
	// ReadImage reads one image and verifies that bytes still match the
	// recorded reference.
	ReadImage(ref ImageAttachmentRef) (StoredImageAttachment, error)
	// ImageHostPath locates the provider-owned normalized object in the
	// harness host filesystem; ok is false when this backend is not
	// host-file-backed. An invalid durable reference fails loud.
	ImageHostPath(ref ImageAttachmentRef) (path string, ok bool, err error)
	// ReadImageRequest generates or reads one deterministic model-request
	// version from the stored normalized image.
	ReadImageRequest(ref ImageAttachmentRef, policy ImageRequestPolicy) (RequestImageAttachment, error)
}

// FileStore is the verbatim generic-file face of the attachment service
// (official AttachmentStore file half, 0.1.3-alpha.1). Files carry no
// admission limits: any byte content and length is admissible, because the
// file's bytes are exactly what was uploaded.
type FileStore interface {
	// SaveFile durably commits one file byte-for-byte before its owning
	// session event is appended.
	SaveFile(input SaveFileAttachment) (FileAttachmentRef, error)
	// SaveFileStream commits one file from bounded chunks without retaining
	// the whole sequence in memory.
	SaveFileStream(input SaveFileStreamAttachment) (FileAttachmentRef, error)
	// FileHostPath locates the verbatim stored object in the harness host
	// filesystem; ok is false when this backend is not host-file-backed. An
	// invalid durable reference fails loud.
	FileHostPath(ref FileAttachmentRef) (path string, ok bool, err error)
}

// ValidateImageBatch enforces the base-store batch admission policy: count
// and aggregate-byte limits, then media-type membership per member.
func ValidateImageBatch(limits ImageAttachmentLimits, inputs []SaveImageAttachment) error {
	if len(inputs) > limits.MaxImagesPerMessage {
		return NewAttachmentError("Image batch exceeds the configured image-count limit.", CodeTooManyImages)
	}
	totalBytes := 0
	for _, input := range inputs {
		totalBytes += len(input.Data)
	}
	if totalBytes > limits.MaxMessageImageBytes {
		return NewAttachmentError("Image batch exceeds the configured aggregate image-byte limit.", CodeImagesTooLarge)
	}
	for _, input := range inputs {
		accepted := false
		for _, mediaType := range limits.MediaTypes {
			if input.MediaType == mediaType {
				accepted = true
				break
			}
		}
		if !accepted {
			return NewAttachmentError(fmt.Sprintf("Image type %s is not accepted by this deployment.", input.MediaType), CodeUnsupportedImgType)
		}
	}
	return nil
}

// decodeBase64 decodes one upload payload while rejecting non-canonical
// base64 forms.
// decodeCanonicalBase64 decodes one upload payload while rejecting
// non-canonical base64 forms; the empty policy distinguishes images
// (reject) from files (a zero-byte upload is valid).
func decodeCanonicalBase64(data, empty, code string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if (len(data) == 0 && empty == "reject") || err != nil || base64.StdEncoding.EncodeToString(decoded) != data {
		message := "Image upload is not canonical base64."
		if code == CodeInvalidFileBase64 {
			message = "File upload is not canonical base64."
		}
		return nil, NewAttachmentError(message, code)
	}
	return decoded, nil
}

// AdmitEncodedFile admits one wire file upload: enforce canonical base64
// (an empty file is a valid zero-byte payload), then delegate verbatim
// commit to Store.SaveFile. The shared entry for every RPC endpoint
// accepting browser file uploads (official admitEncodedFile).
func AdmitEncodedFile(attachments FileStore, file EncodedFileAttachment) (FileAttachmentRef, error) {
	data, err := decodeCanonicalBase64(file.Data, "accept", CodeInvalidFileBase64)
	if err != nil {
		return FileAttachmentRef{}, err
	}
	return attachments.SaveFile(SaveFileAttachment{Data: data, Name: file.Name})
}

func decodeBase64(data string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(data) == 0 || base64.StdEncoding.EncodeToString(decoded) != data {
		return nil, NewAttachmentError("Image upload is not canonical base64.", CodeInvalidImageBase64)
	}
	return decoded, nil
}

// saveInput converts one wire upload into a store input.
func saveInput(image EncodedImageAttachment) (SaveImageAttachment, error) {
	data, err := decodeBase64(image.Data)
	if err != nil {
		return SaveImageAttachment{}, err
	}
	return SaveImageAttachment{Data: data, MediaType: image.MediaType, Name: image.Name}, nil
}

// AdmitEncodedImages admits one wire image batch: canonical base64 on every
// member, then batch admission — count and aggregate-byte limits, media-type
// and per-image validation, ordered commit — to the store. This is the
// shared entry for every endpoint accepting browser uploads.
func AdmitEncodedImages(attachments Store, images []EncodedImageAttachment) ([]ImageAttachmentRef, error) {
	// Fast-path gate before any decode: the store's checks stay
	// authoritative, but decoding unbounded payloads first would allocate
	// ahead of the limit decision. DecodedLen prices each encoded payload
	// exactly, and a zero limit means the deployment configured no gate.
	limits := attachments.ImageLimits()
	if limits.MaxImagesPerMessage > 0 && len(images) > limits.MaxImagesPerMessage {
		return nil, NewAttachmentError("Image batch exceeds the configured image-count limit.", CodeTooManyImages)
	}
	totalBytes := 0
	for _, image := range images {
		totalBytes += base64.StdEncoding.DecodedLen(len(image.Data))
	}
	if limits.MaxMessageImageBytes > 0 && totalBytes > limits.MaxMessageImageBytes {
		return nil, NewAttachmentError("Image batch exceeds the configured aggregate image-byte limit.", CodeImagesTooLarge)
	}
	inputs := make([]SaveImageAttachment, 0, len(images))
	for _, image := range images {
		input, err := saveInput(image)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return attachments.SaveImages(inputs)
}

// AdmitPromptContent admits one browser prompt: text-only blocks pass
// through without touching the store, and each image block's encoded payload
// is batch-admitted to a durable reference that replaces it in place (the
// upstream admitPromptContent). The store's batch policy — count,
// aggregate-byte, media-type, per-image validation — applies to the image
// members as one group; a refused batch admits nothing.
func AdmitPromptContent(store Store, blocks []llm.ContentBlock) ([]llm.ContentBlock, error) {
	hasImage := false
	for _, block := range blocks {
		if block.Type == llm.BlockImage {
			hasImage = true
			break
		}
	}
	if !hasImage {
		return blocks, nil
	}
	encoded := make([]EncodedImageAttachment, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != llm.BlockImage {
			continue
		}
		image, ok := block.Attachment.(EncodedImageAttachment)
		if !ok {
			return nil, NewAttachmentError("Image prompt block must carry an encoded attachment payload.", CodeInvalidImageBase64)
		}
		encoded = append(encoded, image)
	}
	refs, err := AdmitEncodedImages(store, encoded)
	if err != nil {
		return nil, err
	}
	admitted := make([]llm.ContentBlock, 0, len(blocks))
	next := 0
	for _, block := range blocks {
		if block.Type != llm.BlockImage {
			admitted = append(admitted, block)
			continue
		}
		admitted = append(admitted, llm.ContentBlock{
			Type:       llm.BlockImage,
			Attachment: refs[next],
		})
		next++
	}
	return admitted, nil
}
