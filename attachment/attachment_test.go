package attachment_test

import (
	"errors"
	"strings"
	"testing"

	"dshgo/attachment"
	"dshgo/llm"
)

func TestRequestImageDimensions(t *testing.T) {
	cases := []struct {
		name          string
		width, height int
		maxPixels     int
		wantW, wantH  int
	}{
		{"small images are not enlarged", 10, 20, 100_000, 10, 20},
		{"square downscale", 2000, 2000, 1_000_000, 1000, 1000},
		{"wide downscale keeps aspect", 4000, 1000, 1_000_000, 2000, 500},
		{"tall downscale keeps aspect", 1000, 4000, 1_000_000, 500, 2000},
		{"extreme aspect floors at one pixel", 10_000, 2, 1_000, 1000, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := attachment.RequestImageDimensions(testCase.width, testCase.height, testCase.maxPixels)
			if got.Width != testCase.wantW || got.Height != testCase.wantH {
				t.Fatalf("got %+v want %dx%d", got, testCase.wantW, testCase.wantH)
			}
			if got.Width*got.Height > testCase.maxPixels {
				t.Fatalf("%+v exceeds the pixel budget %d", got, testCase.maxPixels)
			}
		})
	}
}

func TestValidateImageBatch(t *testing.T) {
	limits := attachment.ImageAttachmentLimits{
		MaxImagesPerMessage:  2,
		MaxMessageImageBytes: 100,
		MediaTypes:           []string{attachment.MediaPNG, attachment.MediaJPEG},
	}
	if err := attachment.ValidateImageBatch(limits, make([]attachment.SaveImageAttachment, 3)); err == nil ||
		err.(*attachment.AttachmentError).Code != attachment.CodeTooManyImages {
		t.Fatalf("count = %v", err)
	}
	if err := attachment.ValidateImageBatch(limits, []attachment.SaveImageAttachment{
		{Data: make([]byte, 60), MediaType: attachment.MediaPNG},
		{Data: make([]byte, 60), MediaType: attachment.MediaPNG},
	}); err == nil || err.(*attachment.AttachmentError).Code != attachment.CodeImagesTooLarge {
		t.Fatalf("aggregate = %v", err)
	}
	if err := attachment.ValidateImageBatch(limits, []attachment.SaveImageAttachment{
		{Data: make([]byte, 10), MediaType: attachment.MediaWebP},
	}); err == nil || err.(*attachment.AttachmentError).Code != attachment.CodeUnsupportedImgType {
		t.Fatalf("media type = %v", err)
	}
	if err := attachment.ValidateImageBatch(limits, []attachment.SaveImageAttachment{
		{Data: make([]byte, 10), MediaType: attachment.MediaPNG},
		{Data: make([]byte, 10), MediaType: attachment.MediaJPEG},
	}); err != nil {
		t.Fatalf("valid batch = %v", err)
	}
}

// fakeStore records batch order and answers saves with deterministic refs.
type fakeStore struct {
	batches [][]attachment.SaveImageAttachment
}

func (f *fakeStore) ImageLimits() attachment.ImageAttachmentLimits {
	return attachment.ImageAttachmentLimits{}
}
func (f *fakeStore) ValidateImage(attachment.SaveImageAttachment) error { return nil }
func (f *fakeStore) SaveImages(inputs []attachment.SaveImageAttachment) ([]attachment.ImageAttachmentRef, error) {
	f.batches = append(f.batches, inputs)
	refs := make([]attachment.ImageAttachmentRef, 0, len(inputs))
	for index, input := range inputs {
		refs = append(refs, attachment.ImageAttachmentRef{AttachmentID: "id-" + string(rune('a'+index)), MediaType: input.MediaType})
	}
	return refs, nil
}
func (f *fakeStore) SaveImage(attachment.SaveImageAttachment) (attachment.ImageAttachmentRef, error) {
	return attachment.ImageAttachmentRef{}, nil
}
func (f *fakeStore) ReadImage(attachment.ImageAttachmentRef) (attachment.StoredImageAttachment, error) {
	return attachment.StoredImageAttachment{}, nil
}
func (f *fakeStore) ImageHostPath(attachment.ImageAttachmentRef) (string, bool, error) {
	return "", false, nil
}
func (f *fakeStore) ReadImageRequest(attachment.ImageAttachmentRef, attachment.ImageRequestPolicy) (attachment.RequestImageAttachment, error) {
	return attachment.RequestImageAttachment{}, nil
}

