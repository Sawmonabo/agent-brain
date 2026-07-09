package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// testBinaryPath is a REAL, freshly built agent-brain binary (see TestMain).
// Every fixture that wires filter.agentbrain.clean/smudge (gitx.InstallFilters)
// must point it at THIS, never at os.Executable(). Inside a test process,
// os.Executable() IS the compiled daemon.test binary — wiring a git filter
// at it means git invokes daemon.test as its own clean/smudge driver. A Go
// test binary given an unrecognized positional arg ("git-clean") does not
// error; it falls through to running the whole suite again, and with no
// -test.timeout (only `go test` injects that — a git-spawned subprocess
// bypasses it entirely), each nested run reinstalls filters pointing at
// itself and recurses without bound. That is what happened on 2026-07-08:
// ~70GB of nested `go test` processes and a hard reboot. testBinaryPath
// removes the cause; TestMain's tripwire below is the backstop that turns
// any recurrence into one loud, immediate failure instead of a repeat.
var testBinaryPath string

// TestMain's FIRST action, before the testing package's own flag parsing or
// m.Run(), must be the tripwire above: a git filter invocation would arrive
// as a bare positional arg, which nothing else in this file inspects this
// early. See testBinaryPath's doc comment for the incident this prevents.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "git-clean", "git-smudge", "git-textconv", "git-merge":
			fmt.Fprintln(os.Stderr, "daemon.test invoked as a git filter — a fixture wired filter config at the test binary; see the 2026-07-08 fork-bomb incident (testBinaryPath's doc comment, this file)")
			os.Exit(1)
		}
	}
	os.Exit(testMain(m))
}

// testMain builds the real binary testBinaryPath points at, then runs the
// suite. Building once per package-test-run (not per fixture) keeps every
// daemon test's filter wiring pointed at the same real binary at near-zero
// added cost.
func testMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "agent-brain-daemon-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(root) }()

	testBinaryPath = filepath.Join(root, "agent-brain")
	build := exec.Command("go", "build", "-o", testBinaryPath, "../../cmd/agent-brain")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		return 1
	}

	return m.Run()
}

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

// gitOut runs git and returns its trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	res, err := gitx.Run(context.Background(), dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v\nstderr: %s", args, err, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

