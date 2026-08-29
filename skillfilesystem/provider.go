// Package skillfilesystem ports @deepseek-ai/dsh-skill-filesystem: the local
// filesystem skill provider. It discovers directory-bundle and flat Markdown
// skills from project, custom, user, and bundled roots, parses YAML
// frontmatter, and loads bodies from disk.
//
// Go adaptations: there is no ctx.fs service, so reads go through os
// directly (the trustedHost fast path is the only path); the chokidar /
// fs.watchFile watcher machinery becomes a polling observer per retained
// root with the same observable contract — relevant drift invalidates the
// catalog, project watches are bounded, and startup failure leaves readable
// candidates as an incomplete observation.
package skillfilesystem

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"dshgo/cordis"
	"dshgo/homepaths"
	"dshgo/skill"
)

// Root ranks: stable precedence for the standard root kinds.
const (
	ProjectDshRank    = 100
	ProjectAgentsRank = 200
	CustomRank        = 300
	UserDshRank       = 400
	UserAgentsRank    = 500
)

// Watch defaults.
const (
	DefaultWatchStabilityThresholdMs = 200
	DefaultWatchPollIntervalMs       = 100
	DefaultWatchMaxProjects          = 128
)

// Config is the local filesystem skill provider configuration.
type Config struct {
	// ProviderName is the unique provider name; empty applies
	// "filesystem".
	ProviderName string
	// IncludeDefaultRoots selects whether project and user roots are
	// included around custom roots. Nil applies the official true default.
	IncludeDefaultRoots *bool
	// DSHHome is the DeepSeek Harness config root; empty resolves
	// $DSH_HOME or ~/.dsh.
	DSHHome string
	// AgentsHome is the shared agent config root; empty resolves
	// $DSH_AGENTS_HOME or ~/.agents.
	AgentsHome string
	// CustomSkillDirs are additional skill roots scanned after project
	// roots and before user roots.
	CustomSkillDirs []string
	// Watch enables host-local skill-root observation. Nil applies the
	// official true default.
	Watch *bool
	// WatchStabilityThresholdMs is how long a changed skill entry must
	// remain stable before it is observed; zero applies 200.
	WatchStabilityThresholdMs int
	// WatchPollIntervalMs is the interval between stability probes; zero
	// applies 100.
	WatchPollIntervalMs int
	// WatchMaxProjects bounds the distinct project roots whose skill
	// directories remain watched; zero applies 128.
	WatchMaxProjects int
	// WatchFollowSymlinks selects whether watched symbolic links follow
	// their target files. Nil applies the official true default.
	WatchFollowSymlinks *bool
	// BundledSkillDir is the bundled skill root; empty applies
	// $DSH_BUNDLED_SKILL_DIR when default roots are included, otherwise
	// mounts none.
	BundledSkillDir string
	// Getenv overrides environment reads for tests; nil uses os.Getenv.
	Getenv func(name string) string
}

// Resolved is the validated configuration.
type Resolved struct {
	ProviderName              string
	IncludeDefaultRoots       bool
	DSHHome                   string
	AgentsHome                string
	CustomSkillDirs           []string
	Watch                     bool
	WatchUsePolling           bool
	WatchStabilityThresholdMs int
	WatchPollIntervalMs       int
	WatchMaxProjects          int
	WatchFollowSymlinks       bool
	BundledSkillDir           string
	Getenv                    func(name string) string
}

