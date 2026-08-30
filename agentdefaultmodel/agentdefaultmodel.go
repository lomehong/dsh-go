// Package agentdefaultmodel ports @deepseek-ai/dsh-agent-default-model: the
// default model selection for an Agent without a session-specific
// selection. It owns the selection independently of any Host or transport;
// the composition entry remains usable without a settings provider, and
// when one is mounted its user layer is read live.
package agentdefaultmodel

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/settings"
)

// SettingsNamespace carries the default model selection for future Agents.
const SettingsNamespace = "agent-default-model"

// Settings is the stored and composed default model selection.
type Settings struct {
	// Provider is the registered provider route.
	Provider string `json:"provider"`
	// Model is the provider-owned model id.
	Model string `json:"model"`
	// ReasoningEffort is the adapter-owned reasoning effort, or empty for
	// provider/default behavior.
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
}

// Config owns the default model selection. Every consumer reads through
// CurrentSelection, so no registration-level fact needs rebuilding when the
// settings document changes.
type Config struct {
	mu   sync.Mutex
	base Settings
	// source is the live settings layer when one is mounted; nil keeps the
	// composition entry.
	source func() Settings
}

// New builds one config from the composition entry.
func New(entry Settings) (*Config, error) {
	if msg := validate(entry); msg != "" {
		return nil, fmt.Errorf("agent-default-model: %s", msg)
	}
	return &Config{base: entry}, nil
}

func validate(entry Settings) string {
	if strings.TrimSpace(entry.Provider) == "" {
		return "provider must be a non-empty string"
	}
	if strings.TrimSpace(entry.Model) == "" {
		return "model must be a non-empty string"
	}
	return ""
}

// SetSource installs the live settings layer (the store scope's user
// document). A nil source falls back to the composition entry.
func (c *Config) SetSource(source func() Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.source = source
}

func (c *Config) current() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.source != nil {
		return c.source()
	}
	return c.base
}

// CurrentSelection reads the current default model selection as a detached
// provider, model, and optional reasoning selection.
func (c *Config) CurrentSelection() agent.ModelSelection {
	entry := c.current()
	selection := agent.ModelSelection{Provider: entry.Provider, Model: entry.Model}
	if entry.ReasoningEffort != "" {
		selection.ReasoningEffort = llm.ReasoningEffortID(entry.ReasoningEffort)
		selection.HasReasoningEffort = true
	}
	return selection
}

// SaveSelection saves the complete default model selection. A deployment
// without a settings provider keeps its composition entry.
func (c *Config) SaveSelection(store *settings.Store, scope *settings.Scope, next agent.ModelSelection) error {
	if store == nil || scope == nil {
		c.mu.Lock()
		c.base = Settings{Provider: next.Provider, Model: next.Model, ReasoningEffort: string(next.ReasoningEffort)}
		c.mu.Unlock()
		return nil
	}
	document := map[string]any{"provider": next.Provider, "model": next.Model}
	if next.HasReasoningEffort && next.ReasoningEffort != "" {
		document["reasoningEffort"] = string(next.ReasoningEffort)
	}
	if err := scope.Replace(document); err != nil {
		return err
	}
	return nil
}

// ParseSection validates one settings document and returns the selection it
// carries (the zod schema's runtime half). Unknown keys are ignored by the
// section envelope; provider and model are required.
func ParseSection(value map[string]any) (Settings, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Settings{}, err
	}
	var decoded Settings
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Settings{}, err
	}
	if msg := validate(decoded); msg != "" {
		return Settings{}, fmt.Errorf("agent-default-model: %s", msg)
	}
	return decoded, nil
}

// Envelope is the JSON-schema envelope for the settings section.
func Envelope() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"provider": {"type": "string"},
			"model": {"type": "string"},
			"reasoningEffort": {"type": "string"}
		}
	}`)
}

// RegisterSection mounts the settings section and couples its user layer to
// the service. base doubles as the section's composition default. The
// section scope returns for SaveSelection callers.
func RegisterSection(store *settings.Store, base Settings) (*Config, *settings.Scope, error) {
	if store == nil {
		return nil, nil, fmt.Errorf("agent-default-model: the settings store is required")
	}
	config, err := New(base)
	if err != nil {
		return nil, nil, err
	}
	defaults := func() map[string]any {
		// Fixed initial values: the Defaults hook runs inside the store
		// lock, so it must not read back through the live scope.
		document := map[string]any{"provider": base.Provider, "model": base.Model}
		if base.ReasoningEffort != "" {
			document["reasoningEffort"] = base.ReasoningEffort
		}
		return document
	}
	scope, err := store.Register(SettingsNamespace, &settings.Schema{
		Envelope: Envelope(),
		Defaults: defaults,
		Validate: func(value map[string]any) error {
			_, err := ParseSection(value)
			return err
		},
	}, defaults())
	if err != nil {
		return nil, nil, err
	}
	config.SetSource(func() Settings {
		parsed, err := ParseSection(scope.Get())
		if err != nil {
			return config.currentBase()
		}
		return parsed
	})
	return config, scope, nil
}

// currentBase reads the composition entry (the validation-failure
// fallback).
func (c *Config) currentBase() Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.base
}
