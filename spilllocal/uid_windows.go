//go:build windows

package spilllocal

import "os"

// rootUID reports -1: uid has no meaning on Windows, and the trust checks
// short-circuit to trusted there.
func rootUID(info os.FileInfo) int {
	return -1
}
