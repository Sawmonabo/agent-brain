package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/huh/v2"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/claude"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// allowAllCallbacks is the "say yes to everything" enrollCallbacks a test
// uses when the scenario under test isn't about the picker/confirm
// decisions themselves.
func allowAllCallbacks() enrollCallbacks {
	return enrollCallbacks{
		pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
			indices := make([]int, len(candidates))
			for i := range candidates {
				indices[i] = i
			}
			return indices, nil
		},
		confirmProjectPath:   func(guess string) (string, error) { return guess, nil },
		nameRemotelessFolder: func(hint string) (string, error) { return hint, nil },
	}
}

func TestTrackUntrackMigrateCommandsAreRegistered(t *testing.T) {
	root := Root()
	for _, name := range []string{"track", "untrack", "migrate"} {
		cmd, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%q): %v", name, err)
		}
		if cmd.Name() != name {
			t.Fatalf("Find(%q) returned %q", name, cmd.Name())
		}
	}
}

// --- runTrackDiscover ---

func TestRunTrackDiscoverEnrollsChosenCandidate(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"}},
		identity:   provider.Identity{ProjectID: "github.com/u/project-a", PreferredFolder: "project-a"},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-a" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	var out bytes.Buffer
	enrolledAny, err := runTrackDiscover(context.Background(), deps, client, allowAllCallbacks(), &out)
	if err != nil {
		t.Fatalf("runTrackDiscover: %v", err)
	}
	if !enrolledAny {
		t.Fatal("enrolledAny = false, want true")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].ProjectID != "github.com/u/project-a" {
		t.Fatalf("TrackRequests = %+v", requests)
	}
}

func TestRunTrackDiscoverNoCandidatesPrintsNothingDiscovered(t *testing.T) {
	registry, err := provider.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	var out bytes.Buffer
	enrolledAny, err := runTrackDiscover(context.Background(), deps, nil, allowAllCallbacks(), &out)
	if err != nil {
		t.Fatalf("runTrackDiscover: %v", err)
	}
	if enrolledAny {
		t.Fatal("enrolledAny = true, want false")
	}
	if !strings.Contains(out.String(), "no new memory roots") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunTrackDiscoverAllModeSkipsRemotelessWithWarning(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/remoteless/.claude/memory", Label: "remoteless", PathGuess: "/tmp/remoteless"}},
		identity:   provider.Identity{}, // empty ProjectID => remoteless
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "x" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	callbacks := resolveEnrollCallbacks("all", false, true)
	var out bytes.Buffer
	enrolledAny, err := runTrackDiscover(context.Background(), deps, client, callbacks, &out)
	if err != nil {
		t.Fatalf("runTrackDiscover: %v", err)
	}
	if enrolledAny {
		t.Fatal("enrolledAny = true, want false (remoteless was skipped)")
	}
	if len(getRequests()) != 0 {
		t.Fatalf("track --all must not enroll a remoteless project without a name: got %+v", getRequests())
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Fatalf("output missing skip warning: %s", out.String())
	}
}

// TestRunTrackDiscoverCancelMidLoopSkipsThatUnitAndContinues pins the loop's
// cancel branch: cancelling one candidate's confirm-path prompt must skip
// only that unit (with an honest "cancelled — nothing enrolled" message,
// never a false "nothing changed" claim) and continue to the next
// candidate, rather than aborting the whole run.
func TestRunTrackDiscoverCancelMidLoopSkipsThatUnitAndContinues(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{
			{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"},
			{LocalDir: "/tmp/project-b/.claude/memory", Label: "project-b", PathGuess: "/tmp/project-b"},
		},
		identity: provider.Identity{ProjectID: "github.com/u/project-b", PreferredFolder: "project-b"},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-b" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	callbacks := enrollCallbacks{
		pickEnrollUnits: func(candidates []enrollCandidate) ([]int, error) {
			indices := make([]int, len(candidates))
			for i := range candidates {
				indices[i] = i
			}
			return indices, nil
		},
		confirmProjectPath: func(guess string) (string, error) {
			if guess == "/tmp/project-a" {
				return "", huh.ErrUserAborted
			}
			return guess, nil
		},
		nameRemotelessFolder: func(hint string) (string, error) { return hint, nil },
	}
	var out bytes.Buffer
	enrolledAny, err := runTrackDiscover(context.Background(), deps, client, callbacks, &out)
	if err != nil {
		t.Fatalf("runTrackDiscover: %v", err)
	}
	if !enrolledAny {
		t.Fatal("enrolledAny = false, want true (project-b still enrolled after project-a was cancelled)")
	}
	if !strings.Contains(out.String(), "enroll: cancelled — nothing enrolled for /tmp/project-a/.claude/memory") {
		t.Fatalf("output missing project-a cancellation message: %s", out.String())
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].ProjectID != "github.com/u/project-b" {
		t.Fatalf("TrackRequests = %+v, want exactly one for project-b", requests)
	}
}

// --- resolveTrackPath / runTrackPath ---

func TestResolveTrackPathMatchesPathGuess(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"}},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	p, discovered, err := resolveTrackPath(context.Background(), registry, t.TempDir(), "/tmp/project-a")
	if err != nil {
		t.Fatalf("resolveTrackPath: %v", err)
	}
	if p.Name() != "fakeproj" || discovered.LocalDir != "/tmp/project-a/.claude/memory" {
		t.Fatalf("resolved = %s %+v", p.Name(), discovered)
	}
}

// TestResolveTrackPathFallsBackToClaudeSlugForWhenPathGuessMisses pins the
// residual-ambiguity case GuessPath's doc comment describes: a hyphenated
// leaf project directory with no decoy sibling for the reverse walk to
// land on. A provider named "claude" with NOTHING in its Discover results
// proves the fallback does not depend on discovery finding it at all — it
// probes the forward-only SlugFor encoding directly against the
// filesystem.
func TestResolveTrackPathFallsBackToClaudeSlugForWhenPathGuessMisses(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(home, "dev", "agent-brain")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	memoryDir := claude.MemoryDirFor(home, projectPath)
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}

	p, discovered, err := resolveTrackPath(context.Background(), registry, home, projectPath)
	if err != nil {
		t.Fatalf("resolveTrackPath: %v", err)
	}
	if p.Name() != "claude" || discovered.LocalDir != memoryDir {
		t.Fatalf("resolved = %s %+v, want claude %s", p.Name(), discovered, memoryDir)
	}
}

