package gateway

import (
	"context"

	"dshgo/typert"
)

// settingsProviderAbsent is the official absent-provider diagnostic verbatim
// (settings-controller index.ts provider()): the namespaces stay registered
// so calls answer with this actionable message instead of failing transport.
const settingsProviderAbsent = "settings service is absent: this deployment does not mount a settings provider (e.g. @deepseek-ai/dsh-settings-file) in its composition"

// SettingsController hosts the settings Remote namespace (official
// settings-controller). The Go web profile does not mount a settings
// provider yet, so every call answers the official absent-provider
// diagnostic; the write-side endpoints carry their full wire parameters so
// generated clients bind them once a provider lands.
type SettingsController struct{}

// NewSettingsController builds the namespace host.
func NewSettingsController() *SettingsController { return &SettingsController{} }

func absentProvider() (any, error) {
	return nil, wrapGatewayError("gateway/internal", "settings", "", nil, "%s", settingsProviderAbsent)
}

// Describe answers the redacted layered snapshot of every registered
// namespace plus its serialized schema. The Go settings store composes no
// schema namespaces yet, so the honest describe is a writable provider with
// an empty namespace view — enough for the client settings plugin to boot,
// provide settingsScope, and let the locale cascade activate.
func (c *SettingsController) Describe(ctx context.Context) (any, error) {
	return map[string]any{
		"writable":    true,
		"hasDocument": false,
		"namespaces":  []any{},
	}, nil
}

// CanOpenAgentPresetDirectory reports whether the deployment opens an
// authored Agent preset directory natively.
func (c *SettingsController) CanOpenAgentPresetDirectory(ctx context.Context) (any, error) {
	return false, nil
}

// Update merges a patch into one namespace's stored user section. The
// optional expectedRevision rides as a decoded JSON number (any — a typed
// pointer would panic under the reflection dispatcher when present).
func (c *SettingsController) Update(ctx context.Context, ns string, patch map[string]any, expectedRevision any) (any, error) {
	return absentProvider()
}

// Replace overwrites one namespace's stored user section wholesale.
func (c *SettingsController) Replace(ctx context.Context, ns string, section map[string]any, expectedRevision any) (any, error) {
	return absentProvider()
}

// Mutate applies path-addressed edits to one namespace's user section.
func (c *SettingsController) Mutate(ctx context.Context, ns string, ops []any, expectedRevision any) (any, error) {
	return absentProvider()
}

// OpenSettingsDocument opens the settings document in the host editor.
func (c *SettingsController) OpenSettingsDocument(ctx context.Context) (any, error) {
	return absentProvider()
}

// OpenAgentPresetDirectory opens the authored Agent preset directory.
func (c *SettingsController) OpenAgentPresetDirectory(ctx context.Context) (any, error) {
	return absentProvider()
}

// Contribution is the strict typert definition of the settings namespace.
// Wire methods stay lowercase (official endpoint grammar); Implementation
// names the Go receiver method the reflection dispatcher calls.
func (c *SettingsController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	param := func(name string) typert.InvocationParameterDescriptor {
		return typert.InvocationParameterDescriptor{Name: name, Wire: name, Source: typert.SourceJSON, Codec: jsonCodec}
	}
	inv := typert.InvocationReceiver{Kind: typert.ReceiverDirect}
	descriptor := func(id, method, implementation string, params ...typert.InvocationParameterDescriptor) typert.InvocationDescriptor {
		return typert.InvocationDescriptor{
			ID:                    id,
			Service:               "settingsController",
			Namespace:             "settings",
			Method:                method,
			Implementation:        implementation,
			Invocation:            inv,
			CancellationParameter: "signal",
			Parameters:            params,
			Result:                jsonCodec,
		}
	}
	return typert.Contribution{
		Package: "settings-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			descriptor("settings.describe", "describe", "Describe"),
			descriptor("settings.canOpenAgentPresetDirectory", "canOpenAgentPresetDirectory", "CanOpenAgentPresetDirectory"),
			descriptor("settings.update", "update", "Update", param("ns"), param("patch"), param("expectedRevision")),
			descriptor("settings.replace", "replace", "Replace", param("ns"), param("section"), param("expectedRevision")),
			descriptor("settings.mutate", "mutate", "Mutate", param("ns"), param("ops"), param("expectedRevision")),
			descriptor("settings.openSettingsDocument", "openSettingsDocument", "OpenSettingsDocument"),
			descriptor("settings.openAgentPresetDirectory", "openAgentPresetDirectory", "OpenAgentPresetDirectory", param("path")),
		},
	}
}
