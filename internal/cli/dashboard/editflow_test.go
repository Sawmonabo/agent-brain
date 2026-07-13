package dashboard

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/editorx"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// flowT0 is the fixed model clock every edit-flow test starts from — the
// fake clock the brief mandates: m.now only ever advances through tickMsg
// (or direct assignment here), never through time.Now in an Update path.
var flowT0 = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// strikethroughPattern matches an SGR sequence carrying attribute 9
// (strikethrough) as a standalone parameter ANYWHERE in the sequence — how
// the stack footer renders a visibly-disabled row. lipgloss merges a
// style's attributes into one CSI sequence, so 9 may sit between faint (2)
// and the foreground (38;…) rather than last; requiring whole ";"-delimited
// parameters keeps 29/39/49 from false-matching.
var strikethroughPattern = regexp.MustCompile(`\x1b\[(?:[0-9]+;)*9(?:;[0-9]+)*m`)

// newFlowModel builds a root model wired for edit-flow tests: an isolated
// scratch cache root, a scripted empty environment (so a developer machine's
// real $EDITOR/$VISUAL can never leak into an assertion), and the fixed
// flowT0 clock.
func newFlowModel(t *testing.T, settings config.Settings) (Model, string) {
	t.Helper()
	cacheRoot := t.TempDir()
	m := New(Config{Data: &fakeData{}, CacheRoot: cacheRoot, Settings: settings})
	m.getenv = func(string) string { return "" }
	m.now = flowT0
	return m, cacheRoot
}

// terminalEditorSettings configures a resolvable in-terminal editor. The
// command ("true") is never actually run by these tests — the launch Cmd is
// deliberately not executed on the ExecProcess path, and the finish paths are
// driven by sending editorFinishedMsg directly.
func terminalEditorSettings() config.Settings {
	return config.Settings{Editor: config.EditorSettings{Command: "true", InTerminal: true}}
}

// writeFlowMemory seeds one provider file on disk and returns its Memory
// (fact class — the editable default; tests that need a derived class
// override the field).
func writeFlowMemory(t *testing.T, dir, rel, content string) memoryfs.Memory {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return memoryfs.Memory{
		Provider: "claude",
		Folder:   "acme",
		LocalDir: dir,
		RelPath:  rel,
		RepoPath: "claude/" + rel,
		Name:     strings.TrimSuffix(path.Base(rel), path.Ext(rel)),
		Class:    provider.ClassFact,
	}
}

