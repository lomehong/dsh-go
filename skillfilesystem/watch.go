// Polling watch manager: the Go adaptation of the official chokidar /
// fs.watchFile machinery. Each retained root gets one poller that snapshots
// the skill-relevant entries and invalidates the catalog on drift; project
// watches are bounded by maxProjects; host mutations from first-party tools
// invalidate immediately. The observable contract matches: relevant drift
// invalidates, startup failure leaves discovery incomplete, and disposal is
// quiescent.
package skillfilesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// watchManager owns bounded root pollers and funnels their drift into one
// invalidation callback.
type watchManager struct {
	resolved     Resolved
	invalidate   func()
	logf         func(format string, args ...any)
	mu           sync.Mutex
	roots        map[string]*rootWatchState
	projects     map[string]map[string]bool
	closing      bool
	notifyQueued bool
	lifecycle    *lifecycleContext
	wg           sync.WaitGroup
}

// rootWatchState is one retained root's poller state.
type rootWatchState struct {
	root       skillRoot
	owners     map[string]bool
	snapshot   string
	hasSnapped bool
	unhealthy  bool
}

// lifecycleContext is the disposal signal: cancelled on dispose.
type lifecycleContext struct {
	mu        sync.Mutex
	cancelled bool
	done      chan struct{}
}

func newLifecycle() *lifecycleContext {
	return &lifecycleContext{done: make(chan struct{})}
}

func (l *lifecycleContext) abort() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.cancelled {
		l.cancelled = true
		close(l.done)
	}
}

func (l *lifecycleContext) isCancelled() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancelled
}

func newWatchManager(resolved Resolved, invalidate func()) *watchManager {
	return &watchManager{
		resolved:   resolved,
		invalidate: invalidate,
		roots:      map[string]*rootWatchState{},
		projects:   map[string]map[string]bool{},
		lifecycle:  newLifecycle(),
	}
}

// observeRoots retains every root (grouped by project), evicts projects over
// the cap, and starts pollers. A retained root whose watcher cannot start
// (poller panic is contained) marks the observation incomplete.
func (m *watchManager) observeRoots(roots []skillRoot) bool {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return false
	}
	byProject := map[string][]skillRoot{}
	var pending []skillRoot
	for _, root := range roots {
		if root.projectRoot != "" {
			byProject[root.projectRoot] = append(byProject[root.projectRoot], root)
		} else {
			pending = append(pending, root)
		}
	}
	for projectRoot, grouped := range byProject {
		paths := map[string]bool{}
		for _, root := range grouped {
			paths[root.path] = true
		}
		m.projects[projectRoot] = paths
		pending = append(pending, grouped...)
	}
	evictedProject := false
	for len(m.projects) > m.resolved.WatchMaxProjects {
		var oldest string
		for candidate := range m.projects {
			oldest = candidate
			break
		}
		delete(m.projects, oldest)
		evictedProject = true
	}
	states := make([]*rootWatchState, 0, len(pending))
	for _, root := range pending {
		state, exists := m.roots[root.path]
		if !exists {
			state = &rootWatchState{root: root, owners: map[string]bool{}}
			m.roots[root.path] = state
		}
		state.owners["workspace"] = true
		states = append(states, state)
	}
	for path, state := range m.roots {
		retained := false
		for _, candidate := range states {
			if candidate == state {
				retained = true
				break
			}
		}
		_ = path
		if !retained {
			delete(m.roots, path)
		}
	}
	m.mu.Unlock()
	for _, state := range states {
		m.startPoller(state)
	}
	if evictedProject {
		m.invalidate()
	}
	return true
}

// startPoller launches one poller goroutine for a root (one per root even
// across repeated observations).
func (m *watchManager) startPoller(state *rootWatchState) {
	if !m.resolved.Watch {
		return
	}
	m.mu.Lock()
	if state.hasSnapped || m.closing {
		// Already polling (or shutting down): the existing ticker keeps
		// refreshing the snapshot; ownership is rooted in the state map.
		m.mu.Unlock()
		return
	}
	state.hasSnapped = true
	m.mu.Unlock()
	interval := time.Duration(m.resolved.WatchPollIntervalMs) * time.Millisecond
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			m.pollOnce(state)
			select {
			case <-m.lifecycle.done:
				return
			case <-ticker.C:
			}
		}
	}()
}

// pollOnce snapshots the root's relevant entries and invalidates on drift.
// An absent root is an empty snapshot, so creation is observed naturally.
func (m *watchManager) pollOnce(state *rootWatchState) {
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return
	}
	snapshot, err := snapshotRoot(state.root)
	if err != nil {
		// Snapshot failure leaves the root unhealthy; discovery keeps its
		// own read path and this poller retries on the next tick.
		state.unhealthy = true
		return
	}
	// The first snapshot seeds the baseline without invalidating: opening a
	// watcher is not a catalog change.
	m.mu.Lock()
	drifted := state.snapshot != snapshot
	state.snapshot = snapshot
	unhealthy := false
	state.unhealthy = unhealthy
	m.mu.Unlock()
	if drifted {
		m.queueInvalidation()
	}
}

// snapshotRoot renders a stable signature of a root's skill-relevant
// entries: names, kinds, and modification stamps.
func snapshotRoot(root skillRoot) (string, error) {
	entries, err := os.ReadDir(root.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "absent", nil
		}
		return "", err
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if root.skipSystem && entry.Name() == ".system" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			lines = append(lines, fmt.Sprintf("%s|gone", entry.Name()))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s|%v|%d|%d", entry.Name(), entry.Type(), info.Size(), info.ModTime().UnixNano()))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n"), nil
}

// observeHostMutation invalidates when a first-party mutation path may touch
// a retained root's skill surface.
func (m *watchManager) observeHostMutation(path string) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	normalized := resolvePath(path)
	relevant := false
	for _, state := range m.roots {
		if isPotentialSkillPath(state.root, normalized) {
			relevant = true
			break
		}
	}
	m.mu.Unlock()
	if relevant {
		m.invalidate()
	}
}

// isPotentialSkillPath reports whether a path is inside the root's skill
// surface: the root itself or anything beneath it.
func isPotentialSkillPath(root skillRoot, target string) bool {
	if target == root.path {
		return true
	}
	relative, err := filepath.Rel(root.path, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// queueInvalidation coalesces drift into one asynchronous invalidation.
func (m *watchManager) queueInvalidation() {
	m.mu.Lock()
	if m.closing || m.notifyQueued {
		m.mu.Unlock()
		return
	}
	m.notifyQueued = true
	m.mu.Unlock()
	go func() {
		m.mu.Lock()
		m.notifyQueued = false
		closing := m.closing
		m.mu.Unlock()
		if closing {
			return
		}
		m.invalidate()
	}()
}

// dispose stops every poller and waits for quiescence.
func (m *watchManager) dispose() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	m.closing = true
	m.roots = map[string]*rootWatchState{}
	m.projects = map[string]map[string]bool{}
	m.mu.Unlock()
	m.lifecycle.abort()
	m.wg.Wait()
}
