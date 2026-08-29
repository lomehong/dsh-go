package spilllocal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"dshgo/cordis"
	"dshgo/spill"
)

// msPerDay converts the CleanupPeriodDays config to the sweep cutoff.
const msPerDay = 24 * 60 * 60 * 1000

// Config is the plugin config (all optional — ResolveConfig supplies the
// defaults).
type Config struct {
	// Root is the root directory for spill files. Empty uses a lazily-created
	// private (0700) per-process directory under the OS temp dir — the safe
	// default for a local deployment. Set it to keep spill files under a
	// known location.
	Root string
	// CleanupPeriodDays is the age in days after which a spill file is
	// eligible for the one-shot startup cleanup sweep. Zero means "not
	// configured" and resolves to 30; a negative value is rejected. Zero is
	// representable via CleanupPeriodDaysSet. Files whose mtime is strictly
	// older than the cutoff are deleted and emptied directories are pruned;
	// fresh files, symlinks, and unrelated entries are left untouched.
	// Retention is deliberate — a resumed or forked session may still
	// reference an older locator until it ages out.
	CleanupPeriodDays int
	// CleanupPeriodDaysSet marks an explicit CleanupPeriodDays (including 0,
	// which disables cleanup entirely).
	CleanupPeriodDaysSet bool
}

// ResolveConfig applies the documented default: cleanup after 30 days unless
// explicitly configured.
func ResolveConfig(config Config) Config {
	if config.CleanupPeriodDaysSet {
		if config.CleanupPeriodDays < 0 {
			panic(fmt.Sprintf("spill-local: cleanupPeriodDays must be a non-negative integer (got %d)", config.CleanupPeriodDays))
		}
		return config
	}
	config.CleanupPeriodDays = 30
	config.CleanupPeriodDaysSet = true
	return config
}

// LocalSpillStore is the local-filesystem spill backend. Files land under
// `<root>/session-<hash>/…` with unpredictable names, an exclusive owner-only
// (0600) write, and a private (0700) root — a spilled tool result must not be
// readable by other local users or redirectable via a planted symlink.
//
// After activation it launches ONE best-effort cleanup sweep that reclaims
// expired spill files without delaying service availability; Close awaits
// the sweep, so an unload never returns before it quiesces.
type LocalSpillStore struct {
	// Root is the resolved absolute spill root (config Root, else the
	// private default), fixed at construction.
	Root string

	// cleanupPeriodDays is the resolved retention knob.
	cleanupPeriodDays int

	// defaultRootsBase is the directory scanned for prior default roots —
	// the OS tmpdir, where PrivateRoot creates them. Overridable per
	// instance for tests.
	defaultRootsBase string

	// gatherRoots is the sweep's one async gather seam; tests override it to
	// inject an isolated root set and to hold the sweep open across Close
	// for the quiescence check.
	gatherRoots func(warn WarnFn) []SweepRoot

	// sweep is the in-flight (or settled) startup cleanup sweep, held so
	// Close can await it; nil when cleanup is disabled.
	sweepWait sync.WaitGroup
	sweepOnce sync.Once
}

// NewLocalSpillStore builds the store and launches the one best-effort
// startup sweep when cleanup is enabled. Service availability is never
// delayed: the sweep runs in the background and Close awaits the SAME work.
func NewLocalSpillStore(config Config, logger cordis.Logger) (*LocalSpillStore, error) {
	resolved := ResolveConfig(config)
	store := &LocalSpillStore{cleanupPeriodDays: resolved.CleanupPeriodDays}
	if resolved.Root != "" {
		absolute, err := filepath.Abs(resolved.Root)
		if err != nil {
			return nil, err
		}
		store.Root = absolute
	} else {
		root, err := PrivateRoot()
		if err != nil {
			return nil, err
		}
		store.Root = root
	}
	store.defaultRootsBase = os.TempDir()
	store.gatherRoots = func(warn WarnFn) []SweepRoot {
		return GatherSweepRoots(store.Root, warn, store.defaultRootsBase)
	}
	if store.cleanupPeriodDays > 0 {
		store.sweepOnce.Do(func() {
			store.sweepWait.Add(1)
			go func() {
				defer store.sweepWait.Done()
				store.runCleanup(func(message string) {
					if logger != nil {
						logger.Warn(message)
					}
				})
			}()
		})
	}
	return store, nil
}

// runCleanup runs the one-shot cleanup: gather the roots to sweep and sweep
// all of them at the age cutoff. Best-effort — sweepSpillRoots contains
// every filesystem failure, so this never fails and cannot fail activation
// or a concurrent spill write.
func (s *LocalSpillStore) runCleanup(warn WarnFn) {
	cutoff := time.Now().UnixMilli() - int64(s.cleanupPeriodDays)*msPerDay
	sweepSpillRoots(SweepOptions{Roots: s.gatherRoots(warn), CutoffUnixMilli: cutoff, Warn: warn})
}

// Close awaits the startup sweep so a fiber unload reaches quiescence (no
// sweep I/O outlives the store). Disabled cleanup closes immediately.
func (s *LocalSpillStore) Close() {
	s.sweepWait.Wait()
}

// SaveText persists the input's content to a private session-scoped file and
// returns the path locator with local read/grep retrieval guidance.
func (s *LocalSpillStore) SaveText(ctx context.Context, input spill.SaveTextSpill) (spill.SpillRef, error) {
	saved, err := SaveTextFile(SaveTextOptions{
		Root:          s.Root,
		SessionID:     input.Owner.SessionID,
		SuggestedName: input.SuggestedName,
		Content:       input.Content,
	})
	if err != nil {
		return spill.SpillRef{}, err
	}
	return spill.SpillRef{
		Locator:       saved.Path,
		Bytes:         saved.Bytes,
		RetrievalHint: "Use read with offset/limit, or grep this path to search within it.",
	}, nil
}

// SetDefaultRootsBase points default-root discovery at an isolated fixture
// instead of the real tmpdir. It is a test seam, not a deployment knob.
func (s *LocalSpillStore) SetDefaultRootsBase(base string) {
	s.defaultRootsBase = base
}

// SetGatherRoots injects an isolated root set — and, being the sweep's one
// async gather point, holds the sweep open across Close for the quiescence
// check. It is a test seam, not a deployment knob.
func (s *LocalSpillStore) SetGatherRoots(gather func(warn WarnFn) []SweepRoot) {
	s.gatherRoots = gather
}

// LaunchSweepForTest runs one cleanup sweep through the current gather seam
// in the background and returns a waiter for its completion. Test seam only.
func (s *LocalSpillStore) LaunchSweepForTest(warn WarnFn) func() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runCleanup(warn)
	}()
	return wg.Wait
}