// ResolveConfig applies defaults and validates the positive-integer knobs.
func ResolveConfig(config Config) (Resolved, error) {
	resolved := Resolved{
		ProviderName:        config.ProviderName,
		Getenv:              config.Getenv,
		WatchUsePolling:     true,
		WatchFollowSymlinks: true,
	}
	if resolved.ProviderName == "" {
		resolved.ProviderName = "filesystem"
	}
	getenv := resolved.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	resolved.IncludeDefaultRoots = config.IncludeDefaultRoots == nil || *config.IncludeDefaultRoots
	resolved.DSHHome = homepaths.ResolveDshHome(config.DSHHome, getenv)
	agentsHome := config.AgentsHome
	if agentsHome == "" {
		agentsHome = getenv("DSH_AGENTS_HOME")
		if agentsHome == "" {
			agentsHome = filepath.Join(homepaths.ExpandHomePath("~"), ".agents")
		}
	}
	resolved.AgentsHome = resolvePath(agentsHome)
	resolved.CustomSkillDirs = make([]string, 0, len(config.CustomSkillDirs))
	for _, dir := range config.CustomSkillDirs {
		resolved.CustomSkillDirs = append(resolved.CustomSkillDirs, resolvePath(dir))
	}
	resolved.Watch = config.Watch == nil || *config.Watch
	resolved.WatchStabilityThresholdMs = config.WatchStabilityThresholdMs
	if resolved.WatchStabilityThresholdMs == 0 {
		resolved.WatchStabilityThresholdMs = DefaultWatchStabilityThresholdMs
	}
	resolved.WatchPollIntervalMs = config.WatchPollIntervalMs
	if resolved.WatchPollIntervalMs == 0 {
		resolved.WatchPollIntervalMs = DefaultWatchPollIntervalMs
	}
	resolved.WatchMaxProjects = config.WatchMaxProjects
	if resolved.WatchMaxProjects == 0 {
		resolved.WatchMaxProjects = DefaultWatchMaxProjects
	}
	resolved.WatchFollowSymlinks = config.WatchFollowSymlinks == nil || *config.WatchFollowSymlinks
	if config.WatchStabilityThresholdMs != 0 && config.WatchStabilityThresholdMs < 1 {
		return Resolved{}, fmt.Errorf("skill-filesystem: watchStabilityThresholdMs must be a positive integer")
	}
	if config.WatchPollIntervalMs != 0 && config.WatchPollIntervalMs < 1 {
		return Resolved{}, fmt.Errorf("skill-filesystem: watchPollIntervalMs must be a positive integer")
	}
	if config.WatchMaxProjects != 0 && config.WatchMaxProjects < 1 {
		return Resolved{}, fmt.Errorf("skill-filesystem: watchMaxProjects must be a positive integer")
	}
	bundled := config.BundledSkillDir
	if bundled == "" && resolved.IncludeDefaultRoots {
		bundled = getenv("DSH_BUNDLED_SKILL_DIR")
	}
	if bundled != "" {
		resolved.BundledSkillDir = resolvePath(bundled)
	}
	return resolved, nil
}

func resolvePath(path string) string {
	expanded := homepaths.ExpandHomePath(path)
	if abs, err := filepath.Abs(expanded); err == nil {
		return abs
	}
	return expanded
}

// skillRoot is one discovered root with its prompt-visible source and rank.
type skillRoot struct {
	path        string
	source      string
	rank        float64
	skipSystem  bool
	projectRoot string
	trustedHost bool
}

// rootEntry is one directory entry of a skill root.
type rootEntry struct {
	name string
	kind string // "directory", "file", "other"
	path string
}

// localLocator is the opaque provider-owned handle carried by candidates.
type localLocator struct {
	path      string
	directory string
}

// Provider maps local project/user skill roots into the skill registry.
type Provider struct {
	name            string
	resolved        Resolved
	invalidate      func()
	watcher         *watchManager
	bundledSkillDir string
	logger          cordis.Logger
}

// New builds the provider. The invalidation callback must be
// ProviderControl.Invalidate; the returned disposer closes every watcher.
func New(resolved Resolved, invalidate func(), logger cordis.Logger) *Provider {
	if invalidate == nil {
		invalidate = func() {}
	}
	if logger == nil {
		logger = cordis.Discard{}
	}
	return &Provider{
		name:            resolved.ProviderName,
		resolved:        resolved,
		invalidate:      invalidate,
		bundledSkillDir: resolved.BundledSkillDir,
		logger:          logger,
		watcher:         newWatchManager(resolved, invalidate),
	}
}

// Name is the unique provider name in the registry.
func (p *Provider) Name() string { return p.name }

// List discovers local skill summaries for a cwd-sensitive workspace.
// Watcher startup failure returns readable candidates as an incomplete
// observation.
func (p *Provider) List(options skill.LookupOptions) (skill.ProviderObservation, error) {
	roots := p.roots(options.CWD)
	complete := p.watcher.observeRoots(roots)
	var candidates []skill.Candidate
	for _, root := range roots {
		discovered, err := p.discoverRoot(root, options)
		if err != nil {
			return skill.ProviderObservation{}, err
		}
		candidates = append(candidates, discovered...)
	}
	return skill.ProviderObservation{Candidates: candidates, Complete: complete}, nil
}

// Get loads a complete local skill body from the candidate's file locator.
// A file that disappeared is absent.
func (p *Provider) Get(candidate skill.Candidate, options skill.LookupOptions) (*skill.Definition, error) {
	locator, ok := candidate.Locator.(localLocator)
	if !ok {
		return nil, nil
	}
	parsed, err := p.parseSkillFile(locator.path, options)
	if err != nil || parsed == nil {
		return nil, err
	}
	return &skill.Definition{
		Summary: skill.Summary{
			Name:         parsed.name,
			Description:  parsed.description,
			WhenToUse:    parsed.whenToUse,
			Invocation:   parsed.invocation,
			Source:       candidate.Source,
			Provider:     p.name,
			ResourceBase: &skill.ResourceBase{Kind: "directory", Path: locator.directory},
		},
		Path:     locator.path,
		Metadata: parsed.metadata,
		Content:  parsed.content,
	}, nil
}

