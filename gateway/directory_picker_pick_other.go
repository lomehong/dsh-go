//go:build !windows

// Native directory choosers for the non-Windows platforms, mirroring the
// official native backend's per-platform choice: macOS `osascript choose
// folder`, Linux `zenity --file-selection --directory`. Cancellation maps
// to an empty path (the official pick resolves null on cancel — the
// "User canceled" / -128 osascript race and zenity's exit-1 cancel are
// both folded into that outcome, never an error).
package gateway

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// pickNativeDirectory opens the platform folder chooser and returns the
// selected absolute path. An empty path with a nil error means the
// operator cancelled the dialog.
func pickNativeDirectory(ctx context.Context) (string, error) {
	switch runtimeGOOS() {
	case "darwin":
		return pickDarwin(ctx)
	case "linux":
		return pickLinux(ctx)
	default:
		return "", fmt.Errorf("native directory picker is unsupported on %s", runtimeGOOS())
	}
}

// pickDarwin drives the macOS folder chooser through osascript.
func pickDarwin(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", "POSIX path of (choose folder)")
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", nil
		}
		if isUserCancel(err) {
			return "", nil
		}
		return "", fmt.Errorf("native directory picker failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// pickLinux drives the Zenity folder chooser (the official primary; the
// KDialog fallback stays with the official implementation).
func pickLinux(ctx context.Context) (string, error) {
	if _, lookErr := exec.LookPath("zenity"); lookErr != nil {
		return "", fmt.Errorf("no supported native directory picker found (install zenity)")
	}
	cmd := exec.CommandContext(ctx, "zenity", "--file-selection", "--directory")
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", nil
		}
		if isUserCancel(err) {
			return "", nil
		}
		return "", fmt.Errorf("native directory picker failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}
