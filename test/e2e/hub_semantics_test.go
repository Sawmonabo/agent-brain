package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// hubMachine is one real daemon instance, its checkout, its bare remote, and
// its one enrolled claude unit — the fixture every test in this file uses to
// prove the spec 01-dashboard-hub-spec.md §17 acceptance rows that only a
// real daemon plus real git can prove: capture round-tripping through the
// exact hub write surface (memoryfs), restore-from-history, and the
// /v0/history and /v0/blob read funnel's equivalence with raw git. Unlike the
// rest of this package (which drives internal/engine directly, or a bare
// compiled-binary CLI process), every test here runs a real daemon
// in-process (daemon.New + Run, the same shape internal/daemon/
// daemon_test.go's own startDaemon uses — duplicated rather than imported,
// since that package's helpers are unexported) and drives it only through
// api.Client (UDS) and the real memoryfs mutation calls, never a bypass.
type hubMachine struct {
	client   *api.Client
	checkout string
	bare     string
	unit     repo.Unit
	host     string
}

// hubRegistry is the provider table every hubMachine's daemon runs under: one
// claude fact glob under memories/, the same shape sync_engine_test.go and
// internal/daemon/daemon_test.go already use for their own fixtures.
func hubRegistry(t *testing.T) *provider.Registry {
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

// hubSyncSettings keeps the ticker quiet (every capture in this file is
// driven explicitly through mustSync, never the timer) with a short
// debounce — internal/daemon/daemon_test.go's own fastSettings shape,
// duplicated here since that package's copy is unexported.
func hubSyncSettings() config.Settings {
	return config.Settings{Sync: config.SyncSettings{
		Ticker:   config.Duration(time.Hour),
		Debounce: config.Duration(50 * time.Millisecond),
		Poll:     config.Duration(0),
	}}
}

// currentHostname mirrors internal/daemon's own unexported hostname()
// fallback, so the commit-subject prefix this file expects always matches
// what THIS SAME process's daemon.New(...).Run(...) actually captures, even
// in the (practically never, but structurally possible) case os.Hostname()
// errors.
func currentHostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "unknown-host"
	}
	return name
}

