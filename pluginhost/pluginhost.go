// Package pluginhost implements the managed-subprocess plugin hosting
// infrastructure: spawn a TS plugin as a child process, wire its stdio to
// the sdk/protocol JSON-RPC transport, run the initialize handshake, and
// surface the plugin's declared contributions to the host composition.
//
// Lifecycle guarantees: the child's stderr is drained (never a pipe-full
// deadlock), the handshake carries a default timeout, and Close terminates
// the process tree (not just stdin EOF).
//
// This is the Go-native answer to the official "plugin ABI" — TS plugins
// run as managed subprocesses communicating over stdio JSON-RPC.
package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"dshgo/sdk/protocol"
	"dshgo/subprocess"
)

// DefaultHandshakeTimeout bounds the initialize round trip when the
// caller's context carries no deadline.
const DefaultHandshakeTimeout = 15 * time.Second

// Config configures one plugin subprocess.
type Config struct {
	// Argv is the plugin's executable and arguments.
	Argv []string
	// Cwd is the plugin's working directory.
	Cwd string
	// Name is a human-readable label for diagnostics.
	Name string
	// HandshakeTimeout overrides the initialize round-trip budget; zero
	// uses DefaultHandshakeTimeout.
	HandshakeTimeout time.Duration
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
	closeOnce sync.Once
}

// Start spawns the plugin subprocess, wires its stdio to a JSON-RPC
// transport (stderr drained to discard), and runs the initialize
// handshake under a timeout. On failure the child is terminated before
// Start returns.
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
	plugin := &Plugin{config: config, handle: handle}

	// Drain stderr: an unread OutputPipe fills its 64KB buffer and blocks
	// the child forever. Read-and-discard keeps the pipe empty.
	if stderr := handle.Stderr(); stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, stderr) }()
	}

	transport := protocol.NewLineTransport(handle.Stdout(), handle.Stdin())
	transport.Start()
	plugin.transport = transport

	// The handshake runs under the caller's context AND the handshake
	// budget (config override, else the default). WithTimeout takes the
	// earlier of the two deadlines, so a Background caller cannot hang
	// forever on a silent plugin and a short config budget fires even
	// under a long caller deadline.
	timeout := config.HandshakeTimeout
	if timeout <= 0 {
		timeout = DefaultHandshakeTimeout
	}
	handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Initialize handshake: send the JSON-RPC "initialize" request and
	// decode the InitializeResult from the response. A malformed result
	// fails the start (no silent zero-value degradation).
	peer := protocol.Peer(transport)
	raw, reqErr := peer.Request(handshakeCtx, "initialize", map[string]any{
		"cwd": config.Cwd,
	})
	if reqErr != nil {
		plugin.Close()
		return nil, nil, fmt.Errorf("pluginhost %q: initialize: %w", config.Name, reqErr)
	}
	b, marshalErr := json.Marshal(raw)
	if marshalErr != nil {
		plugin.Close()
		return nil, nil, fmt.Errorf("pluginhost %q: initialize result re-encode: %w", config.Name, marshalErr)
	}
	var initResult protocol.InitializeResult
	if unmarshalErr := json.Unmarshal(b, &initResult); unmarshalErr != nil {
		plugin.Close()
		return nil, nil, fmt.Errorf("pluginhost %q: initialize result malformed: %w", config.Name, unmarshalErr)
	}
	if initResult.ServerInfo.Name == "" {
		plugin.Close()
		return nil, nil, fmt.Errorf("pluginhost %q: initialize result carries no serverInfo.name", config.Name)
	}

	contributions := &DiscoveredContributions{
		Name:       initResult.ServerInfo.Name,
		Version:    initResult.ServerInfo.Version,
		ServerInfo: initResult.ServerInfo,
	}
	return plugin, contributions, nil
}

// Transport exposes the JSON-RPC transport for further protocol calls.
func (p *Plugin) Transport() *protocol.LineTransport { return p.transport }

// Pid returns the plugin process id.
func (p *Plugin) Pid() int { return p.handle.Pid() }

// Close closes the transport (stdin EOF) and terminates the process tree.
// Safe to call multiple times; safe on a nil receiver.
func (p *Plugin) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.transport != nil {
			p.transport.Close()
		}
		p.handle.Terminate()
	})
	return nil
}

// Done resolves when the subprocess exits (channel closes).
func (p *Plugin) Done() <-chan struct{} {
	return p.handle.Done()
}
