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
