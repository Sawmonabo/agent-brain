// Package watch coalesces filesystem events under enrolled provider
// dirs into engine-cycle triggers (ADR 07). It never routes per-unit:
// one global trigger, one full cycle — the cycle is the rescan.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Config tunes the manager. Debounce is the trailing quiet window;
// Poll is the backstop rescan interval (0 disables — unit tests).
type Config struct {
	Debounce time.Duration
	Poll     time.Duration
}

// Trigger asks the daemon for one engine cycle.
type Trigger struct {
	// Reason is "fs" (debounced events), "poll" (backstop tick), or
	// "overflow" (the watcher lost events; the cycle IS the rescan).
	Reason string
}

// Manager owns one fsnotify watcher and the coalescing state machine.
type Manager struct {
	config   Config
	watcher  *fsnotify.Watcher
	triggers chan Trigger
	// pendingRoots maps a watched ancestor to the not-yet-existing
	// roots waiting under it (deleted/recreated provider dirs).
	pendingRoots map[string][]string
}

// New creates the manager with a buffered watcher (ADR 07: burst
// absorption while the pump goroutine is mid-iteration).
func New(config Config) (*Manager, error) {
	if config.Debounce <= 0 {
		return nil, fmt.Errorf("watch: debounce must be positive, got %v", config.Debounce)
	}
	if config.Poll < 0 {
		return nil, fmt.Errorf("watch: poll must be >= 0, got %v", config.Poll)
	}
	watcher, err := fsnotify.NewBufferedWatcher(64)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	return &Manager{
		config:       config,
		watcher:      watcher,
		triggers:     make(chan Trigger, 1),
		pendingRoots: map[string][]string{},
	}, nil
}

// Add watches root and its whole subtree. A missing root watches its
// nearest existing ancestor instead, attaching when the root appears.
// Call before Run; the pump owns all state afterwards.
func (m *Manager) Add(root string) error {
	root = filepath.Clean(root)
	if info, err := os.Stat(root); err == nil && info.IsDir() {
		return m.attachTree(root)
	}
	ancestor := filepath.Dir(root)
	for {
		if info, err := os.Stat(ancestor); err == nil && info.IsDir() {
			break
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("watch: no existing ancestor for %s", root)
		}
		ancestor = parent
	}
	if err := m.watcher.Add(ancestor); err != nil {
		return fmt.Errorf("watch %s: %w", ancestor, err)
	}
	m.pendingRoots[ancestor] = append(m.pendingRoots[ancestor], root)
	return nil
}

// attachTree adds a watch on every directory under (and including) dir.
// fsnotify is non-recursive by design (ADR 07); this walk is the
// recursion, and dir-Create events extend it dynamically.
func (m *Manager) attachTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil // raced with a delete; the next event re-attaches
			}
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if err := m.watcher.Add(p); err != nil {
			return fmt.Errorf("watch %s: %w", p, err)
		}
		return nil
	})
}

// Triggers is the coalesced output. Capacity 1, drop-when-full: an
// untaken buffered trigger already guarantees a future cycle sees any
// change that lands before the consumer takes it — merged, never lost.
func (m *Manager) Triggers() <-chan Trigger { return m.triggers }

// Close releases the underlying watcher.
func (m *Manager) Close() error { return m.watcher.Close() }

// Run pumps events until ctx is cancelled (returns nil) or the watcher
// dies (returns the error). Cancellation wins over a concurrently closed
// watcher: callers stop the manager by cancelling and then Closing
// without waiting for Run to return, so the select can observe the
// closed event/error stream instead of ctx.Done — the runtime picks
// among simultaneously ready cases at random. Once shutdown has been
// requested, a closed stream is orderly teardown, never a death.
func (m *Manager) Run(ctx context.Context) error {
	debounce := time.NewTimer(m.config.Debounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	debouncing := false

	var pollC <-chan time.Time
	if m.config.Poll > 0 {
		poll := time.NewTicker(m.config.Poll)
		defer poll.Stop()
		pollC = poll.C
	}

	for {
		select {
		case <-ctx.Done():
			debounce.Stop()
			return nil

		case event, ok := <-m.watcher.Events:
			if !ok {
				if ctx.Err() != nil {
					debounce.Stop()
					return nil // Close raced our own shutdown; orderly, not a death
				}
				return errors.New("watch: event stream closed")
			}
			if event.Op == fsnotify.Chmod {
				continue // atime/permission noise; content changes emit Write/Create
			}
			if event.Op.Has(fsnotify.Create) {
				m.handleCreate(event.Name)
			}
			if debouncing && !debounce.Stop() {
				<-debounce.C
			}
			debounce.Reset(m.config.Debounce)
			debouncing = true

		case <-debounce.C:
			debouncing = false
			m.emit("fs")

		case <-pollC:
			m.emit("poll")

		case err, ok := <-m.watcher.Errors:
			if !ok {
				if ctx.Err() != nil {
					debounce.Stop()
					return nil // Close raced our own shutdown; orderly, not a death
				}
				return errors.New("watch: error stream closed")
			}
			_ = err // overflow or transient watch error
			m.emit("overflow")
		}
	}
}

// handleCreate attaches newly created directories (and pending roots
// that just appeared). Files that landed inside before the watch
// attached are covered by the trigger this very event produces.
func (m *Manager) handleCreate(name string) {
	if roots, ok := m.pendingRoots[filepath.Dir(name)]; ok {
		remaining := roots[:0]
		for _, root := range roots {
			if root == name || isUnder(root, name) {
				if info, err := os.Stat(root); err == nil && info.IsDir() {
					_ = m.attachTree(root)
					continue
				}
			}
			remaining = append(remaining, root)
		}
		if len(remaining) == 0 {
			delete(m.pendingRoots, filepath.Dir(name))
		} else {
			m.pendingRoots[filepath.Dir(name)] = remaining
		}
	}
	if info, err := os.Stat(name); err == nil && info.IsDir() {
		_ = m.attachTree(name)
	}
}

func isUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) &&
		rel != "." && !hasDotDotPrefix(rel)
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator)
}

func (m *Manager) emit(reason string) {
	select {
	case m.triggers <- Trigger{Reason: reason}:
	default: // coalesce into the already-buffered trigger
	}
}
