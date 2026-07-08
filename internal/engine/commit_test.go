package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

func TestCommitProjectsOneCommitPerProject(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	alpha, beta := unit(t, "alpha"), unit(t, "beta")
	writeLocal(t, alpha, "memories/a.md", "A\n")
	writeLocal(t, beta, "memories/b.md", "B\n")
	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{alpha, beta}, manifest); err != nil {
		t.Fatal(err)
	}
	if err := manifest.Save(engine.layout.ManifestFile(engine.host)); err != nil {
		t.Fatal(err)
	}

	subjects, err := engine.commitProjects(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	wantSubjects := []string{
		"memory: host-a alpha " + fixedStamp,
		"memory: host-a beta " + fixedStamp,
	}
	if len(subjects) != 2 || subjects[0] != wantSubjects[0] || subjects[1] != wantSubjects[1] {
		t.Fatalf("subjects = %v, want %v", subjects, wantSubjects)
	}

	metaSubject, err := engine.commitMeta(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	if want := "memory: host-a manifest " + fixedStamp; metaSubject != want {
		t.Fatalf("meta subject = %q, want %q", metaSubject, want)
	}

	status := mustGit(t, checkout, "status", "--porcelain")
	if strings.TrimSpace(status.Stdout) != "" {
		t.Fatalf("tree dirty after commits:\n%s", status.Stdout)
	}
	log := mustGit(t, checkout, "log", "--format=%s", "-n", "3")
	got := strings.Split(strings.TrimSpace(log.Stdout), "\n")
	want := []string{
		"memory: host-a manifest " + fixedStamp,
		"memory: host-a beta " + fixedStamp,
		"memory: host-a alpha " + fixedStamp,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("log[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestChangedTopLevelsHandlesRenames covers the porcelain -z rename
// record (`R  <dest>\0<origin>\0`, verified against real git output):
// moving a file across top-level folders must mark BOTH the origin and
// destination folders changed, not just the destination that `record`
// itself carries.
func TestChangedTopLevelsHandlesRenames(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	alpha, beta := unit(t, "alpha"), unit(t, "beta")
	writeLocal(t, alpha, "memories/a.md", "A\n")
	writeLocal(t, beta, "memories/seed.md", "seed\n")
	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{alpha, beta}, manifest); err != nil {
		t.Fatal(err)
	}
	// Commit the seed state directly (not via commitProjects, which
	// every other test already exercises): this test's subject is
	// changedTopLevels' own porcelain parsing, not the commit helper.
	mustGit(t, checkout, "add", "-A")
	mustGit(t, checkout, "commit", "-m", "seed")

	mustGit(t, checkout, "mv",
		"alpha/claude/memories/a.md",
		"beta/claude/memories/a-moved.md")

	folders, err := engine.changedTopLevels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "beta"}
	if len(folders) != len(want) || folders[0] != want[0] || folders[1] != want[1] {
		t.Fatalf("changedTopLevels = %v, want %v", folders, want)
	}
}

// TestCommitProjectsAfterLastFileDeleted covers the case where mirrorIn's
// deletion pass `git rm`s a project folder's only file: git removes the
// now-empty folder from the working tree while staging the deletion in
// the index, so commitPaths must not rely on `git add -A -- <folder>`
// succeeding (it fails with exit 128 "pathspec did not match any files"
// once the folder is gone from the working tree).
func TestCommitProjectsAfterLastFileDeleted(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	alpha := unit(t, "alpha")
	writeLocal(t, alpha, "memories/ephemeral.md", "short-lived\n")
	// manifest stays in-memory across both mirrorIn calls below (like a
	// single long-lived Engine would hold it mid-Sync); no disk
	// round-trip is needed to reproduce the bug, and saving it here
	// would leave an untracked, never-committed .agent-brain/ behind
	// since this test never calls commitMeta.
	manifest := repo.NewManifest()
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{alpha}, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.commitProjects(ctx, fixedStamp); err != nil {
		t.Fatal(err)
	}

	// Delete the only file on the provider side, then mirror in again:
	// this is what stages the git rm and removes the now-empty folder
	// from the working tree.
	if err := os.Remove(filepath.Join(alpha.LocalDir, "memories", "ephemeral.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.mirrorIn(ctx, []repo.Unit{alpha}, manifest); err != nil {
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

	if _, err := os.Stat(filepath.Join(checkout, "alpha")); !os.IsNotExist(err) {
		t.Fatalf("checkout dir for alpha still present: err = %v", err)
	}
	status := mustGit(t, checkout, "status", "--porcelain")
	if strings.TrimSpace(status.Stdout) != "" {
		t.Fatalf("tree dirty after commit:\n%s", status.Stdout)
	}
}

func TestCommitsAreNoopsWhenClean(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	ctx := context.Background()

	subjects, err := engine.commitProjects(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	metaSubject, err := engine.commitMeta(ctx, fixedStamp)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 || metaSubject != "" {
		t.Fatalf("clean tree produced commits: %v %q", subjects, metaSubject)
	}
}
