package gateway

import (
	"context"

	"dshgo/typert"
)

// AgentPresetsController hosts the agentPresets Remote namespace (official
// dsh-agent-presets). The Go host keeps a composed preset catalog on the
// agent side but no Remote authoring surface yet, so list answers the honest
// empty roster and authorable=false (the UI hides preset authoring instead
// of exposing a broken create button).
type AgentPresetsController struct{}

// NewAgentPresetsController builds the namespace host.
func NewAgentPresetsController() *AgentPresetsController { return &AgentPresetsController{} }

// List answers the preset roster (official agentPresets/list): the shape the
// Settings Agent-preset panel validates, with an empty presets array.
func (c *AgentPresetsController) List(ctx context.Context) (any, error) {
	return map[string]any{"presets": []any{}, "authorable": false}, nil
}

// Contribution is the strict typert definition of the agentPresets namespace.
func (c *AgentPresetsController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "agent-presets-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "agentPresets.list",
				Service:               "agentPresetsController",
				Namespace:             "agentPresets",
				Method:                "list",
				Implementation:        "List",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
		},
	}
}

// LlmController hosts the llm Remote namespace (official dsh-llm). The Go
// host runs model configuration through the settings surface, not this
// discovery namespace; the catalog polls answer honest empty rosters so the
// Settings Models panel renders instead of failing schema validation.
type LlmController struct{}

// NewLlmController builds the namespace host.
func NewLlmController() *LlmController { return &LlmController{} }

// ListProviders answers the configured-provider roster (official
// llm/listProviders): array of {id,name}, empty until a provider registers.
func (c *LlmController) ListProviders(ctx context.Context) (any, error) {
	return []any{}, nil
}

// ListConfigurableProviders answers the configurable-provider catalog
// (official llm/listConfigurableProviders): array of {provider,displayName,
// settingsNs,settingsPath}, empty until provider settings namespaces exist.
func (c *LlmController) ListConfigurableProviders(ctx context.Context) (any, error) {
	return []any{}, nil
}

// Contribution is the strict typert definition of the llm namespace.
func (c *LlmController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "llm-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "llm.listProviders",
				Service:               "llmController",
				Namespace:             "llm",
				Method:                "listProviders",
				Implementation:        "ListProviders",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
			{
				ID:                    "llm.listConfigurableProviders",
				Service:               "llmController",
				Namespace:             "llm",
				Method:                "listConfigurableProviders",
				Implementation:        "ListConfigurableProviders",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
		},
	}
}

// PluginInventoryController hosts the pluginInventory Remote namespace
// (official dsh-host-plugin-inventory). The Go host's loader rows are the
// boot catalog, not dynamic client plugins, so the snapshot answers an empty
// entry list (the Settings Plugins panel renders its empty state).
type PluginInventoryController struct{}

// NewPluginInventoryController builds the namespace host.
func NewPluginInventoryController() *PluginInventoryController { return &PluginInventoryController{} }

// List answers the loader-entry snapshot (official pluginInventory/list):
// {entries:[], agentPresets:[]} — no dynamic rows on the Go host.
func (c *PluginInventoryController) List(ctx context.Context) (any, error) {
	return map[string]any{"entries": []any{}}, nil
}

// Contribution is the strict typert definition of the pluginInventory namespace.
func (c *PluginInventoryController) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	return typert.Contribution{
		Package: "plugin-inventory-controller",
		Face:    typert.FaceHost,
		Invocations: []typert.InvocationDescriptor{
			{
				ID:                    "pluginInventory.list",
				Service:               "pluginInventoryController",
				Namespace:             "pluginInventory",
				Method:                "list",
				Implementation:        "List",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Result:                jsonCodec,
			},
		},
	}
}
