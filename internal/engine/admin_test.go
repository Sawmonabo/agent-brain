package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/keys"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// commitCount is the number of commits reachable from HEAD.
func commitCount(t *testing.T, checkout string) int {
	t.Helper()
	out := strings.TrimSpace(mustGit(t, checkout, "rev-list", "--count", "HEAD").Stdout)
	n, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("parse commit count %q: %v", out, err)
	}
	return n
}

// lastSubject is HEAD's commit subject.
func lastSubject(t *testing.T, checkout string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, checkout, "log", "-1", "--format=%s").Stdout)
}

func writeSeedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("expected %s to be absent, but it exists", path)
	}
}

// plantResidentPoison commits a git-meta file into the checkout — the state
// a fresh clone of a poisoned main materializes (F1, Phase-3 final review):
// no earlier cycle of THIS machine has ever scrubbed it.
func plantResidentPoison(t *testing.T, checkout, rel string) {
	t.Helper()
	writeCheckout(t, checkout, rel, "* -filter\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "simulate poisoned clone")
}

// assertPoisonHealed asserts rel is gone from the worktree AND from HEAD's
// tree — the admin op scrubbed it and its own commit carried the heal.
func assertPoisonHealed(t *testing.T, checkout, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(checkout, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Fatalf("resident poison %q still on disk after the admin op", rel)
	}
	if _, err := gitx.Run(context.Background(), checkout, "cat-file", "-e", "HEAD:"+rel); err == nil {
		t.Fatalf("resident poison %q still in HEAD's tree after the admin op", rel)
	}
}

// TestAdminOpsScrubResidentGitMetaPoison pins the F1 boundary rule for the
// three standalone admin commits: every busy-guarded engine entry point that
// can create a commit scrubs resident git-meta poison FIRST, so no `git add`
// ever consults hostile attributes. The Sync entry point is pinned at the
// wire level by the adversarial corpus (fresh_join_resident_folder_poison);
// these pins are the fast unit-level equivalents for register/purge/seed.
// The poison sits OUTSIDE each op's own pathspec where that is possible, so
// the pin proves a WHOLE-CHECKOUT scrub, not an op-scoped one.
func TestAdminOpsScrubResidentGitMetaPoison(t *testing.T) {
	t.Parallel()

	t.Run("register", func(t *testing.T) {
		t.Parallel()
		checkout, _ := newTestCheckout(t)
		e := newTestEngine(t, checkout)
		ctx := context.Background()
		plantResidentPoison(t, checkout, "alpha/.gitattributes")

		if _, err := e.RegisterProject(ctx, "claude", "id-beta", "beta"); err != nil {
			t.Fatal(err)
		}
		assertPoisonHealed(t, checkout, "alpha/.gitattributes")
	})

	t.Run("purge", func(t *testing.T) {
		t.Parallel()
		checkout, _ := newTestCheckout(t)
		e := newTestEngine(t, checkout)
		ctx := context.Background()
		folder, err := e.RegisterProject(ctx, "claude", "id-alpha", "alpha")
		if err != nil {
			t.Fatal(err)
		}
		writeCheckout(t, checkout, "alpha/claude/note.md", "hi\n")
		mustGit(t, checkout, "add", "-A")
		mustGit(t, checkout, "commit", "--quiet", "-m", "seed alpha")
		plantResidentPoison(t, checkout, "gamma/.gitattributes")

		if err := e.PurgeProject(ctx, folder); err != nil {
			t.Fatal(err)
		}
		assertPoisonHealed(t, checkout, "gamma/.gitattributes")
	})

	t.Run("seed", func(t *testing.T) {
		t.Parallel()
		checkout, _ := newTestCheckout(t)
		e := newTestEngine(t, checkout)
		ctx := context.Background()
		plantResidentPoison(t, checkout, "alpha/.gitattributes")
		src := t.TempDir()
		writeSeedFile(t, filepath.Join(src, "imported.md"), "legacy\n")

		report, err := e.SeedProject(ctx, "alpha", "claude", "my-slug", src)
		if err != nil {
			t.Fatal(err)
		}
		if report.Files != 1 || report.Skipped {
			t.Fatalf("seed report = %+v, want 1 file / not skipped", report)
		}
		assertPoisonHealed(t, checkout, "alpha/.gitattributes")
		assertExists(t, filepath.Join(checkout, "alpha", "claude", "imported.md"))
	})
}

// TestAdminOpsRecoverCrashedRebase pins prepareCheckout's other half for the
// admin entry points. A daemon crash mid-integrate leaves a STOPPED rebase:
// detached HEAD plus .git/rebase-merge. Sync's recovery aborts it at the next
// cycle — but an admin op can be the FIRST engine entry after a restart.
// Without the same recovery, the op would either refuse (unmerged index
// entries) or commit onto the detached HEAD, and the eventual abort would
// orphan that commit — a silently lost purge.
func TestAdminOpsRecoverCrashedRebase(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	folder, err := e.RegisterProject(ctx, "claude", "id-alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// A genuinely stopped rebase: divergent edits to one memory file.
	writeCheckout(t, checkout, "alpha/claude/note.md", "base\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "base")
	mustGit(t, checkout, "checkout", "--quiet", "-b", "side")
	writeCheckout(t, checkout, "alpha/claude/note.md", "side\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "side edit")
	mustGit(t, checkout, "checkout", "--quiet", "main")
	writeCheckout(t, checkout, "alpha/claude/note.md", "main\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "main edit")
	if _, err := gitx.Run(ctx, checkout, "rebase", "side"); err == nil {
		t.Fatal("fixture rebase unexpectedly succeeded — it must stop on a conflict")
	}

	if err := e.PurgeProject(ctx, folder); err != nil {
		t.Fatalf("purge as the first engine entry after a crashed rebase: %v", err)
	}
	// The op recovered first: no rebase in progress, HEAD attached to main,
	// and the purge commit is ON main, not orphaned on a detached HEAD.
	if _, err := os.Lstat(filepath.Join(checkout, ".git", "rebase-merge")); !os.IsNotExist(err) {
		t.Fatal("rebase-merge state still present after the admin op")
	}
	branch := strings.TrimSpace(mustGit(t, checkout, "rev-parse", "--abbrev-ref", "HEAD").Stdout)
	if branch != "main" {
		t.Fatalf("HEAD = %q after the admin op, want main", branch)
	}
	if got := lastSubject(t, checkout); got != "purge: alpha (host-a)" {
		t.Fatalf("purge commit not at HEAD: subject = %q", got)
	}
}

func TestRegisterProjectIdempotent(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	before := commitCount(t, checkout)
	folder, err := e.RegisterProject(ctx, "claude", "id-alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "alpha" {
		t.Fatalf("folder = %q, want alpha", folder)
	}
	if got := commitCount(t, checkout); got != before+1 {
		t.Fatalf("commit count = %d, want %d (one registration commit)", got, before+1)
	}
	// The registration rides commit.go's meta convention (projects.toml is
	// machine-shared metadata like the manifest).
	if got := lastSubject(t, checkout); got != "memory: host-a manifest "+fixedStamp {
		t.Fatalf("registration subject = %q, want commit.go meta convention", got)
	}
	if info, err := os.Stat(filepath.Join(checkout, "alpha", "claude")); err != nil || !info.IsDir() {
		t.Fatalf("alpha/claude dir missing after register: %v", err)
	}

	// Re-registering the same id returns the same folder with no new commit.
	folder2, err := e.RegisterProject(ctx, "claude", "id-alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if folder2 != "alpha" {
		t.Fatalf("second folder = %q, want alpha", folder2)
	}
	if got := commitCount(t, checkout); got != before+1 {
		t.Fatalf("commit count = %d after idempotent re-register, want %d", got, before+1)
	}
}

func TestRegisterProjectCollision(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	first, err := e.RegisterProject(ctx, "claude", "id-one", "shared")
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.RegisterProject(ctx, "claude", "id-two", "shared")
	if err != nil {
		t.Fatal(err)
	}
	// Disambiguation delegates to Projects.Add — asserted through the engine.
	if first != "shared" || second != "shared-2" {
		t.Fatalf("folders = %q, %q; want shared, shared-2", first, second)
	}
	projects, err := repo.LoadProjects(repo.NewLayout(checkout).ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	if f, _ := projects.FolderFor("id-one"); f != "shared" {
		t.Fatalf("id-one → %q, want shared", f)
	}
	if f, _ := projects.FolderFor("id-two"); f != "shared-2" {
		t.Fatalf("id-two → %q, want shared-2", f)
	}
}

func TestPurgeProject(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	folder, err := e.RegisterProject(ctx, "claude", "id-alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	// Give the folder tracked content so the purge has something to remove.
	dir := filepath.Join(checkout, folder, "claude")
	writeSeedFile(t, filepath.Join(dir, "note.md"), "hi\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "seed alpha")

	before := commitCount(t, checkout)
	if err := e.PurgeProject(ctx, folder); err != nil {
		t.Fatal(err)
	}
	// One commit removes both the folder and the projects.toml entry.
	if got := commitCount(t, checkout); got != before+1 {
		t.Fatalf("commit count = %d, want %d (one purge commit)", got, before+1)
	}
	if tracked := strings.TrimSpace(mustGit(t, checkout, "ls-files", folder).Stdout); tracked != "" {
		t.Fatalf("git ls-files %s = %q, want empty", folder, tracked)
	}
	projects, err := repo.LoadProjects(repo.NewLayout(checkout).ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := projects.FolderFor("id-alpha"); ok {
		t.Fatal("projects.toml still records id-alpha after purge")
	}
}

func TestSeedProject(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	src := t.TempDir()
	writeSeedFile(t, filepath.Join(src, "keep.md"), "keep\n")
	writeSeedFile(t, filepath.Join(src, "sub", "topic.md"), "topic\n")
	writeSeedFile(t, filepath.Join(src, ".lock"), "lock\n")
	writeSeedFile(t, filepath.Join(src, "x.sync-pending"), "pending\n")
	// Hostile git-meta at the root and nested: the seed must refuse both
	// (spec §5 git-meta scrub contract), or a poisoned .gitattributes could
	// unscope the encryption filter for the seeded subtree.
	writeSeedFile(t, filepath.Join(src, ".gitattributes"), "* -filter\n")
	writeSeedFile(t, filepath.Join(src, "evil", ".gitignore"), "*\n")

	before := commitCount(t, checkout)
	report, err := e.SeedProject(ctx, "alpha", "claude", "my-slug", src)
	if err != nil {
		t.Fatal(err)
	}
	if report.Files != 2 || report.Skipped || report.Folder != "alpha" {
		t.Fatalf("report = %+v, want 2 files / not skipped / folder alpha", report)
	}
	if got := commitCount(t, checkout); got != before+1 {
		t.Fatalf("commit count = %d, want %d (one seed commit)", got, before+1)
	}
	if got := lastSubject(t, checkout); got != "migrate: seed alpha from host-a:my-slug" {
		t.Fatalf("seed subject = %q", got)
	}

	dir := filepath.Join(checkout, "alpha", "claude")
	assertExists(t, filepath.Join(dir, "keep.md"))
	assertExists(t, filepath.Join(dir, "sub", "topic.md"))
	assertAbsent(t, filepath.Join(dir, ".lock"))
	assertAbsent(t, filepath.Join(dir, "x.sync-pending"))
	assertAbsent(t, filepath.Join(dir, ".gitattributes"))
	assertAbsent(t, filepath.Join(dir, "evil"))

	// One commit carries the two files AND the manifest marker.
	tracked := mustGit(t, checkout, "ls-tree", "-r", "--name-only", "HEAD").Stdout
	for _, want := range []string{
		"alpha/claude/keep.md",
		"alpha/claude/sub/topic.md",
		".agent-brain/manifests/host-a.json",
	} {
		if !strings.Contains(tracked, want) {
			t.Fatalf("seed commit missing %q:\n%s", want, tracked)
		}
	}
	for _, absent := range []string{".gitattributes\n", "alpha/claude/.gitignore", "alpha/claude/evil"} {
		if strings.Contains(tracked, "alpha/claude/"+absent) {
			t.Fatalf("seed commit leaked git-meta %q:\n%s", absent, tracked)
		}
	}
	manifest, err := repo.LoadManifest(repo.NewLayout(checkout).ManifestFile("host-a"))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ImportedFrom["my-slug"] != "alpha" {
		t.Fatalf("ImportedFrom = %+v, want my-slug→alpha", manifest.ImportedFrom)
	}

	// Rerun no-ops forever after via the marker: skipped, no new commit.
	countBefore := commitCount(t, checkout)
	report2, err := e.SeedProject(ctx, "alpha", "claude", "my-slug", src)
	if err != nil {
		t.Fatal(err)
	}
	if !report2.Skipped {
		t.Fatalf("rerun report = %+v, want Skipped", report2)
	}
	if got := commitCount(t, checkout); got != countBefore {
		t.Fatalf("rerun produced a commit: count %d → %d", countBefore, got)
	}
}

// TestReencryptAllRenormalizesCommitsPushes is the ReencryptAll wire proof
// through REAL filters: after a keyset rotation, `git add --renormalize` re-runs
// the clean filter over every filtered file, so deterministic AEAD re-seals
// EVERY memory blob under the new primary. Exactly one commit lands, both
// blobs' stored ciphertext changes on the fake remote (staying ciphertext,
// never leaking plaintext), and the worktree plaintext is byte-identical
// (renormalize never touches the worktree).
//
// Non-parallel: it uses t.Setenv to give the real-binary filters a PRIVATE
// keyset dir it can rotate in isolation, and t.Setenv forbids t.Parallel.
func TestReencryptAllRenormalizesCommitsPushes(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	checkout, bare := newEncryptedCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	// Two committed, pushed memory files — ciphertext on the wire under K1.
	const oneText, twoText = "first fact about the build\n", "second fact about the build\n"
	writeCheckout(t, checkout, "alpha/claude/one.md", oneText)
	writeCheckout(t, checkout, "alpha/claude/two.md", twoText)
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "seed two facts")
	mustGit(t, checkout, "push", "--quiet", "origin", "main")

	oneBefore := remoteBlobBytes(t, bare, "alpha/claude/one.md")
	twoBefore := remoteBlobBytes(t, bare, "alpha/claude/two.md")
	if !strings.HasPrefix(oneBefore, magicPrefix) || !strings.HasPrefix(twoBefore, magicPrefix) {
		t.Fatal("precondition: seeded memory is not ciphertext on the wire (filters not wired?)")
	}

	// Rotate → new primary K2 (K1 retained), then re-encrypt the whole repo.
	if err := keys.Rotate(keysetPath); err != nil {
		t.Fatal(err)
	}
	before := commitCount(t, checkout)
	report, err := e.ReencryptAll(ctx)
	if err != nil {
		t.Fatalf("ReencryptAll: %v", err)
	}

	if report.Files != 2 {
		t.Fatalf("report.Files = %d, want 2", report.Files)
	}
	if !report.Pushed || report.PushQueued {
		t.Fatalf("report = %+v, want Pushed=true PushQueued=false", report)
	}
	if got := commitCount(t, checkout); got != before+1 {
		t.Fatalf("commit count = %d, want %d (exactly one re-encrypt commit)", got, before+1)
	}
	if got := lastSubject(t, checkout); got != "chore(key): rotate primary key" {
		t.Fatalf("re-encrypt subject = %q, want chore(key): rotate primary key", got)
	}

	// Both blobs' stored ciphertext changed on the wire, stayed ciphertext,
	// and never leaked plaintext.
	oneAfter := remoteBlobBytes(t, bare, "alpha/claude/one.md")
	twoAfter := remoteBlobBytes(t, bare, "alpha/claude/two.md")
	if oneAfter == oneBefore || twoAfter == twoBefore {
		t.Fatal("a memory blob did not change on the wire after re-encrypt under the new primary")
	}
	for _, blob := range []string{oneAfter, twoAfter} {
		if !strings.HasPrefix(blob, magicPrefix) {
			t.Fatal("re-encrypted blob is not agent-brain ciphertext")
		}
	}
	if strings.Contains(oneAfter, "first fact") || strings.Contains(twoAfter, "second fact") {
		t.Fatal("SAFETY VIOLATION: plaintext memory content in a re-encrypted git object")
	}

	// Worktree plaintext is byte-identical — renormalize re-stages the index,
	// never the working tree.
	if got := readCheckout(t, checkout, "alpha/claude/one.md"); got != oneText {
		t.Fatalf("worktree one.md = %q, want plaintext unchanged", got)
	}
	if got := readCheckout(t, checkout, "alpha/claude/two.md"); got != twoText {
		t.Fatalf("worktree two.md = %q, want plaintext unchanged", got)
	}
}

// TestReencryptAllNoopWedgeFree pins that a re-encrypt with no primary change
// is a clean no-op, not a wedge: the first ReencryptAll (after a rotation) does
// the work; a second one, primary now unchanged, renormalizes to zero staged
// files, makes no commit, and reports Files == 0.
//
// Non-parallel for the same t.Setenv keyset-isolation reason as above.
func TestReencryptAllNoopWedgeFree(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("AGENT_BRAIN_CONFIG_DIR", configDir)
	keysetPath := filepath.Join(configDir, "keyset.json")
	if err := keys.Generate(keysetPath); err != nil {
		t.Fatal(err)
	}

	checkout, _ := newEncryptedCheckout(t)
	e := newTestEngine(t, checkout)
	ctx := context.Background()

	writeCheckout(t, checkout, "alpha/claude/fact.md", "a durable fact\n")
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "--quiet", "-m", "seed a fact")
	mustGit(t, checkout, "push", "--quiet", "origin", "main")

	if err := keys.Rotate(keysetPath); err != nil {
		t.Fatal(err)
	}
	first, err := e.ReencryptAll(ctx)
	if err != nil {
		t.Fatalf("first ReencryptAll: %v", err)
	}
	if first.Files != 1 {
		t.Fatalf("first ReencryptAll Files = %d, want 1", first.Files)
	}

	// No rotation between the two runs → the primary is unchanged, so
	// renormalize re-seals every blob to the SAME bytes: nothing to stage.
	before := commitCount(t, checkout)
	second, err := e.ReencryptAll(ctx)
	if err != nil {
		t.Fatalf("second ReencryptAll: %v", err)
	}
	if second.Files != 0 {
		t.Fatalf("second ReencryptAll Files = %d, want 0 (no primary change → no-op)", second.Files)
	}
	if second.Pushed || second.PushQueued {
		t.Fatalf("second ReencryptAll report = %+v, want no push (nothing committed)", second)
	}
	if got := commitCount(t, checkout); got != before {
		t.Fatalf("second ReencryptAll committed: count %d → %d, want no new commit", before, got)
	}
}
