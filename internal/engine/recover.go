package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// recoverState heals whatever a previous crash left behind before the cycle
// commits (spec §4 crash safety): it aborts a stranded rebase or merge, resets
// a crash-staged index, and restores stray worktree-only deletions. Runs at the
// top of every cycle: a mid-cycle SIGKILL heals on the next tick, not the next
// daemon restart.
func (e *Engine) recoverState(ctx context.Context) error {
	gitDir := filepath.Join(e.checkout, ".git")
	steps := []struct {
		marker string
		abort  []string
	}{
		{"rebase-merge", []string{"rebase", "--abort"}},
		{"rebase-apply", []string{"rebase", "--abort"}},
		{"MERGE_HEAD", []string{"merge", "--abort"}},
	}
	for _, s := range steps {
		if _, err := os.Lstat(filepath.Join(gitDir, s.marker)); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if _, err := gitx.Run(ctx, e.checkout, s.abort...); err != nil {
			return fmt.Errorf("recover: git %v: %w", s.abort, err)
		}
	}

	// A crash between a cycle's `git add`/`git rm --cached` and its commit
	// leaves a staged index the aborts above never touch; the next cycle's
	// conservative deletion propagation then refuses ("local modifications")
	// and the folder wedges (Phase-3 final review F3). The index is wholly
	// derived state — every entry point re-stages what it needs — so clear
	// residue with a MIXED reset (worktree untouched). Unborn HEAD (a brand
	// new checkout before its first commit) has nothing to reset or wedge —
	// and it is the ONLY skip: rev-parse's nonzero exit is data (RunStatus),
	// while a real execution failure (canceled context, spawn, signal-kill)
	// propagates instead of masquerading as "nothing staged".
	head, err := gitx.RunStatus(ctx, e.checkout, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return fmt.Errorf("recover: git rev-parse HEAD: %w", err)
	}
	if head.ExitCode == 0 {
		staged, diffErr := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
		if diffErr != nil {
			return fmt.Errorf("recover: git diff --cached: %w", diffErr)
		}
		if staged.ExitCode != 0 {
			if _, resetErr := gitx.Run(ctx, e.checkout, "reset", "--quiet"); resetErr != nil {
				return fmt.Errorf("recover: git reset: %w", resetErr)
			}
		}
		// With the index now equal to HEAD, restore any stray worktree-only
		// deletion a crash stranded. This shares the unborn-HEAD
		// guard: an unborn HEAD has no committed path to strand or restore.
		if err := e.restoreStrayWorktreeDeletions(ctx); err != nil {
			return err
		}
	}
	return nil
}

// restoreStrayWorktreeDeletions restores every tracked path present in HEAD but
// missing from the worktree — the crash-window sibling of the in-integrate
// worktree heal (integrate.go). A degraded integrate's rebase/merge can
// partial-update the worktree and then smudge-fail on an undecryptable upstream
// blob; git's own --abort restores HEAD and the index but NOT the worktree,
// stranding an UNSTAGED deletion. The in-integrate heal fixes that on every non-Integrated
// return, but a daemon crash BETWEEN the failed rebase and that heal leaves the
// stray deletion for the next cycle, where commitProjects' `git add -A` would
// commit it and mirror-out would propagate a phantom deletion fleet-wide (silent
// data loss, spec §5/§11).
//
// The safe signature: a legitimate
// memory deletion ONLY ever appears STAGED, as mirror-in's `git rm` in the same
// cycle; an UNSTAGED (worktree-only) deletion at cycle start is ALWAYS git
// residue (an interrupted checkout-update, a crash mid-integrate) and never user
// intent. Restoring from HEAD is therefore always safe: if the provider copy is
// also gone (a real deletion interrupted before it was staged), THIS cycle's
// mirror-in re-detects it and stages the deletion properly; if the provider copy
// is present (pure residue), restoration prevents the loss. recoverState calls
// this AFTER the staged-index reset, so a staged deletion a crash left has
// already been rewound to a worktree-only deletion and is restored here too —
// then mirror-in re-derives the correct action from the provider dir.
//
// Scope is DELETIONS ONLY. A stray worktree MODIFICATION is deliberately left
// alone: mirror-in legitimately overwrites edits (they converge next cycle). So
// `git ls-files --deleted` — paths IN the index but MISSING from the worktree —
// is exactly the set to restore, and `git checkout HEAD -- <paths>` writes each
// back through the smudge filter (the local HEAD is this host's own, always
// decryptable — the undecryptable blob was the discarded upstream one). A git
// failure propagates, never swallowed: proceeding past a failed heal would
// re-open the exact window this closes.
func (e *Engine) restoreStrayWorktreeDeletions(ctx context.Context) error {
	deleted, err := gitx.Run(ctx, e.checkout, "ls-files", "--deleted", "-z")
	if err != nil {
		return fmt.Errorf("recover: list stray worktree deletions: %w", err)
	}
	var paths []string
	for p := range strings.SplitSeq(deleted.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// One checkout restores every stray deletion: HEAD's blob for each path,
	// smudged back into the worktree (and the index, already == HEAD after the
	// reset above). --quiet silences progress only; a real failure still errors.
	//
	// Every path rides in a single argv, so the batch is bounded by the OS ARG_MAX
	// (darwin's ~256 KiB is the tightest platform we target). That is ample at
	// memory-tree scale — the deleted set is a handful of small provider files, not
	// a repo-wide checkout. If some future caller ever drives this over
	// pathologically many paths, the escape hatch is to stream them on stdin with
	// `--pathspec-from-file=- --pathspec-file-nul` instead of packing the argv.
	args := append([]string{"checkout", "--quiet", "HEAD", "--"}, paths...)
	if _, err := gitx.Run(ctx, e.checkout, args...); err != nil {
		return fmt.Errorf("recover: restore stray worktree deletions: %w", err)
	}
	return nil
}
