package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"charm.land/huh/v2"
	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
)

// withFakeChezmoiOnPath prepends a directory containing an executable
// named "chezmoi" (a tiny shell script) to PATH, so runMigratePreflight's
// exec.CommandContext("chezmoi", ...) finds this fake instead of any real
// chezmoi binary — tests must never invoke real chezmoi or read the real
// home (binding context E).
func withFakeChezmoiOnPath(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "chezmoi")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// --- runMigratePreflight ---

// defaultTestPreflightTimeout stands in for config.DefaultSettings()'s
// migrate.preflight_timeout (30s) wherever a test doesn't care about the
// timeout value itself — only TestRunMigratePreflightHonorsConfiguredTimeout
// exercises the deadline directly.
const defaultTestPreflightTimeout = 30 * time.Second

func TestRunMigratePreflightPassesWhenConfigAbsent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "chezmoi.toml") // never created
	if err := runMigratePreflight(context.Background(), configPath, defaultTestPreflightTimeout); err != nil {
		t.Fatalf("runMigratePreflight with absent config: %v", err)
	}
}

func TestRunMigratePreflightPassesOnEmptyDiff(t *testing.T) {
	withFakeChezmoiOnPath(t, `exit 0`) // no stdout output = empty diff
	configPath := filepath.Join(t.TempDir(), "chezmoi.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMigratePreflight(context.Background(), configPath, defaultTestPreflightTimeout); err != nil {
		t.Fatalf("runMigratePreflight with empty diff: %v", err)
	}
}

func TestRunMigratePreflightRefusesOnNonEmptyDiff(t *testing.T) {
	withFakeChezmoiOnPath(t, `echo "some diff output"; exit 0`)
	configPath := filepath.Join(t.TempDir(), "chezmoi.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runMigratePreflight(context.Background(), configPath, defaultTestPreflightTimeout)
	if err == nil {
		t.Fatal("runMigratePreflight: want refusal on non-empty diff")
	}
	if !strings.Contains(err.Error(), "adjudicate") {
		t.Fatalf("error must give adjudication instructions: %v", err)
	}
}

func TestRunMigratePreflightRefusesWhenChezmoiBinaryMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no chezmoi anywhere on PATH
	configPath := filepath.Join(t.TempDir(), "chezmoi.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runMigratePreflight(context.Background(), configPath, defaultTestPreflightTimeout); err == nil {
		t.Fatal("runMigratePreflight: want refusal when chezmoi binary is missing")
	}
}