// newHubMachine provisions Paths under a fresh temp root, a seeded memories
// checkout cloned from a real bare remote (newBareRepo — the harness's own
// hermetic git env), a real Tink keyset, and real filter wiring at the
// harness's own compiled binary (binPath from TestMain — never a second
// build, never os.Executable(), per harness_test.go's fork-bomb warning). One
// claude unit is enrolled directly into the local registry (skipping the
// /v0/track HTTP round trip — already proven elsewhere, e.g. daemon_test.go's
// TestTrackEnrollsCommitsAndSyncs, and irrelevant to what this file proves),
// then a real daemon starts against it and this call blocks until its API
// answers.
func newHubMachine(t *testing.T) *hubMachine {
	t.Helper()
	// A short, non-nested prefix, NOT t.TempDir(): t.TempDir() nests under a
	// path that includes this test's full function name (sanitized), and the
	// resulting AGENT_BRAIN_RUNTIME_DIR/agent-brain.sock routinely exceeds
	// the ~104-byte unix socket sun_path limit for a test name this long —
	// the same reason internal/daemon/daemon_test.go's provisionMemories
	// uses this exact os.MkdirTemp("", "ab") shape instead.
	base, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	paths := config.Paths{ConfigDir: filepath.Join(base, "cfg"), DataDir: filepath.Join(base, "data")}

	// Non-parallel guards (t.Setenv panics under a parallel ancestor) that
	// restore the suite-wide values testMain set — the identical pattern
	// rotate_test.go already uses for these same two env vars.
	t.Setenv("AGENT_BRAIN_RUNTIME_DIR", filepath.Join(base, "run"))
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", paths.ConfigDir)

	bare := newBareRepo(t)
	checkout := paths.MemoriesDir()
	if err := os.MkdirAll(paths.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, filepath.Dir(checkout), "clone", "--quiet", bare, checkout)
	gitRun(t, checkout, "config", "user.name", "hub-e2e")
	gitRun(t, checkout, "config", "user.email", "hub-e2e@test.invalid")

	if err := keys.Generate(paths.Keyset()); err != nil {
		t.Fatal(err)
	}
	if err := gitx.InstallFilters(suiteCtx, checkout, binPath); err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteAttributes(repo.NewLayout(checkout), hubRegistry(t)); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "add", "-A")
	gitRun(t, checkout, "commit", "--quiet", "-m", "init: repo skeleton")
	gitRun(t, checkout, "push", "--quiet", "-u", "origin", "main")

	unit := repo.Unit{
		Provider:  "claude",
		ProjectID: "id-alpha",
		Folder:    "alpha",
		LocalDir:  filepath.Join(base, "proj", ".claude", "memory"),
	}
	if err := os.MkdirAll(unit.LocalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	localRegistry := repo.NewLocalRegistry()
	if err := localRegistry.Enroll(unit); err != nil {
		t.Fatal(err)
	}
	if err := localRegistry.Save(paths.LocalRegistryFile()); err != nil {
		t.Fatal(err)
	}

	hubDaemon, err := daemon.New(daemon.Config{
		Paths:      paths,
		Settings:   hubSyncSettings(),
		Registry:   hubRegistry(t),
		Version:    "test-e2e",
		BinaryPath: binPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(suiteCtx)
	runDone := make(chan error, 1)
	go func() { runDone <- hubDaemon.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-runDone:
			if err != nil {
				t.Errorf("hub daemon Run returned %v on shutdown", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("hub daemon did not shut down within 5s")
		}
	})

	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		t.Fatal(err)
	}
	client := api.NewClient(socketPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Status(suiteCtx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub daemon API never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}

	return &hubMachine{client: client, checkout: checkout, bare: bare, unit: unit, host: currentHostname()}
}

// mustSync triggers a cycle through the api client — the same call the hub's
// own manual sync action makes — and requires it to complete synchronously.
// It does not return the cycle's own summary: callers assert on the bare
// remote's own state afterward (remoteFolderCommitCount et al.), never on a
// specific call's self-reported Pushed/Commits fields, because the daemon's
// fsnotify watcher is live throughout this file (matching production) — a
// watch-triggered cycle can race a given mustSync call and legitimately
// leave IT with nothing new to report even though the write it followed was
// captured. Because the engine is a single-goroutine writer, mustSync's own
// "completed" reply still guarantees every write issued before this call has
// been committed and pushed by the time it returns, regardless of which
// cycle actually did it.
func (m *hubMachine) mustSync(t *testing.T) {
	t.Helper()
	syncResponse, err := m.client.Sync(suiteCtx, "")
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if syncResponse.Status != "completed" || syncResponse.Summary == nil {
		t.Fatalf("sync = %+v, want completed with a summary", syncResponse)
	}
}

// repoPath composes rel into m's /v0/history path key (<provider>/<rel> —
// this unit has no RepoSubdir). Duplicating memoryfs's own unexported
// repoPath join here is the same one-line tradeoff its classifyRel doc
// comment already makes: not worth crossing a package boundary for one join.
func (m *hubMachine) repoPath(rel string) string {
	return m.unit.Provider + "/" + rel
}

// treePath composes rel into the full on-disk path within the checkout/
// remote tree (<folder>/<provider>/<rel>) that raw git commands need, as
// opposed to repoPath's API-facing key.
func (m *hubMachine) treePath(rel string) string {
	return m.unit.Folder + "/" + m.repoPath(rel)
}

// remoteFolderCommitCount is the number of commits on main in the bare
// remote whose tree touches folder. Deliberately NOT a bare `rev-list
// --count main`: each cycle that captures a change also lands a SEPARATE
// per-host manifest commit (`memory: <host> manifest <stamp>`, touching only
// .agent-brain/**), which internal/engine/history.go's own parseCaptureSubject
// doc comment notes is filtered out of every folder-scoped History query by
// its pathspec. Scoping to folder here matches that same pathspec, so this
// counts exactly what /v0/history's folder-wide mode would — "how many times
// was this folder's content captured", not the daemon's internal
// bookkeeping.
func remoteFolderCommitCount(t *testing.T, bare, folder string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(gitRun(t, bare, "rev-list", "--count", "main", "--", folder)))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// remoteFolderSubject is the subject of main's newest commit that touches
// folder in the bare remote — skipping over any newer manifest-only commit,
// for the same reason remoteFolderCommitCount scopes to folder.
func remoteFolderSubject(t *testing.T, bare, folder string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, bare, "log", "-1", "--format=%s", "main", "--", folder))
}

