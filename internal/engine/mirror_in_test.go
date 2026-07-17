package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/provider/providertest"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestMirrorInCopiesChangedFilesAndSkipsIgnored(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/go-style.md", "# fact\n")
	writeLocal(t, u, "scratch.tmp", "never syncs\n")

	manifest := repo.NewManifest()
	stats, snapshot, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Fatalf("Copied = %d, want 1", stats.Copied)
	}
	copied := filepath.Join(checkout, "alpha", "claude", "memories", "go-style.md")
	data, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "# fact\n" {
		t.Fatalf("checkout content = %q", data)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "scratch.tmp")); !os.IsNotExist(err) {
		t.Fatal("ClassIgnore file reached the checkout")
	}
	if !manifest.Has("alpha/claude/memories/go-style.md") {
		t.Fatal("manifest missing the synced path")
	}
	if _, ok := snapshot["alpha/claude/memories/go-style.md"]; !ok {
		t.Fatal("snapshot missing the synced path")
	}
}

func TestMirrorInSecondRunIsNoop(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/a.md", "content\n")

	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Deleted != 0 {
		t.Fatalf("second run stats = %+v, want zero copies/deletes", stats)
	}
}

func TestMirrorInDeletesViaManifestOnly(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/gone.md", "will be deleted\n")

	manifest := repo.NewManifest()
	ctx := context.Background()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	// Case 1: in manifest + gone locally = deleted here → git rm.
	if err := os.Remove(filepath.Join(u.LocalDir, "memories", "gone.md")); err != nil {
		t.Fatal(err)
	}
	// Case 2: in checkout + NOT in manifest = new from remote → untouched.
	fromRemote := filepath.Join(checkout, "alpha", "claude", "memories", "remote-new.md")
	if err := os.WriteFile(fromRemote, []byte("landed via integrate\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "gone.md")); !os.IsNotExist(err) {
		t.Fatal("deleted-here file still in checkout")
	}
	if manifest.Has("alpha/claude/memories/gone.md") {
		t.Fatal("manifest still lists the deleted path")
	}
	if _, err := os.Stat(fromRemote); err != nil {
		t.Fatal("new-from-remote file was wrongly removed:", err)
	}
}

// TestMirrorInRefusesGitMetaFiles pins the inbound guard (spec §5): a
// git-meta file in the provider dir — most dangerously a .gitattributes
// whose `* -filter` would override the checkout-root attributes for the
// unit subtree and disable the encryption clean filter — must never be
// mirrored into the checkout, regardless of provider classification. A
// normal fact sibling must still sync.
func TestMirrorInRefusesGitMetaFiles(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	writeLocal(t, u, ".gitattributes", "* -filter\n")
	writeLocal(t, u, ".gitignore", "memories/\n")
	// Case variant lives in its own dir: a case-insensitive filesystem
	// (macOS) would collide it with a lowercase sibling in the same dir.
	writeLocal(t, u, "caps/.GITATTRIBUTES", "* -filter\n")
	writeLocal(t, u, "sub/.gitattributes", "* -filter\n")
	// A normal fact sibling MUST still sync.
	writeLocal(t, u, "memories/real.md", "# real fact\n")

	manifest := repo.NewManifest()
	stats, snapshot, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}

	gitMeta := []string{".gitattributes", ".gitignore", "caps/.GITATTRIBUTES", "sub/.gitattributes"}
	for _, rel := range gitMeta {
		if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("git-meta file %q reached the checkout — encryption filter can be disabled", rel)
		}
		if manifest.Has("alpha/claude/" + rel) {
			t.Fatalf("git-meta file %q entered the manifest", rel)
		}
		if _, ok := snapshot["alpha/claude/"+rel]; ok {
			t.Fatalf("git-meta file %q entered the snapshot", rel)
		}
	}
	if stats.Skipped != len(gitMeta) {
		t.Fatalf("Skipped = %d, want %d (every git-meta file)", stats.Skipped, len(gitMeta))
	}
	if stats.Copied != 1 {
		t.Fatalf("Copied = %d, want 1 (only the normal fact)", stats.Copied)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "real.md")); err != nil {
		t.Fatal("normal fact sibling did not sync:", err)
	}
	if !manifest.Has("alpha/claude/memories/real.md") {
		t.Fatal("manifest missing the normal fact")
	}
}