// TestRunMigratePreflightHonorsConfiguredTimeout proves the timeout
// argument — not a hardcoded const — bounds the chezmoi subprocess even
// when the child forks helpers (real chezmoi shells out to git): the fake
// backgrounds a long-lived grandchild that inherits the stdout pipe, so a
// naive single-PID kill would leave the pipe held and Wait blocked for
// the grandchild's full lifetime. The call must return well before that,
// and the process-group kill must take the grandchild down with it
// (spec §10; a cold NFS home or a huge legacy tree must be able to raise
// the timeout past the old fixed 30s via config.MigrateSettings).
func TestRunMigratePreflightHonorsConfiguredTimeout(t *testing.T) {
	grandchildPIDPath := filepath.Join(t.TempDir(), "grandchild.pid")
	// The 1s timeout dwarfs the fake's startup (~10ms) so the PID file is
	// written before the kill fires even on a badly starved runner, and the
	// 30s sleeps dwarf every bound below so neither ends by natural exit.
	withFakeChezmoiOnPath(t, fmt.Sprintf(`sleep 30 & echo $! > %q
sleep 30; exit 0`, grandchildPIDPath))
	configPath := filepath.Join(t.TempDir(), "chezmoi.toml")
	if err := os.WriteFile(configPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err := runMigratePreflight(context.Background(), configPath, time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runMigratePreflight: want a timeout error, got nil")
	}
	// Generous ceiling: timeout (1s) + WaitDelay (2s) + heavy-runner
	// scheduling slack still land far under 5s; only an unbounded wait for
	// the 30s grandchild can breach it.
	if elapsed >= 5*time.Second {
		t.Fatalf("runMigratePreflight took %v to return — the configured 1s timeout did not bound the call (fake chezmoi's grandchild sleeps 30s)", elapsed)
	}

	pidBytes, err := os.ReadFile(grandchildPIDPath)
	if err != nil {
		t.Fatalf("fake chezmoi never wrote its grandchild PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("grandchild PID file content %q: %v", pidBytes, err)
	}
	// The process-group kill must reap the grandchild, not just chezmoi
	// itself — poll briefly, since SIGKILL delivery is asynchronous.
	// syscall.Kill(pid, 0) probes liveness: ESRCH means the grandchild is
	// gone.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild (pid %d) survived the preflight timeout — the process-group kill did not reach it", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMigrateCommandRunEHonorsConfiguredPreflightTimeout closes a
// gap: every other preflight-timeout test drives runMigratePreflight
// directly, which pins that FUNCTION's behavior but never proves the
// command's own RunE actually reads config.toml's [migrate] table and
// threads it through — a regression that hardcoded some other value (or
// silently dropped the LoadSettings call) would slip past those tests
// undetected. This drives the real `migrate` command end-to-end via
// runCmd: a config.toml with preflight_timeout = "50ms" plus a
// chezmoi.toml (so the gate actually shells out) and a fake chezmoi that
// sleeps 2s must make the command return well before that sleep elapses.
func TestMigrateCommandRunEHonorsConfiguredPreflightTimeout(t *testing.T) {
	configDir := t.TempDir()
	// t.Setenv forbids t.Parallel (runCmd's own doc comment) — belt and
	// suspenders, also isolates os.UserHomeDir() (buildTrackDeps calls it
	// directly) from the real machine's home.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	t.Setenv("AGENT_BRAIN_DATA_DIR", t.TempDir())
	// A short runtime dir, NOT t.TempDir(): that embeds this test's full
	// (long) name in the path, which trips config.ValidateSocketPath's
	// ~104-byte unix-socket cap before newAPIClient ever returns — the
	// command would then fail at socket-path validation, before the
	// preflight-timeout code this test targets ever runs, and the
	// assertions below would pass vacuously for the wrong reason.
	runtimeDir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", runtimeDir)

	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[migrate]\npreflight_timeout = \"50ms\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// runMigratePreflight only shells out when chezmoi.toml exists.
	if err := os.WriteFile(filepath.Join(configDir, "chezmoi.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	withFakeChezmoiOnPath(t, `sleep 10; exit 0`)

	start := time.Now()
	_, err = runCmd(t, nil, "migrate")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("migrate: want the preflight timeout error, got nil")
	}
	// Generous ceiling (same rationale as the function-level test): the
	// 50ms timeout plus WaitDelay plus scheduling slack stay far under 5s;
	// only waiting out the fake's full 10s sleep can breach it.
	if elapsed >= 5*time.Second {
		t.Fatalf("migrate took %v to return — the configured 50ms preflight_timeout in config.toml did not bound the call (fake chezmoi sleeps 10s)", elapsed)
	}
}

func TestPrintSkipPreflightWarningCitesSpecSection(t *testing.T) {
	var out bytes.Buffer
	if err := printSkipPreflightWarning(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "§10") {
		t.Fatalf("warning must cite spec §10: %s", out.String())
	}
	if !strings.Contains(out.String(), "WARNING") {
		t.Fatalf("warning must be visually distinct: %s", out.String())
	}
}

// --- enumerateLegacySlugs / hasRealContent ---

func TestEnumerateLegacySlugsSkipsNonDirsAndSortsDeterministically(t *testing.T) {
	root := t.TempDir()
	for _, slug := range []string{"-Users-u-dev-beta", "-Users-u-dev-alpha"} {
		if err := os.MkdirAll(filepath.Join(root, slug), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "stray-file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	slugs, err := enumerateLegacySlugs(root)
	if err != nil {
		t.Fatalf("enumerateLegacySlugs: %v", err)
	}
	want := []string{"-Users-u-dev-alpha", "-Users-u-dev-beta"}
	if diff := cmp.Diff(want, slugs); diff != "" {
		t.Fatalf("enumerateLegacySlugs (-want +got):\n%s", diff)
	}
}

func TestEnumerateLegacySlugsOnMissingRootIsEmptyNotError(t *testing.T) {
	slugs, err := enumerateLegacySlugs(filepath.Join(t.TempDir(), "nonexistent"))
	if err != nil {
		t.Fatalf("enumerateLegacySlugs: %v", err)
	}
	if len(slugs) != 0 {
		t.Fatalf("slugs = %v, want none", slugs)
	}
}

func TestHasRealContentIgnoresLockAndSyncPendingDroppings(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".lock", "session.sync-pending"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	found, err := hasRealContent(dir)
	if err != nil {
		t.Fatalf("hasRealContent: %v", err)
	}
	if found {
		t.Fatal("hasRealContent = true for a dir with only droppings")
	}

	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("# notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err = hasRealContent(dir)
	if err != nil {
		t.Fatalf("hasRealContent: %v", err)
	}
	if !found {
		t.Fatal("hasRealContent = false with a real file present")
	}
}

// --- runMigrate ---

// startFakeDaemonForMigrate serves /v0/status, /v0/migrate (recording every
// request and replying with respondWith's return value), and /v0/sync.
func startFakeDaemonForMigrate(t *testing.T, respondWith func(api.MigrateRequest) api.MigrateResponse) func() []api.MigrateRequest {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var mu sync.Mutex
	var requests []api.MigrateRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/migrate", func(w http.ResponseWriter, r *http.Request) {
		var req api.MigrateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(respondWith(req))
	})
	mux.HandleFunc("/v0/sync", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{Pushed: true}})
	})
	listener, err := net.Listen("unix", filepath.Join(dir, "agent-brain.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
	return func() []api.MigrateRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.MigrateRequest(nil), requests...)
	}
}

func TestRunMigrateNothingToMigrateWhenLegacyRootAbsent(t *testing.T) {
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, home: t.TempDir()} // no ~/.agent-brain at all
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, nil, migrateCallbacks{}, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunMigrateNothingToMigrateWhenOnlyDroppingsFound(t *testing.T) {
	home := t.TempDir()
	legacyDir := filepath.Join(legacyRoot(home), "-Users-u-dev-alpha")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, ".lock"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, home: home}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, nil, migrateCallbacks{}, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to migrate") {
		t.Fatalf("output: %s", out.String())
	}
}