// cacheRootEntries counts entries under the scratch cache root — 0 proves no
// scratch dir was ever created, 1 proves exactly one edit session staged.
func cacheRootEntries(t *testing.T, cacheRoot string) int {
	t.Helper()
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestEditRefusedWithoutEditor(t *testing.T) {
	t.Parallel()
	m, cacheRoot := newFlowModel(t, config.Settings{})
	memory := writeFlowMemory(t, t.TempDir(), "note.md", "# note\n")

	m, cmd := step(m, views.EditRequestMsg{Memory: memory})

	const want = "no editor configured — set $EDITOR or editor.command in config"
	if got := plain(m.toastLine()); got != want {
		t.Errorf("toast = %q, want exactly %q", got, want)
	}
	if m.editing != nil {
		t.Error("a session started despite no editor resolving")
	}
	if cmd != nil {
		t.Errorf("request produced a Cmd (%#v); want none", cmd())
	}
	if got := cacheRootEntries(t, cacheRoot); got != 0 {
		t.Errorf("cache root has %d entries, want 0 (no scratch dir may be created)", got)
	}
}

func TestEditUnchangedIsCancelled(t *testing.T) {
	t.Parallel()
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# original\n")
	targetPath := filepath.Join(unitDir, "note.md")
	staleModTime := time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC)
	if err := os.Chtimes(targetPath, staleModTime, staleModTime); err != nil {
		t.Fatal(err)
	}

	m, _ = step(m, views.EditRequestMsg{Memory: memory})
	if m.editing == nil {
		t.Fatal("setup: no edit session after the request")
	}
	scratchDir := filepath.Dir(m.editing.scratchPath)

	m, _ = step(m, editorFinishedMsg{})

	if got := plain(m.toastLine()); got != "edit cancelled, no changes made" {
		t.Errorf("toast = %q, want the exact cancelled wording", got)
	}
	content, err := os.ReadFile(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# original\n" {
		t.Errorf("target content = %q, want untouched original", content)
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(staleModTime) {
		t.Errorf("target mtime = %v, want untouched %v (a cancelled edit must not rewrite the file)", info.ModTime(), staleModTime)
	}
	if _, err := os.Stat(scratchDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratch dir still present after a cancelled edit: %v", err)
	}
	if m.editing != nil {
		t.Error("session still active after finish")
	}
	if m.pendingCapture != nil {
		t.Error("a cancelled edit set a pendingCapture; nothing landed, nothing to confirm")
	}
}

func TestEditChangedLandsAtomically(t *testing.T) {
	t.Parallel()
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# original\n")

	m, _ = step(m, views.EditRequestMsg{Memory: memory})
	if m.editing == nil {
		t.Fatal("setup: no edit session after the request")
	}
	scratchDir := filepath.Dir(m.editing.scratchPath)
	if err := os.WriteFile(m.editing.scratchPath, []byte("# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ = step(m, editorFinishedMsg{})

	content, err := os.ReadFile(filepath.Join(unitDir, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# edited\n" {
		t.Errorf("target content = %q, want the edited bytes", content)
	}
	if m.pendingCapture == nil {
		t.Fatal("no pendingCapture after a landed edit")
	}
	if m.pendingCapture.folder != "acme" {
		t.Errorf("pendingCapture.folder = %q, want %q", m.pendingCapture.folder, "acme")
	}
	if !m.pendingCapture.since.Equal(flowT0) {
		t.Errorf("pendingCapture.since = %v, want the model clock %v", m.pendingCapture.since, flowT0)
	}
	if _, err := os.Stat(scratchDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratch dir still present after a landed edit: %v", err)
	}
	if got := plain(m.toastLine()); got != "saved" {
		t.Errorf("toast = %q, want %q", got, "saved")
	}
}

// TestEditDerivedClassRefused pins the derived-class gate for every request
// that targets an existing memory — e, r, and d all funnel through the one
// class check, so a MEMORY.md-style derived index can never start a session,
// a rename, or a delete.
func TestEditDerivedClassRefused(t *testing.T) {
	t.Parallel()
	derived := func(t *testing.T) memoryfs.Memory {
		memory := writeFlowMemory(t, t.TempDir(), "MEMORY.md", "# index\n")
		memory.Class = provider.ClassDerivedIndex
		return memory
	}
	tests := []struct {
		name    string
		request func(memoryfs.Memory) tea.Msg
	}{
		{name: "edit", request: func(m memoryfs.Memory) tea.Msg { return views.EditRequestMsg{Memory: m} }},
		{name: "rename", request: func(m memoryfs.Memory) tea.Msg { return views.RenameRequestMsg{Memory: m} }},
		{name: "delete", request: func(m memoryfs.Memory) tea.Msg { return views.DeleteRequestMsg{Memory: m} }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m, cacheRoot := newFlowModel(t, terminalEditorSettings())
			memory := derived(t)

			m, _ = step(m, testCase.request(memory))

			const want = "derived index — regenerated by the provider; edit the memory files instead"
			if got := plain(m.toastLine()); got != want {
				t.Errorf("toast = %q, want exactly %q", got, want)
			}
			if m.editing != nil {
				t.Error("a session started on a derived-class memory")
			}
			if m.flowModal != nil {
				t.Error("a flow modal opened on a derived-class memory")
			}
			if got := cacheRootEntries(t, cacheRoot); got != 0 {
				t.Errorf("cache root has %d entries, want 0", got)
			}
		})
	}
}

func TestNewStagesSkeletonAndRemindsIndex(t *testing.T) {
	t.Parallel()
	unit := func(t *testing.T) api.UnitInfo {
		return api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}
	}
	typeName := func(t *testing.T, m Model, name string) Model {
		t.Helper()
		for _, r := range name {
			m, _ = step(m, key(string(r)))
		}
		return m
	}

	t.Run("edited skeleton lands with the MEMORY.md reminder", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		claudeUnit := unit(t)
		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})
		if m.flowModal == nil {
			t.Fatal("n request did not open the name modal")
		}
		m = typeName(t, m, "api-notes")
		m, _ = step(m, key("enter"))

		if m.editing == nil {
			t.Fatal("no edit session after submitting the name")
		}
		if m.editing.targetRel != "api-notes.md" {
			t.Errorf("targetRel = %q, want %q (claude names are forced to .md)", m.editing.targetRel, "api-notes.md")
		}
		staged, err := os.ReadFile(m.editing.scratchPath)
		if err != nil {
			t.Fatal(err)
		}
		wantSkeleton := memoryfs.Skeleton("claude", "api-notes")
		if string(staged) != wantSkeleton {
			t.Errorf("staged scratch = %q, want the provider skeleton %q", staged, wantSkeleton)
		}

		if err := os.WriteFile(m.editing.scratchPath, []byte(wantSkeleton+"actual content\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, _ = step(m, editorFinishedMsg{})

		landed, err := os.ReadFile(filepath.Join(claudeUnit.LocalDir, "api-notes.md"))
		if err != nil {
			t.Fatalf("new memory did not land: %v", err)
		}
		if !strings.Contains(string(landed), "actual content") {
			t.Errorf("landed content = %q, want the edited bytes", landed)
		}
		if got := plain(m.toastLine()); got != "saved — remember the MEMORY.md index line" {
			t.Errorf("toast = %q, want the exact MEMORY.md reminder", got)
		}
		if m.pendingCapture == nil || m.pendingCapture.folder != "acme" {
			t.Errorf("pendingCapture = %+v, want folder acme", m.pendingCapture)
		}
	})

	t.Run("unedited skeleton is cancelled and creates nothing", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		claudeUnit := unit(t)
		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})
		m = typeName(t, m, "api-notes")
		m, _ = step(m, key("enter"))
		if m.editing == nil {
			t.Fatal("no edit session after submitting the name")
		}

		m, _ = step(m, editorFinishedMsg{})

		if got := plain(m.toastLine()); got != "edit cancelled, no changes made" {
			t.Errorf("toast = %q, want the exact cancelled wording (kubectl rule: byte-equal save is a cancel)", got)
		}
		if _, err := os.Stat(filepath.Join(claudeUnit.LocalDir, "api-notes.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an unedited skeleton still landed a file: %v", err)
		}
		if m.pendingCapture != nil {
			t.Error("a cancelled new set a pendingCapture")
		}
	})
}

