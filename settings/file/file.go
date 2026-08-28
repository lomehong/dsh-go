// Package file persists the user settings document as one YAML file and
// pushes external edits into the store. Source:
// packages/settings/settings-file — the provider stores the raw document,
// writes atomically with retry, and external edits reload with the provider
// source; a section the owner rejects keeps the namespace's last good value.
package file

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"dshgo/cordis"
	"dshgo/settings"
)

// Store owns one settings document file. Host-side writes persist through the
// store's update stream; external edits reload on a polling watch (the
// package keeps a dependency-free watcher; the official provider uses fs
// notifications behind the same contract).
type Store struct {
	path     string
	store    *settings.Store
	logger   cordis.Logger
	interval time.Duration

	stop    chan struct{}
	stopped chan struct{}

	mu      sync.Mutex
	lastMod time.Time
}

// Open loads the document at path into the store (provider source) and starts
// watching it. A missing file is an empty document, not an error.
func Open(path string, store *settings.Store, logger cordis.Logger) (*Store, error) {
	if logger == nil {
		logger = cordis.Discard{}
	}
	f := &Store{
		path:     path,
		store:    store,
		logger:   logger,
		interval: 500 * time.Millisecond,
		stop:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	if err := f.load(); err != nil {
		return nil, err
	}
	store.OnUpdated(func(event *settings.UpdateEvent) {
		if event.Source != settings.SourceUpdate {
			return
		}
		if err := f.Save(); err != nil {
			f.logger.Error(fmt.Sprintf("settings/file: persisting %s failed: %v", f.path, err))
		}
	})
	go f.watch()
	return f, nil
}

// SetPollInterval adjusts the external-edit watch cadence; call before Open's
// watcher matters (tests use this to keep runs fast).
func (f *Store) SetPollInterval(interval time.Duration) {
	f.interval = interval
}

// Close stops the watcher.
func (f *Store) Close() error {
	close(f.stop)
	<-f.stopped
	return nil
}

func (f *Store) load() error {
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("settings/file: failed to read %s: %w", f.path, err)
	}
	if fi, statErr := os.Stat(f.path); statErr == nil {
		f.mu.Lock()
		f.lastMod = fi.ModTime()
		f.mu.Unlock()
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	var document map[string]map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("settings/file: failed to parse %s: %w", f.path, err)
	}
	for ns, section := range document {
		if section == nil {
			continue
		}
		// A section the owner rejects keeps the namespace's last good value;
		// the warning is the only trace, exactly as the official provider.
		if err := f.store.ProviderPush(ns, section); err != nil {
			f.logger.Warn(fmt.Sprintf("settings/file: section %q from %s rejected, keeping last good value: %v", ns, f.path, err))
		}
	}
	return nil
}

// Save writes the current user document atomically: temp file plus rename,
// retrying the rename while Windows file locks (EBUSY/EPERM-style) clear.
func (f *Store) Save() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	document := f.store.Document()
	data, err := yaml.Marshal(document)
	if err != nil {
		return fmt.Errorf("settings/file: failed to encode %s: %w", f.path, err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("settings/file: failed to write %s: %w", tmp, err)
	}
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if err := os.Rename(tmp, f.path); err == nil {
			if fi, statErr := os.Stat(f.path); statErr == nil {
				f.lastMod = fi.ModTime()
			}
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("settings/file: failed to move %s into place: %w", tmp, lastErr)
}

func (f *Store) watch() {
	defer close(f.stopped)
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-ticker.C:
			fi, err := os.Stat(f.path)
			if err != nil {
				continue
			}
			f.mu.Lock()
			unchanged := fi.ModTime().Equal(f.lastMod)
			f.mu.Unlock()
			if unchanged {
				continue
			}
			// An unreadable or invalid edit keeps the previous document; the
			// watcher only logs and stays alive.
			if err := f.load(); err != nil {
				f.logger.Warn(fmt.Sprintf("settings/file: reload of %s failed, keeping previous document: %v", f.path, err))
				f.mu.Lock()
				f.lastMod = fi.ModTime()
				f.mu.Unlock()
			}
		}
	}
}