func TestResolveTrackPathNotFoundNamesClaudeProjectsDir(t *testing.T) {
	registry, err := provider.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = resolveTrackPath(context.Background(), registry, t.TempDir(), "/tmp/nonexistent-project")
	if err == nil {
		t.Fatal("resolveTrackPath: want error for an unresolvable path")
	}
	if !strings.Contains(err.Error(), "~/.claude/projects") {
		t.Fatalf("error must name ~/.claude/projects: %v", err)
	}
}

func TestRunTrackPathResolvesAndEnrollsViaPathGuess(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"}},
		identity:   provider.Identity{ProjectID: "github.com/u/project-a", PreferredFolder: "project-a"},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-a" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	var out bytes.Buffer
	enrolledAny, err := runTrackPath(context.Background(), deps, client, allowAllCallbacks(), &out, "/tmp/project-a")
	if err != nil {
		t.Fatalf("runTrackPath: %v", err)
	}
	if !enrolledAny {
		t.Fatal("enrolledAny = false, want true")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].ProjectID != "github.com/u/project-a" {
		t.Fatalf("TrackRequests = %+v", requests)
	}
}

// TestRunTrackPathCancelledEnrollsNothing pins the single-path cancel
// branch: cancelling the confirm-path prompt must report the same honest
// "cancelled — nothing enrolled" message the discovery loop uses, return no
// error, and never reach nameRemotelessFolder or the daemon.
func TestRunTrackPathCancelledEnrollsNothing(t *testing.T) {
	t.Parallel()
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"}},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	callbacks := enrollCallbacks{
		confirmProjectPath: func(string) (string, error) { return "", huh.ErrUserAborted },
		nameRemotelessFolder: func(string) (string, error) {
			t.Fatal("nameRemotelessFolder must not be called after confirmProjectPath was cancelled")
			return "", nil
		},
	}
	var out bytes.Buffer
	enrolledAny, err := runTrackPath(context.Background(), deps, nil, callbacks, &out, "/tmp/project-a")
	if err != nil {
		t.Fatalf("runTrackPath: %v", err)
	}
	if enrolledAny {
		t.Fatal("enrolledAny = true, want false (cancelled)")
	}
	if !strings.Contains(out.String(), "enroll: cancelled — nothing enrolled for /tmp/project-a/.claude/memory") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunTrackPathEnrollsViaClaudeSlugForFallback(t *testing.T) {
	home := t.TempDir()
	projectPath := filepath.Join(home, "dev", "agent-brain")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	memoryDir := claude.MemoryDirFor(home, projectPath)
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}

	fp := &fakeProvider{name: "claude", scope: provider.ScopePerProject, identity: provider.Identity{ProjectID: "github.com/u/agent-brain", PreferredFolder: "agent-brain"}}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "agent-brain" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: home}
	var out bytes.Buffer
	enrolledAny, err := runTrackPath(context.Background(), deps, client, allowAllCallbacks(), &out, projectPath)
	if err != nil {
		t.Fatalf("runTrackPath: %v", err)
	}
	if !enrolledAny {
		t.Fatal("enrolledAny = false, want true")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].LocalDir != memoryDir {
		t.Fatalf("TrackRequests = %+v, want LocalDir %s", requests, memoryDir)
	}
}