// TestRunMigrateSubmitsRequestWithSeedDirDistinctFromLocalDir is the core
// spec §10 assertion: the daemon must seed from the legacy tree while
// enrolling the LIVE claude memory dir, so the overlay lands as a second
// commit over the seed (engine/admin.go's SeedProject + Track ordering).
func TestRunMigrateSubmitsRequestWithSeedDirDistinctFromLocalDir(t *testing.T) {
	home := t.TempDir()
	slug := "-Users-u-dev-alpha"
	legacyDir := filepath.Join(legacyRoot(home), slug)
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "MEMORY.md"), []byte("# alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject, identity: provider.Identity{ProjectID: "github.com/u/alpha", PreferredFolder: "alpha"}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForMigrate(t, func(api.MigrateRequest) api.MigrateResponse {
		return api.MigrateResponse{Folder: "alpha", Files: 1}
	})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	callbacks := migrateCallbacks{
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(hint string) (string, error) { return hint, nil },
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, client, callbacks, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}

	requests := getRequests()
	if len(requests) != 1 {
		t.Fatalf("MigrateRequests = %+v, want exactly one", requests)
	}
	req := requests[0]
	if req.SeedDir != legacyDir {
		t.Fatalf("SeedDir = %q, want %q", req.SeedDir, legacyDir)
	}
	if req.Slug != slug {
		t.Fatalf("Slug = %q, want %q", req.Slug, slug)
	}
	if req.LocalDir == req.SeedDir {
		t.Fatalf("LocalDir must differ from SeedDir (live dir vs legacy seed): both are %q", req.LocalDir)
	}
	// The confirmed path is claude.GuessPath(slug, <real os.Stat>) unchanged
	// (the test's confirmProjectPath echoes the guess) — compute the
	// expected LocalDir via the exact same functions migrateOne uses.
	statDir := func(p string) bool { info, err := os.Stat(p); return err == nil && info.IsDir() }
	confirmedPath := claude.GuessPath(slug, statDir)
	wantLocalDir := claude.MemoryDirFor(home, confirmedPath)
	if req.LocalDir != wantLocalDir {
		t.Fatalf("LocalDir = %q, want %q", req.LocalDir, wantLocalDir)
	}
	if !strings.Contains(out.String(), "seeded 1 file") {
		t.Fatalf("output missing seeded-files line: %s", out.String())
	}
	if !strings.Contains(out.String(), "§10") {
		t.Fatalf("output missing retirement pointer: %s", out.String())
	}
}

