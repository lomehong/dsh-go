package gateway

import (
	"context"

	"dshgo/settings"
	"dshgo/typert"
)

// settingsProviderAbsent is the official absent-provider diagnostic verbatim
// (settings-controller index.ts provider()): the namespaces stay registered
// so calls answer with this actionable message instead of failing transport.
const settingsProviderAbsent = "settings service is absent: this deployment does not mount a settings provider (e.g. @deepseek-ai/dsh-settings-file) in its composition"

// SettingsController hosts the settings Remote namespace (official
// settings-controller). The controller delegates every write to the
// composition-provided settings Store (the @deepseek-ai/dsh-settings-file
// row's document store); when the composition mounts no provider, calls
// answer the official absent-provider diagnostic.
type SettingsController struct {
	lookup func() any
}

// NewSettingsController builds the namespace host. A lookup that returns nil
// (or a non-store) answers the absent-provider diagnostic, so profiles
// without a settings provider keep a working transport with an actionable
// message.
func NewSettingsController(lookup func() any) *SettingsController {
	if lookup == nil {
		lookup = func() any { return nil }
	}
	return &SettingsController{lookup: lookup}
}

func (c *SettingsController) store() *settings.Store {
	value := c.lookup()
	if store, ok := value.(*settings.Store); ok && store != nil {
		return store
	}
	return nil
}

func (c *SettingsController) provider() (any, error) {
	return nil, wrapGatewayError("gateway/internal", "settings", "", nil, "%s", settingsProviderAbsent)
}

func (c *SettingsController) storeScope(ns string) *settings.Scope {
	store := c.store()
	if store == nil {
		return nil
	}
	return store.EnsurePassthrough(ns)
}

func (c *SettingsController) namespaceAbsent(ns string) error {
	return wrapGatewayError("gateway/internal", "settings", "", nil,
		"settings: namespace %q has no registered schema in this deployment", ns)
}

// Describe answers the redacted layered snapshot of every registered
// namespace plus its serialized schema. Namespaces come from the store's
// registration table; an absent store answers the empty writable describe so
// the client settings plugin still boots.
func (c *SettingsController) Describe(ctx context.Context) (any, error) {
	if c.store() == nil {
		return map[string]any{
			"writable":    true,
			"hasDocument": false,
			"namespaces":  []any{},
		}, nil
	}
	namespaces := []any{}
	for _, ns := range c.store().Namespaces() {
		namespaces = append(namespaces, c.store().NamespaceView(ns))
	}
	return map[string]any{
		"writable":    true,
		"hasDocument": len(namespaces) > 0,
		"namespaces":  namespaces,
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
	if c.store() == nil {
		return c.provider()
	}
	return nil, c.storeScope(ns).Update(patch)
}

// Replace overwrites one namespace's stored user section wholesale.
func (c *SettingsController) Replace(ctx context.Context, ns string, section map[string]any, expectedRevision any) (any, error) {
	if c.store() == nil {
		return c.provider()
	}
	return nil, c.storeScope(ns).Replace(section)
}

// Mutate applies path-addressed edits to one namespace's user section.
func (c *SettingsController) Mutate(ctx context.Context, ns string, ops []any, expectedRevision any) (any, error) {
	if c.store() == nil {
		return c.provider()
	}
	scope := c.storeScope(ns)
	pathOps := make([]settings.PathOp, 0, len(ops))
	for _, raw := range ops {
		op, ok := raw.(map[string]any)
		if !ok {
			return nil, wrapGatewayError("gateway/internal", "settings", "", nil, "settings: mutate op is not an object")
		}
		kind, _ := op["op"].(string)
		if kind == "" {
			return nil, wrapGatewayError("gateway/internal", "settings", "", nil, "settings: mutate op has no op field")
		}
		pathOp := settings.PathOp{Op: kind}
		if rawPath, ok := op["path"].([]any); ok {
			for _, p := range rawPath {
				if s, ok := p.(string); ok {
					pathOp.Path = append(pathOp.Path, s)
				}
			}
		}
		pathOp.Value = op["value"]
		pathOps = append(pathOps, pathOp)
	}
	var revision *uint64
	if expectedRevision != nil {
		if f, ok := expectedRevision.(float64); ok {
			r := uint64(f)
			revision = &r
		}
	}
	return nil, scope.Mutate(pathOps, revision)
}

// OpenSettingsDocument opens the settings document in the host editor.
func (c *SettingsController) OpenSettingsDocument(ctx context.Context) (any, error) {
	return c.provider()
}

// OpenAgentPresetDirectory opens the authored Agent preset directory.
func (c *SettingsController) OpenAgentPresetDirectory(ctx context.Context) (any, error) {
	return c.provider()
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
