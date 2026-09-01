// The model-facing read_image tool (official tool-fs read-image.ts): it
// commits a PNG/JPEG/WebP/GIF file to the attachment store and returns the
// image beside its text envelope. Registration happens only while an
// attachment store is mounted; execution still re-checks the store for
// direct callers and gates on the calling route's declared image input.
package toolfs

import (
	"fmt"
	"path"
	"strings"

	"dshgo/agent"
	"dshgo/attachment"
	"dshgo/fs"
	"dshgo/llm"
	"dshgo/tools"
)

// AttachmentStoreFace is the narrow store face read_image uses (the
// composed *local.AttachmentStore).
type AttachmentStoreFace interface {
	ImageLimits() attachment.ImageAttachmentLimits
	SaveImage(input attachment.SaveImageAttachment) (attachment.ImageAttachmentRef, error)
}

// LlmRouteSource resolves one exact model route's capabilities (the
// composed *llm.Runtime).
type LlmRouteSource interface {
	ResolveModelInfo(provider, model string) (llm.LlmResolvedModelInfo, error)
}

// imageMediaTypeForPath maps a model-supplied path to its declared image
// media type by extension; magic-byte validation at the attachment service
// stays authoritative. An extension-less path returns "" — the caller then
// sniffs the file bytes.
func imageMediaTypeForPath(filePath string) string {
	switch strings.ToLower(path.Ext(filePath)) {
	case ".png":
		return attachment.MediaPNG
	case ".jpg", ".jpeg":
		return attachment.MediaJPEG
	case ".webp":
		return attachment.MediaWebP
	case ".gif":
		return attachment.MediaGIF
	default:
		return ""
	}
}

// sniffImageMediaType identifies a supported image container from its file
// signature — the extension-less attachment-path path (upstream
// sniffImageMediaType). The result names the container the leading bytes
// claim; the attachment service's full decode stays authoritative.
func sniffImageMediaType(data []byte) string {
	if len(data) >= 8 &&
		data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4e && data[3] == 0x47 &&
		data[4] == 0x0d && data[5] == 0x0a && data[6] == 0x1a && data[7] == 0x0a {
		return attachment.MediaPNG
	}
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return attachment.MediaJPEG
	}
	if len(data) >= 6 && (string(data[0:6]) == "GIF87a" || string(data[0:6]) == "GIF89a") {
		return attachment.MediaGIF
	}
	if len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP" {
		return attachment.MediaWebP
	}
	return ""
}

// assertImageCapableRoute enforces the strict image-capability gate for the
// calling route: the session's latest routed provider/model (request header
// config, then agent options) must resolve through the LLM runtime to a
// model declaring `image` input explicitly. Unknown capability refuses
// instead of relying on an adapter failure after filesystem and attachment
// work.
func assertImageCapableRoute(llmSource LlmRouteSource, caller *agent.Agent, requestedPath string) error {
	if caller == nil {
		return fmt.Errorf("cannot read %q as an image: the current model route could not be resolved", requestedPath)
	}
	routed := caller.Session.RequestHeader()
	provider := ""
	model := ""
	if routed != nil {
		provider = routed.Config.Provider
		model = routed.Config.Model
	}
	if provider == "" {
		provider = caller.Options.Provider
	}
	if model == "" {
		model = caller.Options.Model
	}
	if provider == "" || model == "" || llmSource == nil {
		return fmt.Errorf("cannot read %q as an image: the current model route could not be resolved", requestedPath)
	}
	active, err := llmSource.ResolveModelInfo(provider, model)
	if err != nil {
		return err
	}
	declares := false
	for _, modality := range active.InputModalities {
		if modality == llm.ModalityImage {
			declares = true
			break
		}
	}
	if !declares {
		return fmt.Errorf("cannot read %q as an image: model %q does not declare image input; switch to an image-capable model to read images", requestedPath, model)
	}
	return nil
}

