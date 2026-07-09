package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// isGitMetaPath reports whether any slash-separated segment of the
// unit-relative rel is a git metadata name — `.gitattributes`,
// `.gitignore`, or `.git` — compared case-insensitively.
//
// SECURITY (spec §5, absolute invariant): a `.gitattributes` mirrored into
// a unit subtree overrides the checkout-root attributes for that subtree by
// git's deepest-file-wins precedence. A single `* -filter` line unsets the
// encryption clean filter — no filter runs, `filter.agentbrain.required`
// never fires — so sibling memory files commit as PLAINTEXT and push to the
// remote in the clear. A `.gitignore` sibling silently stops files syncing;
// a `.git` segment embeds a gitlink or nested repo. The invariant must not
// depend on provider classification tables, so the engine refuses these
// names unconditionally in every sync path (mirror-in inbound, checkout
// scrub, mirror-out outbound). EqualFold because case-insensitive
// filesystems (macOS) resolve `.GITATTRIBUTES` when git opens
// `.gitattributes`. rel is unit-relative, so the checkout-root
// `.gitattributes` (managed by repo.WriteAttributes) is never in scope.
func isGitMetaPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.EqualFold(seg, ".gitattributes") ||
			strings.EqualFold(seg, ".gitignore") ||
			strings.EqualFold(seg, ".git") {
			return true
		}
	}
	return false
}

// scrubCheckoutFile removes a checkout file both from git's index and from
// disk. git rm --ignore-unmatch silently no-ops on an untracked file: a
// prior cycle can crash after mirror-in wrote it but before commit, or the
// engine plants it this cycle. Remove it directly too, or the orphan reads
// as new-from-remote next cycle and gets copied back to the provider dir —
// resurrecting a file the user deleted (or, for git-meta, re-poisoning).
func (e *Engine) scrubCheckoutFile(ctx context.Context, repoRel, fullPath string) error {
	if _, err := gitx.Run(ctx, e.checkout, "rm", "--quiet", "--ignore-unmatch", "--", repoRel); err != nil {
		return err
	}
	if _, statErr := os.Lstat(fullPath); statErr == nil {
		// fullPath comes from walking this unit's own checkout dir, and
		// os.Remove doesn't follow symlinks — no TOCTOU exfiltration path.
		if err := os.Remove(fullPath); err != nil {
			return fmt.Errorf("remove untracked orphan %s: %w", fullPath, err)
		}
	}
	return nil
}

// forceScrubGitMeta is the removal path for git-meta POISON: no
// up-to-date check may gate it. A raw-pushed plaintext .gitignore is
// filter-subject, so its re-cleaned worktree bytes never match the
// hostile index blob and scrubCheckoutFile's plain `git rm` refuses
// ("local modifications") — wedging every subsequent cycle on
// attacker-supplied input. Poison is never user data (pass 1 refuses
// git-meta before Classify), so delete the disk copy directly and drop
// the index entry with no content comparison (stageRemoval: --cached,
// filter-free).
func (e *Engine) forceScrubGitMeta(ctx context.Context, repoRel, fullPath string) error {
	if _, statErr := os.Lstat(fullPath); statErr == nil {
		// Same walk-derived, symlink-safe removal rationale as
		// scrubCheckoutFile above.
		if err := os.Remove(fullPath); err != nil {
			return fmt.Errorf("remove git-meta %s: %w", fullPath, err)
		}
	}
	return e.stageRemoval(ctx, repoRel)
}

