package pluginhost

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestStartEchoPlugin: spawn a real subprocess that speaks line-delimited
// JSON-RPC on stdio (the Go test binary itself in a helper mode), run the
// initialize handshake, and verify the discovered contributions.
//
// The child is a tiny pwsh one-liner acting as a JSON-RPC echo server:
// it reads lines, and for "initialize" replies with a fixed InitializeResult.
func TestStartEchoPlugin(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH; skipping live plugin host test")
	}

	// A minimal JSON-RPC server over stdio in PowerShell: reads lines from
	// stdin; for the initialize method, emits a JSON-RPC response with a
	// fixed serverInfo; exits when stdin closes.
	script := `
while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  try { $req = $line | ConvertFrom-Json } catch { continue }
  if ($req.method -eq 'initialize') {
    $res = @{ jsonrpc = '2.0'; id = $req.id; result = @{ serverInfo = @{ name = 'test-plugin'; version = '1.0.0' } } }
    [Console]::Out.WriteLine(($res | ConvertTo-Json -Compress -Depth 5))
  }
}
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plugin, contributions, err := Start(ctx, Config{
		Argv: []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", script},
		Cwd:  ".",
		Name: "test-echo-plugin",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer plugin.Close()

	if plugin.Pid() <= 0 {
		t.Fatalf("pid = %d", plugin.Pid())
	}
	if contributions == nil {
		t.Fatal("contributions missing")
	}
	if contributions.Name != "test-plugin" {
		t.Fatalf("name = %q, want test-plugin", contributions.Name)
	}
	if contributions.Version != "1.0.0" {
		t.Fatalf("version = %q, want 1.0.0", contributions.Version)
	}
}

// TestStartEmptyArgvFails: an empty argv must fail at start.
func TestStartEmptyArgvFails(t *testing.T) {
	ctx := context.Background()
	if _, _, err := Start(ctx, Config{Name: "empty"}); err == nil {
		t.Fatal("empty argv must fail")
	}
}

// TestStartNonexistentExecutableFails: spawning a nonexistent program
// must return a spawn error, not a live plugin.
func TestStartNonexistentExecutableFails(t *testing.T) {
	ctx := context.Background()
	_, _, err := Start(ctx, Config{
		Argv: []string{"definitely-not-a-real-binary-xyz"},
		Name: "nonexistent",
	})
	if err == nil {
		t.Fatal("nonexistent executable must fail to start")
	}
}

// TestCloseTerminatesProcess: Close must terminate the process tree (not
// just close stdin) — Done resolves after Close.
func TestCloseTerminatesProcess(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH; skipping live plugin host test")
	}
	// A server that never exits on its own: it just echoes initialize
	// responses and keeps reading.
	script := `
while ($true) {
  $line = [Console]::In.ReadLine()
  if ($null -eq $line) { break }
  try { $req = $line | ConvertFrom-Json } catch { continue }
  if ($req.method -eq 'initialize') {
    $res = @{ jsonrpc = '2.0'; id = $req.id; result = @{ serverInfo = @{ name = 'long-lived'; version = '1.0.0' } } }
    [Console]::Out.WriteLine(($res | ConvertTo-Json -Compress -Depth 5))
  }
}
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	plugin, _, err := Start(ctx, Config{
		Argv: []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", script},
		Cwd:  ".",
		Name: "long-lived-plugin",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := plugin.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Close is idempotent.
	if err := plugin.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	// The process must exit: Done resolves within the timeout.
	select {
	case <-plugin.Done():
		// terminated — good
	case <-time.After(15 * time.Second):
		t.Fatal("plugin process did not terminate within 15s of Close")
	}
}

// TestGarbageInitializeResultFails: a child that answers initialize with
// malformed JSON must fail the start (no silent zero-value degradation).
func TestGarbageInitializeResultFails(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH; skipping live plugin host test")
	}
	// Responds to initialize with a JSON-RPC result that is not an
	// InitializeResult shape.
	script := `
$line = [Console]::In.ReadLine()
$req = $line | ConvertFrom-Json
$res = @{ jsonrpc = '2.0'; id = $req.id; result = @{ unexpected = $true } }
[Console]::Out.WriteLine(($res | ConvertTo-Json -Compress -Depth 5))
Start-Sleep -Seconds 60
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := Start(ctx, Config{
		Argv: []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", script},
		Cwd:  ".",
		Name: "garbage-plugin",
	})
	if err == nil {
		t.Fatal("garbage initialize result must fail the start")
	}
}

// TestHandshakeTimeout: a child that never answers initialize must fail the
// start when the configured handshake budget expires (the child is
// terminated by the failure path, so the test does not leak it).
func TestHandshakeTimeout(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not on PATH; skipping live plugin host test")
	}
	// Reads the request but never answers.
	script := `
$null = [Console]::In.ReadLine()
Start-Sleep -Seconds 60
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	started := time.Now()
	_, _, err := Start(ctx, Config{
		Argv:             []string{"pwsh", "-NoProfile", "-NonInteractive", "-Command", script},
		Cwd:              ".",
		Name:             "silent-plugin",
		HandshakeTimeout: 300 * time.Millisecond,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("silent plugin must fail the handshake")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("handshake took %s, want ~300ms budget", elapsed)
	}
}