// formatImageReadOutput formats an image read as the model-facing envelope
// beside its image block. A downscaled read names the on-disk dimensions
// and the multiplier that maps coordinates measured on the attached image
// back onto the original file.
func formatImageReadOutput(displayPath string, ref attachment.ImageAttachmentRef) string {
	scaled := ""
	if ref.OriginalDimensions != nil {
		// Integer rounding can give the two axes slightly different ratios,
		// so the advice names one multiplier only when both round to the
		// same value.
		x := fmt.Sprintf("%.2f", float64(ref.OriginalDimensions.Width)/float64(ref.Width))
		y := fmt.Sprintf("%.2f", float64(ref.OriginalDimensions.Height)/float64(ref.Height))
		advice := fmt.Sprintf("multiply x coordinates by %s and y coordinates by %s", x, y)
		if x == y {
			advice = fmt.Sprintf("multiply coordinates by %s", x)
		}
		scaled = fmt.Sprintf(" (downscaled from %dx%d px; %s to locate features in the original file)",
			ref.OriginalDimensions.Width, ref.OriginalDimensions.Height, advice)
	}
	return fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s image, %dx%d px, %d bytes%s\n</content>",
		displayPath, ref.MediaType, ref.Width, ref.Height, ref.Bytes, scaled)
}

// imageReadContent projects one image read into its two content blocks.
func imageReadContent(value map[string]any) []llm.ContentBlock {
	ref, _ := value["image"].(attachment.ImageAttachmentRef)
	displayPath, _ := value["path"].(string)
	return []llm.ContentBlock{
		{Type: llm.BlockText, Text: formatImageReadOutput(displayPath, ref)},
		{Type: llm.BlockImage, Attachment: ref},
	}
}

// imageValueSchema is the structured `image` outcome schema.
func imageValueSchema() tools.ValueSchemaSpec {
	return tools.ValueSchemaSpec{
		Type:                 "object",
		AdditionalProperties: boolPtr(false),
		Properties: map[string]tools.PropSpec{
			"attachmentId": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
			"mediaType": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type: "string", Enum: []any{attachment.MediaPNG, attachment.MediaJPEG, attachment.MediaWebP, attachment.MediaGIF},
			}, Required: true},
			"bytes":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
			"width":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
			"height": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
			"name":   {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}},
			"originalDimensions": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"width":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
					"height": {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "integer"}, Required: true},
				},
			}},
		},
	}
}

// registerReadImage defines and registers the read_image tool. The caller
// owns the attachments gate: it invokes this only while a durable store is
// mounted.
func registerReadImage(runtime *tools.ToolRuntime, controller *controller, deps RegisterDeps, store AttachmentStoreFace) (func(), error) {
	executable := func(args map[string]any, exec *tools.ToolRunContext) (any, error) {
		filePath, _ := args["file_path"].(string)
		if strings.TrimSpace(filePath) == "" {
			return nil, fmt.Errorf("file_path must be a non-empty string")
		}
		// Every gate runs before any filesystem I/O so a refusal never
		// leaks partial reads or attachment writes. The declared media type
		// comes from the extension when one declares it; an extension-less
		// path is accepted and identified from its file signature after the
		// read (upstream read-image extensionless attachment paths).
		declared := imageMediaTypeForPath(filePath)
		if declared == "" && path.Ext(filePath) != "" {
			return nil, fmt.Errorf("cannot read %q: the %s extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats", filePath, strings.ToLower(path.Ext(filePath)))
		}
		if store == nil {
			return nil, fmt.Errorf("cannot read %q as an image: no attachment service is mounted", filePath)
		}
		limits := store.ImageLimits()
		caller := deps.Agents.ResolveAgent(execScope(exec))
		if err := assertImageCapableRoute(deps.Llm, caller, filePath); err != nil {
			return nil, err
		}
		ctx := execSignal(exec)
		target, info, err := resolveRegularReadTarget(ctx, controller, filePath, exec)
		if err != nil {
			return nil, err
		}
		// The tool result is one message carrying one image, so the
		// per-message aggregate bound applies beside the per-image bound.
		byteCap := int64(limits.MaxImageBytes)
		if limits.MaxMessageImageBytes < limits.MaxImageBytes {
			byteCap = int64(limits.MaxMessageImageBytes)
		}
		data, err := deps.Backend.ReadBytes(ctx, target, byteCap)
		if err != nil {
			return nil, err
		}
		mediaType := declared
		if mediaType == "" {
			mediaType = sniffImageMediaType(data)
		}
		if mediaType == "" {
			return nil, fmt.Errorf("cannot read %q: the file content is not a supported image format; read_image accepts PNG/JPEG/WebP/GIF", target.DisplayPath)
		}
		accepted := false
		for _, acceptedType := range limits.MediaTypes {
			if acceptedType == mediaType {
				accepted = true
				break
			}
		}
		if !accepted {
			return nil, fmt.Errorf("cannot read %q: %s images are not accepted by this deployment", target.DisplayPath, mediaType)
		}
		// Persist before returning: the image block must reference a
		// durably committed object by the time the tool/result event is
		// appended.
		ref, err := store.SaveImage(attachment.SaveImageAttachment{
			Data:      data,
			MediaType: mediaType,
			Name:      path.Base(target.DisplayPath),
		})
		if err != nil {
			return nil, mapAttachmentRefusal(err, target.DisplayPath, mediaType, limits)
		}
		controller.recordObservation(target, fs.ObservationPresent(info.Version), exec)
		return map[string]any{
			"path":  target.DisplayPath,
			"image": ref,
		}, nil
	}
	definition, err := tools.DefineTool(tools.DefineToolOptions{
		Name: "read_image",
		Description: "Read a PNG/JPEG/WebP/GIF file and return the image itself. " +
			"Harness validates and downscales large supported images before the next model request, so use this tool directly instead of installing image libraries or creating thumbnails merely to inspect an image. " +
			"Independent files may be read concurrently in small batches. Requires the current model to accept image input.",
		Parameters: map[string]tools.PropSpec{
			"file_path": {ValueSchemaSpec: tools.ValueSchemaSpec{
				Type:        "string",
				Description: "Path to the image file, resolved by the filesystem backend.",
			}, Required: true},
		},
		Output: tools.ToolOutput{
			Schema: &tools.ValueSchemaSpec{
				Type:                 "object",
				AdditionalProperties: boolPtr(false),
				Properties: map[string]tools.PropSpec{
					"path":  {ValueSchemaSpec: tools.ValueSchemaSpec{Type: "string"}, Required: true},
					"image": {ValueSchemaSpec: imageValueSchema(), Required: true},
				},
			},
			Render: func(args map[string]any, value any) []llm.ContentBlock {
				outcome, _ := value.(map[string]any)
				return imageReadContent(outcome)
			},
		},
		// Content-addressed attachment writes are idempotent, so
		// concurrent reads of the same file cannot conflict.
		IsConcurrencySafe: func(args map[string]any) bool { return true },
		Execute:           executable,
	})
	if err != nil {
		return nil, err
	}
	return runtime.Register(definition)
}