// TestNewNameValidation pins the name rules at the submit seam: empty and
// slash-carrying names are refused with the modal kept open, traversal-shaped
// names are refused by the shared repo.ValidateRelPath guard, an existing
// file's name is refused rather than silently staging an overwrite, and the
// claude .md forcing never double-appends.
func TestNewNameValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		typed         string
		wantToastPart string
	}{
		{name: "empty name refused", typed: "", wantToastPart: "name must not be empty"},
		{name: "slash refused", typed: "sub/notes", wantToastPart: "must not contain /"},
		{name: "traversal refused", typed: "..", wantToastPart: `".."`},
		{name: "existing name refused", typed: "taken", wantToastPart: "already exists"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m, cacheRoot := newFlowModel(t, terminalEditorSettings())
			claudeUnit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}
			if err := os.WriteFile(filepath.Join(claudeUnit.LocalDir, "taken.md"), []byte("# taken\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})
			if m.flowModal == nil {
				t.Fatal("n request did not open the name modal")
			}
			for _, r := range testCase.typed {
				m, _ = step(m, key(string(r)))
			}

			m, _ = step(m, key("enter"))

			if got := plain(m.toastLine()); !strings.Contains(got, testCase.wantToastPart) {
				t.Errorf("toast = %q, want it to contain %q", got, testCase.wantToastPart)
			}
			if m.flowModal == nil {
				t.Error("modal closed on a refused name; it must stay open for a correction")
			}
			if m.editing != nil {
				t.Error("a session started despite the refused name")
			}
			if got := cacheRootEntries(t, cacheRoot); got != 0 {
				t.Errorf("cache root has %d entries, want 0", got)
			}
		})
	}

	t.Run("md suffix is not double-appended", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		claudeUnit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}
		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})
		for _, r := range "api-notes.md" {
			m, _ = step(m, key(string(r)))
		}
		m, _ = step(m, key("enter"))
		if m.editing == nil {
			t.Fatal("no edit session after submitting the name")
		}
		if m.editing.targetRel != "api-notes.md" {
			t.Errorf("targetRel = %q, want %q", m.editing.targetRel, "api-notes.md")
		}
	})
}

func TestRenameAndDeleteFlows(t *testing.T) {
	t.Parallel()

	t.Run("rename validates then renames", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		unitDir := t.TempDir()
		memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")

		m, _ = step(m, views.RenameRequestMsg{Memory: memory})
		if m.flowModal == nil {
			t.Fatal("r request did not open the rename modal")
		}
		if got := m.flowModal.input.Value(); got != "note.md" {
			t.Fatalf("rename input prefill = %q, want the current name %q", got, "note.md")
		}

		// A typed keystroke must reach the input (the modal owns the
		// keyboard); "note.mdx" then exercises the extension guard.
		m, _ = step(m, key("x"))
		if got := m.flowModal.input.Value(); got != "note.mdx" {
			t.Fatalf("typed rune did not reach the rename input; value = %q", got)
		}
		m, _ = step(m, key("enter"))
		if got := plain(m.toastLine()); !strings.Contains(got, "extension must not change") {
			t.Errorf("toast = %q, want the extension guard's refusal", got)
		}
		if m.flowModal == nil {
			t.Fatal("modal closed on a refused rename; it must stay open for a correction")
		}

		modal := *m.flowModal
		modal.input.SetValue("renamed.md")
		m.flowModal = &modal
		m, _ = step(m, key("enter"))

		if m.flowModal != nil {
			t.Error("modal still open after a successful rename")
		}
		if _, err := os.Stat(filepath.Join(unitDir, "renamed.md")); err != nil {
			t.Errorf("renamed file missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(unitDir, "note.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("old name still present after rename: %v", err)
		}
		if got := plain(m.toastLine()); got != "renamed to renamed.md" {
			t.Errorf("toast = %q, want %q", got, "renamed to renamed.md")
		}
		if m.pendingCapture != nil {
			t.Error("rename set a pendingCapture; the brief scopes capture confirmation to edit/new/delete")
		}
	})

	t.Run("delete requires y and n or esc abort", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		unitDir := t.TempDir()
		memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")
		targetPath := filepath.Join(unitDir, "note.md")

		for _, abort := range []string{"n", "esc"} {
			m, _ = step(m, views.DeleteRequestMsg{Memory: memory})
			if m.flowModal == nil {
				t.Fatalf("d request did not open the confirm before %q", abort)
			}
			if got := plain(m.footer()); !strings.Contains(got, "delete note.md? it stays recoverable from history (y/N)") {
				t.Fatalf("confirm footer = %q, want the exact question naming the file", got)
			}
			m, _ = step(m, key(abort))
			if m.flowModal != nil {
				t.Fatalf("%q did not abort the delete confirm", abort)
			}
			if _, err := os.Stat(targetPath); err != nil {
				t.Fatalf("file gone after %q abort: %v", abort, err)
			}
		}

		m, _ = step(m, views.DeleteRequestMsg{Memory: memory})
		m, _ = step(m, key("y"))
		if _, err := os.Stat(targetPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("file still present after y confirm: %v", err)
		}
		if m.pendingCapture == nil || m.pendingCapture.folder != "acme" {
			t.Errorf("pendingCapture = %+v, want folder acme", m.pendingCapture)
		}
		if got := plain(m.toastLine()); got != "deleted note.md" {
			t.Errorf("toast = %q, want %q", got, "deleted note.md")
		}
	})
}