// checkoutRevs returns the commit hashes touching path in dir, newest
// first — the raw-git ground truth /v0/history is checked against.
func checkoutRevs(t *testing.T, dir, path string) []string {
	t.Helper()
	out := strings.TrimSpace(gitRun(t, dir, "log", "--format=%H", "--", path))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// checkoutTextconv decrypts path at rev in dir through the checkout's own
// textconv driver (installed by gitx.InstallFilters) — the ground truth
// /v0/blob's content is checked against.
func checkoutTextconv(t *testing.T, dir, rev, path string) string {
	t.Helper()
	return gitRun(t, dir, "cat-file", "--textconv", rev+":"+path)
}

// revsOf extracts each version's Rev, in order.
func revsOf(versions []api.HistoryVersion) []string {
	revs := make([]string, len(versions))
	for i, version := range versions {
		revs[i] = version.Rev
	}
	return revs
}

// TestEditRoundTripSemantics proves spec §17's "Edit round-trip" row end to
// end through a real daemon: a brand-new memory and a changed save each land
// as exactly one clean capture commit on the remote, ciphertext throughout,
// while saving byte-identical content again produces zero new commits.
func TestEditRoundTripSemantics(t *testing.T) {
	machine := newHubMachine(t)
	const (
		relPath = "memories/testing-style.md"
		v1      = "the codebase uses table-driven tests exclusively\n"
		v2      = "the codebase uses table-driven tests, revised for clarity\n"
	)
	wantPrefix := "memory: " + machine.host + " " + machine.unit.Folder + " "

	// Baseline: a brand-new memory is exactly one clean capture (spec §17
	// "New ... produce exactly one clean capture").
	before := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)
	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(v1)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	afterBaseline := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)
	if afterBaseline != before+1 {
		t.Fatalf("baseline commit count = %d, want %d (exactly one new commit)", afterBaseline, before+1)
	}
	if subject := remoteFolderSubject(t, machine.bare, machine.unit.Folder); !strings.HasPrefix(subject, wantPrefix) {
		t.Fatalf("baseline subject = %q, want prefix %q", subject, wantPrefix)
	}
	if blob := remoteBlob(t, machine.bare, machine.treePath(relPath)); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatalf("baseline blob is not agent-brain ciphertext: %q", blob[:min(16, len(blob))])
	}
	assertNoPlaintextOnWire(t, machine.bare, v1)

	// Changed content: exactly one more clean capture (spec §17 "exactly one
	// capture commit per changed save").
	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(v2)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	afterEdit := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)
	if afterEdit != afterBaseline+1 {
		t.Fatalf("edit commit count = %d, want %d (exactly one new commit)", afterEdit, afterBaseline+1)
	}
	if subject := remoteFolderSubject(t, machine.bare, machine.unit.Folder); !strings.HasPrefix(subject, wantPrefix) {
		t.Fatalf("edit subject = %q, want prefix %q", subject, wantPrefix)
	}
	assertNoPlaintextOnWire(t, machine.bare, v2)

	// No-op: byte-identical content produces zero new commits (spec §17
	// "byte-equal save produces zero commits").
	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(v2)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	afterNoop := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)
	if afterNoop != afterEdit {
		t.Fatalf("no-op commit count = %d, want unchanged at %d", afterNoop, afterEdit)
	}
}

