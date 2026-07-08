package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// secondClone clones the same bare remote as an independent "machine".
func secondClone(t *testing.T, bare string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "memories-b")
	mustGit(t, t.TempDir(), "clone", bare, other)
	mustGit(t, other, "config", "user.name", "engine-test-b")
	mustGit(t, other, "config", "user.email", "engine-test-b@example.invalid")
	return other
}

func commitFileOn(t *testing.T, checkout, rel, content, message string) {
	t.Helper()
	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "-A", "--", rel)
	mustGit(t, checkout, "commit", "--quiet", "-m", message)
}

func TestIntegrateFastForwardsWhenBehind(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	other := secondClone(t, bare)
	commitFileOn(t, other, "alpha/claude/memories/from-b.md", "B's fact\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated || outcome.Offline || len(outcome.Degraded) != 0 {
		t.Fatalf("outcome = %+v, want clean integration", outcome)
	}
	if _, err := os.Stat(filepath.Join(checkout, "alpha", "claude", "memories", "from-b.md")); err != nil {
		t.Fatal("remote file did not land:", err)
	}
}

func TestIntegrateRebasesLocalCommitsLinearly(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	commitFileOn(t, checkout, "alpha/claude/memories/local.md", "local fact\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, "beta/claude/memories/remote.md", "remote fact\n", "memory: host-b beta ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Integrated {
		t.Fatalf("outcome = %+v, want Integrated", outcome)
	}
	for _, rel := range []string{"alpha/claude/memories/local.md", "beta/claude/memories/remote.md"} {
		if _, err := os.Stat(filepath.Join(checkout, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing after integrate: %v", rel, err)
		}
	}
	// Rebase, not merge: exactly the one local commit sits atop origin/main.
	ahead := mustGit(t, checkout, "rev-list", "--count", "origin/main..HEAD")
	if got := strings.TrimSpace(ahead.Stdout); got != "1" {
		t.Fatalf("commits ahead = %s, want 1 (linear rebase)", got)
	}
}

func TestIntegrateOfflineIsNotAnError(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "vanished.git"))

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal("offline fetch must not error:", err)
	}
	if !outcome.Offline || outcome.Integrated {
		t.Fatalf("outcome = %+v, want Offline", outcome)
	}
}

// TestIntegrateDriverFailureDegradesProject forces the exact spec §4
// scenario: the merge driver unexpectedly fails (driver = `false`), the
// rebase aborts clean, the merge-commit fallback also fails, and the
// owning project is degraded while the checkout returns to its
// pre-integrate state.
func TestIntegrateDriverFailureDegradesProject(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "config", "merge.agentbrain.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain.driver", "false")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.name", "failing driver")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.driver", "false")

	conflictPath := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, conflictPath, "ours\n", "memory: host-a alpha ts")
	other := secondClone(t, bare)
	commitFileOn(t, other, conflictPath, "theirs\n", "memory: host-b alpha ts")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want project-scoped degradation", outcome)
	}
	if len(outcome.Degraded) != 1 || outcome.Degraded[0] != "alpha" {
		t.Fatalf("Degraded = %v, want [alpha]", outcome.Degraded)
	}
	// Aborts restored the local state: no rebase/merge in progress, ours intact.
	gitDir := filepath.Join(checkout, ".git")
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, marker)); !os.IsNotExist(err) {
			t.Fatalf("stranded %s after aborts", marker)
		}
	}
	data, err := os.ReadFile(filepath.Join(checkout, filepath.FromSlash(conflictPath)))
	if err != nil || string(data) != "ours\n" {
		t.Fatalf("local content = %q, %v; want pre-integrate state", data, err)
	}
}

func TestIntegrateMetaConflictDegradesAll(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	// .agent-brain/** carries `-merge` (Task 2 attributes): an add/add
	// there cannot content-merge, forcing the degrade-all path.
	notePath := repo.MetaDirName + "/note.txt"
	commitFileOn(t, checkout, notePath, "ours\n", "meta ours")
	other := secondClone(t, bare)
	commitFileOn(t, other, notePath, "theirs\n", "meta theirs")
	mustGit(t, other, "push", "origin", "main")

	outcome, err := engine.integrate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Integrated || !outcome.DegradedAll {
		t.Fatalf("outcome = %+v, want DegradedAll", outcome)
	}
}
