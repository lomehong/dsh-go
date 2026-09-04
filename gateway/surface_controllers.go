package gateway

import (
	"context"

	"dshgo/llm"
	"dshgo/preset"
	"dshgo/typert"
)

// AgentPresetsController hosts the agentPresets Remote namespace (official
// dsh-agent-presets). List projects the composed preset roster — the same
// shipped/user roots the launcher mounts agents from.
type AgentPresetsController struct {
	lookup func() any
}

// NewAgentPresetsController builds the namespace host. The lookup resolves
// the composition's preset mounts per call (nil provider answers an empty
// roster with authorable=false).
func NewAgentPresetsController(lookup func() any) *AgentPresetsController {
	if lookup == nil {
		lookup = func() any { return nil }
	}
	return &AgentPresetsController{lookup: lookup}
}

// mounts resolves the composed preset mounts, or nil when absent.
func (c *AgentPresetsController) mounts() *preset.Mounts {
	if mounts, ok := c.lookup().(*preset.Mounts); ok && mounts != nil {
		return mounts
	}
	return nil
}

// List answers the preset roster (official agentPresets/list): every
// discovered preset with its trust/default/broken facts, plus the
// authorable flag the UI gates preset authoring on.
func (c *AgentPresetsController) List(ctx context.Context) (any, error) {
	mounts := c.mounts()
	if mounts == nil {
		return map[string]any{"presets": []any{}, "authorable": false}, nil
	}
	presets, err := mounts.List()
	if err != nil {
		return nil, wrapGatewayError("gateway/internal", "agentPresets/list", "", err, "preset discovery failed")
	}
	defaultID := mounts.DefaultID()
	items := make([]any, 0, len(presets))
	for _, p := range presets {
		row := map[string]any{
			"id":        p.ID,
			"trust":     p.Trust,
			"isDefault": p.ID == defaultID,
		}
		if p.Name != nil {
			row["name"] = *p.Name
		}
		if p.Description != nil {
			row["description"] = *p.Description
		}
		if p.Broken != nil {
			row["broken"] = *p.Broken
		}
		items = append(items, row)
	}
	return map[string]any{"presets": items, "authorable": mounts.Authorable()}, nil
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

// LlmController hosts the llm Remote namespace (official dsh-llm).
type LlmController struct {
	runtimeLookup func() any
}

// NewLlmController builds the namespace host.
func NewLlmController(runtimeLookup func() any) *LlmController {
	if runtimeLookup == nil {
		runtimeLookup = func() any { return nil }
	}
	return &LlmController{runtimeLookup: runtimeLookup}
}

func (c *LlmController) runtime() *llm.Runtime {
	if v, ok := c.runtimeLookup().(*llm.Runtime); ok && v != nil {
		return v
	}
	return nil
}

// ListProviders answers the configured-provider roster (official
// llm/listProviders): array of {id,name}, empty until a provider registers.
func (c *LlmController) ListProviders(ctx context.Context) (any, error) {
	rt := c.runtime()
	if rt == nil {
		return []any{}, nil
	}
	infos := rt.ListProviders()
	out := make([]any, 0, len(infos))
	for _, info := range infos {
		out = append(out, map[string]any{"id": info.ID, "name": info.Name})
	}
	return out, nil
}

// ListConfigurableProviders answers the configurable-provider catalog
// (official llm/listConfigurableProviders): array of {provider,displayName,
// settingsNs,settingsPath}, empty until provider settings namespaces exist.
func (c *LlmController) ListConfigurableProviders(ctx context.Context) (any, error) {
	rt := c.runtime()
	if rt == nil {
		return []any{}, nil
	}
	entries := rt.ListConfigurableProviders()
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"provider":     e.Provider,
			"displayName":  e.DisplayName,
			"settingsNs":   e.SettingsNs,
			"settingsPath": e.SettingsPath,
		})
	}
	return out, nil
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

// PluginInventoryRow is one wire inventory row: the composed profile entry
// facts the Settings Plugins panel renders.
type PluginInventoryRow struct {
	EntryID    any    `json:"entryId"`
	ModuleName string `json:"moduleName"`
	Enabled    bool   `json:"enabled"`
	FiberPhase *any   `json:"fiberPhase"`
}

// PluginInventoryController hosts the pluginInventory Remote namespace
// (official dsh-host-plugin-inventory): a read-only projection of the
// composed profile rows.
type PluginInventoryController struct {
	rows   func() []PluginInventoryRow
	presets func() any
}

// NewPluginInventoryController builds the namespace host over the composed
// entry snapshot.
func NewPluginInventoryController(rows func() []PluginInventoryRow, presets func() any) *PluginInventoryController {
	if rows == nil {
		rows = func() []PluginInventoryRow { return nil }
	}
	if presets == nil {
		presets = func() any { return nil }
	}
	return &PluginInventoryController{rows: rows, presets: presets}
}

// List answers the loader-entry snapshot (official pluginInventory/list):
// every composed profile row with its enabled flag; fiberPhase stays null
// because the Go host holds no live plugin fibers.
func (c *PluginInventoryController) List(ctx context.Context) (any, error) {
	rows := c.rows()
	entries := make([]any, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, row)
	}
	result := map[string]any{"entries": entries}
	if m, ok := c.presets().(*preset.Mounts); ok && m != nil {
		list, err := m.List()
		if err == nil {
			presets := make([]any, 0, len(list))
			for _, p := range list {
				presets = append(presets, p)
			}
			result["agentPresets"] = presets
		}
	}
	return result, nil
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
