// Package homepaths ports packages/util/home-paths: shared filesystem path
// helpers for DeepSeek Harness user data.
//
// Go adaptation: canonicalizeWatchPath (native-watcher realpath canonicalization)
// is deferred — the Go ports drive polling or ReadDirectoryChangesW watchers
// through different backends with their own canonicalization; the resolution
// helpers below are the shared surface every consumer needs.
package homepaths

import (
	"os"
	"path/filepath"
	"strings"
)

// DshHomeDirName is the directory name for the default DeepSeek Harness home
// under the OS home.
const DshHomeDirName = ".dsh"

// DefaultDshHomeDisplay is the stable user-facing display form for the
// default DeepSeek Harness home.
const DefaultDshHomeDisplay = "~/" + DshHomeDirName

// DshHomeEnv is the environment variable that overrides the default DeepSeek
// Harness home.
const DshHomeEnv = "DSH_HOME"

// Getenv consults one environment variable; nil means os.Getenv.
type Getenv func(key string) string

// osHome is the OS-home lookup seam (tests override it; the official source
// reads the same ambient env through node:os homedir).
var osHome = os.UserHomeDir

func homeDir() string {
	home, err := osHome()
	if err != nil {
		return ""
	}
	return home
}

// DefaultDshHome resolves the default DeepSeek Harness home under the OS
// home directory.
func DefaultDshHome() string {
	return filepath.Join(homeDir(), DshHomeDirName)
}

// ExpandHomePath expands supported tilde prefixes against the operating
// system home: `~`, `~/`, or `~\`. Any other value returns unchanged.
func ExpandHomePath(path string) string {
	if path == "~" {
		return homeDir()
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		return filepath.Join(homeDir(), path[2:])
	}
	return path
}

// ResolveDshHome resolves the single-root DeepSeek Harness home. Precedence,
// highest first: an explicit configured path, `$DSH_HOME`, then `~/.dsh`.
// The harness keeps all user data under one root. An empty or
// whitespace-only `$DSH_HOME` is treated as unset, so a blank override never
// resolves the home to the current working directory.
func ResolveDshHome(configured string, getenv Getenv) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	selected := configured
	if selected == "" {
		if fromEnv := getenv(DshHomeEnv); strings.TrimSpace(fromEnv) != "" {
			selected = fromEnv
		} else {
			selected = DefaultDshHome()
		}
	}
	return resolve(ExpandHomePath(selected))
}

// DshHomePath joins path segments onto the resolved DeepSeek Harness home;
// an empty list returns the home itself.
func DshHomePath(getenv Getenv, segments ...string) string {
	return filepath.Join(append([]string{ResolveDshHome("", getenv)}, segments...)...)
}

// DshHomeDisplay describes a resolved harness home symbolically for
// user-facing display. It never returns an absolute machine path: the
// default home is labelled `~/.dsh`, and any configured home is labelled
// `$DSH_HOME`.
func DshHomeDisplay(resolvedHome string) string {
	if resolvedHome == resolve(DefaultDshHome()) {
		return DefaultDshHomeDisplay
	}
	return "$" + DshHomeEnv
}

func resolve(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return absolute
}
