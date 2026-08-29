//go:build !windows

package spilllocal

import (
	"os"
	"syscall"
)

// rootUID reports the file's owning uid for the POSIX trust checks.
func rootUID(info os.FileInfo) int {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid)
	}
	return -1
}