// TestMirrorInScrubsGitMetaFromCheckout pins the checkout scrub (spec §5):
// a git-meta file that arrived from a poisoned remote (tracked + committed,
// absent from this host's manifest) is new-from-remote, so the ordinary
// manifest-gated deletion path leaves it alone. The unconditional scrub
// must remove it anyway — healing an already-poisoned repo — while leaving
// an innocent tracked fact untouched.
func TestMirrorInScrubsGitMetaFromCheckout(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	ctx := context.Background()

	unitDir := engine.layout.UnitDir("alpha", "claude")
	if err := os.MkdirAll(filepath.Join(unitDir, "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "evil", ".gitattributes"), []byte("* -filter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(unitDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "memories", "keep.md"), []byte("innocent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "simulate poisoned integrate")

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}

	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1 (the scrubbed .gitattributes)", stats.Deleted)
	}
	evil := filepath.Join(unitDir, "evil", ".gitattributes")
	if _, err := os.Stat(evil); !os.IsNotExist(err) {
		t.Fatal("poisoned .gitattributes still on disk after scrub")
	}
	status := strings.TrimSpace(mustGit(t, checkout, "status", "--porcelain", "--", "alpha/claude/evil/.gitattributes").Stdout)
	if !strings.HasPrefix(status, "D") {
		t.Fatalf("scrub did not stage the deletion: status = %q", status)
	}
	if got, err := os.ReadFile(filepath.Join(unitDir, "memories", "keep.md")); err != nil || string(got) != "innocent\n" {
		t.Fatalf("innocent tracked file disturbed: %q, %v", got, err)
	}
}

// TestMirrorInForceScrubsFilterSubjectGitMeta pins forceScrubGitMeta's
// force semantics (no up-to-date check) at mirror-in pass 2 — the half
// of e3d23fd the adversarial row filter_subject_gitignore_file does NOT
// cover (that row's poison is removed by scrubIntegrated, which runs at
// integrate time before mirror-in ever sees it). A committed
// filter-subject .gitignore — poison from before the defense existed,
// or surviving a scrub that crashed mid-cycle — re-cleans to bytes that
// differ from its plaintext index blob, so the conservative `git rm`
// used for ordinary deletion propagation refuses ("local
// modifications") and would wedge every subsequent cycle. This test
// FAILS if pass 2 regresses to scrubCheckoutFile.
func TestMirrorInForceScrubsFilterSubjectGitMeta(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	ctx := context.Background()

	unitDir := engine.layout.UnitDir("alpha", "claude")
	if err := os.MkdirAll(filepath.Join(unitDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "memories", "keep.md"), []byte("innocent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, ".gitignore"), []byte("memories/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The plaintext blob enters the index UNFILTERED (no filter is wired
	// yet), exactly like an attacker's raw push arriving via integrate.
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "simulate poisoned integrate")

	// Only now wire the path to a content-transforming clean filter, the
	// way the real checkout's root .gitattributes + filter.agentbrain
	// cover .gitignore (which, unlike .gitattributes, is filter-subject).
	// The committed plaintext index blob no longer matches the worktree
	// copy's re-clean — the exact state that makes an up-to-date-checking
	// `git rm` refuse. `tr` stands in for the encryption filter: a shell
	// command, never a test binary (CLAUDE.md).
	if err := os.WriteFile(filepath.Join(checkout, ".gitattributes"), []byte("alpha/claude/.gitignore filter=fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "config", "filter.fake.clean", "tr a-z A-Z")
	// Bump the worktree mtime so the index entry is no longer stat-clean:
	// git skips the clean-filter content comparison entirely for
	// stat-clean entries, and whether a same-second entry is "racily
	// clean" (content-checked) is timing luck. A changed mtime forces the
	// content path deterministically — the same path a real integrate
	// reaches via its same-instant checkout stats.
	bumped := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(unitDir, ".gitignore"), bumped, bumped); err != nil {
		t.Fatal(err)
	}

	// Prove the trap is armed: the conservative `git rm` (what
	// scrubCheckoutFile runs) must refuse this state, or the fixture can
	// no longer distinguish force from conservative removal.
	if _, err := gitx.Run(ctx, checkout, "rm", "--quiet", "--ignore-unmatch", "--", "alpha/claude/.gitignore"); err == nil {
		t.Fatal("fixture failed to arm: plain `git rm` accepted the filter-subject poison")
	}

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatalf("mirror-in wedged on filter-subject git-meta: %v", err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1 (the force-scrubbed .gitignore)", stats.Deleted)
	}
	if _, err := os.Stat(filepath.Join(unitDir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("filter-subject poison still on disk after force scrub")
	}
	status := strings.TrimSpace(mustGit(t, checkout, "status", "--porcelain", "--", "alpha/claude/.gitignore").Stdout)
	if !strings.HasPrefix(status, "D") {
		t.Fatalf("force scrub did not stage the deletion: status = %q", status)
	}
	if got, err := os.ReadFile(filepath.Join(unitDir, "memories", "keep.md")); err != nil || string(got) != "innocent\n" {
		t.Fatalf("innocent tracked file disturbed: %q, %v", got, err)
	}
}

// TestIsGitMetaPathDelegatesToRepo pins that the engine's guard IS
// repo.IsGitMetaPath — the semantics themselves are pinned by that
// package's table test (repo/gitmeta_test.go). Two copies of a security
// predicate are two predicates, and the one that drifts is the hole; this
// asserts there is exactly one. The sampled rows are the load-bearing
// shapes: the folder-level poison that motivated the shared definition, a
// case-insensitive match (macOS resolves .GITATTRIBUTES for
// .gitattributes), and a whole-segment non-match.
func TestIsGitMetaPathDelegatesToRepo(t *testing.T) {
	t.Parallel()
	for _, rel := range []string{
		"alpha/.gitattributes", ".GITIGNORE", "a/b/.git",
		".github/notes.md", "memories/real.md", "",
	} {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			if got, want := isGitMetaPath(rel), repo.IsGitMetaPath(rel); got != want {
				t.Fatalf("engine isGitMetaPath(%q) = %v, repo.IsGitMetaPath = %v — the engine grew a second definition", rel, got, want)
			}
		})
	}
}

// TestMirrorInScrubHealCommits pins the full heal: the scrub's staged
// deletion must survive commitProjects (including its Lstat guard on the
// folder pathspec) so the poison's removal reaches HEAD and propagates to
// other machines on push — not merely sit staged in this host's index.
func TestMirrorInScrubHealCommits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	ctx := context.Background()

	unitDir := engine.layout.UnitDir("alpha", "claude")
	if err := os.MkdirAll(filepath.Join(unitDir, "evil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "evil", ".gitattributes"), []byte("* -filter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(unitDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, "memories", "keep.md"), []byte("innocent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "simulate poisoned integrate")

	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	subjects, err := engine.commitProjects(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	wantSubjects := []string{"memory: host-a alpha " + fixedStamp}
	if len(subjects) != 1 || subjects[0] != wantSubjects[0] {
		t.Fatalf("subjects = %v, want %v", subjects, wantSubjects)
	}

	tracked := mustGit(t, checkout, "ls-tree", "-r", "--name-only", "HEAD", "--", "alpha").Stdout
	if strings.Contains(tracked, ".gitattributes") {
		t.Fatalf("poisoned .gitattributes still tracked at HEAD:\n%s", tracked)
	}
	if !strings.Contains(tracked, "alpha/claude/memories/keep.md") {
		t.Fatalf("innocent file missing from HEAD:\n%s", tracked)
	}
	status := mustGit(t, checkout, "status", "--porcelain")
	if strings.TrimSpace(status.Stdout) != "" {
		t.Fatalf("tree dirty after heal commit:\n%s", status.Stdout)
	}
}

func TestMirrorInRefusesSymlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("keyset material\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(u.LocalDir, "memories"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(u.LocalDir, "memories", "planted.md")); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Skipped != 1 {
		t.Fatalf("stats = %+v, want 0 copied / 1 skipped", stats)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "planted.md")); !os.IsNotExist(err) {
		t.Fatal("symlink target content reached the checkout — exfiltration path")
	}
}

func TestMirrorInUnknownProviderIsError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	bad := repo.Unit{Provider: "gemini", ProjectID: "x", Folder: "alpha", LocalDir: t.TempDir()}
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{bad}, repo.NewManifest()); err == nil {
		t.Fatal("unenrollable provider silently skipped; want loud error")
	}
}

func TestMirrorInRemovesUntrackedOrphanOnDeletion(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	// Simulate a prior cycle that crashed after mirror-in wrote this
	// file into the checkout but before commitProjects ran: the file
	// exists on disk but was never `git add`ed, so it is untracked.
	orphan := filepath.Join(checkout, "alpha", "claude", "memories", "orphan.md")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("crashed before commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	entry := repo.ManifestEntry{Size: 22, MTimeUnixNano: 1, SHA256: "deadbeef"}
	if err := manifest.Set("alpha/claude/memories/orphan.md", entry); err != nil {
		t.Fatal(err)
	}
	// Provider-local dir intentionally has no matching file: this path
	// is only reachable through the manifest-driven deletion branch.
	if _, err := os.Stat(filepath.Join(u.LocalDir, "memories", "orphan.md")); !os.IsNotExist(err) {
		t.Fatal("test setup error: orphan.md unexpectedly present locally")
	}

	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", stats.Deleted)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("untracked orphan still on disk after the deletion pass — resurrection hole")
	}
	if manifest.Has("alpha/claude/memories/orphan.md") {
		t.Fatal("manifest still lists the removed orphan")
	}
}

func TestMirrorInRefreshesLedgerOnIdenticalContentTouch(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")
	writeLocal(t, u, "memories/a.md", "content\n")

	manifest := repo.NewManifest()
	ctx := context.Background()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest); err != nil {
		t.Fatal(err)
	}
	before, ok := manifest.Get("alpha/claude/memories/a.md")
	if !ok {
		t.Fatal("manifest missing entry after first sync")
	}

	full := filepath.Join(u.LocalDir, "memories", "a.md")
	touched := time.Unix(0, before.MTimeUnixNano).Add(time.Hour)
	if err := os.Chtimes(full, touched, touched); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	wantMTime := info.ModTime().UnixNano()
	if wantMTime == before.MTimeUnixNano {
		t.Fatal("test setup error: touched mtime did not change on disk")
	}

	stats, _, err := engine.mirrorIn(ctx, []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 {
		t.Fatalf("Copied = %d, want 0 — content-identical touch must not recopy", stats.Copied)
	}
	after, ok := manifest.Get("alpha/claude/memories/a.md")
	if !ok {
		t.Fatal("manifest missing entry after touch cycle")
	}
	if after.MTimeUnixNano != wantMTime {
		t.Fatalf("manifest MTimeUnixNano = %d, want %d (refreshed to the new mtime)", after.MTimeUnixNano, wantMTime)
	}
}

// TestMirrorInSeparatesManifestKeysAcrossRepoSubdirUnits pins the
// namespace fix that goes with RepoSubdir plumbing: two units
// sharing one (folder, provider) but mapped to different RepoSubdirs
// (the codex memories+chronicle shape, spec §3) must never alias each
// other's manifest entries. Without RepoSubdir folded into the
// per-unit manifest-key prefix, pass 3's dangling-entry cleanup for one
// unit would walk the OTHER unit's entries too (same prefix) and,
// finding no file at ITS OWN unitDir for that rel, silently delete the
// other unit's still-valid ledger entry every cycle.
func TestMirrorInSeparatesManifestKeysAcrossRepoSubdirUnits(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	memoriesLocal, chronicleLocal := t.TempDir(), t.TempDir()
	// Provider is "claude" (the only provider the test registry knows)
	// purely so the engine can resolve it; the mechanism under test —
	// per-unit manifest-key separation — is provider-agnostic.
	memories := repo.Unit{Provider: "claude", ProjectID: "codex-memories", Folder: "shared", LocalDir: memoriesLocal, RepoSubdir: "memories"}
	chronicle := repo.Unit{Provider: "claude", ProjectID: "codex-chronicle", Folder: "shared", LocalDir: chronicleLocal, RepoSubdir: "chronicle"}

	if err := os.WriteFile(filepath.Join(memoriesLocal, "note.md"), []byte("from memories\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chronicleLocal, "note.md"), []byte("from chronicle\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(context.Background(), []repo.Unit{memories, chronicle}, manifest); err != nil {
		t.Fatal(err)
	}

	if !manifest.Has("shared/claude/memories/note.md") {
		t.Fatal("manifest missing the memories unit's entry — key collided with chronicle's")
	}
	if !manifest.Has("shared/claude/chronicle/note.md") {
		t.Fatal("manifest missing the chronicle unit's entry — key collided with memories'")
	}

	memData, err := os.ReadFile(filepath.Join(checkout, "shared", "claude", "memories", "note.md"))
	if err != nil || string(memData) != "from memories\n" {
		t.Fatalf("memories content = %q, %v", memData, err)
	}
	chronData, err := os.ReadFile(filepath.Join(checkout, "shared", "claude", "chronicle", "note.md"))
	if err != nil || string(chronData) != "from chronicle\n" {
		t.Fatalf("chronicle content = %q, %v", chronData, err)
	}

	// A second cycle must be a clean no-op for BOTH units: pass 3
	// (ledger hygiene) must not cross-contaminate and drop either
	// unit's entry.
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{memories, chronicle}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 0 || stats.Deleted != 0 {
		t.Fatalf("second run stats = %+v, want zero copies/deletes", stats)
	}
	if !manifest.Has("shared/claude/memories/note.md") || !manifest.Has("shared/claude/chronicle/note.md") {
		t.Fatal("a manifest entry was dropped by the OTHER unit's ledger-hygiene pass")
	}
}

// TestMirrorInClassifiesThroughRepoSubdirNamespace pins the
// pattern-namespace contract end-to-end through the REAL mirrorInUnit
// call site — not just the standalone classifyRel function
// (TestUnitDirAndClassifyRel covers that in isolation). A provider's
// Patterns() globs are written against the provider-dir-relative shape
// (spec: "**/<provider>/<glob>", the same namespace GenerateAttributes
// emits), so a codex-shaped glob like "memories/*.tmp" only ignores a
// file at the LOCAL root of a RepoSubdir:"memories" unit if
// mirrorInUnit folds RepoSubdir into the rel it hands to Classify. If
// that wrapping were ever dropped from the call site, this test — not
// just the pure-function one — would catch it.
func TestMirrorInClassifiesThroughRepoSubdirNamespace(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	fake := providertest.New("codexlike", provider.ScopeGlobal, []provider.Pattern{
		{Glob: "memories/*.tmp", Class: provider.ClassIgnore},
	})
	registry, err := provider.NewRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(checkout, "host-a", registry, fixedNow)
	if err != nil {
		t.Fatal(err)
	}

	localDir := t.TempDir()
	u := repo.Unit{Provider: "codexlike", Folder: repo.GlobalFolder, LocalDir: localDir, RepoSubdir: "memories"}
	if err := os.WriteFile(filepath.Join(localDir, "scratch.tmp"), []byte("ignore me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "keep.md"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := repo.NewManifest()
	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Copied != 1 {
		t.Fatalf("Copied = %d, want 1 (only keep.md — scratch.tmp must be ignored)", stats.Copied)
	}
	if _, err := os.Stat(filepath.Join(checkout, "_global", "codexlike", "memories", "scratch.tmp")); !os.IsNotExist(err) {
		t.Fatal("scratch.tmp synced despite matching \"memories/*.tmp\" once RepoSubdir-namespaced — classifyRel not wired at the real call site")
	}
	if _, err := os.Stat(filepath.Join(checkout, "_global", "codexlike", "memories", "keep.md")); err != nil {
		t.Fatal("keep.md should have synced:", err)
	}
}

// TestMirrorInDropsStaleManifestEntries covers pass 3 (ledger hygiene):
// verified against the actual code, an entry whose file is gone from
// both the provider dir and the checkout is dropped from the manifest
// silently and is NOT counted in stats.Deleted — pass 2 (the only pass
// that increments Deleted) only ever visits files that still exist on
// the checkout side, so it never sees this path. Nothing was deleted
// this cycle; this is stale-bookkeeping cleanup only.
func TestMirrorInDropsStaleManifestEntries(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	u := unit(t, "alpha")

	manifest := repo.NewManifest()
	entry := repo.ManifestEntry{Size: 4, MTimeUnixNano: 1, SHA256: "deadbeef"}
	if err := manifest.Set("alpha/claude/memories/stale.md", entry); err != nil {
		t.Fatal(err)
	}

	stats, _, err := engine.mirrorIn(context.Background(), []repo.Unit{u}, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Has("alpha/claude/memories/stale.md") {
		t.Fatal("stale manifest entry (gone from both sides) was not dropped")
	}
	if stats.Deleted != 0 {
		t.Fatalf("Deleted = %d, want 0 — pass 3 is ledger hygiene, not a this-cycle deletion", stats.Deleted)
	}
}
