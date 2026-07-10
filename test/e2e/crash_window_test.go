package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCrashWindowStrayDeletionDoesNotPropagate is the crash-window
// characterization for Task 4.6 — the process-death sibling of Task 4's
// in-integrate worktree heal.
//
// The stranded state: a degraded integrate's rebase/merge partially updates
// the worktree and then smudge-fails on an undecryptable upstream blob; git's
// own --abort restores HEAD and the index but NOT the worktree, leaving a
// stray UNSTAGED deletion. Task 4 heals that on every non-Integrated return,
// but if the daemon CRASHES between the failed rebase and that heal, the stray
// deletion survives into the next cycle. Pre-fix, recoverState left it,
// mirror-in's manifest fast-path skipped re-copy (the provider file is
// untouched, so size+mtime still match the ledger), and commitProjects'
// `git add -A` committed the deletion — mirror-out then propagated a phantom
// deletion fleet-wide (silent data loss).
//
// This constructs the post-abort residue DIRECTLY — stray worktree deletion
// with the provider copy present — and asserts recoverState's cycle-start heal
// restores it: no deletion commit, the blob intact and still ciphertext on the
// wire, and the provider copy intact. RED before the heal (the cycle commits
// the deletion); GREEN after.
func TestCrashWindowStrayDeletionDoesNotPropagate(t *testing.T) {
	t.Parallel()
	bare := newBareRepo(t)
	a := newSyncMachine(t, "host-a", bare, true)

	const fact = "memories/architecture.md"
	const text = "the engine is the single writer to the checkout\n"
	repoPath := "alpha/claude/" + fact

	a.write(t, fact, text)
	if r := a.sync(t); !r.Pushed {
		t.Fatalf("A initial push failed: %+v", r)
	}
	if !strings.HasPrefix(remoteBlob(t, bare, repoPath), magicPrefix) {
		t.Fatal("pre-crash blob is not agent-brain ciphertext")
	}

	// The checkout worktree copy vanishes (unstaged deletion) while the
	// provider copy — and its manifest mtime+size — stay put: exactly what a
	// `git rebase --abort` leaves after a mid-rebase smudge failure.
	if err := os.Remove(filepath.Join(a.checkout, filepath.FromSlash(repoPath))); err != nil {
		t.Fatal(err)
	}
	// The residue MUST be worktree-only (unstaged): Task 1's staged-index reset
	// does not cover it — only the Task 4.6 heal does. ` D` is the porcelain
	// signature of an unstaged deletion.
	if status := gitRun(t, a.checkout, "status", "--porcelain", "--", repoPath); !strings.HasPrefix(status, " D") {
		t.Fatalf("expected an unstaged worktree deletion, got porcelain %q", status)
	}

	report := a.sync(t)

	// No deletion commit was created this cycle...
	if len(report.Commits) != 0 {
		t.Fatalf("crash-window cycle committed %v — a stray worktree deletion was propagated as a real deletion", report.Commits)
	}
	// ...the blob is still present and ciphertext on the wire...
	if _, err := gitRunEnv(t, bare, nil, "cat-file", "-e", "main:"+repoPath); err != nil {
		t.Fatalf("fact deleted from the wire by a phantom crash-window deletion: %v", err)
	}
	if !strings.HasPrefix(remoteBlob(t, bare, repoPath), magicPrefix) {
		t.Fatal("post-cycle blob is not agent-brain ciphertext")
	}
	// ...and the provider copy survived.
	if got := a.read(t, fact); got != text {
		t.Fatalf("provider fact = %q, want %q", got, text)
	}
}

// TestCrashWindowRealDeletionStillPropagates is the safety complement: it
// proves the heal restores worktree-only deletions WITHOUT blocking a genuine
// deletion. It models a real deletion interrupted before it was staged — the
// stray worktree deletion is present AND the provider copy is gone (the user
// really removed the fact). The heal restores the checkout copy at cycle start,
// but THIS cycle's mirror-in then re-detects the absent provider file and
// re-stages the deletion properly, so it still propagates.
//
// This is the "safe signature" in action: restoration keys off the worktree
// (always safe — a legitimate deletion never arrives as a stray worktree-only
// deletion), while the keep-or-delete decision keys off the provider dir, which
// mirror-in re-reads every cycle. It passes both before and after the heal (the
// deletion propagates either way), pinning that the fix introduces no
// regression for real deletions.
func TestCrashWindowRealDeletionStillPropagates(t *testing.T) {
	t.Parallel()
	bare := newBareRepo(t)
	a := newSyncMachine(t, "host-a", bare, true)

	const fact = "memories/ephemeral.md"
	const text = "short-lived working note\n"
	repoPath := "alpha/claude/" + fact

	a.write(t, fact, text)
	if r := a.sync(t); !r.Pushed {
		t.Fatalf("A initial push failed: %+v", r)
	}

	// Interrupted real deletion: the provider copy is gone (user intent) AND
	// the checkout worktree copy was stranded unstaged by the crash.
	if err := os.Remove(filepath.Join(a.unit.LocalDir, filepath.FromSlash(fact))); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.checkout, filepath.FromSlash(repoPath))); err != nil {
		t.Fatal(err)
	}

	a.sync(t)

	// The genuine deletion propagated: the blob is gone from the wire...
	if _, err := gitRunEnv(t, bare, nil, "cat-file", "-e", "main:"+repoPath); err == nil {
		t.Fatal("real deletion did not propagate — the heal wrongly resurrected a user-deleted fact")
	}
	// ...and the provider copy stays gone (the heal never wrote it back there).
	if _, err := os.Stat(filepath.Join(a.unit.LocalDir, filepath.FromSlash(fact))); !os.IsNotExist(err) {
		t.Fatalf("provider fact resurrected by the heal: stat err = %v", err)
	}
}
