package engine

import (
	"path"
	"path/filepath"

	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// unitDir resolves the checkout directory one unit mirrors to:
// <folder>/<provider>[/<repo_subdir>]. RepoSubdir is validated
// slash-relative at registry load; FromSlash makes it OS-correct here.
func (e *Engine) unitDir(u repo.Unit) string {
	dir := e.layout.UnitDir(u.Folder, u.Provider)
	if u.RepoSubdir != "" {
		dir = filepath.Join(dir, filepath.FromSlash(u.RepoSubdir))
	}
	return dir
}

// classifyRel is the provider-dir-relative name for classification —
// the namespace Patterns() globs and generated attribute rows share.
func classifyRel(u repo.Unit, rel string) string {
	return path.Join(u.RepoSubdir, rel)
}
