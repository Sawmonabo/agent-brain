package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/google/renameio/v2"
	toml "github.com/pelletier/go-toml/v2"
)

// Unit binds one enrolled local provider directory to its repo location.
// This is the engine's work item: mirror LocalDir ↔
// <Folder>/<Provider>[/<RepoSubdir>].
type Unit struct {
	Provider  string `toml:"provider"`
	ProjectID string `toml:"project_id"` // canonical id; empty for global scope
	Folder    string `toml:"folder"`     // repo folder, or GlobalFolder
	LocalDir  string `toml:"local_dir"`  // absolute, machine-local
	// RepoSubdir maps this unit under a subdirectory of the provider dir
	// (<folder>/<provider>/<repo_subdir>). Empty for providers with one
	// root (claude). Validated by ValidateRelPath when set.
	RepoSubdir string `toml:"repo_subdir,omitempty"`
}

func (u Unit) validate() error {
	if u.Provider == "" {
		return fmt.Errorf("unit has empty provider")
	}
	if u.Folder != GlobalFolder {
		if err := ValidateFolderName(u.Folder); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(u.LocalDir) {
		return fmt.Errorf("unit local_dir %q is not absolute", u.LocalDir)
	}
	if u.RepoSubdir != "" {
		if err := ValidateRelPath(u.RepoSubdir); err != nil {
			return fmt.Errorf("unit repo_subdir: %w", err)
		}
	}
	return nil
}

// LocalRegistry is this machine's enrollment state, stored at
// <data-dir>/registry-local.toml. It NEVER enters the memories repo —
// local slugs and paths are machine-specific (spec §3).
type LocalRegistry struct {
	Version int    `toml:"version"`
	Units   []Unit `toml:"units"`
}

// NewLocalRegistry returns an empty registry at the current version.
func NewLocalRegistry() *LocalRegistry {
	return &LocalRegistry{Version: RegistryVersion}
}

// LoadLocalRegistry reads path; a missing file is an empty registry.
// Corrupt or invalid content fails loudly, naming the offending unit —
// a silently-dropped unit is a project that silently stops syncing.
func LoadLocalRegistry(path string) (*LocalRegistry, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is supplied by the daemon/CLI composition layer (program-derived registry location), not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return NewLocalRegistry(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read local registry: %w", err)
	}
	var r LocalRegistry
	if err := toml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse local registry %s: %w", path, err)
	}
	if r.Version != RegistryVersion {
		return nil, fmt.Errorf("local registry %s: unsupported version %d (this binary supports %d)", path, r.Version, RegistryVersion)
	}
	for i, u := range r.Units {
		if err := u.validate(); err != nil {
			return nil, fmt.Errorf("local registry %s: unit %d: %w", path, i, err)
		}
	}
	// Enroll upholds two cross-unit invariants that a hand-edited file can
	// smuggle past the per-unit checks above: no two units feed the same
	// (provider, folder, repo_subdir), and no two units record the same
	// (provider, local dir). Check both here so a corrupt state fails
	// loudly at load — never silently at sync time. RepoSubdir is part
	// of the first key (not just provider+folder) because one global
	// provider legitimately maps several local roots under its provider
	// dir via DIFFERENT RepoSubdirs (codex: memories + chronicle, spec
	// §3) — those are disjoint checkout subtrees, not a collision.
	seenFolder := make(map[[3]string]string, len(r.Units))   // [provider, folder, repo_subdir] -> local dir
	seenLocalDir := make(map[[2]string]string, len(r.Units)) // [provider, local dir] -> folder
	for _, u := range r.Units {
		folderKey := [3]string{u.Provider, u.Folder, u.RepoSubdir}
		if existing, dup := seenFolder[folderKey]; dup {
			return nil, fmt.Errorf("local registry %s: folder %q repo_subdir %q fed by %s from two local dirs (%s and %s)", path, u.Folder, u.RepoSubdir, u.Provider, existing, u.LocalDir)
		}
		seenFolder[folderKey] = u.LocalDir

		localDirKey := [2]string{u.Provider, u.LocalDir}
		if existing, dup := seenLocalDir[localDirKey]; dup {
			return nil, fmt.Errorf("local registry %s: local dir %q for provider %s feeds two folders (%s and %s)", path, u.LocalDir, u.Provider, existing, u.Folder)
		}
		seenLocalDir[localDirKey] = u.Folder
	}
	return &r, nil
}

// Save atomically writes the registry (0600 — it maps this machine's
// private filesystem layout), creating the parent dir 0700 when needed.
// Units are sorted for deterministic bytes.
func (r *LocalRegistry) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create local-registry dir: %w", err)
	}
	sort.Slice(r.Units, func(i, j int) bool {
		a, b := r.Units[i], r.Units[j]
		if a.Folder != b.Folder {
			return a.Folder < b.Folder
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		return a.LocalDir < b.LocalDir
	})
	data, err := toml.Marshal(r)
	if err != nil {
		return fmt.Errorf("encode local registry: %w", err)
	}
	if err := renameio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write local registry: %w", err)
	}
	return nil
}

// Enroll adds u. Re-enrolling the same (provider, local dir) is an
// idempotent no-op. A DIFFERENT local dir for an already-fed (provider,
// folder, repo_subdir) is rejected: two local sources mirroring into one
// checkout dir would ping-pong overwrite each other. A DIFFERENT
// RepoSubdir under the same (provider, folder) is NOT a collision — it
// is the codex two-root shape (memories + chronicle both under
// _global/codex/, spec §3): disjoint checkout subtrees, both allowed.
func (r *LocalRegistry) Enroll(u Unit) error {
	if err := u.validate(); err != nil {
		return err
	}
	for _, existing := range r.Units {
		if existing.Provider == u.Provider && existing.LocalDir == u.LocalDir {
			return nil
		}
		if existing.Provider == u.Provider && existing.Folder == u.Folder && existing.RepoSubdir == u.RepoSubdir {
			return fmt.Errorf("folder %q already fed by %s on this machine (%s); untrack it first", u.Folder, u.Provider, existing.LocalDir)
		}
	}
	r.Units = append(r.Units, u)
	return nil
}

// Remove drops the unit for (providerName, localDir), reporting whether
// anything was removed.
func (r *LocalRegistry) Remove(providerName, localDir string) bool {
	for i, u := range r.Units {
		if u.Provider == providerName && u.LocalDir == localDir {
			r.Units = append(r.Units[:i], r.Units[i+1:]...)
			return true
		}
	}
	return false
}
