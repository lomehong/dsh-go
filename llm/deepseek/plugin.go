// The llm-deepseek plugin assembly: registers the DeepSeek adapter for the
// `deepseek-official` provider route with connection facts resolved per
// request instead of frozen at load. The plugin layers its config under the
// optional `llm-deepseek` user-settings section and resolves the API key
// through the optional credential seam, so a changed base URL, catalog, or
// key reaches the very next request without restarting anything, while an
// in-flight stream keeps the facts it started with. The one
// registration-captured fact — the retry policy — re-registers the route in
// place when it changes. Port of packages/llm/llm-deepseek/src/index.ts
// (apply/resolve half; the image / Files-API surface is deferred with the
// adapter's serializer path).
package deepseek

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"dshgo/cordis"
	"dshgo/credentials"
	"dshgo/llm"
	"dshgo/settings"
)

// SettingsNamespace is the user-settings section this plugin owns.
const SettingsNamespace = "llm-deepseek"

// PluginDeps carries the seams the official apply() reads off the cordis
// context. Every seam except Runtime is optional; an absent seam degrades
// exactly like ctx.get(name) returning undefined.
type PluginDeps struct {
	// Runtime is the LLM registry the adapter registers on.
	Runtime *llm.Runtime
	// Settings hosts the `llm-deepseek` section; nil keeps the static
	// config authoritative forever.
	Settings *settings.Store
	// Credentials is the managed credential store; nil makes the
	// environment the whole credential plane.
	Credentials credentials.Provider
	// Logger receives keep-last-good and replace-failure records.
	Logger cordis.Logger
	// Extensions registers independently owned top-level request fields
	// contributed by companion plugins; nil carries none.
	Extensions *ExtensionRegistry
	// Environment reads launch-environment layers (base URL, ambient key);
	// nil skips the ambient lookups.
	Environment environmentValue
	// ResolveUserID resolves the anonymous user id; nil omits the header
	// (the adapter tolerates absence).
	ResolveUserID func() string
}

// Plugin is one live llm-deepseek registration.
type Plugin struct {
	deps PluginDeps

	mu sync.Mutex
	// replaceMu serializes EnsureRegistrationFacts without blocking the
	// adapter's re-entrant Options reads during a replace.
	replaceMu        sync.Mutex
	current          Config // the authoritative raw config (static or latest snapshot)
	rawJSON          string // canonical JSON of the config behind lastGood
	lastGood         *ConnectionOptions
	badReportedJSON  string // the bad snapshot already logged (empty = none)
	registeredPolicy *llm.ResolvedRetryPolicy
	registration     *llm.RegistrationHandle
	scope            *settings.Scope
	settingsDisposer cordis.Disposer
}

// Apply validates the initial config, registers the adapter, and installs
// the settings section. Fails loud when the static composition cannot
// resolve (the official load-time contract).
func Apply(deps PluginDeps, config Config) (*Plugin, error) {
	if deps.Runtime == nil {
		return nil, fmt.Errorf("llm-deepseek: the llm runtime is required")
	}
	if deps.Logger == nil {
		deps.Logger = nilLogger{}
	}
	plugin := &Plugin{deps: deps}
	// The one load-time resolve: a static composition that cannot produce
	// valid connection facts fails before anything registers.
	options, err := ResolveAdapterOptions(config, deps.Environment)
	if err != nil {
		return nil, err
	}
	plugin.mu.Lock()
	plugin.current = config
	plugin.rawJSON = canonicalConfig(config)
	plugin.lastGood = options
	plugin.mu.Unlock()

	adapter := NewAdapter(AdapterOptions{
		Options:       plugin.Options,
		ResolveAPIKey: plugin.ResolveAPIKey,
		ResolveUserID: deps.ResolveUserID,
		Extensions:    deps.Extensions,
	})
	if err := deps.Runtime.RegisterConfigurableProviders([]llm.ConfigurableProvider{{
		Provider:     ProviderRoute,
		DisplayName:  "DeepSeek",
		SettingsNs:   SettingsNamespace,
		SettingsPath: []string{},
	}}); err != nil {
		return nil, err
	}
	registration, err := deps.Runtime.RegisterAdapter([]string{ProviderRoute}, adapter)
	if err != nil {
		return nil, err
	}
	plugin.registration = registration
	plugin.registeredPolicy = options.RetryPolicy

	if deps.Settings != nil {
		if err := plugin.installSettingsSection(config); err != nil {
			registration.Dispose()
			return nil, err
		}
	}
	return plugin, nil
}

// Section exposes the settings scope this plugin owns (nil without a
// settings store) so carriers can read or update the live section.
func (p *Plugin) Section() *settings.Scope {
	return p.scope
}

// installSettingsSection registers the namespace and re-points the config
// source at every committed snapshot.
func (p *Plugin) installSettingsSection(base Config) error {
	scope, err := p.deps.Settings.Register(SettingsNamespace, sectionSchema(), sectionLayer(base))
	if err != nil {
		return err
	}
	p.scope = scope
	p.settingsDisposer = p.deps.Settings.OnUpdated(func(event *settings.UpdateEvent) {
		if event.Namespace != SettingsNamespace {
			return
		}
		var next Config
		if err := decodeSection(event.Next, &next); err != nil {
			// A schema-validating store refuses bad writes before they
			// land; this branch means the section was replaced wholesale.
			p.deps.Logger.Warn(fmt.Sprintf("llm-deepseek: ignoring unreadable settings section: %v", err))
			return
		}
		p.mu.Lock()
		p.current = next
		p.mu.Unlock()
		// The registry captures the retry policy at registration, so it is
		// the one fact per-request resolution cannot refresh.
		p.EnsureRegistrationFacts()
	})
	return nil
}

