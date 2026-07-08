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
