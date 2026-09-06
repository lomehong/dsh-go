// Package pluginhost implements the managed-subprocess plugin hosting
// infrastructure: spawn a TS plugin as a child process, wire its stdio to
// the sdk/protocol JSON-RPC transport, run the initialize handshake, and
// surface the plugin's declared contributions to the host composition.
//
// This is the Go-native answer to the official "plugin ABI" — TS plugins
// run as managed subprocesses communicating over stdio JSON-RPC.
package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"dshgo/sdk/protocol"
	"dshgo/subprocess"
)

// Config configures one plugin subprocess.
type Config struct {
	// Argv is the plugin's executable and arguments.
	Argv []string
	// Cwd is the plugin's working directory.
	Cwd string
	// Name is a human-readable label for diagnostics.
	Name string
}

// DiscoveredContributions is what the host learns from the initialize
// handshake: the plugin's identity and its declared surface.
type DiscoveredContributions struct {
	// Name is the plugin's self-reported name.
	Name string
	// Version is the plugin's self-reported version.
	Version string
	// ServerInfo carries the SDK server identity from the handshake.
	ServerInfo protocol.ServerIdentity
}

// Plugin is one managed plugin subprocess lifecycle.
type Plugin struct {
	config    Config
	handle    subprocess.Handle
	transport *protocol.LineTransport
	mu        sync.Mutex
	closed    bool
}

// Start spawns the plugin subprocess, wires its stdio to a JSON-RPC
// transport, and runs the initialize handshake. On success the plugin is
// live and its contributions are discovered.
func Start(ctx context.Context, config Config) (*Plugin, *DiscoveredContributions, error) {
	if len(config.Argv) == 0 {
		return nil, nil, fmt.Errorf("pluginhost %q: empty argv", config.Name)
	}

	local := subprocess.NewLocal()
	handle, err := local.Spawn(ctx, subprocess.SpawnSpec{
		Argv:    config.Argv,
		Cwd:     config.Cwd,
		GraceMs: 5000,
		Stdio: subprocess.Stdio{
			Stdin:  subprocess.StdinPipe{},
			Stdout: subprocess.OutputPipe{},
			Stderr: subprocess.OutputPipe{},
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("pluginhost %q: spawn: %w", config.Name, err)
	}

	transport := protocol.NewLineTransport(handle.Stdout(), handle.Stdin())
	transport.Start()

	// Initialize handshake: send the JSON-RPC "initialize" request and
	// decode the InitializeResult from the response.
	peer := protocol.Peer(transport)
	raw, reqErr := peer.Request(ctx, "initialize", map[string]any{
		"cwd": config.Cwd,
	})
	if reqErr != nil {
		transport.Close()
		return nil, nil, fmt.Errorf("pluginhost %q: initialize: %w", config.Name, reqErr)
	}
	var initResult protocol.InitializeResult
	if b, marshalErr := json.Marshal(raw); marshalErr == nil {
		json.Unmarshal(b, &initResult)
	}

	contributions := &DiscoveredContributions{
		Name:       initResult.ServerInfo.Name,
		Version:    initResult.ServerInfo.Version,
		ServerInfo: initResult.ServerInfo,
	}

	return &Plugin{
		config:    config,
		handle:    handle,
		transport: transport,
	}, contributions, nil
}

// Transport exposes the JSON-RPC transport for further protocol calls.
func (p *Plugin) Transport() *protocol.LineTransport { return p.transport }

// Pid returns the plugin process id.
func (p *Plugin) Pid() int { return p.handle.Pid() }

// Close terminates the subprocess and closes the transport.
func (p *Plugin) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.transport.Close()
	return nil
}

// Done resolves when the subprocess exits (channel closes).
func (p *Plugin) Done() <-chan struct{} {
	return p.handle.Done()
}
