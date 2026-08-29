package attachment

import (
	"errors"
	"fmt"
)

// Attachment error codes: stable machine-routing failures used for protocol
// error routing. Image admission codes are caller-correctable; the rest are
// storage faults.
const (
	CodeTooManyImages         = "TOO_MANY_IMAGES"
	CodeImagesTooLarge        = "IMAGES_TOO_LARGE"
	CodeUnsupportedImgType    = "UNSUPPORTED_IMAGE_TYPE"
	CodeInvalidImageBase64    = "INVALID_IMAGE_BASE64"
	CodeInvalidImage          = "INVALID_IMAGE"
	CodeImageTypeMismatch     = "IMAGE_TYPE_MISMATCH"
	CodeImageTooLarge         = "IMAGE_TOO_LARGE"
	CodeImageTooManyPixels    = "IMAGE_TOO_MANY_PIXELS"
	CodeImageDimTooLarge      = "IMAGE_DIMENSION_TOO_LARGE"
	CodeInvalidAttachmentRef  = "INVALID_ATTACHMENT_REF"
	CodeAttachmentCorrupt     = "ATTACHMENT_CORRUPT"
	CodeAttachmentWriteFailed = "ATTACHMENT_WRITE_FAILED"
	CodeAttachmentNotFound    = "ATTACHMENT_NOT_FOUND"
	CodeAttachmentReadFailed  = "ATTACHMENT_READ_FAILED"
	CodeProjectionUnsupported = "ATTACHMENT_PROJECTION_UNSUPPORTED"
)

// imageAdmissionCodes is the runtime membership for structurally compatible
// errors crossing package boundaries.
var imageAdmissionCodes = map[string]bool{
	CodeTooManyImages:      true,
	CodeImagesTooLarge:     true,
	CodeUnsupportedImgType: true,
	CodeInvalidImageBase64: true,
	CodeInvalidImage:       true,
	CodeImageTypeMismatch:  true,
	CodeImageTooLarge:      true,
	CodeImageTooManyPixels: true,
	CodeImageDimTooLarge:   true,
}

// AttachmentError is a stable failure suitable for host RPC error mapping.
// Go note: errors.As/errors.Is route on the concrete type; consumers also
// route on Code for wire-boundary stability.
type AttachmentError struct {
	Message string
	Code    string
	Cause   error
}

func (e *AttachmentError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %v", e.Message, e.Code, e.Cause)
	}
	return fmt.Sprintf("%s (%s)", e.Message, e.Code)
}

func (e *AttachmentError) Unwrap() error {
	if e.Cause != nil {
		return e.Cause
	}
	return errors.New(e.Message)
}

// NewAttachmentError builds one attachment failure.
func NewAttachmentError(message, code string) *AttachmentError {
	return &AttachmentError{Message: message, Code: code}
}

// WrappedAttachmentError builds one attachment failure with a chained cause.
func WrappedAttachmentError(message, code string, cause error) *AttachmentError {
	return &AttachmentError{Message: message, Code: code, Cause: cause}
}

// IsImageAdmissionError distinguishes caller-correctable image admission
// failures from storage faults.
func IsImageAdmissionError(err error) bool {
	var attachmentErr *AttachmentError
	if !errors.As(err, &attachmentErr) {
		return false
	}
	return imageAdmissionCodes[attachmentErr.Code]
}