// TestRunMigrateCancelStopsRunWithoutSkippingAhead proves cancelling one
// slug's confirm-path prompt stops the run there — it must NOT skip just
// that slug and continue to the next (unlike track's discovery loop): a
// later slug that would sort after the cancelled one must never even be
// offered a prompt.
func TestRunMigrateCancelStopsRunWithoutSkippingAhead(t *testing.T) {
	home := t.TempDir()
	slugs := []string{"-Users-u-dev-alpha", "-Users-u-dev-beta", "-Users-u-dev-gamma"}
	for _, slug := range slugs {
		legacyDir := filepath.Join(legacyRoot(home), slug)
		if err := os.MkdirAll(legacyDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDir, "MEMORY.md"), []byte("# "+slug), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject, identity: provider.Identity{ProjectID: "github.com/u/alpha", PreferredFolder: "alpha"}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForMigrate(t, func(api.MigrateRequest) api.MigrateResponse {
		return api.MigrateResponse{Folder: "alpha", Files: 1}
	})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	var confirmCalls int
	callbacks := migrateCallbacks{
		confirmProjectPath: func(guess string) (string, error) {
			confirmCalls++
			switch confirmCalls {
			case 1:
				return guess, nil // alpha: succeeds
			case 2:
				return "", huh.ErrUserAborted // beta: cancelled
			default:
				t.Fatal("confirmProjectPath called a third time; the run must stop at the cancelled slug, not skip ahead to gamma")
				return "", nil
			}
		},
		nameRemotelessFolder: func(hint string) (string, error) { return hint, nil },
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, client, callbacks, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}

	if !strings.Contains(out.String(), "migrate: cancelled at -Users-u-dev-beta") {
		t.Fatalf("output missing the cancel message naming the slug it stopped at: %s", out.String())
	}
	if strings.Contains(out.String(), "retirement checklist") {
		t.Fatal("a cancelled (incomplete) run must not print the full-completion retirement message")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].Slug != "-Users-u-dev-alpha" {
		t.Fatalf("MigrateRequests = %+v, want exactly one, for alpha (beta cancelled, gamma never reached)", requests)
	}
}

// TestRunMigrateCancelOnFirstSlugSkipsSyncAndNothingToMigrateMessage proves
// two adjacent messages never both fire: a cancel is not "nothing to
// migrate" (that phrasing is reserved for the all-droppings case), and with
// nothing yet migrated there is nothing worth an explicit sync for.
func TestRunMigrateCancelOnFirstSlugSkipsSyncAndNothingToMigrateMessage(t *testing.T) {
	home := t.TempDir()
	slug := "-Users-u-dev-alpha"
	legacyDir := filepath.Join(legacyRoot(home), slug)
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "MEMORY.md"), []byte("# alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	startFakeDaemonForMigrate(t, func(api.MigrateRequest) api.MigrateResponse {
		t.Fatal("daemon Migrate must not be called when the only slug was cancelled")
		return api.MigrateResponse{}
	})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	callbacks := migrateCallbacks{
		confirmProjectPath: func(string) (string, error) { return "", huh.ErrUserAborted },
		nameRemotelessFolder: func(string) (string, error) {
			t.Fatal("nameRemotelessFolder must not be called after confirmProjectPath was cancelled")
			return "", nil
		},
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, client, callbacks, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !strings.Contains(out.String(), "migrate: cancelled at -Users-u-dev-alpha") {
		t.Fatalf("output missing cancel message: %s", out.String())
	}
	if strings.Contains(out.String(), "nothing to migrate") {
		t.Fatal(`a cancelled run must not also claim "nothing to migrate" — that message is reserved for the all-droppings case`)
	}
	if strings.Contains(out.String(), "sync completed") {
		t.Fatal("a cancel with nothing migrated yet must not trigger a sync")
	}
}

