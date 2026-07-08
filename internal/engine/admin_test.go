package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
