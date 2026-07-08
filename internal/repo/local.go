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
// This is the engine's work item: mirror LocalDir ↔ <Folder>/<Provider>.
type Unit struct {
	Provider  string `toml:"provider"`
	ProjectID string `toml:"project_id"` // canonical id; empty for global scope
	Folder    string `toml:"folder"`     // repo folder, or GlobalFolder
	LocalDir  string `toml:"local_dir"`  // absolute, machine-local
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
// idempotent no-op. A DIFFERENT local dir for an already-fed
// (provider, folder) is rejected: two local sources mirroring into one
// checkout dir would ping-pong overwrite each other.
func (r *LocalRegistry) Enroll(u Unit) error {
	if err := u.validate(); err != nil {
		return err
	}
	for _, existing := range r.Units {
		if existing.Provider == u.Provider && existing.LocalDir == u.LocalDir {
			return nil
		}
		if existing.Provider == u.Provider && existing.Folder == u.Folder {
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