func TestRunTrackPathAlreadyTrackedIsANoOp(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{LocalDir: "/tmp/project-a/.claude/memory", Label: "project-a", PathGuess: "/tmp/project-a"}},
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	paths := config.Paths{DataDir: dataDir}
	local := repo.NewLocalRegistry()
	if err := local.Enroll(repo.Unit{Provider: "fakeproj", Folder: "project-a", LocalDir: "/tmp/project-a/.claude/memory"}); err != nil {
		t.Fatal(err)
	}
	if err := local.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}

	// No /v0/track handler registered: if the already-tracked short-circuit
	// didn't fire, calling Track would surface as a clear request error
	// rather than a goroutine-unsafe t.Fatal from the HTTP handler.
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{}, api.ProjectsResponse{})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}

	deps := trackDeps{paths: paths, registry: registry, home: t.TempDir()}
	var out bytes.Buffer
	enrolledAny, err := runTrackPath(context.Background(), deps, client, allowAllCallbacks(), &out, "/tmp/project-a")
	if err != nil {
		t.Fatalf("runTrackPath: %v", err)
	}
	if enrolledAny {
		t.Fatal("enrolledAny = true, want false (already tracked)")
	}
	if !strings.Contains(out.String(), "already tracked") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestTrackPathAndAllAreMutuallyExclusive(t *testing.T) {
	root := Root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"track", "/tmp/whatever", "--all"})
	err := root.Execute()
	if err == nil {
		t.Fatal("track <path> --all must be rejected")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Fatalf("error should name --all: %v", err)
	}
}

// --- syncAfterTrack ---

func TestSyncAfterTrackPrintsSummary(t *testing.T) {
	startFakeDaemon(t, api.StatusResponse{}, api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{Pushed: true}}, api.ProjectsResponse{})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := syncAfterTrack(context.Background(), client, &out); err != nil {
		t.Fatalf("syncAfterTrack: %v", err)
	}
	if !strings.Contains(out.String(), "sync completed") || !strings.Contains(out.String(), "pushed") {
		t.Fatalf("output: %s", out.String())
	}
}

// --- resolveUntrackTarget ---

