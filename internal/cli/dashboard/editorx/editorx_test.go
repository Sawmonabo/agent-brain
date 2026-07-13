package editorx_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/editorx"
	"github.com/Sawmonabo/agent-brain/internal/config"
)

// fakeGetenv builds a getenv seam over a fixed map, the same role
// os.Getenv plays in production (selfupdate.Updater.Getenv precedent):
// an absent key and a present-but-empty key both return "".
func fakeGetenv(env map[string]string) func(string) string {
	return func(name string) string { return env[name] }
}

// TestResolvePrecedence pins the settings.Command -> $VISUAL -> $EDITOR
// order: the first non-empty source wins, and a source that is set but
// blank counts as unset exactly like an absent one.
func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings config.EditorSettings
		env      map[string]string
		wantArgv []string
		wantErr  error
	}{
		{
			name:     "command set wins over VISUAL and EDITOR",
			settings: config.EditorSettings{Command: "cursor --wait", InTerminal: true},
			env:      map[string]string{"VISUAL": "code --wait", "EDITOR": "vim"},
			wantArgv: []string{"cursor", "--wait"},
		},
		{
			name:     "VISUAL wins over EDITOR when command is unset",
			settings: config.EditorSettings{InTerminal: true},
			env:      map[string]string{"VISUAL": "code --wait", "EDITOR": "vim"},
			wantArgv: []string{"code", "--wait"},
		},
		{
			name:     "EDITOR is used when command and VISUAL are unset",
			settings: config.EditorSettings{InTerminal: true},
			env:      map[string]string{"EDITOR": "vim"},
			wantArgv: []string{"vim"},
		},
		{
			name:     "all three set: command still wins",
			settings: config.EditorSettings{Command: "nano", InTerminal: true},
			env:      map[string]string{"VISUAL": "code --wait", "EDITOR": "vim"},
			wantArgv: []string{"nano"},
		},
		{
			name:     "none set yields ErrNoEditor",
			settings: config.EditorSettings{InTerminal: true},
			env:      map[string]string{},
			wantErr:  editorx.ErrNoEditor,
		},
		{
			name:     "VISUAL set but blank counts as unset, falls through to EDITOR",
			settings: config.EditorSettings{InTerminal: true},
			env:      map[string]string{"VISUAL": "", "EDITOR": "vim"},
			wantArgv: []string{"vim"},
		},
		{
			name:     "command set but blank counts as unset, falls through to VISUAL",
			settings: config.EditorSettings{Command: "", InTerminal: true},
			env:      map[string]string{"VISUAL": "code --wait"},
			wantArgv: []string{"code", "--wait"},
		},
		{
			name:     "all three set but blank yields ErrNoEditor",
			settings: config.EditorSettings{Command: "", InTerminal: true},
			env:      map[string]string{"VISUAL": "", "EDITOR": ""},
			wantErr:  editorx.ErrNoEditor,
		},
		{
			// Non-empty but whitespace-only survives the emptiness guard and
			// parses to zero words — the zero-words branch must fall through
			// to the next source exactly like an unset one.
			name:     "command of only whitespace parses to zero words, falls through to VISUAL",
			settings: config.EditorSettings{Command: "   ", InTerminal: true},
			env:      map[string]string{"VISUAL": "code --wait"},
			wantArgv: []string{"code", "--wait"},
		},
		{
			name:     "in_terminal false is carried through resolution",
			settings: config.EditorSettings{Command: "cursor --wait", InTerminal: false},
			env:      map[string]string{},
			wantArgv: []string{"cursor", "--wait"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := editorx.Resolve(tc.settings, fakeGetenv(tc.env))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Resolve() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.wantArgv, got.Argv); diff != "" {
				t.Fatalf("Argv mismatch (-want +got):\n%s", diff)
			}
			if got.InTerminal != tc.settings.InTerminal {
				t.Fatalf("InTerminal = %v, want %v", got.InTerminal, tc.settings.InTerminal)
			}
		})
	}
}