// ObserveHostMutation invalidates the catalog after a first-party
// filesystem mutation whose path may touch a retained skill root.
func (p *Provider) ObserveHostMutation(path string) {
	p.watcher.observeHostMutation(path)
}

// Dispose closes every watcher and contains late callbacks.
func (p *Provider) Dispose() {
	p.watcher.dispose()
}

// roots assembles the cwd-sensitive root list: project roots around custom
// roots around user roots, then the bundled root.
func (p *Provider) roots(cwd string) []skillRoot {
	var roots []skillRoot
	if p.resolved.IncludeDefaultRoots && cwd != "" {
		projectRoot := findProjectRoot(resolvePath(cwd))
		roots = append(roots,
			skillRoot{path: filepath.Join(projectRoot, ".dsh", "skills"), source: "project-dsh", rank: ProjectDshRank, projectRoot: projectRoot},
			skillRoot{path: filepath.Join(projectRoot, ".agents", "skills"), source: "project-agents", rank: ProjectAgentsRank, projectRoot: projectRoot},
		)
	}
	for _, dir := range p.resolved.CustomSkillDirs {
		roots = append(roots, skillRoot{path: dir, source: "custom", rank: CustomRank})
	}
	if p.resolved.IncludeDefaultRoots {
		roots = append(roots,
			skillRoot{path: filepath.Join(p.resolved.DSHHome, "skills"), source: "user-dsh", rank: UserDshRank, skipSystem: true},
			skillRoot{path: filepath.Join(p.resolved.AgentsHome, "skills"), source: "user-agents", rank: UserAgentsRank},
		)
	}
	if p.bundledSkillDir != "" {
		roots = append(roots, skillRoot{path: p.bundledSkillDir, source: "bundled", rank: skill.BundledSkillRank, trustedHost: true})
	}
	return roots
}

// discoverRoot reads one root's entries and parses every skill file.
func (p *Provider) discoverRoot(root skillRoot, options skill.LookupOptions) ([]skill.Candidate, error) {
	var candidates []skill.Candidate
	entries, err := p.listRootEntries(root)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	for _, entry := range entries {
		if root.skipSystem && entry.name == ".system" {
			continue
		}
		var locator *localLocator
		switch {
		case entry.kind == "directory":
			locator = &localLocator{path: filepath.Join(entry.path, "SKILL.md"), directory: entry.path}
		case entry.kind == "file" && strings.HasSuffix(entry.name, ".md"):
			locator = &localLocator{path: entry.path, directory: root.path}
		}
		if locator == nil {
			continue
		}
		parsed, err := p.parseSkillFile(locator.path, options)
		if err != nil {
			return nil, err
		}
		if parsed == nil {
			continue
		}
		metadata := parsed.metadata
		candidates = append(candidates, skill.Candidate{
			Summary: skill.Summary{
				Name:         parsed.name,
				Description:  parsed.description,
				WhenToUse:    parsed.whenToUse,
				Invocation:   parsed.invocation,
				Provider:     p.name,
				Source:       root.source,
				ResourceBase: &skill.ResourceBase{Kind: "directory", Path: locator.directory},
			},
			Rank:     root.rank,
			Locator:  *locator,
			Path:     locator.path,
			Metadata: metadata,
		})
	}
	return candidates, nil
}

// listRootEntries reads one root's directory entries; an absent or
// unreadable root is empty, other failures propagate as incomplete
// discovery.
func (p *Provider) listRootEntries(root skillRoot) ([]rootEntry, error) {
	dirEntries, err := os.ReadDir(root.path)
	if err != nil {
		if isAbsentPathError(err) || errors.Is(err, syscall.ENOTDIR) {
			return nil, nil
		}
		return nil, err
	}
	result := make([]rootEntry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		full := filepath.Join(root.path, entry.Name())
		kind := "other"
		switch {
		case entry.IsDir():
			kind = "directory"
		case entry.Type().IsRegular():
			kind = "file"
		default:
			// A symbolic link (or special file) follows to its target kind.
			info, err := os.Stat(full)
			if err != nil {
				p.warnf("skill entry %s ignored: failed to follow symbolic link: %v", full, err)
				continue
			}
			switch {
			case info.IsDir():
				kind = "directory"
			case info.Mode().IsRegular():
				kind = "file"
			}
		}
		result = append(result, rootEntry{name: entry.Name(), kind: kind, path: full})
	}
	return result, nil
}

// findProjectRoot walks upward from cwd to the nearest directory containing
// .git, defaulting to cwd at the filesystem root.
func findProjectRoot(cwd string) string {
	current := cwd
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cwd
		}
		current = parent
	}
}

var _ = fs.ErrNotExist