// mirrorIn implements spec §4 step 1 for every unit: compare provider
// dir ↔ checkout (manifest mtime+size fast path, hash confirm), copy
// local changes in, and resolve deletions through the manifest —
// in-manifest + gone-locally is deleted-here (git rm); in-checkout +
// absent-from-manifest is new-from-remote (left for mirror-out).
func (e *Engine) mirrorIn(ctx context.Context, units []repo.Unit, manifest *repo.Manifest) (MirrorStats, localSnapshot, error) {
	var stats MirrorStats
	snapshot := localSnapshot{}
	for _, u := range units {
		prov, ok := e.registry.Get(u.Provider)
		if !ok {
			return stats, nil, fmt.Errorf("mirror-in %s: provider %q not registered", u.Folder, u.Provider)
		}
		if err := e.mirrorInUnit(ctx, u, prov, manifest, &stats, snapshot); err != nil {
			return stats, nil, fmt.Errorf("mirror-in %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return stats, snapshot, nil
}

func (e *Engine) mirrorInUnit(ctx context.Context, u repo.Unit, prov provider.Provider, manifest *repo.Manifest, stats *MirrorStats, snapshot localSnapshot) error {
	unitDir := e.unitDir(u)
	// unitPrefix folds in RepoSubdir so two units sharing one (folder,
	// provider) but mapped to different RepoSubdirs (codex memories +
	// chronicle, spec §3) never alias each other's manifest entries —
	// pass 3 below filters manifest.Files by this prefix, and a shared
	// prefix would let one unit's ledger hygiene delete the other's
	// still-valid entries.
	unitPrefix := path.Join(u.Folder, u.Provider, u.RepoSubdir) + "/"

	// Pass 1: local → checkout.
	localFiles := map[string]bool{}
	err := filepath.WalkDir(u.LocalDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			// Symlinks and other irregular files never sync: copying
			// through a planted link would commit an arbitrary
			// reachable file into the (shared) repo.
			stats.Skipped++
			return nil
		}
		rel, err := filepath.Rel(u.LocalDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if isGitMetaPath(rel) {
			// SECURITY (spec §5): refuse git-meta files BEFORE Classify so
			// the invariant never rides the provider table. Not recorded in
			// localFiles: the checkout scrub (pass 2) must treat any such
			// path as removable, not "present locally, not ours to delete".
			stats.Skipped++
			return nil
		}
		if provider.Classify(prov, classifyRel(u, rel)) == provider.ClassIgnore {
			return nil
		}
		localFiles[rel] = true
		repoRel := unitPrefix + rel

		// Fast path (spec §4: "mtime+size, hash confirm"): a manifest
		// entry matching size+mtime means unchanged since last sync.
		if entry, ok := manifest.Get(repoRel); ok {
			if info, statErr := d.Info(); statErr == nil &&
				info.Size() == entry.Size && info.ModTime().UnixNano() == entry.MTimeUnixNano {
				snapshot[repoRel] = entry
				return nil
			}
		}
		entry, err := repo.HashFile(fullPath)
		if err != nil {
			return err
		}
		snapshot[repoRel] = entry
		if prev, ok := manifest.Get(repoRel); ok && prev.SHA256 == entry.SHA256 {
			// Touched but content-identical: refresh the ledger only.
			return manifest.Set(repoRel, entry)
		}
		content, err := os.ReadFile(fullPath) //nolint:gosec // G304: path came from walking the enrolled provider dir
		if err != nil {
			return err
		}
		dest := filepath.Join(unitDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := renameio.WriteFile(dest, content, 0o644); err != nil {
			return err
		}
		stats.Copied++
		return manifest.Set(repoRel, entry)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A missing LocalDir is not an error: enrollment outlives an
		// agent dir that hasn't been recreated yet on this machine.
		return err
	}

	// Pass 2: deletions. Walk the checkout side of the unit.
	err = filepath.WalkDir(unitDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(unitDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		repoRel := unitPrefix + rel
		if isGitMetaPath(rel) {
			// SECURITY (spec §5): scrub any git-meta file from the checkout
			// UNCONDITIONALLY — not manifest-gated. A hostile .gitattributes
			// that arrived via integrate is new-from-remote (absent from this
			// host's manifest), which the gate below would preserve. Removing
			// it here blocks freshly-integrated poison AND heals an
			// already-poisoned repo: the removal commits at the existing
			// commit points and propagates fleet-wide. This pass walks the
			// checkout side, so the scrub runs whether or not the provider
			// dir holds such a file. See isGitMetaPath. Force semantics
			// (forceScrubGitMeta, not scrubCheckoutFile): a filter-subject
			// poison file never survives an up-to-date comparison.
			if err := e.forceScrubGitMeta(ctx, repoRel, fullPath); err != nil {
				return err
			}
			manifest.Delete(repoRel)
			stats.Deleted++
			return nil
		}
		if localFiles[rel] || !manifest.Has(repoRel) {
			// Present locally, or never synced by this host
			// (new-from-remote): not ours to delete.
			return nil
		}
		if err := e.scrubCheckoutFile(ctx, repoRel, fullPath); err != nil {
			return err
		}
		manifest.Delete(repoRel)
		stats.Deleted++
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Pass 3: ledger hygiene — entries whose file is gone from BOTH
	// sides (deleted remotely while also deleted here) would otherwise
	// linger forever ("reconcile manifest against reality", spec §4).
	for repoRel := range manifest.Files {
		if !strings.HasPrefix(repoRel, unitPrefix) {
			continue
		}
		rel := strings.TrimPrefix(repoRel, unitPrefix)
		if localFiles[rel] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(unitDir, filepath.FromSlash(rel))); errors.Is(err, fs.ErrNotExist) {
			manifest.Delete(repoRel)
		}
	}
	return nil
}
