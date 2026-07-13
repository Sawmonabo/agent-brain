// Package memoryfs is the filesystem substrate every dashboard hub screen
// that browses or mutates memory content reads and writes through (spec
// §3/§5): it enumerates memory files across enrolled provider units,
// classifies them via the same provider pattern tables the engine's own
// mirror-in uses, and performs every mutation (write/delete/rename)
// atomically. It imports only api/provider/repo/renameio/stdlib — no
// bubbletea, no lipgloss, no engine (package-boundary rule, spec §8) — so
// it stays usable from a plain data layer, not just the TUI event loop.
package memoryfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/renameio/v2"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// maxBodyBytes caps ReadBody's return at 1 MiB — the same defensive posture
// as the /v0/blob endpoint (engine.BlobAt's historyBlobByteCap guards the
// git-history read path; this guards the live provider-file read path).
const maxBodyBytes = 1 << 20

// ErrTooLarge is returned by ReadBody when the file exceeds maxBodyBytes.
var ErrTooLarge = errors.New("memory file exceeds the read cap")

// Memory is one file under an enrolled unit's provider dir.
type Memory struct {
	Provider    string // unit identity —
	Folder      string //   the repo folder it syncs to
	LocalDir    string //   the unit's provider dir root
	RelPath     string // path under LocalDir (filename, or subdir path)
	RepoPath    string // <provider>[/<repo_subdir>]/<RelPath> — the /v0/history path key
	Name        string // frontmatter name, else filename stem
	Description string // frontmatter description, else ""
	Class       provider.Class
	ModTime     time.Time
	Size        int64
}

// fullPath resolves m's on-disk location.
func (m Memory) fullPath() string {
	return localPath(m.LocalDir, m.RelPath)
}

// localPath joins a unit dir and a slash-separated relative path into an
// OS-native filesystem path.
func localPath(dir, rel string) string {
	return filepath.Join(dir, filepath.FromSlash(rel))
}

// List walks each unit's LocalDir (units pre-filtered to one folder by the
// caller), classifies every regular file via provider.Classify — the same
// exported entry point the engine's own mirror-in calls ahead of copying
// (internal/engine/mirror_in.go) — and returns every non-ignore-class file,
// sorted by (Folder, RepoPath) for a deterministic browser order. Symlinks
// are skipped (the engine's own mirror-in exfiltration rule: a planted link
// must never expose an arbitrary reachable file as memory content). A unit
// whose LocalDir does not exist yet yields no entries, not an error — an
// enrolled-but-empty unit (the provider dir hasn't been recreated on this
// machine) is ordinary, not exceptional. A unit naming a provider absent
// from registry is a configuration error and fails the whole call, the same
// fail-loud contract mirror_in.go applies to the identical situation.
func List(registry *provider.Registry, units []api.UnitInfo) ([]Memory, error) {
	var out []Memory
	for _, unit := range units {
		prov, ok := registry.Get(unit.Provider)
		if !ok {
			return nil, fmt.Errorf("memoryfs: list: provider %q not registered (folder %q)", unit.Provider, unit.Folder)
		}
		entries, err := listUnit(prov, unit)
		if err != nil {
			return nil, fmt.Errorf("memoryfs: list %s/%s: %w", unit.Folder, unit.Provider, err)
		}
		out = append(out, entries...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Folder != out[j].Folder {
			return out[i].Folder < out[j].Folder
		}
		return out[i].RepoPath < out[j].RepoPath
	})
	return out, nil
}