// TestDeleteThenRestoreFromHistory proves spec §17's "deleted memory
// restorable from history" row: deleting a captured memory shows up in
// folder-wide history while absent on disk, its pre-delete blob is fetchable
// by rev, and landing that content back through WriteFileAtomic grows
// history with a new commit rather than rewriting any existing one — proven
// independently on a completely fresh clone.
func TestDeleteThenRestoreFromHistory(t *testing.T) {
	machine := newHubMachine(t)
	const (
		relPath = "memories/ephemeral.md"
		content = "short-lived fact, restorable from history\n"
	)
	repoPath := machine.repoPath(relPath)

	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(content)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	baseline, err := machine.client.History(suiteCtx, machine.unit.Folder, repoPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Versions) != 1 {
		t.Fatalf("baseline history = %+v, want exactly one version", baseline.Versions)
	}
	preDeleteRev := baseline.Versions[0].Rev

	// Delete: memoryfs.Delete is the hub's own deletion call, exercised for
	// real, never a bare os.Remove.
	if err := memoryfs.Delete(memoryfs.Memory{LocalDir: machine.unit.LocalDir, RelPath: relPath}); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	if _, err := os.Stat(filepath.Join(machine.unit.LocalDir, filepath.FromSlash(relPath))); !os.IsNotExist(err) {
		t.Fatalf("provider file still present after delete+sync: err=%v", err)
	}

	// Folder-wide history: the path survives in some version's Paths even
	// though the file is gone from disk — the deleted-memories view's data
	// source (spec §6).
	folderWide, err := machine.client.History(suiteCtx, machine.unit.Folder, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(folderWide.Versions, func(v api.HistoryVersion) bool {
		return slices.Contains(v.Paths, repoPath)
	}) {
		t.Fatalf("folder-wide history %+v never lists %q in Paths", folderWide.Versions, repoPath)
	}

	// Path-mode history after the delete: two commits now touch this path
	// (create, delete) — restore has not happened yet.
	afterDelete, err := machine.client.History(suiteCtx, machine.unit.Folder, repoPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDelete.Versions) != 2 {
		t.Fatalf("history after delete = %+v, want exactly two versions (create, delete)", afterDelete.Versions)
	}

	// Fetch the pre-delete blob and restore it verbatim through
	// WriteFileAtomic — the hub's exact restore call shape.
	blob, err := machine.client.Blob(suiteCtx, machine.unit.Folder, repoPath, preDeleteRev)
	if err != nil {
		t.Fatal(err)
	}
	if blob.Content != content {
		t.Fatalf("pre-delete blob = %q, want %q", blob.Content, content)
	}
	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(blob.Content)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)

	// Restore grew history — never rewrote it: every pre-restore rev
	// survives, and the rev count strictly increases across all three
	// phases (1 -> 2 -> 3).
	afterRestore, err := machine.client.History(suiteCtx, machine.unit.Folder, repoPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterRestore.Versions) != 3 {
		t.Fatalf("history after restore = %+v, want exactly three versions (create, delete, restore)", afterRestore.Versions)
	}
	survivingRevs := revsOf(afterRestore.Versions)
	for _, oldVersion := range afterDelete.Versions {
		if !slices.Contains(survivingRevs, oldVersion.Rev) {
			t.Fatalf("restore lost pre-existing rev %q — history was rewritten, not grown", oldVersion.Rev)
		}
	}
	if !afterRestore.Versions[0].Live {
		t.Fatalf("restore's own commit = %+v, want Live", afterRestore.Versions[0])
	}
	for _, version := range afterRestore.Versions {
		if version.Rev == preDeleteRev && !version.Live {
			t.Fatalf("pre-delete version %+v content matches the restore byte-for-byte, want Live", version)
		}
	}

	if got, err := os.ReadFile(filepath.Join(machine.unit.LocalDir, filepath.FromSlash(relPath))); err != nil || string(got) != content {
		t.Fatalf("provider file after restore = (%q, %v), want (%q, nil)", got, err, content)
	}

	// Independent proof on a completely fresh clone (a genuinely different
	// checkout, filters installed from scratch): HEAD's textconv content at
	// this path equals the restored content.
	freshClone := filepath.Join(t.TempDir(), "fresh-clone")
	gitRun(t, filepath.Dir(freshClone), "clone", "--quiet", machine.bare, freshClone)
	if err := gitx.InstallFilters(suiteCtx, freshClone, binPath); err != nil {
		t.Fatal(err)
	}
	if got := checkoutTextconv(t, freshClone, "HEAD", machine.treePath(relPath)); got != content {
		t.Fatalf("fresh clone HEAD:%s = %q, want %q", machine.treePath(relPath), got, content)
	}
}

