package workspace

import (
	"os"
	"path/filepath"
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
	return filepath.EvalSymlinks(path)
}

// dirExists reports whether path currently exists and is a directory. Any
// stat failure (ENOENT, dangling parent, permission loss) means the
// directory is not usable right now.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