func TestResolveUntrackTargetByFolder(t *testing.T) {
	units := []api.UnitInfo{
		{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"},
		{Provider: "claude", Folder: "beta", LocalDir: "/p/beta/.claude/memory"},
	}
	got, err := resolveUntrackTarget(units, "alpha")
	if err != nil {
		t.Fatalf("resolveUntrackTarget: %v", err)
	}
	if got.Folder != "alpha" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveUntrackTargetByExactLocalDir(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	got, err := resolveUntrackTarget(units, "/p/alpha/.claude/memory")
	if err != nil {
		t.Fatalf("resolveUntrackTarget: %v", err)
	}
	if got.Folder != "alpha" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveUntrackTargetByLocalDirSubpath(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	got, err := resolveUntrackTarget(units, "/p/alpha/.claude/memory/MEMORY.md")
	if err != nil {
		t.Fatalf("resolveUntrackTarget: %v", err)
	}
	if got.Folder != "alpha" {
		t.Fatalf("got %+v", got)
	}
}

// TestResolveUntrackTargetByAncestorPathMatchesNestedLocalDir covers a
// provider whose LocalDir genuinely nests under a broader path a user
// might type (codex: ~/.codex/memories under ~/.codex) — unlike claude,
// whose LocalDir lives under ~/.claude/projects/, structurally unrelated
// to the project path track <path> accepts (see the package-level note on
// resolveUntrackTarget for that limitation).
func TestResolveUntrackTargetByAncestorPathMatchesNestedLocalDir(t *testing.T) {
	units := []api.UnitInfo{{Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/memories"}}
	got, err := resolveUntrackTarget(units, "/home/u/.codex")
	if err != nil {
		t.Fatalf("resolveUntrackTarget: %v", err)
	}
	if got.Folder != "_global" {
		t.Fatalf("got %+v", got)
	}
}

func TestResolveUntrackTargetAmbiguousByFolderListsCandidates(t *testing.T) {
	units := []api.UnitInfo{
		{Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/memories"},
		{Provider: "codex", Folder: "_global", LocalDir: "/home/u/.codex/chronicle"},
	}
	_, err := resolveUntrackTarget(units, "_global")
	if err == nil {
		t.Fatal("resolveUntrackTarget: want an ambiguity error")
	}
	if !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "/home/u/.codex/memories") ||
		!strings.Contains(err.Error(), "/home/u/.codex/chronicle") {
		t.Fatalf("error must list both candidates: %v", err)
	}
}

func TestResolveUntrackTargetNotFoundNamesEnrolledFolders(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	_, err := resolveUntrackTarget(units, "nonexistent")
	if err == nil {
		t.Fatal("resolveUntrackTarget: want a not-found error")
	}
	if !strings.Contains(err.Error(), "enrolled folders: alpha") {
		t.Fatalf("error must name enrolled folders: %v", err)
	}
}

// --- runUntrack ---

// startFakeDaemonForUntrack serves /v0/status, /v0/projects, /v0/untrack
// (recording every request), and /v0/sync — the surface runUntrack needs.
func startFakeDaemonForUntrack(t *testing.T, units []api.UnitInfo, untrackResponse api.UntrackResponse) func() []api.UntrackRequest {
	t.Helper()
	dir, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", dir)

	var mu sync.Mutex
	var requests []api.UntrackRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.StatusResponse{State: "ready"})
	})
	mux.HandleFunc("/v0/projects", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProjectsResponse{Units: units})
	})
	mux.HandleFunc("/v0/untrack", func(w http.ResponseWriter, r *http.Request) {
		var req api.UntrackRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(untrackResponse)
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
	return func() []api.UntrackRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.UntrackRequest(nil), requests...)
	}
}

func TestRunUntrackRemovesWithoutPurgeByDefault(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	getRequests := startFakeDaemonForUntrack(t, units, api.UntrackResponse{Removed: true})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	confirmPurge := func(string) (bool, error) {
		t.Fatal("confirmPurge must not be called without --purge")
		return false, nil
	}
	if err := runUntrack(context.Background(), client, &out, "alpha", false, false, confirmPurge); err != nil {
		t.Fatalf("runUntrack: %v", err)
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].Purge {
		t.Fatalf("UntrackRequests = %+v", requests)
	}
	if !strings.Contains(out.String(), "history retains") {
		t.Fatalf("output missing history-retains note: %s", out.String())
	}
}

