package filereference

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// DefaultMaxResults is the maximum candidates rendered for one query.
const DefaultMaxResults = 20

// DefaultMaxEntries is the maximum entries retained in one workspace index.
const DefaultMaxEntries = 50_000

// DefaultExcludedDirectories are the directory basenames omitted from
// traversal unless the deployment overrides them: version-control and
// dependency stores plus build-output names no ecosystem also uses for
// sources. `lib` is deliberately absent — Ruby gems and many npm packages
// keep their sources there, and excluding it would make `@` miss those
// sources entirely and silently.
var DefaultExcludedDirectories = []string{
	".git", "node_modules", "dist", "build", "out", "coverage", "target",
	".next", ".nuxt", ".turbo", ".venv", "__pycache__", ".pytest_cache",
	".mypy_cache", ".gradle",
}

// SearchConfig is the resolved limits and exclusions for one workspace
// index.
type SearchConfig struct {
	// MaxResults is the maximum ranked candidates returned for one query.
	MaxResults int
	// MaxEntries is the maximum indexed files and directories.
	MaxEntries int
	// ExcludedDirectories are directory basenames never traversed or
	// offered.
	ExcludedDirectories []string
}

func validateConfig(config SearchConfig) error {
	if config.MaxResults <= 0 {
		return fmt.Errorf("file search maxResults must be a positive safe integer")
	}
	if config.MaxEntries <= 0 {
		return fmt.Errorf("file search maxEntries must be a positive safe integer")
	}
	for _, name := range config.ExcludedDirectories {
		if name == "" || strings.ContainsAny(name, `/\`) {
			return fmt.Errorf("file search excludedDirectories entries must be non-empty directory basenames")
		}
	}
	return nil
}

// resolvedEntry is one sorted os.DirEntry with its name captured.
type resolvedEntry struct {
	name  string
	isDir bool
}

// indexGeneration is one in-flight or settled traversal.
type indexGeneration struct {
	cancel    context.CancelFunc
	entries   []Candidate
	startedAt int
	done      chan struct{}
	err       error
}

// WorkspaceFileSearch is a cancellable, reusable fuzzy index rooted at one
// agent working directory. Directory-scoped queries list live state; bare
// fuzzy queries share one bounded traversal. Only the first query of a
// workspace waits for that traversal — an invalidated index keeps answering
// while its replacement builds behind the caret.
type WorkspaceFileSearch struct {
	root                string
	config              SearchConfig
	excludedDirectories map[string]bool

	mu          sync.Mutex
	settled     *indexGeneration
	generation  *indexGeneration
	invalidated int
	disposed    bool
}

// NewWorkspaceFileSearch roots one search at root with the given limits. An
// invalid config fails loud.
func NewWorkspaceFileSearch(root string, config SearchConfig) (*WorkspaceFileSearch, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return newSearch(root, config), nil
}

// newSearch builds an already-validated search (Service reuses it because
// NewService validated the config once at construction).
func newSearch(root string, config SearchConfig) *WorkspaceFileSearch {
	excluded := make(map[string]bool, len(config.ExcludedDirectories))
	for _, name := range config.ExcludedDirectories {
		excluded[name] = true
	}
	return &WorkspaceFileSearch{root: root, config: config, excludedDirectories: excluded}
}

// List returns ranked path candidates for the current token. ctx cancels
// this caller's wait without killing an index shared by a newer query.
func (s *WorkspaceFileSearch) List(ctx context.Context, rawQuery string) ([]Candidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("file search aborted: %w", err)
	}
	s.mu.Lock()
	if s.disposed {
		s.mu.Unlock()
		return []Candidate{}, nil
	}
	s.mu.Unlock()
	query := strings.ReplaceAll(rawQuery, `\`, `/`)
	slash := strings.LastIndex(query, "/")
	if query == "" || slash >= 0 {
		directory := ""
		fragment := ""
		if slash >= 0 {
			directory = query[:slash+1]
			fragment = query[slash+1:]
		}
		return s.listDirectory(ctx, directory, fragment)
	}
	indexed, err := s.indexFor(ctx)
	if err != nil {
		return nil, err
	}
	visible := make([]Candidate, 0, len(indexed))
	for _, candidate := range indexed {
		if visibleForGlobalQuery(candidate.Path, query) {
			visible = append(visible, candidate)
		}
	}
	return rankCandidates(visible, query, s.config.MaxResults), nil
}

// Invalidate marks the index stale so a later bare query observes a fresh
// tree. The stale entries are kept and keep answering: a rebuild costs one
// traversal of the whole workspace, and putting that in front of the caret
// is what a caller invalidating on every tool result would otherwise pay.
func (s *WorkspaceFileSearch) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidated++
}

// Dispose aborts any in-flight traversal and makes later queries return no
// candidates.
func (s *WorkspaceFileSearch) Dispose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disposed {
		return
	}
	s.disposed = true
	if s.generation != nil {
		s.generation.cancel()
		s.generation = nil
	}
	s.settled = nil
}

// indexFor returns the entries a bare fuzzy query ranks. Only the first
// query waits for a traversal; afterwards a stale index answers immediately
// while its replacement rebuilds in the background.
func (s *WorkspaceFileSearch) indexFor(ctx context.Context) ([]Candidate, error) {
	s.mu.Lock()
	if s.settled == nil {
		if s.generation == nil {
			s.spawnRebuildLocked()
		}
		pending := s.generation
		s.mu.Unlock()
		select {
		case <-pending.done:
			if pending.err != nil {
				return nil, fmt.Errorf("file search index failed: %w", pending.err)
			}
			return pending.entries, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("file search aborted: %w", ctx.Err())
		}
	}
	settled := s.settled
	if settled.startedAt < s.invalidated && s.generation == nil {
		// Background refresh: a failure is not this caller's error — the
		// stale entries still answer, and startedAt staying behind makes
		// the next bare query start a fresh attempt.
		s.spawnRebuildLocked()
	}
	s.mu.Unlock()
	return settled.entries, nil
}

// spawnRebuildLocked launches one traversal on its own goroutine. Callers
// hold mu.
func (s *WorkspaceFileSearch) spawnRebuildLocked() {
	traversalCtx, cancel := context.WithCancel(context.Background())
	generation := &indexGeneration{cancel: cancel, startedAt: s.invalidated, done: make(chan struct{})}
	s.generation = generation
	go func() {
		entries, err := s.scanWorkspace(traversalCtx)
		s.mu.Lock()
		generation.entries, generation.err = entries, err
		if err == nil && !s.disposed {
			s.generation = nil
			s.settled = generation
		} else if s.generation == generation {
			s.generation = nil
		}
		close(generation.done)
		s.mu.Unlock()
	}()
}

// scanWorkspace walks the workspace breadth-first into one bounded index.
func (s *WorkspaceFileSearch) scanWorkspace(ctx context.Context) ([]Candidate, error) {
	indexed := make([]Candidate, 0, 256)
	type queued struct{ absolute, relative string }
	directories := []queued{{absolute: s.root, relative: ""}}
	for cursor := 0; cursor < len(directories) && len(indexed) < s.config.MaxEntries; cursor++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		directory := directories[cursor]
		// The root is not a subtree: an unreadable branch costs its own
		// candidates, but an unreadable root means the traversal learned
		// nothing and must not publish an empty index.
		entries, err := s.readSortedEntries(directory.absolute)
		if err != nil {
			if cursor == 0 {
				return nil, err
			}
			entries = nil
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := entry.name
			if directory.relative != "" {
				path = directory.relative + "/" + entry.name
			}
			if entry.isDir {
				if s.excludedDirectories[entry.name] {
					continue
				}
				indexed = append(indexed, Candidate{Path: path, Kind: "directory"})
				directories = append(directories, queued{absolute: filepath.Join(directory.absolute, entry.name), relative: path})
			} else {
				indexed = append(indexed, Candidate{Path: path, Kind: "file"})
			}
			if len(indexed) >= s.config.MaxEntries {
				break
			}
		}
	}
	return indexed, nil
}

// readSortedEntries lists one directory, sorted by name; the boolean reports
// whether the directory was readable at all.
func (s *WorkspaceFileSearch) readSortedEntries(absolute string) ([]resolvedEntry, error) {
	dirEntries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, err
	}
	entries := make([]resolvedEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		entries = append(entries, resolvedEntry{name: dirEntry.Name(), isDir: dirEntry.IsDir()})
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].name < entries[right].name
	})
	return entries, nil
}

// listDirectory ranks the live contents of one displayed directory.
func (s *WorkspaceFileSearch) listDirectory(ctx context.Context, displayDirectory, fragment string) ([]Candidate, error) {
	for _, segment := range strings.Split(displayDirectory, "/") {
		if s.excludedDirectories[segment] {
			return []Candidate{}, nil
		}
	}
	absolute := s.resolveDisplayDirectory(ctx, displayDirectory)
	if absolute == "" {
		return []Candidate{}, nil
	}
	entries, err := s.readSortedEntries(absolute)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("file search aborted: %w", ctxErr)
		}
		// An unreadable/missing subtree contributes no candidates; other
		// readable branches remain useful and autocomplete is advisory.
		entries = nil
	}
	candidates := make([]Candidate, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.name, ".") && !strings.HasPrefix(fragment, ".") {
			continue
		}
		if entry.isDir {
			if s.excludedDirectories[entry.name] {
				continue
			}
			candidates = append(candidates, Candidate{Path: displayDirectory + entry.name, Kind: "directory"})
		} else {
			candidates = append(candidates, Candidate{Path: displayDirectory + entry.name, Kind: "file"})
		}
	}
	return rankCandidates(candidates, fragment, s.config.MaxResults), nil
}

// resolveDisplayDirectory resolves one displayed directory under the root,
// rejecting escapes above the root, cross-volume absolutes, and non-
// directory segments. An unusable display directory offers no candidates.
func (s *WorkspaceFileSearch) resolveDisplayDirectory(ctx context.Context, displayDirectory string) string {
	resolvedRoot, err := filepath.Abs(s.root)
	if err != nil {
		return ""
	}
	target := resolvedRoot
	if displayDirectory != "" {
		target = filepath.Join(resolvedRoot, filepath.FromSlash(displayDirectory))
	}
	relative, err := filepath.Rel(resolvedRoot, target)
	if err != nil {
		return ""
	}
	if relative == ".." || strings.HasPrefix(relative, `..`+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return ""
	}
	current := resolvedRoot
	for _, segment := range strings.Split(relative, string(filepath.Separator)) {
		if segment == "" || segment == "." {
			continue
		}
		if err := ctx.Err(); err != nil {
			return ""
		}
		current = filepath.Join(current, segment)
		info, err := os.Stat(current)
		if err != nil || !info.IsDir() {
			return ""
		}
	}
	return current
}

// visibleForGlobalQuery hides dot segments from global queries unless the
// query itself is looking for one.
func visibleForGlobalQuery(path, query string) bool {
	if strings.HasPrefix(query, ".") || strings.Contains(query, "/.") {
		return true
	}
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, ".") {
			return false
		}
	}
	return true
}

// rankCandidates orders scored candidates: score desc, directories first,
// shorter paths (only for non-empty queries), then text order.
func rankCandidates(candidates []Candidate, query string, limit int) []Candidate {
	type ranked struct {
		candidate Candidate
		score     int
	}
	scored := make([]ranked, 0, len(candidates))
	for _, candidate := range candidates {
		if score, ok := scoreCandidate(candidate, query); ok {
			scored = append(scored, ranked{candidate: candidate, score: score})
		}
	}
	sort.Slice(scored, func(left, right int) bool {
		l, r := scored[left], scored[right]
		if l.score != r.score {
			return l.score > r.score
		}
		if kindRank(l.candidate.Kind) != kindRank(r.candidate.Kind) {
			return kindRank(l.candidate.Kind) < kindRank(r.candidate.Kind)
		}
		if query != "" && len(l.candidate.Path) != len(r.candidate.Path) {
			return len(l.candidate.Path) < len(r.candidate.Path)
		}
		return l.candidate.Path < r.candidate.Path
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	ordered := make([]Candidate, 0, len(scored))
	for _, entry := range scored {
		ordered = append(ordered, entry.candidate)
	}
	return ordered
}

// scoreCandidate ranks one candidate against the query; the boolean is false
// when the candidate does not match at all.
func scoreCandidate(candidate Candidate, query string) (int, bool) {
	if query == "" {
		return 0, true
	}
	path := strings.ToLower(candidate.Path)
	name := path[strings.LastIndex(path, "/")+1:]
	needle := strings.ToLower(query)
	directoryBonus := 0
	if candidate.Kind == "directory" {
		directoryBonus = 25
	}
	switch {
	case name == needle:
		return 1000 + directoryBonus, true
	case strings.HasPrefix(name, needle):
		return 900 + directoryBonus, true
	case strings.Contains(name, needle):
		return 700 + directoryBonus, true
	case strings.Contains(path, needle):
		return 500 + directoryBonus, true
	}
	subsequence, ok := subsequenceScore(path, needle)
	if !ok {
		return 0, false
	}
	return 300 + subsequence + directoryBonus, true
}

// subsequenceScore scores an in-order character match: 100 minus the total
// gap between matched characters, floored at 0.
func subsequenceScore(target, query string) (int, bool) {
	targetRunes := []rune(target)
	targetIndex := 0
	gap := 0
	for _, character := range query {
		found := -1
		for index := targetIndex; index < len(targetRunes); index++ {
			if targetRunes[index] == character {
				found = index
				break
			}
		}
		if found < 0 {
			return 0, false
		}
		gap += found - targetIndex
		targetIndex = found + 1
	}
	if gap < 0 {
		gap = 0
	}
	score := 100 - gap
	if score < 0 {
		score = 0
	}
	return score, true
}

func kindRank(kind string) int {
	if kind == "directory" {
		return 0
	}
	return 1
}