// sectionSchema validates a resolved section by running the same resolve
// step the adapter uses — a section the owner could not act on never lands.
func sectionSchema() *settings.Schema {
	return &settings.Schema{
		Validate: func(value map[string]any) error {
			var config Config
			if err := decodeSection(value, &config); err != nil {
				return err
			}
			_, err := ResolveAdapterOptions(config, nil)
			return err
		},
	}
}

// sectionLayer renders the static config as the base (defaults) layer.
func sectionLayer(base Config) map[string]any {
	layer := map[string]any{}
	raw, err := json.Marshal(base)
	if err != nil {
		return layer
	}
	if err := json.Unmarshal(raw, &layer); err != nil {
		return map[string]any{}
	}
	return layer
}

func decodeSection(section map[string]any, out *Config) error {
	raw, err := json.Marshal(section)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("settings section does not decode into the llm-deepseek config: %w", err)
	}
	return nil
}

// Options returns the current validated connection facts, memoized per raw
// config value. A live settings snapshot failing a beyond-schema bound keeps
// serving the last good facts and says so once per bad snapshot; a static
// composition failing at load has no last good facts and fails loud.
func (p *Plugin) Options() (*ConnectionOptions, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw := p.current
	rawJSON := canonicalConfig(raw)
	if rawJSON == p.rawJSON && p.lastGood != nil {
		return p.lastGood, nil
	}
	next, err := ResolveAdapterOptions(raw, p.deps.Environment)
	if err != nil {
		if p.lastGood == nil {
			return nil, err
		}
		if p.badReportedJSON != rawJSON {
			p.badReportedJSON = rawJSON
			p.deps.Logger.Warn(fmt.Sprintf("llm-deepseek: keeping the last good configuration after an invalid settings section: %v", err))
		}
		return p.lastGood, nil
	}
	p.rawJSON = rawJSON
	p.lastGood = next
	p.badReportedJSON = ""
	return next, nil
}

// ResolveAPIKey resolves the bearer token for the connection facts of one
// request. Every credential fact comes from the caller's snapshot, so a
// rejected settings generation cannot leak its key onto the previous
// endpoint.
func (p *Plugin) ResolveAPIKey(connection *ConnectionOptions) (string, error) {
	ref, err := credentials.CredentialRef(connection.APIKeyEnv)
	if err != nil {
		return "", fmt.Errorf("llm-deepseek: %v", err)
	}
	if p.deps.Credentials != nil {
		hit, err := p.deps.Credentials.Resolve(ref)
		if err != nil {
			return "", err
		}
		if hit != nil {
			return llm.AssertUsableApiKey(hit.Value, "llm-deepseek", string(ref))
		}
	} else if p.deps.Environment != nil {
		// Without the seam there is no managed store to rank against, so
		// the environment is the whole credential plane.
		if ambient, ok := p.deps.Environment(connection.APIKeyEnv); ok && ambient != "" {
			return llm.AssertUsableApiKey(ambient, "llm-deepseek", string(ref))
		}
	}
	return "", llm.NewLlmError(
		fmt.Sprintf("llm-deepseek: no API key for provider route %q; store %s through the credentials service (the web Models page writes it), or export %s in the launching environment", ProviderRoute, ref, ref),
		"MISSING_CREDENTIAL", llm.LlmFailure{})
}

// EnsureRegistrationFacts re-registers the route in place when the retry
// policy changed. The registry captures the policy at registration, so it is
// the one fact per-request resolution cannot refresh; `replace` re-reads it
// in one synchronous registry section — disposing and re-registering instead
// would publish an empty route set between the two. The replace lock is
// separate from the state mutex: the registry re-enters Options through the
// adapter's ProviderRetryPolicy while the replace runs.
func (p *Plugin) EnsureRegistrationFacts() {
	p.replaceMu.Lock()
	defer p.replaceMu.Unlock()
	options, err := p.Options()
	if err != nil || options == nil {
		return // last good facts keep serving; nothing to compare
	}
	p.mu.Lock()
	unchanged := reflect.DeepEqual(options.RetryPolicy, p.registeredPolicy)
	p.mu.Unlock()
	if unchanged {
		return
	}
	if err := p.registration.Replace([]string{ProviderRoute}); err != nil {
		p.deps.Logger.Warn(fmt.Sprintf("llm-deepseek: re-registering provider route %q failed: %v", ProviderRoute, err))
		return
	}
	p.mu.Lock()
	p.registeredPolicy = options.RetryPolicy
	p.mu.Unlock()
}

// Dispose releases the registration and the settings subscription.
func (p *Plugin) Dispose() {
	p.mu.Lock()
	disposers := []func(){p.registration.Dispose}
	if p.settingsDisposer != nil {
		disposers = append(disposers, p.settingsDisposer)
	}
	p.mu.Unlock()
	for _, dispose := range disposers {
		dispose()
	}
}

// canonicalConfig renders the config value compared across snapshots.
func canonicalConfig(config Config) string {
	raw, err := json.Marshal(config)
	if err != nil {
		// Config fields are JSON-marshalable by construction; a custom
		// marshaler cannot appear here.
		return fmt.Sprintf("\x00unmarshalable:%v", err)
	}
	return string(raw)
}

type nilLogger struct{}

func (nilLogger) Info(...any)  {}
func (nilLogger) Warn(...any)  {}
func (nilLogger) Error(...any) {}