// TestResolveQuoting pins mvdan.cc/sh/v3/shell.Fields' word-splitting and
// expansion behavior as Resolve exposes it through the getenv adapter —
// verified empirically against v3.13.1, not assumed.
func TestResolveQuoting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		command string
		env     map[string]string
		want    []string
	}{
		{
			name:    "a quoted argument containing a space stays one field",
			command: `"my editor" --flag`,
			want:    []string{"my editor", "--flag"},
		},
		{
			name:    "a bare dollar variable expands via the getenv adapter",
			command: "$EDITOR_BIN --wait",
			env:     map[string]string{"EDITOR_BIN": "code"},
			want:    []string{"code", "--wait"},
		},
		{
			name: "a leading NAME=value token is not a shell assignment prefix",
			// shell.Fields parses in word-sequence mode, not full
			// command/assignment mode: a `NAME=value` token fused to a
			// following quoted string (no separating space) becomes one
			// literal field rather than being consumed as an env
			// assignment ahead of the command the way a real POSIX shell
			// would treat it. editor.command is not a shell command
			// line — this pins that gap so it is never assumed away.
			command: `FOO="my editor" --flag`,
			want:    []string{"FOO=my editor", "--flag"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := config.EditorSettings{Command: tc.command, InTerminal: true}
			got, err := editorx.Resolve(settings, fakeGetenv(tc.env))
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if diff := cmp.Diff(tc.want, got.Argv); diff != "" {
				t.Fatalf("Argv mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestResolvePropagatesEditorCommandParseErrors proves a malformed source
// surfaces its parse error immediately rather than being silently skipped
// in favor of a lower-precedence source — a bad quote in editor.command or
// $VISUAL is a configuration mistake worth surfacing distinctly from
// "nothing configured". Pinned per source so the halt-don't-mask behavior
// cannot silently diverge between the config and env paths.
func TestResolvePropagatesEditorCommandParseErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings config.EditorSettings
		env      map[string]string
	}{
		{
			name:     "malformed settings.Command halts, never falls through to EDITOR",
			settings: config.EditorSettings{Command: `"unterminated`, InTerminal: true},
			env:      map[string]string{"EDITOR": "vim"},
		},
		{
			name:     "malformed VISUAL halts, never falls through to EDITOR",
			settings: config.EditorSettings{InTerminal: true},
			env:      map[string]string{"VISUAL": `"unterminated`, "EDITOR": "vim"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := editorx.Resolve(tc.settings, fakeGetenv(tc.env))
			if err == nil {
				t.Fatal("Resolve() error = nil, want a parse error for malformed quoting")
			}
			if errors.Is(err, editorx.ErrNoEditor) {
				t.Fatalf("Resolve() error = %v, want a parse error distinct from ErrNoEditor", err)
			}
		})
	}
}

// TestNewScratchDirUnderProvidedCacheRoot proves a non-empty cacheRoot is
// honored and the returned directory actually exists.
func TestNewScratchDirUnderProvidedCacheRoot(t *testing.T) {
	t.Parallel()
	cacheRoot := t.TempDir()

	dir, cleanup, err := editorx.NewScratchDir(cacheRoot)
	if err != nil {
		t.Fatalf("NewScratchDir() error = %v", err)
	}
	t.Cleanup(cleanup)

	if rel, relErr := filepath.Rel(cacheRoot, dir); relErr != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf("NewScratchDir() dir = %s, want a child of cacheRoot %s", dir, cacheRoot)
	}
	info, statErr := os.Stat(dir)
	if statErr != nil {
		t.Fatalf("scratch dir does not exist: %v", statErr)
	}
	if !info.IsDir() {
		t.Fatalf("scratch path %s is not a directory", dir)
	}
}

// TestNewScratchDirEmptyCacheRootResolvesUserCacheDir proves cacheRoot ""
// resolves os.UserCacheDir(), never a directory inside any watched tree.
func TestNewScratchDirEmptyCacheRootResolvesUserCacheDir(t *testing.T) {
	t.Parallel()
	wantRoot, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("os.UserCacheDir() unavailable in this environment: %v", err)
	}

	dir, cleanup, err := editorx.NewScratchDir("")
	if err != nil {
		t.Fatalf(`NewScratchDir("") error = %v`, err)
	}
	t.Cleanup(cleanup)

	rel, relErr := filepath.Rel(wantRoot, dir)
	if relErr != nil || strings.HasPrefix(rel, "..") {
		t.Fatalf(`NewScratchDir("") dir = %s, want a child of os.UserCacheDir() %s`, dir, wantRoot)
	}
}

// TestNewScratchDirCleanupRemovesDir proves the returned cleanup actually
// removes the directory it created.
func TestNewScratchDirCleanupRemovesDir(t *testing.T) {
	t.Parallel()
	dir, cleanup, err := editorx.NewScratchDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewScratchDir() error = %v", err)
	}
	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("scratch dir %s still exists after cleanup (stat err = %v)", dir, statErr)
	}
}

// TestStagePreservesFilenameAndBytes proves Stage writes the memory's own
// filename verbatim (never a synthetic name) with unmodified bytes.
func TestStagePreservesFilenameAndBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	content := []byte("---\ntype: fact\n---\nremember this\n")

	scratchPath, err := editorx.Stage(dir, "notes.md", content)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	wantPath := filepath.Join(dir, "notes.md")
	if scratchPath != wantPath {
		t.Fatalf("Stage() scratchPath = %s, want %s (filename must be preserved)", scratchPath, wantPath)
	}
	got, err := os.ReadFile(scratchPath)
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	if diff := cmp.Diff(content, got); diff != "" {
		t.Fatalf("staged bytes mismatch (-want +got):\n%s", diff)
	}
}

