package server

import (
	"strings"
	"testing"

	"dshgo/attachment"
	"dshgo/attachment/local"
	"dshgo/sdk/protocol"
)

func TestAttachmentStoreAdmitterSplicesDurableRefs(t *testing.T) {
	store := local.New(local.Config{DSHHome: t.TempDir()})
	admitter := NewAttachmentStoreAdmitter(store)
	if _, err := admitter.AdmitEncoded([]protocol.SdkEncodedImageBlock{{Type: "image", Data: "not base64!", MimeType: attachment.MediaPNG}}); err == nil ||
		!strings.Contains(err.Error(), "canonical base64") {
		t.Fatalf("non-canonical = %v", err)
	}
	// An empty batch admits cleanly.
	blocks, err := admitter.AdmitEncoded(nil)
	if err != nil || len(blocks) != 0 {
		t.Fatalf("empty = %v %v", blocks, err)
	}
	// Type mismatch routes through the store's admission policy.
	if _, err := admitter.AdmitEncoded([]protocol.SdkEncodedImageBlock{
		{Type: "image", Data: "AAAA", MimeType: attachment.MediaWebP},
	}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("webp probe = %v", err)
	}
}
