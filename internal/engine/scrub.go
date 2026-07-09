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
// after integrate (SECURITY CONTRACT, sync.go): a hostile push can deliver
// .gitattributes/.gitignore at any depth — including folder level, one
// above the unit dirs mirror-in's scrub covers — and the worktree copy is
// what `git add` consults for filter attributes. It also re-canonicalizes
// the ROOT .gitattributes: that file is ours (generated), and a pushed
// mutation of it could unscope the encryption filter for every later add.
//
// Deletions and the root heal are staged (root-scoped removal +
// stageRemoval / git add); the caller's existing post-integrate
// commitProjects commits them, so healed state propagates to other
// machines like any other fix.
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
		return nil, fmt.Errorf("post-integrate scrub: open checkout root: %w", err)
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
		return nil, fmt.Errorf("post-integrate scrub: %w", err)
	}

	canonical := repo.GenerateAttributes(e.registry)
	attrsPath := e.layout.AttributesFile()
	current, readErr := os.ReadFile(attrsPath) //nolint:gosec // G304: path derives from the engine's own Layout, not untrusted input
	if readErr != nil || string(current) != canonical {
		if err := repo.WriteAttributes(e.layout, e.registry); err != nil {
			return nil, fmt.Errorf("post-integrate scrub: heal root attributes: %w", err)
		}
		if _, err := gitx.Run(ctx, e.checkout, "add", "--", ".gitattributes"); err != nil {
			return nil, fmt.Errorf("post-integrate scrub: stage root attributes: %w", err)
		}
		healed = append(healed, ".gitattributes")
	}
	return healed, nil
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
