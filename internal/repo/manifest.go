package repo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"
)

// ManifestEntry is the state of one file as this host last synced it.
type ManifestEntry struct {
	Size          int64  `json:"size"`
	MTimeUnixNano int64  `json:"mtime_unix_nano"`
	SHA256        string `json:"sha256"`
}

// Manifest is this host's sync ledger (spec §4): which repo paths this
// machine has synced, and in what state. It disambiguates deletions —
// "in manifest but gone locally" is deleted-here; "in checkout but not
// in manifest" is new-from-remote — and gates mirror-out deletions
// (remote deletions apply to provider dirs only for paths the manifest
// proves were synced here before). Stored at
// .agent-brain/manifests/<host>.json; each host writes ONLY its own
// file, so manifests never merge-conflict.
type Manifest struct {
	Version int                      `json:"version"`
	Files   map[string]ManifestEntry `json:"files"`
	// ImportedFrom records completed migrate seeds on this host: bash-era
	// slug → repo folder (spec §10 step 5). Presence of a slug makes
	// SeedProject a no-op. Additive and omitempty — the version stays 1, so
	// an older manifest without the key loads unchanged (nil map).
	ImportedFrom map[string]string `json:"imported_from,omitempty"`
}

// NewManifest returns an empty manifest at the current version.
func NewManifest() *Manifest {
	return &Manifest{Version: RegistryVersion, Files: map[string]ManifestEntry{}}
}

// LoadManifest reads path; a missing file is an empty manifest (first
// sync on this host). The file rides the shared repo, so its content is
// remote-influenced input: unknown versions, corrupt JSON, and unsafe
// paths are explicit errors, never best-effort skips.
func LoadManifest(manifestPath string) (*Manifest, error) {
	data, err := os.ReadFile(manifestPath) //nolint:gosec // G304: manifestPath is derived from Layout.ManifestFile, not untrusted input
	if errors.Is(err, fs.ErrNotExist) {
		return NewManifest(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	if m.Version != RegistryVersion {
		return nil, fmt.Errorf("manifest %s: unsupported version %d (this binary supports %d)", manifestPath, m.Version, RegistryVersion)
	}
	if m.Files == nil {
		m.Files = map[string]ManifestEntry{}
	}
	for rel := range m.Files {
		if err := ValidateRelPath(rel); err != nil {
			return nil, fmt.Errorf("manifest %s: %w", manifestPath, err)
		}
	}
	return &m, nil
}

// Save atomically writes the manifest. encoding/json sorts map keys, and
// indentation keeps repo diffs reviewable.
func (m *Manifest) Save(manifestPath string) error {
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o750); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := renameio.WriteFile(manifestPath, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// Has reports whether rel was synced by this host before.
func (m *Manifest) Has(rel string) bool {
	_, ok := m.Files[rel]
	return ok
}

// Get returns the recorded state for rel.
func (m *Manifest) Get(rel string) (ManifestEntry, bool) {
	e, ok := m.Files[rel]
	return e, ok
}

// Set records rel at state e. rel must be a clean slash-separated
// repo-relative path — the same contract LoadManifest enforces.
func (m *Manifest) Set(rel string, e ManifestEntry) error {
	if err := ValidateRelPath(rel); err != nil {
		return err
	}
	m.Files[rel] = e
	return nil
}

// Delete removes rel from the ledger.
func (m *Manifest) Delete(rel string) {
	delete(m.Files, rel)
}

// ValidateRelPath admits only clean, slash-separated, repo-relative
// paths: non-empty, not absolute, no backslashes, no '.'/'..' segments,
// and already in path.Clean form (no '//', no './').
func ValidateRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("empty relative path")
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("path %q is absolute", rel)
	}
	if strings.Contains(rel, `\`) {
		return fmt.Errorf("path %q contains a backslash", rel)
	}
	if path.Clean(rel) != rel {
		return fmt.Errorf("path %q is not in clean form", rel)
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains a %q segment", rel, seg)
		}
	}
	return nil
}

// HashFile computes the manifest entry for an on-disk file.
func HashFile(filePath string) (ManifestEntry, error) {
	f, err := os.Open(filePath) //nolint:gosec // G304: filePath is derived from Layout unit paths, not untrusted input
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("hash %s: %w", filePath, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return ManifestEntry{}, fmt.Errorf("stat %s: %w", filePath, err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ManifestEntry{}, fmt.Errorf("hash %s: %w", filePath, err)
	}
	return ManifestEntry{
		Size:          info.Size(),
		MTimeUnixNano: info.ModTime().UnixNano(),
		SHA256:        hex.EncodeToString(h.Sum(nil)),
	}, nil
}
