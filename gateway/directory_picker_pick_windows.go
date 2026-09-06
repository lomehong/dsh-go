//go:build windows

// The Windows native directory chooser: the System.Windows.Forms
// FolderBrowserDialog over PowerShell (STA is mandatory for the dialog).
// The script is passed as -EncodedCommand (base64 UTF-16LE) so the path
// output and any non-ASCII copy survive the console codepage; the console
// window itself is hidden (only the dialog shows). Runs on the HOST's
// interactive desktop — for the local web deployment that is the
// operator's own screen, matching the official launcher-side chooser.
package gateway

import (
	"context"
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// pickNativeDirectory opens the platform folder chooser and returns the
// selected absolute path. An empty path with a nil error means the
// operator cancelled the dialog (the official pick resolves null on
// cancel — never an error).
func pickNativeDirectory(ctx context.Context) (string, error) {
	script := "Add-Type -AssemblyName System.Windows.Forms | Out-Null\n" +
		"$owner = New-Object System.Windows.Forms.Form -Property @{TopMost=$true;ShowInTaskbar=$false}\n" +
		"$dlg = New-Object System.Windows.Forms.FolderBrowserDialog\n" +
		"$dlg.ShowNewFolderButton = $true\n" +
		"if ($dlg.ShowDialog($owner) -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($dlg.SelectedPath) }\n"

	encoded := base64.StdEncoding.EncodeToString(utf16LittleEndian(script))
	cmd := exec.CommandContext(ctx, "powershell",
		"-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encoded)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.Output()
	if err != nil {
		// The request context cancelling (page closed) kills the dialog
		// process; surface that as the cancel outcome, not a failure.
		if ctx.Err() != nil {
			return "", nil
		}
		return "", fmt.Errorf("native directory picker failed: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// utf16LittleEndian encodes s for PowerShell -EncodedCommand.
func utf16LittleEndian(s string) []byte {
	units := utf16.Encode([]rune(s))
	bytes := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		bytes = append(bytes, byte(unit), byte(unit>>8))
	}
	return bytes
}