// commitCount is the number of commits reachable from HEAD in checkout.
func commitCount(t *testing.T, checkout string) int {
	t.Helper()
	n, err := strconv.Atoi(gitOut(t, checkout, "rev-list", "--count", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return n
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
	// The git-spawned filter/merge subprocess (testBinaryPath) is a separate
	// real process — it resolves its own keyset location via
	// config.DefaultPaths(), inheriting only the environment, not this
	// test's paths variable. AGENT_BRAIN_CONFIG_DIR is how it finds THIS
	// fixture's keyset rather than falling through to a real one.
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", paths.ConfigDir)

	bare := filepath.Join(base, "remote.git")
	checkout := paths.MemoriesDir()
	mustGit(t, base, "init", "--bare", "-b", "main", bare)
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mustGit(t, base, "clone", bare, checkout)
	mustGit(t, checkout, "config", "user.name", "daemon-test")
	mustGit(t, checkout, "config", "user.email", "daemon-test@example.invalid")

	// checkoutState is doctor.SafetyGate (spec §5): every fixture needs a
	// real keyset and filter wiring, or every daemon test would find the
	// machine "uninitialized" regardless of what it actually exercises.
	// binaryPath is testBinaryPath (see its doc comment) — NEVER
	// os.Executable(), which inside this test process is daemon.test itself.
	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallFilters(context.Background(), checkout, testBinaryPath); err != nil {
		t.Fatal(err)
	}

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
		Paths:      paths,
		Settings:   fastSettings(),
		Registry:   testRegistry(t),
		Version:    "test",
		BinaryPath: testBinaryPath,
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
	syncResp, err := client.Sync(context.Background(), "")
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

	second, err := daemon.New(daemon.Config{Paths: paths, Settings: fastSettings(), Registry: testRegistry(t), Version: "test", BinaryPath: testBinaryPath})
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
	if _, err := client.Sync(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "not initialized") {
		t.Fatalf("sync on uninitialized repo: err = %v, want actionable message", err)
	}
}

// TestDaemonStateDetailNamesBrokenAxisAndClearsOnHeal exercises StateDetail
// (api.StatusResponse) end to end: a machine that goes bad WHILE the daemon
// is running must have the broken axis visible to the next Status() caller,
// not just embedded in the error the probe that discovered it received. That
// is what refreshState (daemon.go) exists for — every checkoutState call
// site persists what it saw before acting on it.
func TestDaemonStateDetailNamesBrokenAxisAndClearsOnHeal(t *testing.T) {
	paths, _ := newDaemonEnv(t)
	client := startDaemon(t, paths)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.StateDetail != "" {
		t.Fatalf("status = %+v, want ready with empty StateDetail", status)
	}

	attributesFile := repo.NewLayout(paths.MemoriesDir()).AttributesFile()
	if err := os.WriteFile(attributesFile, []byte("corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The sync attempt is what evaluates the gate (TriggerSync ->
	// refreshState) and discovers the breakage; its error names the axis...
	if _, err := client.Sync(context.Background(), ""); err == nil ||
		!strings.Contains(err.Error(), "attributes") {
		t.Fatalf("sync on corrupted attributes: err = %v, want a message naming attributes", err)
	}

	// ...and a Status() call that follows, with no sync in between, must
	// report the SAME finding — proving refreshState persisted it rather
	// than the caller only learning it from the returned error.
	status, err = client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "uninitialized" || !strings.Contains(status.StateDetail, "attributes") {
		t.Fatalf("status = %+v, want uninitialized with StateDetail naming attributes", status)
	}

	if err := repo.WriteAttributes(repo.NewLayout(paths.MemoriesDir()), testRegistry(t)); err != nil {
		t.Fatal(err)
	}

	syncResp, err := client.Sync(context.Background(), "")
	if err != nil {
		t.Fatalf("sync after healing attributes: %v", err)
	}
	if syncResp.Status != "completed" {
		t.Fatalf("sync after heal = %+v, want completed", syncResp)
	}

	status, err = client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "ready" || status.StateDetail != "" {
		t.Fatalf("status after heal = %+v, want ready with empty StateDetail", status)
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
	syncResp, err := client.Sync(context.Background(), "")
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

// TestTrackEnrollsCommitsAndSyncs is the load-bearing composition test: a
// client.Track lands the enrollment through the ONE engine goroutine
// (projects.toml committed in the checkout, local registry gains the unit),
// and the post-track cycle rebuilds the watcher so a later touch syncs with
// no manual trigger (composition with Task 6's rebuild-on-diff).
func TestTrackEnrollsCommitsAndSyncs(t *testing.T) {
	paths, base := provisionMemories(t)
	client := startDaemon(t, paths)
	checkout := paths.MemoriesDir()

	localDir := filepath.Join(base, "tracked", ".claude", "memory")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, checkout)

	resp, err := client.Track(context.Background(), api.TrackRequest{
		Provider: "claude", ProjectID: "id-tracked", PreferredFolder: "tracked", LocalDir: localDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Folder != "tracked" {
		t.Fatalf("folder = %q, want tracked", resp.Folder)
	}

	// projects.toml was committed inside the checkout by the daemon.
	if got := commitCount(t, checkout); got <= before {
		t.Fatalf("commit count did not grow: %d → %d", before, got)
	}
	projects, err := repo.LoadProjects(repo.NewLayout(checkout).ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := projects.FolderFor("id-tracked"); f != "tracked" {
		t.Fatalf("projects.toml id-tracked → %q, want tracked", f)
	}
	// The local registry gained the unit.
	registry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Units) != 1 || registry.Units[0].Folder != "tracked" {
		t.Fatalf("local registry = %+v, want one tracked unit", registry.Units)
	}

	// Track replies before its post-track cycle (brief: reply, then cycle), so
	// a manual sync is the barrier that guarantees the watcher rebuild
	// completed before the write below — otherwise the write races the
	// rebuild and its fsnotify event is lost.
	if _, err := client.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	// A touch in the freshly-tracked dir syncs with no manual trigger — only
	// the watcher the post-track cycle rebuilt can drive this.
	if err := os.MkdirAll(filepath.Join(localDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "memories", "fact.md"), []byte("tracked fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, err := client.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if status.LastSync != nil && status.LastSync.MirrorIn.Copied > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("tracked dir never synced; last status %+v", status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestUntrackPurgesFolderAndEntry pins the purge path: untrack with Purge
// removes the local enrollment AND (this machine being the last tracker) the
// checkout folder plus its projects.toml entry.
func TestUntrackPurgesFolderAndEntry(t *testing.T) {
	paths, base := provisionMemories(t)
	client := startDaemon(t, paths)
	checkout := paths.MemoriesDir()

	localDir := filepath.Join(base, "doomed", ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(localDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "memories", "fact.md"), []byte("doomed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracked, err := client.Track(context.Background(), api.TrackRequest{
		Provider: "claude", ProjectID: "id-doomed", PreferredFolder: "doomed", LocalDir: localDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A full cycle commits the folder content so the purge has tracked files.
	if _, err := client.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if gitOut(t, checkout, "ls-files", tracked.Folder) == "" {
		t.Fatalf("folder %q has no tracked files before purge", tracked.Folder)
	}

	resp, err := client.Untrack(context.Background(), api.UntrackRequest{
		Provider: "claude", LocalDir: localDir, Purge: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Removed || !resp.Purged {
		t.Fatalf("untrack resp = %+v, want removed+purged", resp)
	}
	if out := gitOut(t, checkout, "ls-files", tracked.Folder); out != "" {
		t.Fatalf("git ls-files %s = %q, want empty after purge", tracked.Folder, out)
	}
	projects, err := repo.LoadProjects(repo.NewLayout(checkout).ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := projects.FolderFor("id-doomed"); ok {
		t.Fatal("projects.toml still records id-doomed after purge")
	}
	registry, err := repo.LoadLocalRegistry(paths.LocalRegistryFile())
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Units) != 0 {
		t.Fatalf("local registry still has units after untrack: %+v", registry.Units)
	}
}

// TestMigrateSeedsBeforeLiveOverlay pins spec §10's layering: the seed commit
// must precede the live-overlay memory commit, so live state overlays the
// imported bash-era tree rather than the other way round.
func TestMigrateSeedsBeforeLiveOverlay(t *testing.T) {
	paths, base := provisionMemories(t)
	client := startDaemon(t, paths)
	checkout := paths.MemoriesDir()

	seedDir := filepath.Join(base, "legacy", "my-project")
	if err := os.MkdirAll(seedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "old.md"), []byte("legacy fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	liveDir := filepath.Join(base, "live", ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(liveDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "memories", "new.md"), []byte("live fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := client.Migrate(context.Background(), api.MigrateRequest{
		Provider: "claude", ProjectID: "id-mig", PreferredFolder: "mig",
		LocalDir: liveDir, Slug: "my-project", SeedDir: seedDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Folder != "mig" || resp.Files != 1 || resp.Skipped {
		t.Fatalf("migrate resp = %+v, want folder mig / 1 file / not skipped", resp)
	}
	// Drive a full cycle so the live overlay lands after the seed commit.
	if _, err := client.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	// Oldest-first log: seed commit index must precede the overlay memory commit.
	subjects := strings.Split(gitOut(t, checkout, "log", "--reverse", "--format=%s"), "\n")
	seedIdx, overlayIdx := -1, -1
	for i, subject := range subjects {
		switch {
		case strings.HasPrefix(subject, "migrate: seed mig from "):
			seedIdx = i
		case strings.HasPrefix(subject, "memory: ") && strings.Contains(subject, " mig "):
			overlayIdx = i
		}
	}
	if seedIdx < 0 {
		t.Fatalf("no seed commit in log:\n%s", strings.Join(subjects, "\n"))
	}
	if overlayIdx < 0 {
		t.Fatalf("no live-overlay memory commit in log:\n%s", strings.Join(subjects, "\n"))
	}
	if seedIdx >= overlayIdx {
		t.Fatalf("seed (idx %d) must precede live overlay (idx %d):\n%s", seedIdx, overlayIdx, strings.Join(subjects, "\n"))
	}
	// The migration merged both layers into the live dir.
	if _, err := os.Stat(filepath.Join(liveDir, "old.md")); err != nil {
		t.Fatalf("seed layer did not reach the live dir: %v", err)
	}
}

// TestSyncProjectFilterScopesCycleAndRejectsUnknown pins `sync --project`:
// a named folder scopes the triggered cycle to that folder's units (the
// others are NOT mirrored in), an unknown folder is a synchronous error that
// names the enrolled folders (a 400), and a following whole-fleet sync still
// covers everything — the filter never shrinks the fleet. Units are enrolled
// BEFORE startup (not via Track) so no post-track cycle runs and the only
// cycles are the manual syncs below, making the assertions deterministic.
func TestSyncProjectFilterScopesCycleAndRejectsUnknown(t *testing.T) {
	paths, base := provisionMemories(t)
	checkout := paths.MemoriesDir()

	alphaDir := filepath.Join(base, "alpha-proj", ".claude", "memory")
	betaDir := filepath.Join(base, "beta-proj", ".claude", "memory")
	for _, dir := range []string{alphaDir, betaDir} {
		if err := os.MkdirAll(filepath.Join(dir, "memories"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(alphaDir, "memories", "a.md"), []byte("alpha fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(betaDir, "memories", "b.md"), []byte("beta fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := repo.NewLocalRegistry()
	for _, unit := range []repo.Unit{
		{Provider: "claude", ProjectID: "id-alpha", Folder: "alpha", LocalDir: alphaDir},
		{Provider: "claude", ProjectID: "id-beta", Folder: "beta", LocalDir: betaDir},
	} {
		if err := registry.Enroll(unit); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}

	client := startDaemon(t, paths)

	// Filtered sync: only alpha's unit is mirrored in.
	resp, err := client.Sync(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Summary == nil || resp.Summary.MirrorIn.Copied != 1 {
		t.Fatalf("filtered sync = %+v, want exactly one file mirrored in (alpha only)", resp.Summary)
	}
	if out := gitOut(t, checkout, "ls-files", "alpha"); out == "" {
		t.Fatal("alpha folder has no tracked files after its filtered sync")
	}
	if out := gitOut(t, checkout, "ls-files", "beta"); out != "" {
		t.Fatalf("beta was synced despite --project alpha: ls-files = %q", out)
	}

	// An unknown folder is a synchronous error naming the enrolled folders.
	if _, err := client.Sync(context.Background(), "ghost"); err == nil ||
		!strings.Contains(err.Error(), "unknown folder") || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("unknown --project: err = %v, want an error naming enrolled folders", err)
	}

	// Whole-fleet sync still covers beta — the filter never shrank the fleet.
	if _, err := client.Sync(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if out := gitOut(t, checkout, "ls-files", "beta"); out == "" {
		t.Fatal("beta was not synced by the whole-fleet cycle")
	}
}

// TestAdminOpsRequireInitializedRepo pins the state gate: track/untrack/
// migrate against an uninitialized checkout return the same actionable error
// as sync (mapped to a 500).
func TestAdminOpsRequireInitializedRepo(t *testing.T) {
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))
	client := startDaemon(t, paths)

	_, trackErr := client.Track(context.Background(), api.TrackRequest{Provider: "claude", ProjectID: "x", PreferredFolder: "x", LocalDir: base})
	_, untrackErr := client.Untrack(context.Background(), api.UntrackRequest{Provider: "claude", LocalDir: base})
	_, migrateErr := client.Migrate(context.Background(), api.MigrateRequest{Provider: "claude", ProjectID: "x", PreferredFolder: "x", LocalDir: base, Slug: "s", SeedDir: base})
	for name, err := range map[string]error{"track": trackErr, "untrack": untrackErr, "migrate": migrateErr} {
		if err == nil || !strings.Contains(err.Error(), "not initialized") {
			t.Fatalf("%s on uninitialized repo: err = %v, want actionable message", name, err)
		}
	}
}