// TestHistoryMatchesGitLog proves spec §17's "History lists match git log ...
// blob content at rev matches git cat-file --textconv" row: after several
// edits, /v0/history for the path returns exactly the revs a direct `git
// log` on the daemon's own checkout prints, in the same order, and each
// version's /v0/blob content equals a direct `git cat-file --textconv` at
// that rev.
func TestHistoryMatchesGitLog(t *testing.T) {
	machine := newHubMachine(t)
	const relPath = "memories/architecture.md"
	repoPath := machine.repoPath(relPath)
	edits := []string{
		"the engine is the single writer to the checkout\n",
		"the engine is the single writer to the checkout, revised\n",
		"the engine is the single writer to the checkout, final\n",
	}
	for _, content := range edits {
		if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, relPath, []byte(content)); err != nil {
			t.Fatal(err)
		}
		machine.mustSync(t)
	}

	wantRevs := checkoutRevs(t, machine.checkout, machine.treePath(relPath))
	if len(wantRevs) != len(edits) {
		t.Fatalf("git log on the checkout found %d revs for %s, want %d", len(wantRevs), repoPath, len(edits))
	}

	historyResponse, err := machine.client.History(suiteCtx, machine.unit.Folder, repoPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(wantRevs, revsOf(historyResponse.Versions)); diff != "" {
		t.Fatalf("/v0/history revs vs git log (-want +got):\n%s", diff)
	}

	wantPrefix := "memory: " + machine.host + " " + machine.unit.Folder + " "
	for i, version := range historyResponse.Versions {
		if !strings.HasPrefix(version.Subject, wantPrefix) {
			t.Fatalf("versions[%d].Subject = %q, want prefix %q", i, version.Subject, wantPrefix)
		}
		if version.Host != machine.host {
			t.Fatalf("versions[%d].Host = %q, want %q", i, version.Host, machine.host)
		}
		want := checkoutTextconv(t, machine.checkout, version.Rev, machine.treePath(relPath))
		blobResponse, err := machine.client.Blob(suiteCtx, machine.unit.Folder, repoPath, version.Rev)
		if err != nil {
			t.Fatalf("Blob(%s): %v", version.Rev, err)
		}
		if blobResponse.Content != want {
			t.Fatalf("versions[%d] (%s): /v0/blob = %q, want git cat-file --textconv %q", i, version.Rev, blobResponse.Content, want)
		}
	}
	if !historyResponse.Versions[0].Live {
		t.Fatalf("newest version = %+v, want Live (content matches HEAD)", historyResponse.Versions[0])
	}
}

// TestRenameProducesSingleCleanCapture closes the one spec §17 acceptance row
// ("New/rename/delete each produce exactly one clean capture") the other
// three tests in this file do not reach: nothing else in the repo drives
// memoryfs.Rename through a live daemon and real git — its own unit tests
// (memoryfs_test.go) never touch a git checkout at all.
func TestRenameProducesSingleCleanCapture(t *testing.T) {
	machine := newHubMachine(t)
	const (
		oldRel  = "memories/draft.md"
		newRel  = "memories/final.md"
		content = "renamed memories keep their content\n"
	)

	if err := memoryfs.WriteFileAtomic(machine.unit.LocalDir, oldRel, []byte(content)); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)
	before := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)

	if err := memoryfs.Rename(memoryfs.Memory{LocalDir: machine.unit.LocalDir, RelPath: oldRel}, newRel); err != nil {
		t.Fatal(err)
	}
	machine.mustSync(t)

	after := remoteFolderCommitCount(t, machine.bare, machine.unit.Folder)
	if after != before+1 {
		t.Fatalf("rename commit count = %d, want %d (exactly one clean capture)", after, before+1)
	}
	wantPrefix := "memory: " + machine.host + " " + machine.unit.Folder + " "
	if subject := remoteFolderSubject(t, machine.bare, machine.unit.Folder); !strings.HasPrefix(subject, wantPrefix) {
		t.Fatalf("rename subject = %q, want prefix %q", subject, wantPrefix)
	}

	if _, err := os.Stat(filepath.Join(machine.unit.LocalDir, filepath.FromSlash(oldRel))); !os.IsNotExist(err) {
		t.Fatalf("old path still present after rename: err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(machine.unit.LocalDir, filepath.FromSlash(newRel))); err != nil || string(got) != content {
		t.Fatalf("new path after rename = (%q, %v), want (%q, nil)", got, err, content)
	}

	if blob := remoteBlob(t, machine.bare, machine.treePath(newRel)); !strings.HasPrefix(blob, magicPrefix) {
		t.Fatalf("renamed blob is not agent-brain ciphertext: %q", blob[:min(16, len(blob))])
	}
	if _, err := gitRunEnv(t, machine.bare, nil, "cat-file", "-e", "main:"+machine.treePath(oldRel)); err == nil {
		t.Fatal("old path still present in the remote tree after rename")
	}
	assertNoPlaintextOnWire(t, machine.bare, content)
}
