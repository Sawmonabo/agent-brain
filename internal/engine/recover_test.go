package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// TestRecoverAbortsStrandedRebase manufactures the crash the spec's
// recovery contract exists for: a rebase that stopped mid-conflict
// (driver failure) and was never aborted — daemon killed, WSL2 torn
// down. recoverState must return the checkout to a clean state.
func TestRecoverAbortsStrandedRebase(t *testing.T) {
	t.Parallel()
	checkout, bare := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	mustGit(t, checkout, "config", "merge.agentbrain.driver", "false")
	mustGit(t, checkout, "config", "merge.agentbrain-lww.driver", "false")

	conflictPath := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, conflictPath, "ours\n", "ours")
	other := secondClone(t, bare)
	commitFileOn(t, other, conflictPath, "theirs\n", "theirs")
	mustGit(t, other, "push", "origin", "main")

	mustGit(t, checkout, "fetch", "origin")
	// Raw rebase, deliberately NOT aborted — the stranded state.
	if res, err := gitx.RunStatus(context.Background(), checkout, "rebase", "origin/main"); err != nil || res.ExitCode == 0 {
		t.Fatalf("expected rebase to stop on conflict, got exit %d err %v", res.ExitCode, err)
	}

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	gitDir := filepath.Join(checkout, ".git")
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD"} {
		if _, err := os.Lstat(filepath.Join(gitDir, marker)); !os.IsNotExist(err) {
			t.Fatalf("%s still present after recovery", marker)
		}
	}
}

func TestRecoverIsNoopOnCleanCheckout(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)
	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRecoverStateResetsStagedDeletion reproduces the F3 wedge shape: a
// crash left a staged deletion (git rm --cached) with the worktree file
// still present. recoverState must clear the staged entry and leave the
// worktree file untouched — a mixed reset, never --hard.
func TestRecoverStateResetsStagedDeletion(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	rel := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, rel, "committed content\n", "add clash")
	mustGit(t, checkout, "rm", "--cached", "--quiet", "--", rel)

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}

	diffCached, err := gitx.RunStatus(context.Background(), checkout, "diff", "--cached", "--quiet")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if diffCached.ExitCode != 0 {
		t.Fatalf("index still differs from HEAD after recoverState, exit %d", diffCached.ExitCode)
	}

	full := filepath.Join(checkout, filepath.FromSlash(rel))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("worktree file missing after recoverState: %v", err)
	}
	if string(got) != "committed content\n" {
		t.Fatalf("worktree file changed by recoverState: got %q", got)
	}
}

// TestRecoverStateResetsStagedModification stages a content change
// (git add after editing) and asserts recoverState unstages it while the
// EDITED bytes stay in the worktree (mirror-in owns reconciling them).
func TestRecoverStateResetsStagedModification(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	rel := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, rel, "original content\n", "add clash")

	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.WriteFile(full, []byte("EDITED content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, checkout, "add", "--", rel)

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}

	diffCached, err := gitx.RunStatus(context.Background(), checkout, "diff", "--cached", "--quiet")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if diffCached.ExitCode != 0 {
		t.Fatalf("index still differs from HEAD after recoverState, exit %d", diffCached.ExitCode)
	}

	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("worktree file missing after recoverState: %v", err)
	}
	if string(got) != "EDITED content\n" {
		t.Fatalf("worktree edit lost after recoverState: got %q", got)
	}
}

// TestRecoverStateNoopOnCleanIndex asserts a clean checkout stays
// byte-for-byte clean: git status --porcelain empty before and after,
// and no error — recoverState runs at the top of EVERY cycle, so the
// happy path must be free of churn.
func TestRecoverStateNoopOnCleanIndex(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	before := mustGit(t, checkout, "status", "--porcelain")
	if before.Stdout != "" {
		t.Fatalf("checkout not clean before recoverState: %q", before.Stdout)
	}

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}

	after := mustGit(t, checkout, "status", "--porcelain")
	if after.Stdout != "" {
		t.Fatalf("recoverState dirtied a clean checkout: %q", after.Stdout)
	}
}

// TestRecoverStateSurvivesUnbornHead runs recoverState in a git init'd
// checkout with no commits (init's window before the first skeleton
// commit) and asserts it returns nil — the rev-parse HEAD gate.
func TestRecoverStateSurvivesUnbornHead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	checkout := filepath.Join(root, "memories")
	mustGit(t, root, "init", "--quiet", "-b", "main", checkout)
	engine := newTestEngine(t, checkout)

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
}
