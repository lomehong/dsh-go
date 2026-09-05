package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RealpathNormalize canonicalizes a directory path via the OS realpath:
// trailing slashes, `..` segments, and symlinks are all resolved. This is
// the ONE uniqueness canon of the package — workspace paths are stored
// canonicalized, uniqueness is string equality of canonicalized paths (a
// symlink to an existing workspace's directory collides), and attach-time
// session cwd checks go through the same canon. A path that does not exist
// fails with the original ENOENT — this is create's reject path (a
// workspace must point at an existing directory).
func RealpathNormalize(path string) (string, error) {
	if !FullyQualifiedWorkspacePath(path) {
		return "", fmt.Errorf("Workspace path is not fully qualified: '%s'", path)
	}
	return filepath.EvalSymlinks(path)
}

// FullyQualifiedWorkspacePath reports whether a path names one fixed Host
// location without process cwd or current-drive resolution. Windows
// requires a drive/UNC form; every other platform requires a leading
// separator (official fullyQualifiedWorkspacePath).
func FullyQualifiedWorkspacePath(path string) bool {
	if runtime.GOOS == "windows" {
		if !filepath.IsAbs(path) {
			return false
		}
		// Drive form (C:\, C:/) or UNC form (\server\share, //s/share).
		if len(path) >= 2 && path[1] == ':' && (path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z') {
			return true
		}
		return strings.HasPrefix(path, `\`) || strings.HasPrefix(path, "//")
	}
	return strings.HasPrefix(path, "/")
}

// DefaultWorkspaceTitle derives a non-empty default title from a canonical
// workspace path: the final segment, or the complete root spelling for a
// filesystem root (official defaultWorkspaceTitle — `C:\` no longer
// collapses to "").
func DefaultWorkspaceTitle(path string) string {
	base := filepath.Base(strings.TrimSuffix(path, string(filepath.Separator)))
	if base != "" && base != "." && base != string(filepath.Separator) {
		return base
	}
	if volume := filepath.VolumeName(path); volume != "" {
		return volume + string(filepath.Separator)
	}
	return path
}

// dirExists reports whether path currently exists and is a directory. Any
// stat failure (ENOENT, dangling parent, permission loss) means the
// directory is not usable right now.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
