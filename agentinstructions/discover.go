// Baseline discovery, scope probes, bounded reads, and content dedup.
package agentinstructions

import (
	"os"
	"path/filepath"
	"strconv"
)

func strconvFormatInt(value int64) string { return strconv.FormatInt(value, 10) }

// discoverOptions carries the discovery parameters for one baseline.
type discoverOptions struct {
	cwd                            string
	projectRoot                    string
	projectRootMarkers             []string
	instructionFileCandidates      []string
	localInstructionFileCandidates []string
}

// DiscoverBaselineInstructionFiles returns the host-visible user-global and
// root-to-cwd candidates in model precedence order, path-deduplicated.
func DiscoverBaselineInstructionFiles(resolved ResolvedConfig, options discoverOptions) []InstructionFile {
	files := discoverInstructionFiles(resolved, options)
	result := make([]InstructionFile, 0, len(files))
	for _, file := range files {
		result = append(result, InstructionFile{AbsolutePath: file.absolutePath, DisplayPath: file.displayPath})
	}
	return result
}

// discoverInstructionFiles orders: user-global once, then base and local
// candidates per directory from the project root down to the cwd. Path
// duplicates collapse to the first occurrence.
func discoverInstructionFiles(resolved ResolvedConfig, options discoverOptions) []instructionFile {
	var files []instructionFile
	seen := map[string]bool{}
	addFile := func(file instructionFile) {
		if seen[file.absolutePath] {
			return
		}
		seen[file.absolutePath] = true
		files = append(files, file)
	}
	// The user-global instruction is the broadest scope; its display path
	// and reconciliation scope both key on the fixed AGENTS.md name.
	userGlobalPath := filepath.Join(resolved.DSHHome, "AGENTS.md")
	switch probe := statFile(userGlobalPath); probe.kind {
	case probePresent:
		addFile(instructionFile{absolutePath: userGlobalPath, displayPath: userGlobalDisplayPath(resolved.DSHHome)})
	case probeAbsent, probeUnavailable:
	}

	cwd, err := filepath.Abs(options.cwd)
	if err != nil {
		cwd = options.cwd
	}
	projectRoot := options.projectRoot
	if projectRoot == "" {
		projectRoot = FindProjectRoot(cwd, resolved.ProjectRootMarkers)
	}
	for _, dir := range AncestorChain(projectRoot, cwd) {
		for _, candidates := range [][]string{resolved.InstructionFileCandidates, resolved.LocalInstructionFileCandidates} {
			for _, file := range allExistingInstructionFiles(dir, projectRoot, candidates) {
				addFile(file)
			}
		}
	}
	return files
}

// allExistingInstructionFiles probes every candidate in one directory;
// present regular files return in candidate order.
func allExistingInstructionFiles(dir string, projectRoot string, candidates []string) []instructionFile {
	var found []instructionFile
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		absolutePath := filepath.Join(dir, candidate)
		if probe := statFile(absolutePath); probe.kind == probePresent {
			found = append(found, instructionFile{
				absolutePath: absolutePath,
				displayPath:  RelativeDisplay(projectRoot, absolutePath),
			})
		}
	}
	return found
}

// ProbeScopeInstruction probes one already-resolved scope candidate path
// directly; the scope-aware variant lives in state.go.

// ReadScopeInstruction reads one probed candidate under the source cap. A
// file that disappeared or grew past the cap after its probe is absent.
func ReadScopeInstruction(file probedInstructionFile, maxSourceBytes int64) *LoadedInstructionFile {
	content, ok := readBounded(file.absolutePath, file.size, maxSourceBytes)
	if !ok {
		return nil
	}
	return &LoadedInstructionFile{
		AbsolutePath: file.absolutePath,
		DisplayPath:  file.displayPath,
		Content:      content,
		Version:      file.version,
	}
}

// readBounded reads the file and enforces the per-file source cap. Metadata
// known oversize never opens the file.
func readBounded(path string, size int64, maxSourceBytes int64) (string, bool) {
	if size > maxSourceBytes {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		// A file may disappear or become unreadable after its metadata probe.
		return "", false
	}
	if int64(len(raw)) > maxSourceBytes {
		return "", false
	}
	return string(raw), true
}

// DedupInstructionFilesByDirectory drops later candidates whose trimmed
// content duplicates an earlier sibling in the same directory. Different
// directories never collapse even when identical.
func DedupInstructionFilesByDirectory(files []LoadedInstructionFile) []LoadedInstructionFile {
	keptDigestsByDir := map[string]map[string]bool{}
	kept := make([]LoadedInstructionFile, 0, len(files))
	for _, file := range files {
		dir := filepath.ToSlash(filepath.Dir(file.DisplayPath))
		digests := keptDigestsByDir[dir]
		if digests == nil {
			digests = map[string]bool{}
			keptDigestsByDir[dir] = digests
		}
		digest := TrimmedInstructionDigest(file.Content)
		if digests[digest] {
			continue
		}
		digests[digest] = true
		kept = append(kept, file)
	}
	return kept
}

// RenderedInstructionSet is the rendered baseline plus the files that
// survived dedup and budgeting.
type RenderedInstructionSet struct {
	Rendered RenderedWorkspaceContext
	Observed []LoadedInstructionFile
	Included []LoadedInstructionFile
}

// LoadBaselineInstructionSet discovers, reads, and renders the baseline
// instruction chain. A disabled budget or nothing loadable yields nil unless
// replacePreviousBaseline demands an explicit empty replacement set.
func LoadBaselineInstructionSet(resolved ResolvedConfig, options discoverOptions, replacePreviousBaseline bool) *RenderedInstructionSet {
	if resolved.MaxBytes <= 0 {
		return nil
	}
	if resolved.MaxSourceBytes <= 0 {
		return nil
	}
	discovered := discoverInstructionFiles(resolved, options)
	loaded := make([]LoadedInstructionFile, 0, len(discovered))
	for _, file := range discovered {
		probe := statFile(file.absolutePath)
		if probe.kind != probePresent {
			continue
		}
		loadedFile := ReadScopeInstruction(probedInstructionFile{
			instructionFile: file,
			version:         probe.version,
			size:            probe.size,
		}, resolved.MaxSourceBytes)
		if loadedFile == nil {
			continue
		}
		loaded = append(loaded, *loadedFile)
	}
	deduped := DedupInstructionFilesByDirectory(loaded)
	if len(deduped) == 0 {
		if !replacePreviousBaseline {
			return nil
		}
		rendered := RenderWorkspaceContext(nil, RenderOptions{MaxBytes: resolved.MaxBytes, ReplacePreviousBaseline: true})
		return &RenderedInstructionSet{Rendered: rendered}
	}
	rendered := RenderWorkspaceContext(deduped, RenderOptions{MaxBytes: resolved.MaxBytes, ReplacePreviousBaseline: replacePreviousBaseline})
	return &RenderedInstructionSet{Rendered: rendered, Observed: loaded, Included: deduped}
}
