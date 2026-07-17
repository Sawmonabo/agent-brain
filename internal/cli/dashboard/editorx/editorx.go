// Package editorx resolves the user's editor and manages the scratch-copy
// lifecycle for the dashboard hub's $EDITOR handoff (ADR 20 decision 2). It
// has no TUI dependency — only stdlib, mvdan.cc/sh/v3/shell, and
// internal/config — so it stays usable outside the dashboard's bubbletea
// event loop; a later task wires it into the hub's edit flow via
// tea.ExecProcess.
package editorx

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mvdan.cc/sh/v3/shell"

	"github.com/Sawmonabo/agent-brain/internal/config"
)

// Editor is a resolved, ready-to-run editor command.
type Editor struct {
	// Argv is the resolved command words (program plus any flags), in the
	// order they were configured; Command appends the scratch file path
	// as the final argument.
	Argv []string
	// InTerminal mirrors config.EditorSettings.InTerminal for the caller
	// that decides whether to suspend the TUI around the child process.
	InTerminal bool
}

// ErrNoEditor is returned by Resolve when settings.Command, $VISUAL, and
// $EDITOR are all unset or blank. The hub gates the edit binding on this
// error — visibly disabling it with an honest message (crush-style) —
// rather than ever falling back to a default editor. The charmbracelet
// x/editor library was evaluated for this exact role and dropped for
// silently defaulting to nano when nothing is configured, among other
// rule violations (the plan's dependency-verification note; ADR 20
// correction).
var ErrNoEditor = errors.New("no editor configured — set $EDITOR or editor.command in config")

// scratchDirPattern names the per-edit temp dir created under the cache
// root; the trailing "-*" is os.MkdirTemp's randomization placeholder.
const scratchDirPattern = "agent-brain-edit-*"

// Resolve picks the editor command in order: settings.Command, then
// $VISUAL, then $EDITOR — the first non-empty source wins. A source that
// is set but blank counts as unset exactly like an absent one, because
// os.Getenv (and any getenv seam built the same way) already returns ""
// for both, so the same blank check handles both cases uniformly.
//
// The winning source string is word-split with mvdan.cc/sh/v3/shell.Fields
// through the getenv adapter, a POSIX-aware splitter that honors quoting
// and escaping — never strings.Fields, which cannot parse a quoted
// argument such as `cursor --wait "my file"`. A source that is non-empty
// but parses to zero words (e.g. all whitespace) is treated as unusable
// and the next source is tried; a source that fails to parse (bad
// quoting) returns that error immediately rather than silently falling
// through, since a malformed editor.command is a configuration mistake
// worth surfacing distinctly from "nothing configured". All sources
// empty or blank returns ErrNoEditor.
//
// getenv is the seam over the process environment: os.Getenv in
// production, a scripted map in tests (selfupdate.Updater.Getenv
// precedent).
func Resolve(settings config.EditorSettings, getenv func(string) string) (Editor, error) {
	candidates := []string{settings.Command, getenv("VISUAL"), getenv("EDITOR")}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		argv, err := shell.Fields(candidate, getenv)
		if err != nil {
			return Editor{}, fmt.Errorf("parse editor command %q: %w", candidate, err)
		}
		if len(argv) == 0 {
			continue
		}
		return Editor{Argv: argv, InTerminal: settings.InTerminal}, nil
	}
	return Editor{}, ErrNoEditor
}

// NewScratchDir creates a fresh, empty directory for one edit session
// under cacheRoot (os.UserCacheDir() when cacheRoot is ""). Scratch
// directories never live inside a watched provider tree: the editor only
// ever sees a disposable copy, so fsnotify never observes the editor's own
// atomic-save churn (vim backupcopy, swap files — a documented hazard
// class for watchers; ADR 20 D2). The caller must invoke the returned
// cleanup once the edit session ends, successful or not, to remove the
// directory and everything staged inside it.
func NewScratchDir(cacheRoot string) (dir string, cleanup func(), err error) {
	cacheDirRoot := cacheRoot
	if cacheDirRoot == "" {
		resolvedCacheDir, resolveErr := os.UserCacheDir()
		if resolveErr != nil {
			return "", nil, fmt.Errorf("resolve user cache dir: %w", resolveErr)
		}
		cacheDirRoot = resolvedCacheDir
	}
	// The cache root may not exist yet on a fresh machine or CI container
	// (no prior program has created it); scratch creation must not depend
	// on something else having created it first.
	if mkdirErr := os.MkdirAll(cacheDirRoot, 0o700); mkdirErr != nil {
		return "", nil, fmt.Errorf("create cache root %s: %w", cacheDirRoot, mkdirErr)
	}
	scratchDir, err := os.MkdirTemp(cacheDirRoot, scratchDirPattern)
	if err != nil {
		return "", nil, fmt.Errorf("create scratch dir: %w", err)
	}
	return scratchDir, func() { _ = os.RemoveAll(scratchDir) }, nil
}

