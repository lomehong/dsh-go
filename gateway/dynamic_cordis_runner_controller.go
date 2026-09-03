package gateway

import (
	"context"

	"dshgo/typert"
)

// DynamicCordisRunnerController hosts the dynamicCordisRunner Remote
// namespace (official cordis-host-runner DynamicCordisRunnerService):
// client plugins sync their inspect-provider manifests through it at boot.
// The Go host keeps no client inspect registry yet, so syncInspectManifest
// accepts the manifest and answers null — the official return value.
type DynamicCordisRunnerController struct{}

// NewDynamicCordisRunnerController builds the namespace host.
func NewDynamicCordisRunnerController() *DynamicCordisRunnerController {
	return &DynamicCordisRunnerController{}
}

// SyncInspectManifest receives one client's inspect-provider manifest batch.
func (c *DynamicCordisRunnerController) SyncInspectManifest(ctx context.Context, providers []any) (any, error) {
	return nil, nil
}

// Inventory answers the client plugin-inventory poll (official
// DynamicCordisRunnerService.inventory). The Go host runs no dynamic cordis
// packages, so the honest empty roster is an empty array — the official
// shape for "no dynamic plugin has ever run".
func (c *DynamicCordisRunnerController) Inventory(ctx context.Context) (any, error) {
	return []any{}, nil
}

// Contribution is the strict typert definition of the dynamicCordisRunner
// namespace.
func (c *DynamicCordisRunnerController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "dynamic-cordis-runner-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "dynamicCordisRunner.syncInspectManifest",
				Service:               "dynamicCordisRunnerController",
				Namespace:             "dynamicCordisRunner",
				Method:                "syncInspectManifest",
				Implementation:        "SyncInspectManifest",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Parameters: []typert.InvocationParameterDescriptor{
					{Name: "providers", Wire: "providers", Source: typert.SourceJSON, Codec: jsonCodec},
				},
				Result: jsonCodec,
			},
			{
				ID:                    "dynamicCordisRunner.inventory",
				Service:               "dynamicCordisRunnerController",
				Namespace:             "dynamicCordisRunner",
				Method:                "inventory",
				Implementation:        "Inventory",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
		},
	}
}
