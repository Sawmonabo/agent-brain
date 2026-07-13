package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/watch"
)

// discardWatchStates is the no-op watch-state recorder for rebuild tests that
// assert manager/trigger behavior rather than the recorded state snapshot.
func discardWatchStates(map[string]string) {}

// TestRebuildWatcherSwapsRootsAndClosesOld pins the rebuild-by-replacement
// path that both enrollment changes and watcher death flow through: a new
// Manager is built for the new root set and the old one is closed. A real
// fsnotify fd death can't be forced portably, so exercising the SAME code
// path here is how the death-recovery branch is covered — loop's watchDied
// handler calls exactly this function.
func TestRebuildWatcherSwapsRootsAndClosesOld(t *testing.T) {
	t.Parallel()
	rootA := t.TempDir()
	rootB := t.TempDir()
	ctx := t.Context()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := watch.Config{Debounce: 20 * time.Millisecond, Poll: 0}
	watchDied := make(chan error, 1)

	// First build: cover rootA only.
	live := rebuildWatcher(ctx, cfg, nil, []string{rootA}, watchDied, logger, discardWatchStates)
	if live.manager == nil {
		t.Fatal("first rebuild produced no manager")
	}
	writeInto(t, rootA, "a1.md")
	waitTrigger(t, live.triggers, "event under rootA before the swap")
	oldTriggers := live.triggers

	// Swap to rootB: closes the old manager. The deliberate close must not
	// masquerade as a watcher death (that would spin loop rebuilding).
	live = rebuildWatcher(ctx, cfg, live, []string{rootB}, watchDied, logger, discardWatchStates)
	if live.manager == nil {
		t.Fatal("second rebuild produced no manager")
	}
	if live.triggers == oldTriggers {
		t.Fatal("rebuild kept the old triggers channel; expected a fresh manager")
	}

	// The new root now triggers cycles.
	writeInto(t, rootB, "b1.md")
	waitTrigger(t, live.triggers, "event under rootB after the swap")

	// The removed root is silent on both the new channel and the old,
	// now-closed one.
	writeInto(t, rootA, "a2.md")
	assertNoTrigger(t, live.triggers, 300*time.Millisecond, "removed rootA on the new channel")
	assertNoTrigger(t, oldTriggers, 300*time.Millisecond, "removed rootA on the closed old channel")

	select {
	case err := <-watchDied:
		t.Fatalf("a deliberate rebuild reported a watcher death: %v", err)
	default:
	}
}

// TestRebuildWatcherGenuineDeathReachesWatchDied pins the detection half
// of death recovery: when the watcher's streams close while BOTH contexts
// are live, the goroutine wrapping Run must deliver the death on
// watchDied so loop's handler can rebuild. Closing the manager without
// cancelling anything forces exactly that shape — fsnotify signals a
// spontaneous death and a deliberate Close identically (readEvents exits
// and closes both streams, no error value attached), so this is the
// observable form of fd exhaustion or WSL2 teardown. Deterministic: with
// no poll ticker, no writes, and no cancellation, the stream close is the
// only thing that can ever wake Run's select.
func TestRebuildWatcherGenuineDeathReachesWatchDied(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := watch.Config{Debounce: 20 * time.Millisecond, Poll: 0}
	watchDied := make(chan error, 1)

	live := rebuildWatcher(t.Context(), cfg, nil, []string{t.TempDir()}, watchDied, logger, discardWatchStates)
	if live.manager == nil {
		t.Fatal("rebuild produced no manager")
	}

	if err := live.manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-watchDied:
		if err == nil {
			t.Fatal("watchDied delivered a nil error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher death never reached watchDied")
	}
}

// TestRebuildWatcherBuildFailureFallsBackToBackstop pins the degraded
// path: when watch.New rejects the config, rebuildWatcher returns a
// manager-less state (nil triggers, so loop's select blocks on it and the
// ticker/poll backstop keeps cycles alive) instead of panicking, and it
// still remembers the roots so a later cycle can retry the build.
func TestRebuildWatcherBuildFailureFallsBackToBackstop(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	watchDied := make(chan error, 1)
	roots := []string{t.TempDir()}

	// Debounce <= 0 is the one config watch.New rejects.
	live := rebuildWatcher(context.Background(), watch.Config{Debounce: 0}, nil, roots, watchDied, logger, discardWatchStates)
	if live.manager != nil || live.triggers != nil {
		t.Fatalf("failed build should yield no manager/triggers, got %+v", live)
	}
	if live.cancel != nil {
		t.Fatal("failed build must not leave a cancel func to call")
	}
	if len(live.watched) != len(roots) || live.watched[0] != roots[0] {
		t.Fatalf("failed build must remember roots for a later retry, got %v", live.watched)
	}

	// A subsequent rebuild with this degraded state as `old` must not panic
	// on the nil cancel/manager.
	next := rebuildWatcher(context.Background(), watch.Config{Debounce: 0}, live, roots, watchDied, logger, discardWatchStates)
	if next.manager != nil {
		t.Fatal("second failed build should still yield no manager")
	}
}

// TestRebuildWatcherRecordsWatchState pins the WatchState capture (Task 6.5): a
// healthy build records "watching" for every attached root, and a build failure
// records a "failed:…" state whose wording conveys the ticker/poll backstop
// still covers the unit — the daemon logs-and-continues, and the per-unit column
// must say so.
func TestRebuildWatcherRecordsWatchState(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	watchDied := make(chan error, 1)
	rootA := t.TempDir()

	var got map[string]string
	record := func(states map[string]string) { got = states }

	// Healthy build: the attached root reports "watching".
	live := rebuildWatcher(context.Background(), watch.Config{Debounce: 20 * time.Millisecond}, nil, []string{rootA}, watchDied, logger, record)
	if live.manager != nil {
		_ = live.manager.Close()
	}
	if got[rootA] != "watching" {
		t.Errorf("healthy watch root state = %q, want watching (all: %v)", got[rootA], got)
	}

	// Failed build (Debounce<=0 → watch.New rejects): the root reports a failure
	// whose wording conveys the backstop still covers it.
	rebuildWatcher(context.Background(), watch.Config{Debounce: 0}, nil, []string{rootA}, watchDied, logger, record)
	if state := got[rootA]; !strings.HasPrefix(state, "failed:") || !strings.Contains(state, "backstop") {
		t.Errorf("failed watch root state = %q, want a failed:… conveying the backstop", state)
	}
}

func writeInto(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitTrigger(t *testing.T, triggers <-chan watch.Trigger, what string) {
	t.Helper()
	select {
	case <-triggers:
	case <-time.After(3 * time.Second):
		t.Fatalf("no trigger for %s within deadline", what)
	}
}

func assertNoTrigger(t *testing.T, triggers <-chan watch.Trigger, window time.Duration, what string) {
	t.Helper()
	select {
	case <-triggers:
		t.Fatalf("unexpected trigger for %s", what)
	case <-time.After(window):
	}
}
