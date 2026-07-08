package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/google/renameio/v2"
	toml "github.com/pelletier/go-toml/v2"
)

// RegistryVersion is the current schema version of both registries.
// Loaders reject anything else explicitly — an older binary must fail
// loudly on a newer file, never misread it.
const RegistryVersion = 1

// ProjectEntry is one canonical project in the shared registry.
type ProjectEntry struct {
	ID string `toml:"id"`
}

// Projects is the machine-shared project registry stored at
// .agent-brain/projects.toml inside the memories repo. Machine-owned and
// comment-free (ADR 17); keys are repo folder names.
type Projects struct {
	Version int                     `toml:"version"`
	Entries map[string]ProjectEntry `toml:"projects"`
}

// NewProjects returns an empty registry at the current version.
func NewProjects() *Projects {
	return &Projects{Version: RegistryVersion, Entries: map[string]ProjectEntry{}}
}

// LoadProjects reads path. A missing file is an empty registry (the
// first machine ever); anything unreadable, unparseable, or at an
// unknown version is an explicit error.
func LoadProjects(path string) (*Projects, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is supplied by the daemon/CLI composition layer (program-derived registry location), not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return NewProjects(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read projects registry: %w", err)
	}
	var p Projects
	if err := toml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse projects registry %s: %w", path, err)
	}
	if p.Version != RegistryVersion {
		return nil, fmt.Errorf("projects registry %s: unsupported version %d (this binary supports %d)", path, p.Version, RegistryVersion)
	}
	if p.Entries == nil {
		p.Entries = map[string]ProjectEntry{}
	}
	for folder, entry := range p.Entries {
		if err := ValidateFolderName(folder); err != nil {
			return nil, fmt.Errorf("projects registry %s: %w", path, err)
		}
		if entry.ID == "" {
			return nil, fmt.Errorf("projects registry %s: folder %q has empty id", path, folder)
		}
	}
	// FolderFor assumes each id names exactly one folder; two folders
	// sharing an id would make its result depend on Go's randomized map
	// iteration order. Add can never produce this — enforce it here too,
	// against a hand-edited file. Folder names are compared, not iterated
	// in map order, so the message is deterministic.
	folderByID := make(map[string]string, len(p.Entries))
	for folder, entry := range p.Entries {
		if existing, dup := folderByID[entry.ID]; dup {
			first, second := existing, folder
			if second < first {
				first, second = second, first
			}
			return nil, fmt.Errorf("projects registry %s: id %q claimed by both folder %q and folder %q", path, entry.ID, first, second)
		}
		folderByID[entry.ID] = folder
	}
	return &p, nil
}

// Save atomically writes the registry with deterministic bytes. Entries
// is a Go map, but go-toml/v2 sorts map keys when encoding a table, so a
// single Marshal of the whole struct always emits folders in the same
// order — the file lives in git; nondeterministic key order would be
// diff churn.
func (p *Projects) Save(path string) error {
	data, err := toml.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode projects registry: %w", err)
	}
	if err := renameio.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write projects registry: %w", err)
	}
	return nil
}

// FolderFor returns the repo folder recorded for a canonical ID.
func (p *Projects) FolderFor(id string) (string, bool) {
	for folder, entry := range p.Entries {
		if entry.ID == id {
			return folder, true
		}
	}
	return "", false
}

// Add registers id under preferredFolder, disambiguating deterministically
// on collision (folder, folder-2, folder-3, …) and returning the folder
// actually recorded. Re-adding an existing id is idempotent (spec §3).
func (p *Projects) Add(id, preferredFolder string) (string, error) {
	if id == "" {
		return "", fmt.Errorf("empty project id")
	}
	if existing, ok := p.FolderFor(id); ok {
		return existing, nil
	}
	if err := ValidateFolderName(preferredFolder); err != nil {
		return "", err
	}
	candidate := preferredFolder
	for suffix := 2; ; suffix++ {
		if _, taken := p.Entries[candidate]; !taken {
			break
		}
		candidate = fmt.Sprintf("%s-%d", preferredFolder, suffix)
		if err := ValidateFolderName(candidate); err != nil {
			return "", err
		}
	}
	p.Entries[candidate] = ProjectEntry{ID: id}
	return candidate, nil
}