// TestStageRejectsFilenameWithPathSeparator hardens Stage against a
// filename that would let content escape the scratch dir it was given —
// whether from a caller bug or a hostile provider-supplied name.
func TestStageRejectsFilenameWithPathSeparator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cases := []string{"../escape.md", "sub/dir.md", "/etc/passwd"}
	for _, filename := range cases {
		t.Run(filename, func(t *testing.T) {
			t.Parallel()
			if _, err := editorx.Stage(dir, filename, []byte("x")); err == nil {
				t.Fatalf("Stage(%q) error = nil, want rejection of the path separator", filename)
			}
		})
	}
}

// TestNewScratchDirAndStageRoundTrip exercises the full scratch lifecycle
// together: create, stage, read back, then clean up — proving cleanup
// removes both the directory and the staged file inside it.
func TestNewScratchDirAndStageRoundTrip(t *testing.T) {
	t.Parallel()
	content := []byte("round trip content\n")

	dir, cleanup, err := editorx.NewScratchDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewScratchDir() error = %v", err)
	}
	defer cleanup()

	scratchPath, err := editorx.Stage(dir, "memory.md", content)
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	got, err := os.ReadFile(scratchPath)
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	if diff := cmp.Diff(content, got); diff != "" {
		t.Fatalf("staged bytes mismatch (-want +got):\n%s", diff)
	}

	cleanup()
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("scratch dir %s still exists after cleanup (stat err = %v)", dir, statErr)
	}
	if _, statErr := os.Stat(scratchPath); !os.IsNotExist(statErr) {
		t.Fatalf("staged file %s still exists after cleanup (stat err = %v)", scratchPath, statErr)
	}
}

// TestChanged pins the kubectl no-op contract: identical bytes mean the
// edit was cancelled, edited bytes are reported back, and a scratch file
// that vanished is always an error, never a false "unchanged".
func TestChanged(t *testing.T) {
	t.Parallel()
	original := []byte("original content\n")

	t.Run("identical bytes report unchanged", func(t *testing.T) {
		t.Parallel()
		scratchPath := filepath.Join(t.TempDir(), "notes.md")
		if err := os.WriteFile(scratchPath, original, 0o600); err != nil {
			t.Fatal(err)
		}
		changed, edited, err := editorx.Changed(original, scratchPath)
		if err != nil {
			t.Fatalf("Changed() error = %v", err)
		}
		if changed {
			t.Fatalf("Changed() changed = true, want false for identical bytes")
		}
		if diff := cmp.Diff(original, edited); diff != "" {
			t.Fatalf("edited bytes mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("edited bytes report changed with the new content", func(t *testing.T) {
		t.Parallel()
		scratchPath := filepath.Join(t.TempDir(), "notes.md")
		newContent := []byte("edited content\n")
		if err := os.WriteFile(scratchPath, newContent, 0o600); err != nil {
			t.Fatal(err)
		}
		changed, edited, err := editorx.Changed(original, scratchPath)
		if err != nil {
			t.Fatalf("Changed() error = %v", err)
		}
		if !changed {
			t.Fatalf("Changed() changed = false, want true for edited bytes")
		}
		if diff := cmp.Diff(newContent, edited); diff != "" {
			t.Fatalf("edited bytes mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a scratch file deleted by a hostile editor is an error, never unchanged", func(t *testing.T) {
		t.Parallel()
		scratchPath := filepath.Join(t.TempDir(), "notes.md") // never created
		changed, edited, err := editorx.Changed(original, scratchPath)
		if err == nil {
			t.Fatalf("Changed() error = nil, want an error for a missing scratch file (changed=%v)", changed)
		}
		if changed {
			t.Fatal("Changed() changed = true alongside an error; want false")
		}
		if edited != nil {
			t.Fatalf("Changed() edited = %v, want nil alongside an error", edited)
		}
	})
}

// TestCommandAppendsScratchPathLast proves Command builds argv with the
// scratch path as the final element, after any configured flags.
func TestCommandAppendsScratchPathLast(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		ed          editorx.Editor
		scratchPath string
		want        []string
	}{
		{
			name:        "single-word editor",
			ed:          editorx.Editor{Argv: []string{"vim"}, InTerminal: true},
			scratchPath: "/scratch/dir/notes.md",
			want:        []string{"vim", "/scratch/dir/notes.md"},
		},
		{
			name:        "editor with flags",
			ed:          editorx.Editor{Argv: []string{"cursor", "--wait"}, InTerminal: false},
			scratchPath: "/scratch/dir/notes.md",
			want:        []string{"cursor", "--wait", "/scratch/dir/notes.md"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := editorx.Command(tc.ed, tc.scratchPath)
			if diff := cmp.Diff(tc.want, cmd.Args); diff != "" {
				t.Fatalf("cmd.Args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
