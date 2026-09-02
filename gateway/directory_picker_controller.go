package gateway

import (
	"context"

	"dshgo/typert"
)

// directoryPickerUnavailable is the diagnostic answered while no
// directory-picking backend is composed in the Go web profile (official
// dsh-host-directory-picker rows: T3-planned, see DECISIONS).
const directoryPickerUnavailable = "directoryPicker: no directory-picking backend is composed in this deployment"

// DirectoryPickerController hosts the directoryPicker Remote namespace
// (official DirectoryPickerController): the OS chooser, the in-app browser
// listing, and child-directory creation, each gated by the composed
// backend's capability kind.
type DirectoryPickerController struct{}

// NewDirectoryPickerController builds the namespace host.
func NewDirectoryPickerController() *DirectoryPickerController {
	return &DirectoryPickerController{}
}

// Pick opens the host's OS chooser; the chosen absolute path or null on
// operator cancel.
func (c *DirectoryPickerController) Pick(ctx context.Context) (any, error) {
	return nil, wrapGatewayError("directory-picker/unavailable", "directoryPicker/pick", "", nil, "%s", directoryPickerUnavailable)
}

// List answers one directory level with its ancestry for the in-app browser.
func (c *DirectoryPickerController) List(ctx context.Context, path *string) (any, error) {
	return nil, wrapGatewayError("directory-picker/unavailable", "directoryPicker/list", "", nil, "%s", directoryPickerUnavailable)
}

// CreateDirectory creates one child directory under an absolute parent.
func (c *DirectoryPickerController) CreateDirectory(ctx context.Context, path string, name string) (any, error) {
	return nil, wrapGatewayError("directory-picker/unavailable", "directoryPicker/createDirectory", "", nil, "%s", directoryPickerUnavailable)
}

// Contribution is the strict typert definition of the directoryPicker
// namespace.
func (c *DirectoryPickerController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	inv := typert.InvocationReceiver{Kind: typert.ReceiverDirect}
	descriptor := func(id, method, implementation string, params ...typert.InvocationParameterDescriptor) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID:                    id,
			Service:               "directoryPickerController",
			Namespace:             "directoryPicker",
			Method:                method,
			Implementation:        implementation,
			Invocation:            inv,
			CancellationParameter: "signal",
			Parameters:            params,
			Result:                jsonCodec,
		}
	}
	return typert.Contribution{
		Package: "directory-picker-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			descriptor("directoryPicker.pick", "pick", "Pick"),
			descriptor("directoryPicker.list", "list", "List", paramJSON("path")),
			descriptor("directoryPicker.createDirectory", "createDirectory", "CreateDirectory", paramJSON("path"), paramJSON("name")),
		},
	}
}

// paramJSON is the standard named JSON parameter descriptor.
func paramJSON(name string) typert.InvocationParameterDescriptor {
	return typert.InvocationParameterDescriptor{
		Name:   name,
		Wire:   name,
		Source: typert.SourceJSON,
		Codec:  typert.Codec{Mode: typert.CodecSrcJSON},
	}
}
