package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
		guess := sessionCWD(filepath.Join(root, slug))
		if guess == "" {
			guess = GuessPath(slug, statDir)
		}
		discovered = append(discovered, provider.Discovered{
			LocalDir:  memoryDir,
			Label:     slug,
			PathGuess: guess,
		})
	}
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].Label < discovered[j].Label })
	return discovered, nil
}

// sessionCWDLineLimit bounds how much of a session file's first line
// sessionCWD will read. Session records can carry large payloads; a
// first line still unterminated past this limit is skipped rather than
// buffered without bound.
const sessionCWDLineLimit = 1 << 20

// sessionCWD returns the project path recorded in projectDir's session
// files, or "" when none is recoverable. Claude Code writes the
// project's absolute path as "cwd" on every session .jsonl record, so
// the recorded value is authoritative where the slug is lossy (every
// non-alphanumeric character folds to '-', see SlugFor) — unicode or
// spaces in a real path are unrecoverable from the slug alone. Files
// are tried newest-mtime first (a moved project's newest session
// records where it lives NOW; older ones record where it used to), and
// only each file's first line is read, size-capped. Any unreadable,
// unparseable, or non-absolute result just tries the next file:
// discovery degrades to GuessPath reconstruction, never errors.
func sessionCWD(projectDir string) string {
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		return ""
	}
	type sessionFile struct {
		name    string
		modTime time.Time
	}
	var sessions []sessionFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		sessions = append(sessions, sessionFile{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(sessions, func(i, j int) bool {
		if !sessions[i].modTime.Equal(sessions[j].modTime) {
			return sessions[i].modTime.After(sessions[j].modTime)
		}
		return sessions[i].name < sessions[j].name
	})
	for _, session := range sessions {
		line, err := firstLine(filepath.Join(projectDir, session.name))
		if err != nil {
			continue
		}
		var record struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if filepath.IsAbs(record.Cwd) {
			return record.Cwd
		}
	}
	return ""
}

// firstLine reads path's first newline-terminated line, capped at
// sessionCWDLineLimit bytes. A first line larger than the cap comes
// back without its terminator and json.Unmarshal rejects the
// truncation — the caller's skip-and-continue handles it.
func firstLine(path string) ([]byte, error) {
	file, err := os.Open(path) //nolint:gosec // G304: path is composed from the adapter's own projects root
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReader(io.LimitReader(file, sessionCWDLineLimit))
	line, err := reader.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	return line, nil
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