func TestRunUntrackPurgeWithYesSkipsConfirmation(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	getRequests := startFakeDaemonForUntrack(t, units, api.UntrackResponse{Removed: true, Purged: true})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	confirmPurge := func(string) (bool, error) { t.Fatal("confirmPurge must not be called with --yes"); return false, nil }
	if err := runUntrack(context.Background(), client, &out, "alpha", true, true, confirmPurge); err != nil {
		t.Fatalf("runUntrack: %v", err)
	}
	requests := getRequests()
	if len(requests) != 1 || !requests[0].Purge {
		t.Fatalf("UntrackRequests = %+v", requests)
	}
	if !strings.Contains(out.String(), "purged") || !strings.Contains(out.String(), "history retains") {
		t.Fatalf("output: %s", out.String())
	}
}

func TestRunUntrackPurgeWithoutYesRequiresConfirmation(t *testing.T) {
	tests := []struct {
		name      string
		confirmed bool
		wantPurge bool
	}{
		{name: "confirmed proceeds with purge", confirmed: true, wantPurge: true},
		{name: "declined aborts before calling untrack", confirmed: false, wantPurge: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
			getRequests := startFakeDaemonForUntrack(t, units, api.UntrackResponse{Removed: true, Purged: true})
			client, err := newAPIClient()
			if err != nil {
				t.Fatal(err)
			}
			var confirmCalledWith string
			confirmPurge := func(folder string) (bool, error) {
				confirmCalledWith = folder
				return tt.confirmed, nil
			}
			var out bytes.Buffer
			if err := runUntrack(context.Background(), client, &out, "alpha", true, false, confirmPurge); err != nil {
				t.Fatalf("runUntrack: %v", err)
			}
			if confirmCalledWith != "alpha" {
				t.Fatalf("confirmPurge called with %q, want %q", confirmCalledWith, "alpha")
			}
			requests := getRequests()
			if tt.wantPurge {
				if len(requests) != 1 || !requests[0].Purge {
					t.Fatalf("UntrackRequests = %+v, want one Purge:true request", requests)
				}
			} else if len(requests) != 0 {
				t.Fatalf("UntrackRequests = %+v, want none (declined confirmation)", requests)
			}
		})
	}
}

// TestPurgeConfirmationResult pins confirmPurgeInteractive's decision table
// without driving a real huh form: a cancel must win even over an exact
// typed match (proving the check happens before typed is trusted), a
// mismatched or empty typed value must decline, and any other error must
// propagate.
func TestPurgeConfirmationResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		typed   string
		folder  string
		err     error
		want    bool
		wantErr error
	}{
		{name: "cancelled beats an exact match", typed: "alpha", folder: "alpha", err: huh.ErrUserAborted, want: false, wantErr: nil},
		{name: "exact match confirms", typed: "alpha", folder: "alpha", err: nil, want: true, wantErr: nil},
		{name: "mismatch declines", typed: "wrong", folder: "alpha", err: nil, want: false, wantErr: nil},
		{name: "non-cancel error propagates", typed: "alpha", folder: "alpha", err: errors.New("boom"), want: false, wantErr: errors.New("boom")},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, gotErr := purgeConfirmationResult(testCase.typed, testCase.folder, testCase.err)
			if got != testCase.want {
				t.Errorf("confirmed = %v, want %v", got, testCase.want)
			}
			if (gotErr == nil) != (testCase.wantErr == nil) || (gotErr != nil && gotErr.Error() != testCase.wantErr.Error()) {
				t.Errorf("err = %v, want %v", gotErr, testCase.wantErr)
			}
		})
	}
}

func TestUntrackCommandWiring(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	getRequests := startFakeDaemonForUntrack(t, units, api.UntrackResponse{Removed: true})
	out := runCommand(t, "untrack", "alpha")
	if !strings.Contains(out, "removed") {
		t.Fatalf("output: %s", out)
	}
	if len(getRequests()) != 1 {
		t.Fatalf("requests = %+v", getRequests())
	}
}

