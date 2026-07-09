package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/watch"
)

const testDebounce = 40 * time.Millisecond

func startManager(t *testing.T, config watch.Config, roots ...string) *watch.Manager {
	t.Helper()
	manager, err := watch.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	for _, root := range roots {
		if err := manager.Add(root); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		if err := manager.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	return manager
}

//nolint:unparam // within is a per-call deadline budget mirroring assertQuiet; kept explicit so call sites read symmetrically.
func awaitTrigger(t *testing.T, manager *watch.Manager, within time.Duration) watch.Trigger {
	t.Helper()
	select {
	case trigger := <-manager.Triggers():
		return trigger
	case <-time.After(within):
		t.Fatal("no trigger within deadline")
		return watch.Trigger{}
	}
}

func assertQuiet(t *testing.T, manager *watch.Manager, within time.Duration) {
	t.Helper()
	select {
	case trigger := <-manager.Triggers():
		t.Fatalf("unexpected trigger %+v", trigger)
	case <-time.After(within):
	}
}

func TestWriteTriggersOnceAfterDebounce(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	if err := os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trigger := awaitTrigger(t, manager, 2*time.Second)
	if trigger.Reason != "fs" {
		t.Fatalf("Reason = %q, want fs", trigger.Reason)
	}
	assertQuiet(t, manager, 6*testDebounce)
}

func TestBurstCoalescesToOneTrigger(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	for i := range 5 {
		name := filepath.Join(root, "memories", "topic-"+string(rune('a'+i))+".md")
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("fact\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	awaitTrigger(t, manager, 2*time.Second)
	assertQuiet(t, manager, 6*testDebounce)
}

// TestNewSubdirectoryGetsWatched proves dynamic attach: a write INSIDE a
// directory created after startup still triggers (fsnotify itself is
// non-recursive — this is the manager's added value).
func TestNewSubdirectoryGetsWatched(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	subdir := filepath.Join(root, "rollout_summaries")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // the mkdir itself

	// Wait out the debounce, then write inside the NEW directory.
	time.Sleep(4 * testDebounce)
	if err := os.WriteFile(filepath.Join(subdir, "s.md"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second)
}

// TestMissingRootAttachesWhenCreated covers the deleted-and-recreated
// provider dir: Add on a nonexistent root watches the nearest existing
// ancestor and attaches the root when it appears.
func TestMissingRootAttachesWhenCreated(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, ".claude", "memory")
	manager := startManager(t, watch.Config{Debounce: testDebounce}, root)

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // creation trigger

	time.Sleep(4 * testDebounce)
	if err := os.WriteFile(filepath.Join(root, "MEMORY.md"), []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitTrigger(t, manager, 2*time.Second) // proves the root is attached
}

func TestPollBackstopFires(t *testing.T) {
	t.Parallel()
	manager := startManager(t, watch.Config{Debounce: testDebounce, Poll: 100 * time.Millisecond}, t.TempDir())
	trigger := awaitTrigger(t, manager, 2*time.Second)
	if trigger.Reason != "poll" {
		t.Fatalf("Reason = %q, want poll", trigger.Reason)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	if _, err := watch.New(watch.Config{Debounce: 0}); err == nil {
		t.Fatal("zero debounce accepted")
	}
	if _, err := watch.New(watch.Config{Debounce: time.Second, Poll: -time.Second}); err == nil {
		t.Fatal("negative poll accepted")
	}
}
