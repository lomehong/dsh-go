package server

import (
	"dshgo/attachment"
	"dshgo/llm"
	"dshgo/sdk/protocol"
)

// AttachmentStoreAdmitter binds a durable attachment store to the prompt
// seam: base64 raster blocks are admitted (canonical base64, batch policy,
// per-image validation, ordered commit) and projected into durable image
// content blocks carrying the normalized references.
type AttachmentStoreAdmitter struct {
	store attachment.Store
}

// NewAttachmentStoreAdmitter wraps one attachment store.
func NewAttachmentStoreAdmitter(store attachment.Store) *AttachmentStoreAdmitter {
	return &AttachmentStoreAdmitter{store: store}
}

// AdmitEncoded admits the wire blocks in order and returns one image block
// per admitted upload, positionally aligned with the input.
func (a *AttachmentStoreAdmitter) AdmitEncoded(images []protocol.SdkEncodedImageBlock) ([]llm.ContentBlock, error) {
	uploads := make([]attachment.EncodedImageAttachment, 0, len(images))
	for _, image := range images {
		uploads = append(uploads, attachment.EncodedImageAttachment{
			MediaType: image.MimeType,
			Data:      image.Data,
		})
	}
	refs, err := attachment.AdmitEncodedImages(a.store, uploads)
	if err != nil {
		return nil, err
	}
	blocks := make([]llm.ContentBlock, 0, len(refs))
	for _, ref := range refs {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockImage, Attachment: ref})
	}
	return blocks, nil
}
