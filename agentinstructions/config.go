// Package agentinstructions ports @deepseek-ai/dsh-agent-instructions: the
// workspace instruction loader for AGENTS.md-compatible files.
//
// Baseline instructions enter durable context before the first request;
// successful read/write/edit tool touches project nested, changed, and
// removed instructions into the inbox for later steps.
//
// Go adaptations: there is no ctx.fs service, so every read probes the host
// filesystem directly and the providerless no-op branch does not exist;
// provider versions become modtime+size stamps; the async projection tail
// becomes synchronous composition (host reads cannot yield), so deferred
// mid-step touches drain at the next pre-step instead of step/end.
package agentinstructions

import (
	"encoding/json"
	"path/filepath"

	"dshgo/homepaths"
)

// Discovery defaults.
const (
	DefaultMaxSourceBytes = 1 << 20
)

var defaultProjectRootMarkers = []string{".git"}
var defaultInstructionFileCandidates = []string{"AGENTS.md", "CLAUDE.md"}
var defaultLocalInstructionFileCandidates = []string{"AGENTS.local.md", "CLAUDE.local.md"}

// reservedPathSegments never carry a candidate file.
var reservedPathSegments = map[string]bool{"": true, ".": true, "..": true}

// Config is the user-facing workspace instruction loader configuration.
type Config struct {
	// DSHHome contains the fixed user-global AGENTS.md; empty resolves
	// $DSH_HOME or ~/.dsh.
	DSHHome string
	// ProjectRootMarkers are directory entries that identify the project
	// root while walking upward from the session cwd; nil applies [.git].
	ProjectRootMarkers []string
	// MaxBytes is the UTF-8 byte cap for one rendered baseline or dynamic
	// batch; non-positive disables loading.
	MaxBytes int64
	// MaxSourceBytes is the maximum UTF-8 bytes read from one instruction
	// file; larger files are ignored. Zero applies 1 MiB.
	MaxSourceBytes int64
	// InstructionFileCandidates are ordered same-directory project
	// candidates; every existing file loads, with per-directory
	// trimmed-content duplicates collapsed to the earliest candidate. Nil
	// applies [AGENTS.md, CLAUDE.md].
	InstructionFileCandidates []string
	// LocalInstructionFileCandidates are ordered same-directory local
	// overlays loaded after the base files under the same dedup; empty
	// disables the overlay. Nil applies [AGENTS.local.md,
	// CLAUDE.local.md].
	LocalInstructionFileCandidates []string
	// Getenv overrides environment reads for tests; nil uses os.Getenv.
	Getenv func(name string) string
}

// ResolvedConfig is the normalized configuration used by discovery and
// reconciliation.
type ResolvedConfig struct {
	DSHHome                        string
	ProjectRootMarkers             []string
	MaxBytes                       int64
	MaxSourceBytes                 int64
	InstructionFileCandidates      []string
	LocalInstructionFileCandidates []string
	getenv                         func(name string) string
}

// ResolveConfig applies defaults and valid same-directory candidates.
func ResolveConfig(config Config) ResolvedConfig {
	resolved := ResolvedConfig{
		MaxBytes:       config.MaxBytes,
		MaxSourceBytes: config.MaxSourceBytes,
	}
	getenv := config.Getenv
	if getenv == nil {
		getenv = defaultGetenv
	}
	resolved.getenv = getenv
	resolved.DSHHome = homepaths.ResolveDshHome(config.DSHHome, getenv)
	resolved.ProjectRootMarkers = normalizeList(config.ProjectRootMarkers, defaultProjectRootMarkers)
	resolved.InstructionFileCandidates = normalizeList(config.InstructionFileCandidates, defaultInstructionFileCandidates)
	resolved.LocalInstructionFileCandidates = normalizeList(config.LocalInstructionFileCandidates, defaultLocalInstructionFileCandidates)
	if resolved.MaxSourceBytes == 0 {
		resolved.MaxSourceBytes = DefaultMaxSourceBytes
	}
	return resolved
}

func defaultGetenv(name string) string { return osGetenv(name) }

// normalizeList applies the fallback and drops reserved or path-bearing
// entries.
func normalizeList(candidates []string, fallback []string) []string {
	source := candidates
	if source == nil {
		source = fallback
	}
	kept := make([]string, 0, len(source))
	for _, candidate := range source {
		if reservedPathSegments[candidate] {
			continue
		}
		if filepath.Separator == '\\' {
			if containsAny(candidate, `/\`) {
				continue
			}
		} else if containsAny(candidate, "/\\") {
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}

func containsAny(value, chars string) bool {
	for _, r := range value {
		for _, c := range chars {
			if r == c {
				return true
			}
		}
	}
	return false
}

// WorkspaceBaselineIdentity serializes the discovery, precedence, and budget
// semantics of one baseline for compatibility checks on resume.
func WorkspaceBaselineIdentity(config ResolvedConfig, cwd string, projectRoot string) string {
	relative, err := filepath.Rel(cwd, projectRoot)
	if err != nil {
		relative = projectRoot
	}
	encoded, err := json.Marshal(struct {
		ProjectRoot                    string   `json:"projectRoot"`
		ProjectRootMarkers             []string `json:"projectRootMarkers"`
		MaxBytes                       int64    `json:"maxBytes"`
		MaxSourceBytes                 int64    `json:"maxSourceBytes"`
		InstructionFileCandidates      []string `json:"instructionFileCandidates"`
		LocalInstructionFileCandidates []string `json:"localInstructionFileCandidates"`
	}{
		ProjectRoot:                    relative,
		ProjectRootMarkers:             config.ProjectRootMarkers,
		MaxBytes:                       config.MaxBytes,
		MaxSourceBytes:                 config.MaxSourceBytes,
		InstructionFileCandidates:      config.InstructionFileCandidates,
		LocalInstructionFileCandidates: config.LocalInstructionFileCandidates,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}
