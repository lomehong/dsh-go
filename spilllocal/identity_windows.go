//go:build windows

package spilllocal

import (
	"strings"
)

// idFromFileInfo uses the canonical lowercased path: Windows file indexes
// are not portable inode identities, and path case is not significant on
// Windows.
func idFromFileInfo(path string, _ any) string {
	return strings.ToLower(path)
}

// idFromPath is the lowercased canonical path identity.
func idFromPath(path string) string {
	return strings.ToLower(path)
}