// ScratchStaleAfter is the age (on the dir's mtime, which staging sets — a
// proxy for when the session began) at which SweepStaleScratch reclaims a
// scratch dir. Scratch dirs hold PLAINTEXT memory copies, so nothing may
// persist indefinitely; but the caller deliberately preserves a scratch on
// its failure paths precisely because it is the user's ONLY remaining copy
// of an edit, and the pointer to it is a transient toast — so the rescue
// window must comfortably span human absence (a weekend, a short trip).
// Destroying the only copy early is strictly worse than a week of lingering
// plaintext inside the user's own 0700 cache dir, the same trust domain as
// the provider dirs holding the same content; after a full week unrescued,
// the edit is abandoned. The age floor also means a sweep at one hub's
// start can never touch another live hub's in-flight session, which is
// hours old at most.
const ScratchStaleAfter = 7 * 24 * time.Hour

// SweepStaleScratch reclaims scratch dirs under cacheRoot aged
// ScratchStaleAfter or more: sessions orphaned by a quit or crash mid-edit
// and preserved-but-abandoned failure scratches would otherwise accumulate
// plaintext memory copies forever. The hub invokes it once at start, before
// any session of its own exists. Best-effort by contract: each candidate is
// judged and removed independently, one failure never stops the sweep, and
// the joined error is advisory — anything unreclaimed simply waits for the
// next launch. Only directories matching NewScratchDir's own naming are
// considered; a missing cacheRoot means nothing was ever staged.
func SweepStaleScratch(cacheRoot string, now time.Time) error {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sweep scratch root %s: %w", cacheRoot, err)
	}
	scratchDirPrefix := strings.TrimSuffix(scratchDirPattern, "*")
	staleCutoff := now.Add(-ScratchStaleAfter)
	var sweepErrors []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), scratchDirPrefix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			sweepErrors = append(sweepErrors, infoErr)
			continue
		}
		if info.ModTime().After(staleCutoff) {
			continue // strictly younger than the cutoff: live or still rescuable
		}
		if removeErr := os.RemoveAll(filepath.Join(cacheRoot, entry.Name())); removeErr != nil {
			sweepErrors = append(sweepErrors, removeErr)
		}
	}
	return errors.Join(sweepErrors...)
}

// Stage writes content into dir under filename — the memory's own
// basename — so the editor keys syntax highlighting and mode selection off
// the real extension (a .md scratch file must stay named *.md). filename
// must be a bare basename: Stage rejects any value containing a path
// separator, so a caller bug or a hostile filename can never write outside
// dir.
func Stage(dir, filename string, content []byte) (scratchPath string, err error) {
	if filepath.Base(filename) != filename {
		return "", fmt.Errorf("stage: filename %q must not contain a path separator", filename)
	}
	scratchPath = filepath.Join(dir, filename)
	if err := os.WriteFile(scratchPath, content, 0o600); err != nil {
		return "", fmt.Errorf("stage %s: %w", scratchPath, err)
	}
	return scratchPath, nil
}

// Command builds the exec.Cmd that runs ed with scratchPath appended as
// its final argument. ed.Argv must be non-empty — only a successful
// Resolve constructs an Editor, and Resolve never returns an empty argv —
// so an empty argv here is a caller bug (a hand-built Editor literal), and
// Command panics with the precondition rather than letting ed.Argv[0]
// fail as a bare index-out-of-range. Command neither starts the process
// nor binds a context — nothing here bounds its lifetime — because the
// TUI owns process lifetime via tea.ExecProcess/Cmd.Run.
func Command(ed Editor, scratchPath string) *exec.Cmd {
	if len(ed.Argv) == 0 {
		panic("editorx: Command called with empty Editor.Argv — only a successful Resolve may construct Editor")
	}
	args := make([]string, 0, len(ed.Argv))
	args = append(args, ed.Argv[1:]...)
	args = append(args, scratchPath)
	//nolint:gosec // G204: ed.Argv is resolved by Resolve from settings.Command/$VISUAL/$EDITOR, not untrusted input
	return exec.Command(ed.Argv[0], args...)
}

// Changed reports whether scratchPath's current bytes differ from
// original — kubectl's no-op contract: identical bytes mean the edit was
// cancelled. A scratch file that no longer exists is always an error,
// never "unchanged": a hostile or misbehaving editor that deletes the
// scratch file must not be mistaken for a no-op edit that silently
// discards the user's memory.
func Changed(original []byte, scratchPath string) (changed bool, edited []byte, err error) {
	//nolint:gosec // G304: scratchPath is the caller's own scratch file from Stage/NewScratchDir, not untrusted input
	edited, err = os.ReadFile(scratchPath)
	if err != nil {
		return false, nil, fmt.Errorf("read scratch %s: %w", scratchPath, err)
	}
	return !bytes.Equal(original, edited), edited, nil
}
