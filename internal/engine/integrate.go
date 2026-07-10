package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

const (
	remoteName    = "origin"
	defaultBranch = "main"
	upstreamRef   = remoteName + "/" + defaultBranch
)

// integrateOutcome reports spec §4 step 3. Integrate is all-or-nothing:
// Integrated means HEAD now contains origin/main; otherwise the checkout
// is back at its pre-integrate state and Degraded names the project
// folders whose paths conflicted (mirror-out withheld for them, §11).
type integrateOutcome struct {
	Offline     bool
	Integrated  bool
	DegradedAll bool
	Degraded    []string
}

// integrate fetches and rebases onto origin/main, falling back per the
// spec §4 ladder: rebase → abort → merge commit → abort → degraded.
// Offline (fetch failure) is a normal outcome, not an error; errors are
// infrastructure failures only.
func (e *Engine) integrate(ctx context.Context) (integrateOutcome, error) {
	if fetch, err := gitx.RunStatus(ctx, e.checkout, "fetch", "--quiet", remoteName); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: fetch: %w", err)
	} else if fetch.ExitCode != 0 {
		return integrateOutcome{Offline: true}, nil
	}

	behind, err := gitx.Run(ctx, e.checkout, "rev-list", "--count", "HEAD.."+upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: behind count: %w", err)
	}
	if strings.TrimSpace(behind.Stdout) == "0" {
		return integrateOutcome{Integrated: true}, nil
	}

	rebase, err := gitx.RunStatus(ctx, e.checkout, "rebase", upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase: %w", err)
	}
	if rebase.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	// Rebase failed (spec: "unexpected driver failure"). Capture the
	// conflicted paths for attribution, abort clean, try a merge commit.
	rebaseConflicts, _ := e.conflictedPaths(ctx)
	if _, err := gitx.RunStatus(ctx, e.checkout, "rebase", "--abort"); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: rebase --abort: %w", err)
	}

	merge, err := gitx.RunStatus(ctx, e.checkout, "merge", "--no-edit", upstreamRef)
	if err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge fallback: %w", err)
	}
	if merge.ExitCode == 0 {
		return integrateOutcome{Integrated: true}, nil
	}

	mergeConflicts, _ := e.conflictedPaths(ctx)
	if _, err := gitx.RunStatus(ctx, e.checkout, "merge", "--abort"); err != nil {
		return integrateOutcome{}, fmt.Errorf("integrate: merge --abort: %w", err)
	}

	conflicts := mergeConflicts
	if len(conflicts) == 0 {
		conflicts = rebaseConflicts
	}
	// Restore the worktree the failed ladder may have stranded before returning
	// degraded — see restoreWorktreeToHead: this closes the smudge-failure
	// data-loss class and MUST run on every non-Integrated return.
	if err := e.restoreWorktreeToHead(ctx); err != nil {
		return integrateOutcome{}, err
	}
	return degradeByPaths(conflicts), nil
}

// restoreWorktreeToHead re-checks-out every tracked path from HEAD, healing a
// worktree a failed rebase/merge left diverged. It enforces the invariant:
// integrate never returns non-Integrated with a worktree diverged from HEAD.
//
// Class it closes: a partial worktree update whose smudge fails mid-rebase/merge
// — the local keyset cannot decrypt an upstream blob after a key rotation (or
// any admin re-encrypt), so git deletes the old worktree file, fails to write
// the new one, and its --abort does NOT restore the deletion (git treats the
// half-applied change as a local edit to preserve). Left unhealed, the next
// cycle's commitProjects `git add -A` stages that stray deletion and commits it
// as a real memory deletion, which mirror-out propagates: silent data loss
// (spec §5/§11).
//
// The heal cannot clobber legitimate work: integrate runs after mirror-in and
// commitProjects (spec §4), so the worktree equals HEAD on entry, and a degraded
// peer always smudges its OWN pre-rotation HEAD. A failure here (disk, unexpected
// git state) is surfaced, never swallowed — proceeding past a failed heal would
// re-open the exact data-loss window this closes.
func (e *Engine) restoreWorktreeToHead(ctx context.Context) error {
	if _, err := gitx.Run(ctx, e.checkout, "checkout", "--force", "HEAD", "--", "."); err != nil {
		return fmt.Errorf("integrate: restore worktree after degrade: %w", err)
	}
	return nil
}

// conflictedPaths lists unmerged paths while a rebase/merge conflict is
// live. Best-effort: attribution failing must not mask the abort.
func (e *Engine) conflictedPaths(ctx context.Context) ([]string, error) {
	res, err := gitx.RunStatus(ctx, e.checkout, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil || res.ExitCode != 0 {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(res.Stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// degradeByPaths maps conflicted paths to project folders. A conflict
// under .agent-brain/ (shared metadata) or an empty attribution is not
// project-scoped — degrade everything, conservatively.
func degradeByPaths(paths []string) integrateOutcome {
	if len(paths) == 0 {
		return integrateOutcome{DegradedAll: true}
	}
	set := map[string]bool{}
	for _, p := range paths {
		folder := topSegment(p)
		if folder == repo.MetaDirName {
			return integrateOutcome{DegradedAll: true}
		}
		set[folder] = true
	}
	folders := make([]string, 0, len(set))
	for folder := range set {
		folders = append(folders, folder)
	}
	sort.Strings(folders)
	return integrateOutcome{Degraded: folders}
}