// listUnit enumerates one unit's regular, non-ignore-class files.
func listUnit(prov provider.Provider, unit api.UnitInfo) ([]Memory, error) {
	var out []Memory
	err := filepath.WalkDir(unit.LocalDir, func(fullPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks (and any other irregular file) are never memory content
		}
		rel, err := filepath.Rel(unit.LocalDir, fullPath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		class := provider.Classify(prov, classifyRel(unit.RepoSubdir, rel))
		if class == provider.ClassIgnore {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		name, description, _ := Meta(fullPath)
		if name == "" {
			name = strings.TrimSuffix(path.Base(rel), path.Ext(rel))
		}
		out = append(out, Memory{
			Provider:    unit.Provider,
			Folder:      unit.Folder,
			LocalDir:    unit.LocalDir,
			RelPath:     rel,
			RepoPath:    repoPath(unit.Provider, unit.RepoSubdir, rel),
			Name:        name,
			Description: description,
			Class:       class,
			ModTime:     info.ModTime(),
			Size:        info.Size(),
		})
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// classifyRel composes the provider-dir-relative name provider.Classify
// expects: RepoSubdir joined ahead of rel, the same namespacing the
// engine's own mirror-in applies (its unexported classifyRel,
// internal/engine/unitpath.go) before calling Classify. Duplicated here as
// a one-line path.Join rather than imported: importing internal/engine
// would cross the package-boundary rule (spec §8 — engine never sits
// upstream of cli), and RepoSubdir-then-rel is a one-line join, not logic
// worth sharing across that boundary.
func classifyRel(repoSubdir, rel string) string {
	return path.Join(repoSubdir, rel)
}

// repoPath composes the /v0/history path key: <provider>[/<repo_subdir>]/<rel>.
func repoPath(providerName, repoSubdir, rel string) string {
	return path.Join(providerName, repoSubdir, rel)
}

// ReadBody returns the file's content, capped at maxBodyBytes.
func ReadBody(m Memory) (string, error) {
	fullPath := m.fullPath()
	info, err := os.Stat(fullPath)
	if err != nil {
		return "", fmt.Errorf("memoryfs: stat %s: %w", fullPath, err)
	}
	if info.Size() > maxBodyBytes {
		return "", fmt.Errorf("%s: %w", fullPath, ErrTooLarge)
	}
	content, err := os.ReadFile(fullPath) //nolint:gosec // G304: fullPath is composed from an enrolled unit's LocalDir + a RelPath this package's own List produced, not untrusted input
	if err != nil {
		return "", fmt.Errorf("memoryfs: read %s: %w", fullPath, err)
	}
	return string(content), nil
}

// WriteFileAtomic lands content at dir/rel via renameio (write-temp + atomic
// rename) — the same call shape every atomic write in this codebase uses,
// including engine.mirrorOutUnit's own write into a unit's LocalDir, so a
// mutation from the hub produces exactly the single clean rename-in event
// the daemon's fsnotify watcher already expects from mirror-out (ADR 20
// D2). Creates parent dirs 0o700 as needed — a codex RepoSubdir root or a
// nested new-memory path a skeleton first touches.
func WriteFileAtomic(dir, rel string, content []byte) error {
	fullPath := localPath(dir, rel)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
		return fmt.Errorf("memoryfs: create parent dir for %s: %w", fullPath, err)
	}
	if err := renameio.WriteFile(fullPath, content, 0o644); err != nil {
		return fmt.Errorf("memoryfs: write %s: %w", fullPath, err)
	}
	return nil
}

// Delete removes the provider file (plain os.Remove — deletion IS the
// mutation the watcher captures; recoverable via history restore, spec §6).
func Delete(m Memory) error {
	fullPath := m.fullPath()
	if err := os.Remove(fullPath); err != nil {
		return fmt.Errorf("memoryfs: delete %s: %w", fullPath, err)
	}
	return nil
}

// Rename moves m.RelPath to newRel inside the same LocalDir (os.Rename —
// atomic same-volume: both paths share LocalDir, so this never crosses a
// filesystem boundary). newRel must validate via repo.ValidateRelPath
// (rejecting traversal, absolute paths, and non-clean forms) and keep the
// same extension — the hub's rename flow only ever renames a memory, never
// changes its kind. Missing intermediate directories for a nested newRel
// are created, mirroring WriteFileAtomic's own parent-dir handling.
func Rename(m Memory, newRel string) error {
	if err := repo.ValidateRelPath(newRel); err != nil {
		return fmt.Errorf("memoryfs: rename target %q: %w", newRel, err)
	}
	if path.Ext(newRel) != path.Ext(m.RelPath) {
		return fmt.Errorf("memoryfs: rename %q to %q: extension must not change", m.RelPath, newRel)
	}
	oldFull := m.fullPath()
	newFull := localPath(m.LocalDir, newRel)
	if err := os.MkdirAll(filepath.Dir(newFull), 0o700); err != nil {
		return fmt.Errorf("memoryfs: create parent dir for %s: %w", newFull, err)
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		return fmt.Errorf("memoryfs: rename %s to %s: %w", oldFull, newFull, err)
	}
	return nil
}

// LocalTarget maps a repo path (as /v0/history reports it: <provider>[/
// <repo_subdir>]/<rel>) back to the enrolled unit's local dir + relative
// path — restore's write target. Matching is longest-prefix-first: two
// units under the same (folder, provider) but different RepoSubdirs (the
// codex memories+chronicle shape, spec §3) can have one whose RepoSubdir is
// "" — its prefix "<provider>/" is then a leading substring of the other's
// "<provider>/<repo_subdir>/" — so the most specific (longest) matching
// prefix wins rather than whichever unit happens to come first in units.
// ok=false when no enrolled unit matches (e.g. the unit was untracked).
func LocalTarget(units []api.UnitInfo, folder, repoPath string) (dir, rel string, ok bool) {
	bestPrefixLen := -1
	for _, unit := range units {
		if unit.Folder != folder {
			continue
		}
		prefix := unit.Provider + "/"
		if unit.RepoSubdir != "" {
			prefix = unit.Provider + "/" + unit.RepoSubdir + "/"
		}
		if !strings.HasPrefix(repoPath, prefix) || len(prefix) <= bestPrefixLen {
			continue
		}
		bestPrefixLen = len(prefix)
		dir, rel, ok = unit.LocalDir, strings.TrimPrefix(repoPath, prefix), true
	}
	return dir, rel, ok
}

// claudeSkeletonTemplate is Claude Code's own memory frontmatter
// convention — name/description/metadata.type — with the type enum spelled
// out as a fill-in hint, plus a body-stub heading matching every other
// provider's plain skeleton.
const claudeSkeletonTemplate = `---
name: %[1]s
description:
metadata:
  type: user | feedback | project | reference
---

# %[1]s

`

// Skeleton is the provider-correct new-memory stub (spec §5 `n`): claude
// gets the frontmatter block (name/description/metadata.type) + body stub;
// every other provider gets "# <name>\n\n".
func Skeleton(providerName, name string) string {
	if providerName == "claude" {
		return fmt.Sprintf(claudeSkeletonTemplate, name)
	}
	return fmt.Sprintf("# %s\n\n", name)
}
