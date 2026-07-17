package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
// worktree file untouched.
//
// REVISED (deliberate, reviewed semantics change): recoverState
// is no longer a blanket "never touch the worktree" — after the staged-index
// reset it also RESTORES stray worktree-only deletions from HEAD (the
// crash-window heal). The safe signature that makes restoration always correct:
// a legitimate memory deletion ONLY ever arrives STAGED, via mirror-in's
// `git rm` in the same cycle; an UNSTAGED worktree-only deletion at cycle start
// is ALWAYS git residue (an interrupted checkout-update or a crash mid-integrate)
// and never user intent — and if the provider copy is also gone, mirror-in
// re-detects and re-stages the deletion THIS cycle, so restoration is safe even
// then. This row stays a NO-RESTORE case precisely because it is not a
// worktree-only deletion: the `git rm --cached` leaves the worktree copy
// present, so the heal's `git ls-files --deleted` never lists it and the mixed
// reset alone (never --hard) heals the index. See
// TestRecoverStateRestoresStrayWorktreeDeletion for the restore path.
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
//
// REVISED: the crash-window heal is DELETIONS ONLY — it restores
// stray worktree deletions from HEAD but never rewinds a worktree
// MODIFICATION. A modified file is present, not missing, so the heal's
// `git ls-files --deleted` never lists it; mirror-in legitimately overwrites
// such edits (they converge next cycle), so preserving the worktree bytes here
// is correct. This row pins that deletions-only scope: recoverState now touches
// the worktree for deletions, yet leaves an edited file's bytes intact.
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

// TestRecoverStateRestoresStrayWorktreeDeletion is the crash-window heal (the
// sibling of the in-integrate worktree heal in integrate.go). It constructs the
// residue a smudge-stranded rebase leaves after git's own --abort restores
// HEAD and the index but NOT the worktree: a stray UNSTAGED worktree deletion,
// with no staged component. The staged-index reset never touches it — the
// ` D` (worktree-only) deletion is invisible to `git diff --cached`. recoverState
// must restore the file from HEAD so the next cycle's commitProjects cannot
// `git add -A` it into a phantom deletion that mirror-out then propagates
// fleet-wide. The provider copy is present here (this test's checkout has no
// enrolled provider), modelling the pure-residue case where restoration is the
// whole fix; the provider-absent interrupted-deletion path is covered by the
// full-cycle e2e (test/e2e/crash_window_test.go).
func TestRecoverStateRestoresStrayWorktreeDeletion(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	rel := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, rel, "committed content\n", "add clash")

	full := filepath.Join(checkout, filepath.FromSlash(rel))
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	// Precondition: the deletion is worktree-only (unstaged) — the exact shape
	// git's --abort leaves, and the exact shape the staged reset ignores.
	porcelain := mustGit(t, checkout, "status", "--porcelain", "--", rel)
	if !strings.HasPrefix(porcelain.Stdout, " D") {
		t.Fatalf("expected an unstaged worktree deletion, got porcelain %q", porcelain.Stdout)
	}

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("stray worktree deletion not restored by recoverState: %v", err)
	}
	if string(got) != "committed content\n" {
		t.Fatalf("restored content = %q, want %q", got, "committed content\n")
	}
	// The heal leaves the checkout byte-for-byte at HEAD: index and worktree
	// both clean, no residual staged change from the restore.
	clean := mustGit(t, checkout, "status", "--porcelain")
	if clean.Stdout != "" {
		t.Fatalf("checkout not clean after heal: %q", clean.Stdout)
	}
}

// TestRecoverStateResetThenHealRestoresGitRmDeletion pins the reset→heal
// COMPOSITION end-to-end — the crash-after-`git rm` shape that justifies
// running the deletion heal AFTER the staged-index reset (recover.go decision).
// `git rm` (not --cached) removes the file from BOTH the index and the
// worktree, so a crash between mirror-in's `git rm` and its commit lands as a
// STAGED deletion whose worktree copy is also gone (`D ` porcelain).
//
// Neither half heals this alone, and the ORDER is load-bearing: the staged
// reset alone rewinds the index to HEAD but leaves the worktree deletion
// stranded (` D`); the heal alone, run FIRST, would miss it, because
// `git ls-files --deleted` lists paths IN the index but missing from the
// worktree and the staged `git rm` has already dropped the path from the index.
// Only reset THEN heal restores it — the reset re-adds the path to the index
// (== HEAD), turning the staged deletion into the worktree-only deletion the
// heal then lists and checks out. A future reorder that breaks this ordering
// fails this row.
func TestRecoverStateResetThenHealRestoresGitRmDeletion(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	rel := "alpha/claude/memories/clash.md"
	commitFileOn(t, checkout, rel, "committed content\n", "add clash")

	// `git rm` drops the file from the worktree AND stages the deletion — the
	// crash-after-mirror-in-`git rm` shape. `D ` (index=D, worktree=blank) is
	// its porcelain signature, distinct from the ` D` unstaged residue above.
	mustGit(t, checkout, "rm", "--quiet", "--", rel)
	staged := mustGit(t, checkout, "status", "--porcelain", "--", rel)
	if !strings.HasPrefix(staged.Stdout, "D ") {
		t.Fatalf("expected a staged deletion with the worktree copy also gone, got porcelain %q", staged.Stdout)
	}

	if err := engine.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}

	full := filepath.Join(checkout, filepath.FromSlash(rel))
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("git rm residue not restored by recoverState: %v", err)
	}
	if string(got) != "committed content\n" {
		t.Fatalf("restored content = %q, want %q", got, "committed content\n")
	}
	// Index and worktree both back at HEAD — the reset healed the index, the
	// heal healed the worktree.
	clean := mustGit(t, checkout, "status", "--porcelain")
	if clean.Stdout != "" {
		t.Fatalf("checkout not clean after reset+heal: %q", clean.Stdout)
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

// TestRecoverStatePropagatesExecutionFailure pins the unborn-HEAD gate's
// error contract: only rev-parse's nonzero EXIT CODE may skip the reset
// (benign unborn HEAD). A genuine execution failure — here a canceled
// context killing the git child before it reports — must propagate, not
// masquerade as "nothing to recover" and silently strand a crash-staged
// index.
func TestRecoverStatePropagatesExecutionFailure(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := engine.recoverState(ctx)
	if err == nil {
		t.Fatal("recoverState returned nil under a canceled context; execution failure must propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("recoverState error does not wrap context.Canceled: %v", err)
	}
}

// TestRestoreStrayWorktreeDeletionsPropagatesFailure pins that the crash-window
// heal never swallows a git execution failure: a swallowed error would let the
// cycle proceed past an unhealed stray deletion and re-open the exact
// data-loss window the heal closes. A canceled context kills the heal's first
// git child (ls-files) before it can report, and the error must wrap
// context.Canceled.
//
// It drives the extracted heal DIRECTLY (the same unit-testability idiom
// integrate.go uses for healAfterFailedIntegrate): through recoverState a
// canceled context trips the earlier rev-parse first, so only calling the heal
// in isolation exercises ITS propagation.
func TestRestoreStrayWorktreeDeletionsPropagatesFailure(t *testing.T) {
	t.Parallel()
	checkout, _ := newTestCheckout(t)
	engine := newTestEngine(t, checkout)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := engine.restoreStrayWorktreeDeletions(ctx)
	if err == nil {
		t.Fatal("restoreStrayWorktreeDeletions returned nil under a canceled context; heal failures must propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("heal error does not wrap context.Canceled: %v", err)
	}
}