// errorsAsAttachmentError unwraps one attachment error.
func errorsAsAttachmentError(err error, target **attachment.AttachmentError) bool {
	if e, ok := err.(*attachment.AttachmentError); ok {
		*target = e
		return true
	}
	return false
}

// mapAttachmentRefusal keeps dimension refusals recoverable: an oversized
// image must never enter durable history, where it would ride every later
// model request past provider-side dimension rejections.
func mapAttachmentRefusal(err error, displayPath string, mediaType string, limits attachment.ImageAttachmentLimits) error {
	var attachmentErr *attachment.AttachmentError
	if !errorsAsAttachmentError(err, &attachmentErr) {
		return err
	}
	switch attachmentErr.Code {
	case attachment.CodeImageDimTooLarge:
		return fmt.Errorf("cannot read %q: at least one image side exceeds the %dpx limit; downscale the image and read the smaller copy", displayPath, limits.MaxImageDimension)
	case attachment.CodeImageTooManyPixels:
		return fmt.Errorf("cannot read %q: the image exceeds the %d-pixel decoded-size limit; downscale the image and read the smaller copy", displayPath, limits.MaxImagePixels)
	case attachment.CodeImageTooLarge:
		return fmt.Errorf("cannot read %q: the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy", displayPath)
	case attachment.CodeAttachmentWriteFailed:
		if strings.Contains(attachmentErr.Message, "16-bit PNG") {
			return fmt.Errorf("cannot read %q: the 16-bit PNG could not be converted to the normalized 8-bit sRGB form; convert it to an 8-bit PNG/JPEG/WebP and retry", displayPath)
		}
		return err
	case attachment.CodeImageTypeMismatch:
		extension := strings.ToLower(path.Ext(displayPath))
		return fmt.Errorf("cannot read %q: the %s extension declares %s, but the bytes use a different image format; rename the file to match its actual format if it is PNG/JPEG/WebP/GIF, or convert it to one of those formats", displayPath, extension, mediaType)
	default:
		return err
	}
}