func TestPendingCaptureToasts(t *testing.T) {
	t.Parallel()
	captureAt := flowT0.Add(10 * time.Second)
	tests := []struct {
		name        string
		folder      string // pending folder; "" means the default "acme"
		tickTo      time.Time
		lastSync    *api.SyncSummary
		wantToast   string
		wantCleared bool
	}{
		{
			name:        "pushed",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 acme 2026-07-13T12:00:10Z"}, Pushed: true},
			wantToast:   "✓ captured — pushed",
			wantCleared: true,
		},
		{
			name:        "push queued",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 acme 2026-07-13T12:00:10Z"}, PushQueued: true},
			wantToast:   "✓ captured — push queued",
			wantCleared: true,
		},
		{
			name:        "cycle error",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Error: "push: remote hung up"},
			wantToast:   "capture failed: push: remote hung up",
			wantCleared: true,
		},
		{
			name:        "90s expiry",
			tickTo:      flowT0.Add(91 * time.Second),
			lastSync:    nil,
			wantToast:   "capture not yet confirmed — daemon may be quiesced or offline (see Activity)",
			wantCleared: true,
		},
		{
			name:        "stale cycle from before the mutation is ignored",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: flowT0.Add(-time.Second), Commits: []string{"memory: host1 acme 2026-07-13T11:00:00Z"}, Pushed: true},
			wantToast:   "",
			wantCleared: false,
		},
		{
			// Pushed is set so the subject's folder match is the ONLY thing
			// keeping the sibling "acme-web" capture from confirming "acme" —
			// without it, the push-state gate would mask a broken folder
			// match. The same rationale holds for every ignored row below.
			name:        "another folder's capture is ignored",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 acme-web 2026-07-13T12:00:10Z"}, Pushed: true},
			wantToast:   "",
			wantCleared: false,
		},
		{
			name:        "sibling folder in the suffix direction is ignored",
			folder:      "web",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 acme-web 2026-07-13T12:00:10Z"}, Pushed: true},
			wantToast:   "",
			wantCleared: false,
		},
		{
			// The pending folder's name equals the HOST field, which appears
			// space-delimited in every subject from that host — only matching
			// the folder by its field POSITION keeps it from false-confirming.
			name:        "folder named like the host is not confirmed by the host field",
			folder:      "host1",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 acme 2026-07-13T12:00:10Z"}, Pushed: true},
			wantToast:   "",
			wantCleared: false,
		},
		{
			// The engine's meta convention reserves the folder-field value
			// "manifest" (`memory: <host> manifest <stamp>`), so manifest
			// bookkeeping must never confirm a folder capture — even for a
			// folder literally named manifest, which degrades to the honest
			// deadline toast instead of being confirmed by every meta commit.
			name:        "manifest bookkeeping never confirms a folder capture",
			folder:      "manifest",
			tickTo:      flowT0.Add(12 * time.Second),
			lastSync:    &api.SyncSummary{At: captureAt, Commits: []string{"memory: host1 manifest 2026-07-13T12:00:10Z"}, Pushed: true},
			wantToast:   "",
			wantCleared: false,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newFlowModel(t, terminalEditorSettings())
			pendingFolder := testCase.folder
			if pendingFolder == "" {
				pendingFolder = "acme"
			}
			m.pendingCapture = &pendingCapture{folder: pendingFolder, since: flowT0}

			m, _ = step(m, tickMsg(testCase.tickTo))
			m, _ = step(m, statusMsg{resp: api.StatusResponse{State: "ready", LastSync: testCase.lastSync}})

			if got := plain(m.toastLine()); got != testCase.wantToast {
				t.Errorf("toast = %q, want %q", got, testCase.wantToast)
			}
			if cleared := m.pendingCapture == nil; cleared != testCase.wantCleared {
				t.Errorf("pendingCapture cleared = %v, want %v", cleared, testCase.wantCleared)
			}
		})
	}
}

// TestInTerminalFalseKeepsUIAlive pins the launch decision at its seam
// (launchEditorCmd) behaviorally, without a running program:
//
//   - InTerminal=false must produce the plain goroutine Cmd — running it
//     runs the editor directly (the marker file appears) and yields
//     editorFinishedMsg, exactly what keeps the TUI live.
//   - InTerminal=true must produce tea.ExecProcess's Cmd — running it yields
//     the runtime's own exec-request message (compared by type against a
//     reference tea.ExecProcess Cmd), and the editor has NOT run: only the
//     program loop, which suspends the terminal first, ever runs it.
func TestInTerminalFalseKeepsUIAlive(t *testing.T) {
	t.Parallel()
	scriptDir := t.TempDir()
	fakeEditor := func(t *testing.T, markerName string) (string, string) {
		t.Helper()
		markerPath := filepath.Join(scriptDir, markerName)
		scriptPath := filepath.Join(scriptDir, markerName+".sh")
		script := "#!/bin/sh\ntouch " + markerPath + "\n"
		if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return scriptPath, markerPath
	}

	t.Run("InTerminal false runs the editor from a live-TUI goroutine Cmd", func(t *testing.T) {
		t.Parallel()
		scriptPath, markerPath := fakeEditor(t, "gui-ran")
		cmd := launchEditorCmd(editorx.Editor{Argv: []string{scriptPath}, InTerminal: false}, filepath.Join(scriptDir, "scratch.md"))

		msg := cmd()

		finished, ok := msg.(editorFinishedMsg)
		if !ok {
			t.Fatalf("Cmd produced %T, want editorFinishedMsg (the live-TUI goroutine shape)", msg)
		}
		if finished.err != nil {
			t.Errorf("editor run failed: %v", finished.err)
		}
		if _, err := os.Stat(markerPath); err != nil {
			t.Errorf("the goroutine Cmd did not actually run the editor: %v", err)
		}
	})

	t.Run("InTerminal true defers to tea.ExecProcess", func(t *testing.T) {
		t.Parallel()
		scriptPath, markerPath := fakeEditor(t, "terminal-ran")
		cmd := launchEditorCmd(editorx.Editor{Argv: []string{scriptPath}, InTerminal: true}, filepath.Join(scriptDir, "scratch2.md"))

		msg := cmd()

		if _, ok := msg.(editorFinishedMsg); ok {
			t.Fatal("Cmd produced editorFinishedMsg directly; want tea.ExecProcess's exec request (the editor must only run under the suspended terminal)")
		}
		reference := tea.ExecProcess(exec.Command("true"), nil)()
		if got, want := reflect.TypeOf(msg), reflect.TypeOf(reference); got != want {
			t.Errorf("Cmd message type = %v, want tea.ExecProcess's %v", got, want)
		}
		if _, err := os.Stat(markerPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ExecProcess path ran the editor outside the program loop: %v", err)
		}
	})
}