// TestRunUntrackReportsNotEnrolledHonestly pins that the CLI READS
// UntrackResponse.Removed rather than announcing "removed" unconditionally.
// The daemon returns Removed=false when the local registry held no such
// enrollment (a race with another untrack, or a stale `projects` listing);
// claiming a removal that never happened is a lie the operator acts on.
func TestRunUntrackReportsNotEnrolledHonestly(t *testing.T) {
	units := []api.UnitInfo{{Provider: "claude", Folder: "alpha", LocalDir: "/p/alpha/.claude/memory"}}
	startFakeDaemonForUntrack(t, units, api.UntrackResponse{Removed: false})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	confirmPurge := func(string) (bool, error) { return false, nil }
	if err := runUntrack(context.Background(), client, &out, "alpha", false, false, confirmPurge); err != nil {
		t.Fatalf("runUntrack: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "not enrolled") {
		t.Fatalf("Removed=false must not report a removal:\n%s", got)
	}
}

// TestRunTrackPathRemotelessHintIsPreferredFolder pins the folder-name
// prompt's prefill for a remoteless project: Identify's PreferredFolder
// (Base of the project path), never Base(LocalDir) — for claude that
// basename is always "memory" (…/projects/<slug>/memory), and the
// prefill is what an empty answer accepts (an interactive Enter or an
// EOF'd headless run, see isAccessible's contract), so a wrong hint
// becomes a wrong enrollment: a headless track once enrolled a real
// project under the folder "memory" this way.
func TestRunTrackPathRemotelessHintIsPreferredFolder(t *testing.T) {
	fp := &fakeProvider{
		name: "fakeproj", scope: provider.ScopePerProject,
		discovered: []provider.Discovered{{
			LocalDir:  "/tmp/project-a/.claude/projects/-tmp-project-a/memory",
			Label:     "project-a",
			PathGuess: "/tmp/project-a",
		}},
		identity: provider.Identity{PreferredFolder: "project-a"}, // no ProjectID: remoteless
	}
	registry, err := provider.NewRegistry(fp)
	if err != nil {
		t.Fatal(err)
	}
	getRequests := startFakeDaemonForEnrollment(t, func(api.TrackRequest) string { return "project-a" })
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var gotHint string
	callbacks := allowAllCallbacks()
	callbacks.nameRemotelessFolder = func(hint string) (string, error) {
		gotHint = hint
		return hint, nil
	}
	deps := trackDeps{paths: config.Paths{DataDir: t.TempDir()}, registry: registry, home: t.TempDir()}
	var out bytes.Buffer
	enrolledAny, err := runTrackPath(context.Background(), deps, client, callbacks, &out, "/tmp/project-a")
	if err != nil {
		t.Fatalf("runTrackPath: %v", err)
	}
	if !enrolledAny {
		t.Fatal("enrolledAny = false, want true")
	}
	if gotHint != "project-a" {
		t.Fatalf("nameRemotelessFolder hint = %q, want %q (PreferredFolder, not Base(LocalDir))", gotHint, "project-a")
	}
	requests := getRequests()
	if len(requests) != 1 || requests[0].ProjectID != "named/project-a" {
		t.Fatalf("TrackRequests = %+v, want one named/project-a", requests)
	}
}

// TestSyncAfterTrackReportsUpToDateOnNoOpCycle pins syncAfterTrack's
// honesty contract: the daemon replies to Track before running its own
// cycle, so the explicit post-track cycle usually finds nothing left —
// that is the SUCCESS shape and must read as up-to-date, not as a
// zeros-itemized summary implying the enrollment synced nothing.
func TestSyncAfterTrackReportsUpToDateOnNoOpCycle(t *testing.T) {
	startFakeDaemon(t,
		api.StatusResponse{State: "ready"},
		api.SyncResponse{Status: "completed", Summary: &api.SyncSummary{}},
		api.ProjectsResponse{})
	client, err := newAPIClient()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := syncAfterTrack(context.Background(), client, &out); err != nil {
		t.Fatalf("syncAfterTrack: %v", err)
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("output = %q, want an up-to-date line for a no-op cycle", out.String())
	}
	if strings.Contains(out.String(), "in: 0 copied") {
		t.Fatalf("output = %q: a no-op cycle must not itemize zeros", out.String())
	}
}
