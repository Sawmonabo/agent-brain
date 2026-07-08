package claude

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sawmonabo/agent-brain/internal/gitx"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// Adapter implements provider.Provider for Claude Code's per-project
// memory (spec §6).
type Adapter struct {
	home string
}

// New constructs a claude Adapter. home is the user home dir — the
// composition root passes os.UserHomeDir(), tests pass t.TempDir().
func New(home string) *Adapter {
	return &Adapter{home: home}
}

// Name returns the stable adapter identifier used in repo paths and
// registries.
func (a *Adapter) Name() string { return "claude" }

// Scope reports that Claude Code memory is per-project.
func (a *Adapter) Scope() provider.Scope { return provider.ScopePerProject }

// Patterns returns Claude's classification table (spec §6): MEMORY.md is
// the derived index; macOS Finder droppings are not memory data.
// Everything else unmatched falls through to ClassFact (spec §6:
// retain-both is the safest default for unknown files).
func (a *Adapter) Patterns() []provider.Pattern {
	return []provider.Pattern{
		{Glob: "MEMORY.md", Class: provider.ClassDerivedIndex},
		{Glob: ".DS_Store", Class: provider.ClassIgnore},
		{Glob: "**/.DS_Store", Class: provider.ClassIgnore},
	}
}

// Discover enumerates every ~/.claude/projects/<slug> whose memory/
// subdirectory exists. A missing projects root means Claude Code is not
// installed on this machine — not an error. Results are sorted by Label
// for a deterministic enrollment-picker order.
func (a *Adapter) Discover(_ context.Context) ([]provider.Discovered, error) {
	root := filepath.Join(a.home, ".claude", "projects")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude discover: read %s: %w", root, err)
	}

	statDir := func(p string) bool {
		info, err := os.Stat(p)
		return err == nil && info.IsDir()
	}

	var discovered []provider.Discovered
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		slug := entry.Name()
		memoryDir := filepath.Join(root, slug, "memory")
		info, err := os.Stat(memoryDir)
		if err != nil || !info.IsDir() {
			continue
		}
		discovered = append(discovered, provider.Discovered{
			LocalDir:  memoryDir,
			Label:     slug,
			PathGuess: GuessPath(slug, statDir),
		})
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].Label < discovered[j].Label })
	return discovered, nil
}

// Identify resolves projectPath's cross-machine identity from its git
// origin remote. Absence of a remote, or projectPath not being a git
// repo at all, is a nameable local project, not an error — the caller
// (enrollment) asks the user for a folder name.
func (a *Adapter) Identify(ctx context.Context, _ provider.Discovered, projectPath string) (provider.Identity, error) {
	fallback := provider.Identity{PreferredFolder: filepath.Base(projectPath)}
	if projectPath == "" {
		return provider.Identity{}, fmt.Errorf("claude identify: empty project path")
	}
	res, err := gitx.RunStatus(ctx, projectPath, "remote", "get-url", "origin")
	if err != nil || res.ExitCode != 0 {
		// Not a git repo, or no origin: a nameable local project, not an
		// error — the enrollment flow asks the user for a folder name.
		return fallback, nil //nolint:nilerr // absence of a remote is the documented remoteless path, not a failure
	}
	id, err := provider.NormalizeRemoteURL(strings.TrimSpace(res.Stdout))
	if err != nil {
		return fallback, nil //nolint:nilerr // unparseable remote → same remoteless path
	}
	return provider.Identity{ProjectID: id, PreferredFolder: path.Base(id)}, nil
}