func TestSecondEditRefusedWhileActive(t *testing.T) {
	t.Parallel()
	m, cacheRoot := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	first := writeFlowMemory(t, unitDir, "first.md", "# first\n")
	second := writeFlowMemory(t, unitDir, "second.md", "# second\n")

	m, _ = step(m, views.EditRequestMsg{Memory: first})
	if m.editing == nil {
		t.Fatal("setup: no session after the first request")
	}
	firstScratch := m.editing.scratchPath

	m, cmd := step(m, views.EditRequestMsg{Memory: second})

	if got := plain(m.toastLine()); !strings.Contains(got, "editor already open") {
		t.Errorf("toast = %q, want the second-edit refusal", got)
	}
	if cmd != nil {
		t.Errorf("second request produced a Cmd (%#v); want none", cmd())
	}
	if m.editing == nil || m.editing.scratchPath != firstScratch {
		t.Errorf("second request disturbed the active session: %+v", m.editing)
	}
	if got := cacheRootEntries(t, cacheRoot); got != 1 {
		t.Errorf("cache root has %d entries, want exactly the first session's 1", got)
	}

	// The new/rename/delete flows are refused by the same session gate: no
	// modal may open over an active handoff.
	m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: unitDir}}, Provider: "claude"})
	if m.flowModal != nil {
		t.Error("n opened a modal while an edit session is active")
	}
}

// TestFlowRequestsRefusedWhileModalOpen pins the one-flow invariant against
// message interleaving: bubbletea gives no ordering guarantee between a
// keystroke and an earlier Cmd's message, so a second mutation request can
// arrive AFTER an earlier request's modal opened (fast typing, key repeat,
// paste). Every such request must be refused outright — a session starting
// under an open modal would later be clobbered by the modal's own submit
// (losing its cleanup and mis-adjudicating its editor's exit), and a queued
// request must never silently replace an open delete confirm.
func TestFlowRequestsRefusedWhileModalOpen(t *testing.T) {
	t.Parallel()
	const wantRefusal = "a prompt is already open — finish or esc it first"

	t.Run("edit request under the open name modal is refused", func(t *testing.T) {
		t.Parallel()
		m, cacheRoot := newFlowModel(t, terminalEditorSettings())
		unitDir := t.TempDir()
		memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")
		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: unitDir}}, Provider: "claude"})
		if m.flowModal == nil || m.flowModal.kind != flowModalNewName {
			t.Fatal("setup: n request did not open the name modal")
		}

		m, cmd := step(m, views.EditRequestMsg{Memory: memory})

		if got := plain(m.toastLine()); got != wantRefusal {
			t.Errorf("toast = %q, want exactly %q", got, wantRefusal)
		}
		if m.editing != nil {
			t.Error("a session started under the open modal")
		}
		if cmd != nil {
			t.Errorf("refused request produced a Cmd (%#v); want none", cmd())
		}
		if m.flowModal == nil || m.flowModal.kind != flowModalNewName {
			t.Error("the refused request disturbed the open modal")
		}
		if got := cacheRootEntries(t, cacheRoot); got != 0 {
			t.Errorf("cache root has %d entries, want 0 (nothing may stage under an open modal)", got)
		}
	})

	t.Run("queued request never replaces the open delete confirm", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		unitDir := t.TempDir()
		memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")
		m, _ = step(m, views.DeleteRequestMsg{Memory: memory})
		if m.flowModal == nil || m.flowModal.kind != flowModalDeleteConfirm {
			t.Fatal("setup: d request did not open the confirm")
		}

		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: unitDir}}, Provider: "claude"})

		if got := plain(m.toastLine()); got != wantRefusal {
			t.Errorf("toast = %q, want exactly %q", got, wantRefusal)
		}
		if m.flowModal == nil || m.flowModal.kind != flowModalDeleteConfirm {
			t.Fatal("the open delete confirm was replaced by the queued request")
		}
		if got := m.flowModal.memory.RelPath; got != "note.md" {
			t.Errorf("delete confirm target = %q, want the original %q", got, "note.md")
		}
	})

	t.Run("name submit refuses when a session is already active", func(t *testing.T) {
		t.Parallel()
		// The request-time conjunct above makes this state unreachable
		// through the message flow, so the pin drives the last line of
		// defense directly — the session guard where sessions are created —
		// by planting an active session under the open modal.
		m, cacheRoot := newFlowModel(t, terminalEditorSettings())
		claudeUnit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}
		m, _ = step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})
		if m.flowModal == nil {
			t.Fatal("setup: n request did not open the name modal")
		}
		for _, r := range "notes" {
			m, _ = step(m, key(string(r)))
		}
		planted := &editSession{scratchPath: "/sentinel/first.md", startedAt: flowT0}
		m.editing = planted

		m, cmd := step(m, key("enter"))

		if got := plain(m.toastLine()); !strings.Contains(got, "editor already open") {
			t.Errorf("toast = %q, want the active-session refusal", got)
		}
		if m.editing != planted || m.editing.scratchPath != "/sentinel/first.md" {
			t.Errorf("submit clobbered the active session: %+v", m.editing)
		}
		if cmd != nil {
			t.Errorf("refused submit produced a Cmd (%#v); want none", cmd())
		}
		if got := cacheRootEntries(t, cacheRoot); got != 0 {
			t.Errorf("cache root has %d entries, want 0 (the refused submit must not stage)", got)
		}
		if _, err := os.Lstat(filepath.Join(claudeUnit.LocalDir, "notes.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused submit created a file: %v", err)
		}
	})
}

