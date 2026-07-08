package repo

import "path/filepath"

const (
	// MetaDirName holds plaintext machine-shared metadata inside the
	// memories repo (registry + manifests) — excluded from filtering by
	// the generated .gitattributes.
	MetaDirName = ".agent-brain"
	// GlobalFolder holds user-global provider pools (Codex) — spec §3.
	GlobalFolder = "_global"

	projectsFileName = "projects.toml"
	manifestDirName  = "manifests"
)

// Layout resolves every path inside a memories checkout. It is pure path
// arithmetic: callers own validation (ValidateFolderName, the provider
// name contract) at the boundaries where names enter the system.
type Layout struct {
	root string
}

// NewLayout wraps the checkout root (absolute path).
func NewLayout(root string) Layout { return Layout{root: root} }

// Root returns the checkout root path.
func (l Layout) Root() string { return l.root }

// MetaDir returns the path to the repo's plaintext metadata directory.
func (l Layout) MetaDir() string { return filepath.Join(l.root, MetaDirName) }

// ProjectsFile returns the path to the project registry file.
func (l Layout) ProjectsFile() string { return filepath.Join(l.MetaDir(), projectsFileName) }

// ManifestDir returns the path to the per-host manifests directory.
func (l Layout) ManifestDir() string { return filepath.Join(l.MetaDir(), manifestDirName) }

// ManifestFile routes through SanitizeHostname so hostile hostnames can
// never escape the manifests dir.
func (l Layout) ManifestFile(host string) string {
	return filepath.Join(l.ManifestDir(), SanitizeHostname(host)+".json")
}

// AttributesFile returns the path to the checkout's .gitattributes file.
func (l Layout) AttributesFile() string { return filepath.Join(l.root, ".gitattributes") }

// UnitDir is the checkout dir for one sync unit: <folder>/<provider>.
// Pass GlobalFolder for global-scope providers.
func (l Layout) UnitDir(folder, providerName string) string {
	return filepath.Join(l.root, folder, providerName)
}
