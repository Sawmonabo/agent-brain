package daemon_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// Settings floors are a LoadSettings contract; tests construct the
// struct directly to run fast cycles.
func fastSettings() config.Settings {
	return config.Settings{Sync: config.SyncSettings{
		Ticker:   config.Duration(time.Hour), // ticker quiet; tests drive via watch/manual
		Debounce: config.Duration(50 * time.Millisecond),
		Poll:     config.Duration(0),
	}}
}

func testRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	fake := providertest.New("claude", provider.ScopePerProject, []provider.Pattern{
		{Glob: "memories/**", Class: provider.ClassFact},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if res, err := gitx.Run(context.Background(), dir, args...); err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
}

// provisionMemories sets up Paths under short temp dirs (sun_path limit)
// and a seeded memories checkout with a bare remote, but enrolls NO
// units. It returns the paths and the base temp dir so callers can
// enroll their own units at fresh dirs under base (e.g. the
// enrolled-after-startup watcher test).
func provisionMemories(t *testing.T) (config.Paths, string) {
	t.Helper()
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))

	bare := filepath.Join(base, "remote.git")
	checkout := paths.MemoriesDir()
	mustGit(t, base, "init", "--bare", "-b", "main", bare)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, base, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "daemon-test")
	mustGit(t, checkout, "config", "user.email", "daemon-test@example.invalid")
	if err := repo.WriteAttributes(repo.NewLayout(checkout), testRegistry(t)); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "init: repo skeleton")
	mustGit(t, checkout, "push", "-u", "origin", "main")
	return paths, base
}

// newDaemonEnv provisions Paths under short temp dirs (sun_path limit),
// a seeded memories checkout with a bare remote, and one enrolled unit.
func newDaemonEnv(t *testing.T) (config.Paths, repo.Unit) {
	t.Helper()
	paths, base := provisionMemories(t)
	localDir := filepath.Join(base, "proj", ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	unit := repo.Unit{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: localDir}
	registry := repo.NewLocalRegistry()
	if err := registry.Enroll(unit); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}
	return paths, unit
}

func startDaemon(t *testing.T, paths config.Paths) *api.Client {
	t.Helper()
	d, err := daemon.New(daemon.Config{
		Paths:    paths,
		Settings: fastSettings(),
		Registry: testRegistry(t),
		Version:  "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v on graceful shutdown", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down within 5s")
		}
	})

	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(socketPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Status(context.Background()); err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon API never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDaemonWatchesSyncsAndReports(t *testing.T) {
	paths, unit := newDaemonEnv(t)
	client := startDaemon(t, paths)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.Version != "test" {
		t.Fatalf("status = %+v, want ready/test", status)
	}

	// Run exports the conflict-log path process-wide so the Phase-1 merge
	// driver (a git child of integrate) records retain-both events for the
	// Phase-3 conflicts view (spec §4). Daemon tests are serial (newDaemonEnv
	// uses t.Setenv), so the live daemon's value is deterministic here.
	if got := os.Getenv("AGENT_BRAIN_CONFLICT_LOG"); got != paths.ConflictLogFile() {
		t.Fatalf("AGENT_BRAIN_CONFLICT_LOG = %q, want %q", got, paths.ConflictLogFile())
	}

	// A file written into the enrolled dir must flow through watch →
	// debounce → cycle → commit → push, no manual trigger involved.
	if err := os.MkdirAll(filepath.Join(unit.LocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit.LocalDir, "memories", "fact.md"), []byte("watched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := client.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.LastSync != nil && status.LastSync.Pushed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pushed cycle within deadline; last status %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Manual trigger returns a completed cycle synchronously.
	syncResp, err := client.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if syncResp.Status != "completed" || syncResp.Summary == nil {
		t.Fatalf("sync = %+v, want completed with summary", syncResp)
	}

	projects, err := client.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects.Units) != 1 || projects.Units[0].Folder != "alpha" {
		t.Fatalf("projects = %+v", projects)
	}
}

func TestSecondDaemonRefusesToStart(t *testing.T) {
	paths, _ := newDaemonEnv(t)
	startDaemon(t, paths)

	second, err := daemon.New(daemon.Config{Paths: paths, Settings: fastSettings(), Registry: testRegistry(t), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Run(context.Background()); !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("second Run = %v, want ErrAlreadyRunning", err)
	}
}

func TestDaemonUninitializedRepoIsHonest(t *testing.T) {
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))

	client := startDaemon(t, paths)
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "uninitialized" {
		t.Fatalf("State = %q, want uninitialized", status.State)
	}
	if _, err := client.Sync(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("sync on uninitialized repo: err = %v, want actionable message", err)
	}
}

func TestLogRotationOnStart(t *testing.T) {
	paths, _ := newDaemonEnv(t)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 11<<20)
	if err := os.WriteFile(paths.DaemonLogFile(), big, 0o600); err != nil {
		t.Fatal(err)
	}
	startDaemon(t, paths)
	rotated, err := os.Stat(paths.DaemonLogFile() + ".1")
	if err != nil {
		t.Fatal("oversized log was not rotated:", err)
	}
	if rotated.Size() != int64(len(big)) {
		t.Fatalf("rotated size = %d, want %d", rotated.Size(), len(big))
	}
	current, err := os.Stat(paths.DaemonLogFile())
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() >= int64(len(big)) {
		t.Fatal("fresh log did not start small")
	}
}

// TestWatcherCoversUnitEnrolledAfterStartup is the load-bearing daemon
// test for this task: a unit enrolled AFTER the daemon is up must be
// watched without a restart. The daemon starts with zero units (the
// watcher covers nothing), then a unit is enrolled at a fresh dir and a
// single manual cycle makes the loop observe it and rebuild the watcher.
// A file written into that dir afterward must drive a full cycle — and
// with the ticker parked at 1h and no further manual trigger, only the
// rebuilt watcher can be responsible.
func TestWatcherCoversUnitEnrolledAfterStartup(t *testing.T) {
	paths, base := provisionMemories(t)
	client := startDaemon(t, paths)

	projects, err := client.Projects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects.Units) != 0 {
		t.Fatalf("expected zero units at startup, got %+v", projects.Units)
	}

	// Simulate `track`: enroll a unit at a fresh dir while the daemon runs.
	newLocalDir := filepath.Join(base, "late", ".claude", "memory")
	if err := os.MkdirAll(newLocalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	registry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Enroll(repo.Unit{Provider: "claude", ProjectID: "id-beta", Folder: "beta", LocalDir: newLocalDir}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}

	// One manual cycle makes the loop observe the new unit and rebuild the
	// watcher over its root. The dir has no memory files yet, so this cycle
	// mirrors nothing in; its timestamp anchors the post-write assertion.
	syncResp, err := client.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if syncResp.Summary == nil {
		t.Fatalf("manual sync = %+v, want a completed summary", syncResp)
	}
	if syncResp.Summary.MirrorIn.Copied != 0 {
		t.Fatalf("manual sync mirrored in %d files, want 0 (no memory files written yet)", syncResp.Summary.MirrorIn.Copied)
	}
	baselineAt := syncResp.Summary.At

	// Write into the freshly-watched root. With the ticker at 1h and no
	// further manual trigger, only the rebuilt watcher can drive a new
	// cycle that mirrors this file in.
	if err := os.MkdirAll(filepath.Join(newLocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newLocalDir, "memories", "fact.md"), []byte("late-enrolled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := client.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.LastSync != nil && status.LastSync.At.After(baselineAt) && status.LastSync.MirrorIn.Copied > 0 {
			break // a new, watcher-driven cycle mirrored the file in
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher never covered the late-enrolled root; last status %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