// assertFlowRequestRefusedUnderChrome runs every flow-request kind against a
// model whose keyboard is already owned by an open chrome surface (openChrome
// installs it) and asserts each request is refused with wantToast and leaks no
// flow state: no modal, no session, no staged scratch, no Cmd, and the chrome
// left undisturbed (chromeStillOpen). The same no-ordering-guarantee race the
// modal pin exercises applies here — a request queued by a screen key can land
// after the chrome opened — and handleKey checks both chrome surfaces before
// the flow modal, so any flow started here would open a modal (or launch an
// editor) beneath a surface that owns the keyboard, starving it invisibly.
func assertFlowRequestRefusedUnderChrome(t *testing.T, openChrome func(*testing.T, Model) Model, wantToast string, chromeStillOpen func(Model) bool) {
	t.Helper()
	kinds := []struct {
		name    string
		request func(memoryfs.Memory, api.UnitInfo) tea.Msg
	}{
		{name: "edit", request: func(mem memoryfs.Memory, _ api.UnitInfo) tea.Msg { return views.EditRequestMsg{Memory: mem} }},
		{name: "new", request: func(_ memoryfs.Memory, unit api.UnitInfo) tea.Msg {
			return views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{unit}, Provider: "claude"}
		}},
		{name: "rename", request: func(mem memoryfs.Memory, _ api.UnitInfo) tea.Msg { return views.RenameRequestMsg{Memory: mem} }},
		{name: "delete", request: func(mem memoryfs.Memory, _ api.UnitInfo) tea.Msg { return views.DeleteRequestMsg{Memory: mem} }},
	}
	for _, testCase := range kinds {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			m, cacheRoot := newFlowModel(t, terminalEditorSettings())
			unitDir := t.TempDir()
			memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")
			unit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: unitDir}

			m = openChrome(t, m)

			m, cmd := step(m, testCase.request(memory, unit))

			if got := plain(m.toastLine()); got != wantToast {
				t.Errorf("toast = %q, want exactly %q", got, wantToast)
			}
			if m.flowModal != nil {
				t.Error("a flow modal opened while chrome owns the keyboard")
			}
			if m.editing != nil {
				t.Error("an edit session started while chrome owns the keyboard")
			}
			if !chromeStillOpen(m) {
				t.Error("the refused request disturbed the open chrome; it must stay open")
			}
			if cmd != nil {
				t.Errorf("refused request produced a Cmd (%#v); want none", cmd())
			}
			if got := cacheRootEntries(t, cacheRoot); got != 0 {
				t.Errorf("cache root has %d entries, want 0 (nothing may stage under open chrome)", got)
			}
		})
	}
}

// TestFlowRequestRefusedWhileSearchOverlayOpen pins the search-overlay half of
// the chrome gate: a mutation request that lands after `/` opened the global
// overlay must be refused for every request kind, with the overlay left open —
// never a flow modal or editor launched beneath it.
func TestFlowRequestRefusedWhileSearchOverlayOpen(t *testing.T) {
	t.Parallel()
	assertFlowRequestRefusedUnderChrome(
		t,
		func(t *testing.T, m Model) Model {
			m, _ = step(m, key("/"))
			if m.searchOverlay == nil {
				t.Fatal("setup: / did not open the search overlay")
			}
			return m
		},
		"search is open — esc it first",
		func(m Model) bool { return m.searchOverlay != nil },
	)
}

// TestFlowRequestRefusedWhilePaletteOpen is the palette twin: a request landing
// after ctrl+k opened the command palette must be refused for every kind, with
// the palette left open.
func TestFlowRequestRefusedWhilePaletteOpen(t *testing.T) {
	t.Parallel()
	assertFlowRequestRefusedUnderChrome(
		t,
		func(t *testing.T, m Model) Model {
			m, _ = step(m, key("ctrl+k"))
			if !m.paletteOpen {
				t.Fatal("setup: ctrl+k did not open the palette")
			}
			return m
		},
		"the palette is open — esc it first",
		func(m Model) bool { return m.paletteOpen },
	)
}

// TestNewRefusedWithoutEditorAtRequest pins n's no-editor refusal at REQUEST
// time, exactly like e's: the exact ErrNoEditor wording lands immediately
// and no name modal ever opens — deferring the refusal to submit would
// collect a name only to discard it.
func TestNewRefusedWithoutEditorAtRequest(t *testing.T) {
	t.Parallel()
	m, cacheRoot := newFlowModel(t, config.Settings{})
	claudeUnit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: t.TempDir()}

	m, cmd := step(m, views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"})

	const want = "no editor configured — set $EDITOR or editor.command in config"
	if got := plain(m.toastLine()); got != want {
		t.Errorf("toast = %q, want exactly %q", got, want)
	}
	if m.flowModal != nil {
		t.Error("the name modal opened despite no editor resolving")
	}
	if m.editing != nil {
		t.Error("a session started despite no editor resolving")
	}
	if cmd != nil {
		t.Errorf("request produced a Cmd (%#v); want none", cmd())
	}
	if got := cacheRootEntries(t, cacheRoot); got != 0 {
		t.Errorf("cache root has %d entries, want 0", got)
	}
}