func TestAdmitEncodedImages(t *testing.T) {
	store := &fakeStore{}
	// Order is preserved through decode and commit.
	refs, err := attachment.AdmitEncodedImages(store, []attachment.EncodedImageAttachment{
		{MediaType: attachment.MediaPNG, Data: "AAAA"},
		{MediaType: attachment.MediaJPEG, Data: "AAA="},
	})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if len(refs) != 2 || refs[0].AttachmentID != "id-a" || refs[1].AttachmentID != "id-b" {
		t.Fatalf("refs = %+v", refs)
	}
	if len(store.batches) != 1 || len(store.batches[0][0].Data) != 3 {
		t.Fatalf("batch = %+v", store.batches)
	}
	// Non-canonical base64 fails loud: unpadded and empty forms refuse.
	if _, err := attachment.AdmitEncodedImages(store, []attachment.EncodedImageAttachment{
		{MediaType: attachment.MediaPNG, Data: "AAA"},
	}); err == nil || !attachment.IsImageAdmissionError(err) ||
		err.(*attachment.AttachmentError).Code != attachment.CodeInvalidImageBase64 {
		t.Fatalf("unpadded = %v", err)
	}
	if _, err := attachment.AdmitEncodedImages(store, []attachment.EncodedImageAttachment{
		{MediaType: attachment.MediaPNG, Data: ""},
	}); err == nil || err.(*attachment.AttachmentError).Code != attachment.CodeInvalidImageBase64 {
		t.Fatalf("empty = %v", err)
	}
}

func TestIsImageAdmissionError(t *testing.T) {
	if !attachment.IsImageAdmissionError(attachment.NewAttachmentError("x", attachment.CodeInvalidImage)) {
		t.Fatal("admission code not recognized")
	}
	if attachment.IsImageAdmissionError(attachment.NewAttachmentError("x", attachment.CodeAttachmentCorrupt)) {
		t.Fatal("storage fault misrouted as admission")
	}
	if attachment.IsImageAdmissionError(errors.New("plain")) {
		t.Fatal("foreign error misrouted")
	}
	// Wrapped attachment errors keep their routing through Unwrap chains.
	wrapped := attachment.WrappedAttachmentError("outer", attachment.CodeAttachmentWriteFailed, attachment.NewAttachmentError("inner", attachment.CodeImageTooLarge))
	if !strings.Contains(wrapped.Error(), "IMAGE_TOO_LARGE") {
		t.Fatalf("wrapped = %v", wrapped)
	}
}

func TestAdmitPromptContent(t *testing.T) {
	store := &fakeStore{}
	// Text-only content never touches the store.
	textOnly := []llm.ContentBlock{{Type: llm.BlockText, Text: "hello"}}
	out, err := attachment.AdmitPromptContent(store, textOnly)
	if err != nil {
		t.Fatalf("text-only admit: %v", err)
	}
	if len(out) != 1 || out[0].Type != llm.BlockText || out[0].Text != "hello" {
		t.Fatalf("text-only = %+v", out)
	}
	if len(store.batches) != 0 {
		t.Fatalf("text-only touched the store: %d batches", len(store.batches))
	}

	// Image blocks admit as one batch and are replaced in place, order kept.
	mixed := []llm.ContentBlock{
		{Type: llm.BlockText, Text: "see:"},
		{Type: llm.BlockImage, Attachment: attachment.EncodedImageAttachment{MediaType: attachment.MediaPNG, Data: "AAAA"}},
		{Type: llm.BlockText, Text: "then"},
		{Type: llm.BlockImage, Attachment: attachment.EncodedImageAttachment{MediaType: attachment.MediaJPEG, Data: "AAA="}},
	}
	out, err = attachment.AdmitPromptContent(store, mixed)
	if err != nil {
		t.Fatalf("mixed admit: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("mixed = %d blocks, want 4", len(out))
	}
	if out[0].Type != llm.BlockText || out[2].Type != llm.BlockText {
		t.Fatalf("text order lost: %+v", out)
	}
	ref0, ok0 := out[1].Attachment.(attachment.ImageAttachmentRef)
	ref1, ok1 := out[3].Attachment.(attachment.ImageAttachmentRef)
	if !ok0 || !ok1 || ref0.AttachmentID != "id-a" || ref1.AttachmentID != "id-b" {
		t.Fatalf("refs = %+v / %+v", out[1].Attachment, out[3].Attachment)
	}
	// One batch of two images.
	if len(store.batches) != 1 || len(store.batches[0]) != 2 {
		t.Fatalf("batches = %+v", store.batches)
	}
}

func TestAdmitPromptContentRejectsBadImageBlock(t *testing.T) {
	store := &fakeStore{}
	// An image block without an encoded payload is a wire violation.
	bad := []llm.ContentBlock{{Type: llm.BlockImage, Attachment: "not-an-encoded"}}
	if _, err := attachment.AdmitPromptContent(store, bad); err == nil {
		t.Fatal("non-encoded image block must fail loud")
	}
}