func TestRunMigrateReportsAlreadyImportedSkip(t *testing.T) {
	home := t.TempDir()
	slug := "-Users-u-dev-alpha"
	legacyDir := filepath.Join(legacyRoot(home), slug)
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "MEMORY.md"), []byte("# alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject, identity: provider.Identity{ProjectID: "github.com/u/alpha", PreferredFolder: "alpha"}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	startFakeDaemonForMigrate(t, func(api.MigrateRequest) api.MigrateResponse {
		return api.MigrateResponse{Folder: "alpha", Skipped: true}
	})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	callbacks := migrateCallbacks{
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(hint string) (string, error) { return hint, nil },
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, client, callbacks, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !strings.Contains(out.String(), "already imported") || !strings.Contains(out.String(), "skipped") {
		t.Fatalf("output: %s", out.String())
	}
}

// TestRunMigrateNamesRemotelessProjectViaPrompt proves migrate NEVER skips
// a remoteless legacy project the way track --all does — every legacy
// project must be accounted for, so the folder-name prompt always fires.
func TestRunMigrateNamesRemotelessProjectViaPrompt(t *testing.T) {
	home := t.TempDir()
	slug := "-Users-u-dev-remoteless"
	legacyDir := filepath.Join(legacyRoot(home), slug)
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "MEMORY.md"), []byte("# remoteless"), 0o644); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject, identity: provider.Identity{}} // remoteless
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForMigrate(t, func(api.MigrateRequest) api.MigrateResponse {
		return api.MigrateResponse{Folder: "chosen-name", Files: 1}
	})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	var nameRemotelessFolderCalled bool
	callbacks := migrateCallbacks{
		confirmProjectPath: func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(_ string) (string, error) {
			nameRemotelessFolderCalled = true
			return "chosen-name", nil
		},
	}
	var out bytes.Buffer
	if err := runMigrate(context.Background(), deps, client, callbacks, &out); err != nil {
		t.Fatalf("runMigrate: %v", err)
	}
	if !nameRemotelessFolderCalled {
		t.Fatal("nameRemotelessFolder was not called for a remoteless legacy project")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].ProjectID != "named/chosen-name" {
		t.Fatalf("MigrateRequests = %+v, want ProjectID named/chosen-name", requests)
	}
}

func TestMigrateCommandHasSkipPreflightAndYesFlags(t *testing.T) {
	cmd := newMigrateCmd()
	for _, name := range []string{"skip-preflight", "yes"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Fatalf("migrate command missing --%s flag", name)
		}
	}
}
