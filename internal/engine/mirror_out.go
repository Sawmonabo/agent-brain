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

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// mirrorOut implements spec §4 step 5: checkout → provider dirs,
// atomic per file. Two hard gates protect local work: overwrites and
// deletions are both skipped when the local file changed since the
// cycle's mirror-in snapshot (converge next cycle), and deletions
// additionally require the manifest to prove this host synced the path
// before. Degraded projects (skip set) are withheld entirely (§11).
func (e *Engine) mirrorOut(ctx context.Context, units []repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, skip map[string]bool) (MirrorStats, error) {
	var stats MirrorStats
	for _, u := range units {
		if skip[u.Folder] {
			continue
		}
		if err := e.mirrorOutUnit(ctx, u, manifest, snapshot, &stats); err != nil {
			return stats, fmt.Errorf("mirror-out %s/%s: %w", u.Folder, u.Provider, err)
		}
	}
	return stats, nil
}

func (e *Engine) mirrorOutUnit(_ context.Context, u repo.Unit, manifest *repo.Manifest, snapshot localSnapshot, stats *MirrorStats) error {
	unitDir := e.layout.UnitDir(u.Folder, u.Provider)
	unitPrefix := path.Join(u.Folder, u.Provider) + "/"

	// localUnchanged reports whether the provider file at rel is safe to
	// replace: absent entirely, or byte-identical to the cycle snapshot.
	localUnchanged := func(rel, repoRel string) (bool, error) {
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if _, err := os.Lstat(localPath); errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		snap, ok := snapshot[repoRel]
		if !ok {
			return false, nil // appeared mid-cycle: hands off
		}
		current, err := repo.HashFile(localPath)
		if err != nil {
			return false, err
		}
		return current.SHA256 == snap.SHA256, nil
	}

	// Pass 1: checkout → local (adds and updates).
	inCheckout := map[string]bool{}
	err := filepath.WalkDir(unitDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(unitDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		repoRel := unitPrefix + rel
		inCheckout[rel] = true

		checkoutEntry, err := repo.HashFile(fullPath)
		if err != nil {
			return err
		}
		if snap, ok := snapshot[repoRel]; ok && snap.SHA256 == checkoutEntry.SHA256 {
			return nil // local state already matches what this cycle mirrored in
		}
		safe, err := localUnchanged(rel, repoRel)
		if err != nil {
			return err
		}
		if !safe {
			stats.Skipped++
			return nil
		}
		content, err := os.ReadFile(fullPath) //nolint:gosec // G304: path came from walking the unit's checkout dir
		if err != nil {
			return err
		}
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o750); err != nil {
			return err
		}
		if err := renameio.WriteFile(localPath, content, 0o644); err != nil {
			return err
		}
		written, err := repo.HashFile(localPath)
		if err != nil {
			return err
		}
		stats.Copied++
		return manifest.Set(repoRel, written)
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// Pass 2: remote deletions — manifest-gated (spec §4 step 5).
	for repoRel := range manifest.Files {
		if !strings.HasPrefix(repoRel, unitPrefix) {
			continue
		}
		rel := strings.TrimPrefix(repoRel, unitPrefix)
		if inCheckout[rel] {
			continue
		}
		localPath := filepath.Join(u.LocalDir, filepath.FromSlash(rel))
		if _, err := os.Lstat(localPath); errors.Is(err, fs.ErrNotExist) {
			manifest.Delete(repoRel) // gone everywhere; drop the entry
			continue
		}
		safe, err := localUnchanged(rel, repoRel)
		if err != nil {
			return err
		}
		if !safe {
			// User edited while remote deleted: keep the edit; it
			// mirrors back in next cycle as a fresh fact.
			stats.Skipped++
			continue
		}
		if err := os.Remove(localPath); err != nil {
			return err
		}
		removeEmptyParents(localPath, u.LocalDir)
		manifest.Delete(repoRel)
		stats.Deleted++
	}
	return nil
}

// removeEmptyParents tidies now-empty directories up to (not including)
// stop. Best-effort: the first non-empty ancestor ends the walk.
func removeEmptyParents(deleted, stop string) {
	stop = filepath.Clean(stop)
	for dir := filepath.Dir(deleted); dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}
