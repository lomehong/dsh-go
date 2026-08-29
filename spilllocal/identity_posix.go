//go:build !windows

package spilllocal

import (
	"fmt"
	"os"
	"syscall"
)

// idFromFileInfo is the stable POSIX identity: device and inode.
func idFromFileInfo(path string, info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
	}
	return strings.ToLower(path)
}

// idFromPath is the path stand-in where no stat identity is available.
func idFromPath(path string) string {
	return strings.ToLower(path)
}
