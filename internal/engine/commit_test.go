package engine

import (
	"context"
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
