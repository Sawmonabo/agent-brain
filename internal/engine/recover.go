package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
)

// recoverState aborts any rebase or merge a previous crash left behind
// (spec §4 crash safety). Runs at the top of every cycle: a mid-cycle
// SIGKILL heals on the next tick, not the next daemon restart.
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
	// new checkout before its first commit) has nothing to reset or wedge.
	if _, err := gitx.Run(ctx, e.checkout, "rev-parse", "--verify", "HEAD"); err == nil {
		staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
		if err != nil {
			return fmt.Errorf("recover: git diff --cached: %w", err)
		}
		if staged.ExitCode != 0 {
			if _, err := gitx.Run(ctx, e.checkout, "reset", "--quiet"); err != nil {
				return fmt.Errorf("recover: git reset: %w", err)
			}
		}
	}
	return nil
}
