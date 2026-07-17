package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// scrubIntegrated enforces the git-meta invariant over the WHOLE checkout
// (SECURITY CONTRACT, sync.go): a hostile push can deliver
// .gitattributes/.gitignore at any depth — including folder level, one
// above the unit dirs mirror-in's scrub covers — and the worktree copy is
// what `git add` consults for filter attributes. It also re-canonicalizes
// the ROOT .gitattributes: that file is ours (generated), and a pushed
// mutation of it could unscope the encryption filter for every later add.
//
// Two callers, two moments. prepareCheckout runs it at the TOP of every
// commit-creating entry point, against poison ALREADY RESIDENT in the
// checkout (a fresh clone of a poisoned main), and commits the heal itself.
// Sync runs it again POST-INTEGRATE, against poison a rebase just delivered,
// where the existing commitProjects commits the staged heal. Both moments
// are load-bearing; neither subsumes the other.
//
// Deletions and the root heal are staged (root-scoped removal +
// stageRemoval / git add), never committed here — the caller decides.
// It returns the repo-relative paths it removed or healed so the cycle log
// names them; a clean tree touches the index zero times and returns nil.
func (e *Engine) scrubIntegrated(ctx context.Context) ([]string, error) {
	var healed []string
	root := e.layout.Root()
	// A root-scoped handle makes the directory removal below refuse to
	// follow a symlink component out of the checkout: WalkDir lstats an
	// entry, but a concurrent swap could repoint a path component before
	// we act. os.Root re-resolves every component within the checkout
	// and errors on any escape.
	checkoutRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("checkout scrub: open checkout root: %w", err)
	}
	defer func() { _ = checkoutRoot.Close() }()
	err = filepath.WalkDir(root, func(abs string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		// The checkout's own git dir is not repo content.
		if rel == ".git" {
			return filepath.SkipDir
		}
		// The root .gitattributes is legitimate (generated); its content is
		// verified below. Everything git-meta ANYWHERE else goes.
		if rel == ".gitattributes" {
			return nil
		}
		if !isGitMetaPath(rel) {
			return nil
		}
		if entry.IsDir() {
			// A directory named .git (nested repo smuggling) or one that
			// shadows a meta filename (.gitattributes/.gitignore delivered as
			// a tree) — remove it within the root scope, drop it from the
			// index, then skip walking into what no longer exists.
			if err := checkoutRoot.RemoveAll(rel); err != nil {
				return fmt.Errorf("scrub dir %s: %w", rel, err)
			}
			if err := e.stageRemoval(ctx, rel); err != nil {
				return err
			}
			healed = append(healed, rel)
			return filepath.SkipDir
		}
		// A git-meta FILE. .gitignore (unlike .gitattributes, which the
		// root attributes exclude from filtering) is filter-subject: a
		// raw-pushed plaintext copy re-cleans to bytes that can never
		// match its hostile index blob, so an up-to-date-checking
		// `git rm` would refuse ("local modifications") exactly when
		// this scrub matters most, wedging the cycle on attacker input.
		// Poison is never user data (mirror-in refuses git-meta before
		// Classify), so remove it unconditionally, the same shape as the
		// directory branch: disk within the root scope, then the index
		// entry without any content comparison.
		if err := checkoutRoot.Remove(rel); err != nil {
			return fmt.Errorf("scrub file %s: %w", rel, err)
		}
		if err := e.stageRemoval(ctx, rel); err != nil {
			return err
		}
		healed = append(healed, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("checkout scrub: %w", err)
	}

	canonical := repo.GenerateAttributes(e.registry)
	attrsPath := e.layout.AttributesFile()
	current, readErr := os.ReadFile(attrsPath) //nolint:gosec // G304: path derives from the engine's own Layout, not untrusted input
	if readErr != nil || string(current) != canonical {
		if err := repo.WriteAttributes(e.layout, e.registry); err != nil {
			return nil, fmt.Errorf("checkout scrub: heal root attributes: %w", err)
		}
		if _, err := gitx.Run(ctx, e.checkout, "add", "--", ".gitattributes"); err != nil {
			return nil, fmt.Errorf("checkout scrub: stage root attributes: %w", err)
		}
		healed = append(healed, ".gitattributes")
	}
	return healed, nil
}

// prepareCheckout makes the checkout safe to commit into and runs FIRST in
// every busy-guarded engine entry point that can create a commit — Sync and
// the three admin ops (RegisterProject/PurgeProject/SeedProject). It aborts a
// rebase/merge a crash left behind (recoverState), then removes any git-meta
// poison already RESIDENT in the checkout and heals the root .gitattributes
// (scrubIntegrated), committing that heal on the spot so no later `git add`
// in this operation consults hostile worktree attributes.
//
// SECURITY (spec §5, absolute invariant): the
// post-integrate scrubIntegrated only guarantees a poison-free tree at a
// cycle's END — it relies on the PREVIOUS cycle having scrubbed. That
// premise fails at exactly two commit-creating boundaries, both of which
// commit new memory beside poison a prior cycle never saw:
//   - a fresh machine whose checkout was cloned from a poisoned main (its
//     first cycle mirrors in + commits before any scrub of its own); and
//   - a standalone seed/admin commit that runs outside the sync cycle.
//
// A folder-level `* -filter` UNSELECTS the encryption clean filter for its
// subtree (deepest-.gitattributes-wins), so `filter.required` never fires
// and the add stores PLAINTEXT. Scrubbing at the TOP closes both windows and
// the symmetric crash-window (a cycle that completed a poison-delivering
// integrate then died before its own post-integrate scrub). The
// post-integrate scrub stays: it still heals poison a mid-cycle integrate
// freshly delivers, and propagates the heal fleet-wide.
//
// Returns the scrubbed paths so the caller's report/log can name them.
func (e *Engine) prepareCheckout(ctx context.Context) ([]string, error) {
	// Pin git's auto maintenance to the foreground BEFORE any command this
	// cycle runs that could trigger it (ADR 19): git's default detaches
	// `git gc --auto` / `git maintenance run --auto` into the background,
	// where it outlives this cycle's git children and races the single
	// writer (ADR 03) — a later cycle, a quiesced init/doctor mutation, or
	// teardown. Stateless and unconditional (no once-flag): two cheap
	// `git config` writes per cycle converge every pre-ADR-19 fleet member
	// on its first post-upgrade cycle and re-heal any later drift, with no
	// engine state to reason about.
	if err := gitx.InstallMaintenancePosture(ctx, e.checkout); err != nil {
		return nil, err
	}
	if err := e.recoverState(ctx); err != nil {
		return nil, err
	}
	healed, err := e.scrubIntegrated(ctx)
	if err != nil {
		return nil, err
	}
	if len(healed) > 0 {
		if err := e.commitHeal(ctx); err != nil {
			return nil, err
		}
	}
	return healed, nil
}

// commitHeal commits whatever prepareCheckout's scrub staged — git-meta
// removals plus a root-attributes heal — under a dedicated, self-describing
// subject, so the heal is a normal commit that propagates on the next push.
// The staged-check makes it a no-op when the scrub only unlinked UNTRACKED
// strays (a crashed cycle's residue): nothing is staged, nothing to commit.
func (e *Engine) commitHeal(ctx context.Context) error {
	staged, err := gitx.RunStatus(ctx, e.checkout, "diff", "--cached", "--quiet")
	if err != nil {
		return err
	}
	if staged.ExitCode == 0 {
		return nil
	}
	subject := fmt.Sprintf("meta: heal git-meta poison (%s)", e.host)
	if _, err := gitx.Run(ctx, e.checkout, "commit", "--quiet", "-m", subject); err != nil {
		return err
	}
	return nil
}

// stageRemoval drops repoRel from the index after its worktree copy has
// already been removed from disk. --cached touches only the index —
// deliberately NO content comparison: for filter-subject git-meta, the
// re-cleaned worktree bytes never match a hostile index blob, and an
// up-to-date-checking `git rm` would refuse mid-scrub. -r recurses a
// directory (harmless on a file path); --ignore-unmatch keeps a
// never-tracked stray (untracked git-meta a crashed cycle left behind)
// from failing the whole scrub. Also the index half of mirror-in's
// forceScrubGitMeta.
func (e *Engine) stageRemoval(ctx context.Context, repoRel string) error {
	_, err := gitx.Run(ctx, e.checkout, "rm", "-r", "--cached", "--ignore-unmatch", "--", repoRel)
	return err
}