// TestEditorFailureCleansUpAndToasts pins the editor-error finish path: the
// scratch is removed (brief: "editor error → toast + cleanup"), nothing
// lands, and no capture is awaited.
func TestEditorFailureCleansUpAndToasts(t *testing.T) {
	t.Parallel()
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# original\n")

	m, _ = step(m, views.EditRequestMsg{Memory: memory})
	if m.editing == nil {
		t.Fatal("setup: no session after the request")
	}
	scratchDir := filepath.Dir(m.editing.scratchPath)
	if err := os.WriteFile(m.editing.scratchPath, []byte("# edited but the editor crashed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ = step(m, editorFinishedMsg{err: errors.New("exit status 1")})

	if got := plain(m.toastLine()); !strings.Contains(got, "editor failed") {
		t.Errorf("toast = %q, want the editor failure surfaced", got)
	}
	content, err := os.ReadFile(filepath.Join(unitDir, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# original\n" {
		t.Errorf("target content = %q, want untouched original after an editor failure", content)
	}
	if _, err := os.Stat(scratchDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("scratch dir still present: %v", err)
	}
	if m.pendingCapture != nil {
		t.Error("an editor failure set a pendingCapture")
	}
}

// TestScratchMissingKeepsTargetAndPreservesDir pins the Changed-error finish
// path: a scratch file that vanished (a hostile or misbehaving editor) never
// lands anything and never reads as "unchanged" — and the scratch dir is
// deliberately kept, since deleting it could destroy whatever remains of the
// user's work.
func TestScratchMissingKeepsTargetAndPreservesDir(t *testing.T) {
	t.Parallel()
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# original\n")

	m, _ = step(m, views.EditRequestMsg{Memory: memory})
	if m.editing == nil {
		t.Fatal("setup: no session after the request")
	}
	scratchDir := filepath.Dir(m.editing.scratchPath)
	if err := os.Remove(m.editing.scratchPath); err != nil {
		t.Fatal(err)
	}

	m, _ = step(m, editorFinishedMsg{})

	if got := plain(m.toastLine()); !strings.Contains(got, "edit not landed") {
		t.Errorf("toast = %q, want the not-landed notice", got)
	}
	content, err := os.ReadFile(filepath.Join(unitDir, "note.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# original\n" {
		t.Errorf("target content = %q, want untouched original", content)
	}
	if _, err := os.Stat(scratchDir); err != nil {
		t.Errorf("scratch dir removed on the failure path; it must be preserved: %v", err)
	}
}

// TestWriteFailureKeepsScratch pins the land-failure path: when the atomic
// write cannot complete, the scratch — now the only copy of the user's edit —
// is preserved and the toast names where it is.
func TestWriteFailureKeepsScratch(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a read-only dir does not refuse writes")
	}
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# original\n")

	m, _ = step(m, views.EditRequestMsg{Memory: memory})
	if m.editing == nil {
		t.Fatal("setup: no session after the request")
	}
	scratchPath := m.editing.scratchPath
	if err := os.WriteFile(scratchPath, []byte("# edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unitDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unitDir, 0o755) })

	m, _ = step(m, editorFinishedMsg{})

	if got := plain(m.toastLine()); !strings.Contains(got, "save failed") || !strings.Contains(got, scratchPath) {
		t.Errorf("toast = %q, want a save failure naming the preserved scratch path", got)
	}
	edited, err := os.ReadFile(scratchPath)
	if err != nil {
		t.Fatalf("scratch removed on a failed save — the user's edit is gone: %v", err)
	}
	if string(edited) != "# edited\n" {
		t.Errorf("scratch content = %q, want the preserved edit", edited)
	}
}

// TestQuiescedFlowRefusals pins spec §15's quiesce posture at both surfaces:
// a flow request arriving while quiesced is refused with the existing
// refusal wording, and a browser-scoped mutation key pressed on a BARE tab
// (empty stack) stays a silent dead key — quiesceGate must not toast about a
// key that was never going to do anything.
func TestQuiescedFlowRefusals(t *testing.T) {
	t.Parallel()
	quiescedStatus := func(m Model) Model {
		until := m.now.Add(time.Minute)
		m.status = api.StatusResponse{State: "ready", QuiescedUntil: &until}
		return m
	}

	t.Run("flow request refused while quiesced", func(t *testing.T) {
		t.Parallel()
		m, cacheRoot := newFlowModel(t, terminalEditorSettings())
		m = quiescedStatus(m)
		memory := writeFlowMemory(t, t.TempDir(), "note.md", "# note\n")

		m, _ = step(m, views.EditRequestMsg{Memory: memory})

		if got := plain(m.toastLine()); !strings.Contains(got, "daemon quiesced until") {
			t.Errorf("toast = %q, want the existing quiesce refusal wording", got)
		}
		if m.editing != nil {
			t.Error("a session started while quiesced")
		}
		if got := cacheRootEntries(t, cacheRoot); got != 0 {
			t.Errorf("cache root has %d entries, want 0", got)
		}
	})

	t.Run("browser mutation key on a bare tab stays silent", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		m = quiescedStatus(m)
		m.active = tabConflicts

		m, _ = step(m, key("e"))

		if got := plain(m.toastLine()); got != "" {
			t.Errorf("toast = %q, want none: e on a bare tab is a dead key, not a refusable mutation", got)
		}
	})
}

// TestStackFooterShowsFlowRowsVisiblyDisabled pins the availability gate's
// rendering contract: a gated row stays IN the footer, struck through
// (SGR 9), rather than vanishing — the user must see that e exists and learn
// why it is dead, crush-style.
func TestStackFooterShowsFlowRowsVisiblyDisabled(t *testing.T) {
	t.Parallel()
	enrolledUnits := []api.UnitInfo{{Provider: "claude", Folder: "acme", LocalDir: "/enrolled/claude"}}
	pushBrowser := func(t *testing.T, m Model, class provider.Class, units []api.UnitInfo) Model {
		t.Helper()
		memory := writeFlowMemory(t, t.TempDir(), "note.md", "# note\n")
		memory.Class = class
		browser := views.NewBrowser(views.BrowserDeps{
			Folder:   "acme",
			Units:    units,
			Now:      m.now,
			ReadBody: func(memoryfs.Memory) (string, error) { return "# note\n", nil },
			List:     func() ([]memoryfs.Memory, error) { return []memoryfs.Memory{memory}, nil },
		})
		m, _ = step(m, views.PushScreenMsg{Screen: browser})
		return m
	}

	t.Run("no editor: edit and new visible but struck", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, config.Settings{}) // no editor resolves
		m = pushBrowser(t, m, provider.ClassFact, enrolledUnits)

		footer := m.footer()
		if got := plain(footer); !strings.Contains(got, "e edit") || !strings.Contains(got, "n new") {
			t.Fatalf("footer %q hides the gated rows; they must stay visible", got)
		}
		if !strikethroughPattern.MatchString(footer) {
			t.Errorf("no SGR strikethrough in the footer; a gated row must be VISIBLY disabled:\n%q", footer)
		}
	})

	t.Run("editor resolves on a fact row: nothing struck", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		m = pushBrowser(t, m, provider.ClassFact, enrolledUnits)

		footer := m.footer()
		if got := plain(footer); !strings.Contains(got, "e edit") || !strings.Contains(got, "r rename") || !strings.Contains(got, "d delete") {
			t.Fatalf("footer %q missing available flow rows", got)
		}
		if strikethroughPattern.MatchString(footer) {
			t.Errorf("footer struck a row while every flow gate passes:\n%q", footer)
		}
	})

	t.Run("derived-class selection: rename and delete struck", func(t *testing.T) {
		t.Parallel()
		m, _ := newFlowModel(t, terminalEditorSettings())
		m = pushBrowser(t, m, provider.ClassDerivedIndex, enrolledUnits)

		footer := m.footer()
		if got := plain(footer); !strings.Contains(got, "r rename") {
			t.Fatalf("footer %q hides the class-gated rows; they must stay visible", got)
		}
		if !strikethroughPattern.MatchString(footer) {
			t.Errorf("no SGR strikethrough with a derived-class selection:\n%q", footer)
		}
	})

	t.Run("no enrolled units: new struck even with an editor", func(t *testing.T) {
		t.Parallel()
		// Editor resolves and a fact row is selected, so e/r/d all pass
		// their gates (the "nothing struck" subtest above proves that
		// combination renders zero strikethrough) — any strike here is
		// therefore attributable to n's units conjunct alone: a folder with
		// no enrolled units has nowhere to put a new memory.
		m, _ := newFlowModel(t, terminalEditorSettings())
		m = pushBrowser(t, m, provider.ClassFact, nil)

		footer := m.footer()
		if got := plain(footer); !strings.Contains(got, "n new") {
			t.Fatalf("footer %q hides the units-gated row; it must stay visible", got)
		}
		if !strikethroughPattern.MatchString(footer) {
			t.Errorf("no SGR strikethrough with zero enrolled units:\n%q", footer)
		}
	})
}

// TestFlowModalFooterStaysOneLine pins the height contract: every flow modal
// renders as the single footer line the root's chrome budget already
// reserves — never a second line that would push the pushed screen's body
// out of its height budget.
func TestFlowModalFooterStaysOneLine(t *testing.T) {
	t.Parallel()
	m, _ := newFlowModel(t, terminalEditorSettings())
	unitDir := t.TempDir()
	memory := writeFlowMemory(t, unitDir, "note.md", "# note\n")
	claudeUnit := api.UnitInfo{Provider: "claude", Folder: "acme", LocalDir: unitDir}

	requests := []struct {
		name    string
		request tea.Msg
	}{
		{name: "new name input", request: views.NewRequestMsg{Folder: "acme", Units: []api.UnitInfo{claudeUnit}, Provider: "claude"}},
		{name: "rename input", request: views.RenameRequestMsg{Memory: memory}},
		{name: "delete confirm", request: views.DeleteRequestMsg{Memory: memory}},
	}
	for _, testCase := range requests {
		t.Run(testCase.name, func(t *testing.T) {
			next, _ := step(m, testCase.request)
			if next.flowModal == nil {
				t.Fatal("request did not open its modal")
			}
			footer := next.footer()
			if lineBreaks := strings.Count(footer, "\n"); lineBreaks != 0 {
				t.Errorf("modal footer spans %d lines, want exactly 1 (the reserved footer slot):\n%q", lineBreaks+1, footer)
			}
			next, _ = step(next, key("esc"))
			if next.flowModal != nil {
				t.Error("esc did not close the modal")
			}
		})
	}
}
