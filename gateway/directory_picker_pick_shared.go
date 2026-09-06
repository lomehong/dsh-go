// Shared helpers of the platform-native directory choosers.
package gateway

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// runtimeGOOS indirection keeps the platform files honest in tests.
func runtimeGOOS() string { return runtime.GOOS }

// isUserCancel reports whether an exec failure is the operator dismissing
// the dialog: osascript cancels exit -128 with "User canceled" in stderr
// (the official regex), and the exec wrapper surfaces stderr in the error
// text. Zenity's cancel is a bare exit 1 — classified by the caller
// before this helper when stdout is empty.
func isUserCancel(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if strings.Contains(message, "-128") || strings.Contains(strings.ToLower(message), "user canceled") {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}
