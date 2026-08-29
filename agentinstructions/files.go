// Content identity and bounded host-filesystem discovery for workspace
// instruction files.
package agentinstructions

import (
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"dshgo/homepaths"
)

// osGetenv indirection keeps the test override local to config.go.
var osGetenv = os.Getenv

// InstructionFile is an instruction candidate identified by absolute and
// model-facing paths.
type InstructionFile struct {
	AbsolutePath string
	DisplayPath  string
}

// LoadedInstructionFile is an instruction file whose UTF-8 content was read
// successfully; Version stamps host freshness (modtime+size).
type LoadedInstructionFile struct {
	AbsolutePath string
	DisplayPath  string
	Content      string
	Version      string
}

// probedInstructionFile is a scope candidate whose metadata probed clean.
type probedInstructionFile struct {
	instructionFile
	version string
	size    int64
}

type instructionFile struct {
	absolutePath string
	displayPath  string
}

// probeKind is the tri-state scope probe: confirmed absence is distinct from
// an unreadable parent (unavailable).
type probeKind int

const (
	probePresent probeKind = iota
	probeAbsent
	probeUnavailable
)

// statProbe is one host metadata probe result.
type statProbe struct {
	kind    probeKind
	version string
	size    int64
}

// InstructionContentSha1 is the exact-content identity used across loading
// and session state: lowercase SHA-1 hex.
func InstructionContentSha1(content string) string {
	sum := sha1.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// TrimmedInstructionDigest is the whitespace-insensitive identity used for
// per-directory duplicate suppression.
func TrimmedInstructionDigest(content string) string {
	return InstructionContentSha1(strings.TrimSpace(content))
}

// FindProjectRoot walks upward to the first directory containing a
// configured root marker, defaulting to cwd.
func FindProjectRoot(cwd string, markers []string) string {
	current, err := filepath.Abs(cwd)
	if err != nil {
		current = cwd
	}
	origin := current
	for {
		for _, marker := range markers {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return origin
		}
		current = parent
	}
}

// AncestorChain builds the inclusive root-to-cwd directory chain, broadest
// first.
func AncestorChain(root string, cwd string) []string {
	var chain []string
	current, err := filepath.Abs(cwd)
	if err != nil {
		current = cwd
	}
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		resolvedRoot = root
	}
	for current != resolvedRoot {
		chain = append(chain, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	chain = append(chain, resolvedRoot)
	return chain
}

// statFile probes one path, following a final-component symlink: a link to a
// regular file loads, a missing path or non-file target is absent, an
// unreadable parent is unavailable.
func statFile(path string) statProbe {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return statProbe{kind: probeAbsent}
		}
		if errorsIsNotDir(err) {
			return statProbe{kind: probeAbsent}
		}
		return statProbe{kind: probeUnavailable}
	}
	if !info.Mode().IsRegular() {
		return statProbe{kind: probeAbsent}
	}
	return statProbe{kind: probePresent, version: fileVersion(info), size: info.Size()}
}

func errorsIsNotDir(err error) bool {
	// A path component through a file yields ENOTDIR; host reads below the
	// missing component are absent, not provider failures.
	return err != nil && strings.Contains(err.Error(), "not a directory")
}

func fileVersion(info os.FileInfo) string {
	return strings.Join([]string{
		strconvFormatInt(info.ModTime().UnixNano()),
		strconvFormatInt(info.Size()),
	}, ":")
}

// RelativeDisplay renders a project-rooted instruction path for the model.
func RelativeDisplay(projectRoot string, absolutePath string) string {
	relative, err := filepath.Rel(projectRoot, absolutePath)
	if err != nil {
		return absolutePath
	}
	return filepath.ToSlash(relative)
}

// DescendantDirsBetween finds descendant directories crossed between a cwd
// and a touched file: shallowest through the touched file's parent. Paths
// outside the root yield nothing.
func DescendantDirsBetween(root string, touchedPath string) []string {
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		resolvedRoot = root
	}
	targetPath := touchedPath
	if !filepath.IsAbs(targetPath) {
		targetPath = filepath.Join(resolvedRoot, targetPath)
	} else if targetPath, err = filepath.Abs(targetPath); err != nil {
		return nil
	}
	targetDir := filepath.Dir(targetPath)
	rel, err := filepath.Rel(resolvedRoot, targetDir)
	if err != nil || rel == "." {
		return nil
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return nil
	}
	chain := AncestorChain(resolvedRoot, targetDir)
	if len(chain) <= 1 {
		return nil
	}
	return chain[1:]
}

func userGlobalDisplayPath(dshHome string) string {
	return homepaths.DshHomeDisplay(dshHome) + "/AGENTS.md"
}
