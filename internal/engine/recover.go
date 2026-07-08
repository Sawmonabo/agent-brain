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
	return nil
}
