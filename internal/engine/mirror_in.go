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
	unitDir := e.layout.UnitDir(u.Folder, u.Provider)
	unitPrefix := path.Join(u.Folder, u.Provider) + "/"

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
		if provider.Classify(prov, rel) == provider.ClassIgnore {
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
		if localFiles[rel] || !manifest.Has(repoRel) {
			// Present locally, or never synced by this host
			// (new-from-remote): not ours to delete.
			return nil
		}
		if _, err := gitx.Run(ctx, e.checkout, "rm", "--quiet", "--ignore-unmatch", "--", repoRel); err != nil {
			return err
		}
		if _, statErr := os.Lstat(fullPath); statErr == nil {
			// git rm --ignore-unmatch silently no-ops on an untracked
			// file: a prior cycle can crash after mirror-in wrote it but
			// before commit. Remove it directly here, or the orphan
			// reads as new-from-remote next cycle and gets copied back
			// to the provider dir — resurrecting a file the user
			// deleted.
			if err := os.Remove(fullPath); err != nil { //nolint:gosec // G122: fullPath comes from walking this unit's own checkout dir, and Remove doesn't follow symlinks — no TOCTOU exfiltration path
				return fmt.Errorf("remove untracked orphan %s: %w", fullPath, err)
			}
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
