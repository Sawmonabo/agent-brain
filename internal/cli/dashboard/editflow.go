package dashboard

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/editorx"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/views"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
	"github.com/Sawmonabo/agent-brain/internal/repo"
)

// This file is the root's half of spec §5 — the $EDITOR handoff and its
// sibling mutations (new/rename/delete). The views only EMIT request
// messages (screen.go); everything with a gate, a modal, or a side effect
// lives here, because the flow needs what only the root holds: the loaded
// editor settings, the scratch cache root, tea.ExecProcess, and the
// toast/footer chrome.
//
// I/O discipline: staging a scratch, reading the original, byte-comparing,
// and the single atomic land are all LOCAL filesystem work, run inline in
// Update under the same documented exception the Screen contract already
// grants Browser.refresh (screen.go) and the root's own NewBrowser
// construction on OpenFolderMsg — local file reads/writes, never a daemon
// round trip, and running them synchronously makes request→session one
// atomic Update step (no interleaving window where a second request could
// sneak past the one-session gate). The editor itself — the long-lived,
// blocking part — always runs through a returned Cmd.

// pendingCaptureDeadline bounds how long a landed mutation waits for the
// daemon's capture confirmation before the hub stops promising one (spec
// §5 step 5). 90s comfortably covers the watch debounce plus a slow cycle;
// past it the honest report is "not yet confirmed", not silence.
const pendingCaptureDeadline = 90 * time.Second

// editKind names what an editSession is editing.
type editKind int

const (
	// editExisting is spec §5's e: a live provider file copied to scratch.
	editExisting editKind = iota
	// editNew is spec §5's n: a provider skeleton staged for a first save.
	editNew
	// editRestore is reserved for Task 14's history restore, which re-runs
	// this exact session machinery over a historical blob.
	editRestore
)

// editSession is one in-flight handoff. The root holds at most one — a
// second request while one is active is refused with a toast, because
// tea.ExecProcess owns the terminal for an in-terminal editor and a GUI
// editor still owns the one scratch→land pipeline; concurrency is
// meaningless either way.
type editSession struct {
	kind editKind
	// memory is the session's source memory — zero-value for editNew until
	// landed (there is no Memory yet; folder/provider below carry the
	// identity the finish paths need).
	memory memoryfs.Memory
	// folder and provider identify the landing unit for the finish paths:
	// pendingCapture watches folder's capture commits, and the claude
	// new-memory land toast keys off provider. For editExisting both mirror
	// the memory's own fields; for editNew they come from the chosen unit.
	folder   string
	provider string

	targetDir   string // unit dir the edit lands in
	targetRel   string // path under targetDir to land
	original    []byte // staged bytes (the skeleton, for editNew)
	scratchPath string
	cleanup     func()
	startedAt   time.Time
}

// editorFinishedMsg reports the editor process's exit — the ExecProcess
// callback's message for an in-terminal editor, the goroutine Cmd's result
// for a GUI one.
type editorFinishedMsg struct{ err error }

// pendingCapture is one landed mutation awaiting the daemon's capture
// confirmation (spec §5 step 5). One at a time; a newer mutation replaces
// it — the newest land is the one the user is watching for.
type pendingCapture struct {
	folder string
	since  time.Time
}

// flowModalKind names which single-line modal owns the keyboard.
type flowModalKind int

const (
	flowModalNewName flowModalKind = iota
	flowModalRenameName
	flowModalDeleteConfirm
)

// flowModal is the root's one-line modal state for the new/rename/delete
// flows, rendered in the footer slot (the quitPrompt precedent — zero extra
// height). Model has value semantics, so every mutation of an open modal
// goes copy-on-write through updateFlowModal: mutating through the shared
// pointer would silently rewrite the modal inside every retained pre-update
// Model copy (the withStack rule, applied to a pointer field).
type flowModal struct {
	kind   flowModalKind
	input  textinput.Model
	memory memoryfs.Memory // rename/delete target; zero for new
	unit   api.UnitInfo    // new: the unit that receives the file
}

// startEditFlow handles EditRequestMsg: gates, then stages and launches the
// handoff for an existing memory.
func (m Model) startEditFlow(memory memoryfs.Memory) (Model, tea.Cmd) {
	if m.refuseFlowStart() || m.refuseNonFact(memory) {
		return m, nil
	}
	original, err := memoryfs.ReadBody(memory)
	if err != nil {
		m.pushToast(err.Error())
		return m, nil
	}
	cmd := m.beginEdit(editSession{
		kind:      editExisting,
		memory:    memory,
		folder:    memory.Folder,
		provider:  memory.Provider,
		targetDir: memory.LocalDir,
		targetRel: memory.RelPath,
		original:  []byte(original),
	})
	return m, cmd
}

// startNewFlow handles NewRequestMsg: gates, picks the receiving unit, and
// opens the name-input modal. The skeleton is staged at submit time
// (submitFlowModal), once the name is known.
func (m Model) startNewFlow(request views.NewRequestMsg) (Model, tea.Cmd) {
	if m.refuseFlowStart() {
		return m, nil
	}
	// Refuse a missing editor at REQUEST time, exactly like e (whose
	// beginEdit resolves the editor as its first act): deferring to submit
	// would collect a name only to discard it with the refusal.
	if _, err := editorx.Resolve(m.settings.Editor, m.getenv); err != nil {
		m.pushToast(err.Error())
		return m, nil
	}
	unit, ok := pickUnit(request.Units, request.Provider)
	if !ok {
		m.pushToast("no enrolled unit in this folder to create a memory in")
		return m, nil
	}
	input := newFlowInput("")
	focusCmd := input.Focus()
	m.flowModal = &flowModal{kind: flowModalNewName, input: input, unit: unit}
	return m, focusCmd
}

// startRenameFlow handles RenameRequestMsg: gates, then opens the rename
// input prefilled with the current name.
func (m Model) startRenameFlow(memory memoryfs.Memory) (Model, tea.Cmd) {
	if m.refuseFlowStart() || m.refuseNonFact(memory) {
		return m, nil
	}
	input := newFlowInput(memory.RelPath)
	focusCmd := input.Focus()
	m.flowModal = &flowModal{kind: flowModalRenameName, input: input, memory: memory}
	return m, focusCmd
}

// startDeleteFlow handles DeleteRequestMsg: gates, then opens the default-No
// confirm naming the file.
func (m Model) startDeleteFlow(memory memoryfs.Memory) Model {
	if m.refuseFlowStart() || m.refuseNonFact(memory) {
		return m
	}
	m.flowModal = &flowModal{kind: flowModalDeleteConfirm, memory: memory}
	return m
}

// newFlowInput builds the single-line modal input: no "> " prompt glyph
// (the footer's own label already introduces it), prefilled with value and
// the cursor at its end for an edit-in-place rename.
func newFlowInput(value string) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.SetValue(value)
	input.CursorEnd()
	return input
}

// pickUnit chooses the unit a new memory lands in: the first whose provider
// matches the browser's placement hint, else the folder's first unit — a
// deterministic default for the common single-unit folder. Two units under
// the same provider (the codex memories+chronicle shape) resolve to the
// first, which enrollment orders as the primary memories dir.
func pickUnit(units []api.UnitInfo, providerHint string) (api.UnitInfo, bool) {
	if len(units) == 0 {
		return api.UnitInfo{}, false
	}
	for _, unit := range units {
		if unit.Provider == providerHint {
			return unit, true
		}
	}
	return units[0], true
}

// refuseFlowStart toasts and refuses when no flow may start right now:
// while a handoff is active (one session, ever), while a flow modal is
// open (bubbletea gives no ordering guarantee between a keystroke and an
// earlier Cmd's message, so a queued request CAN arrive after another
// request's modal opened — admitting it would fork the flow state the
// modal is about to act on, or silently replace an open delete confirm),
// while the search overlay or palette owns the screen (the same
// no-ordering-guarantee race: a request queued by a screen key can land
// after that chrome opened, and handleKey checks both chrome surfaces
// before the flow modal, so the flow it would start — a modal, or an
// ExecProcess editor launch — would sit underneath a surface that owns the
// keyboard and starve invisibly), or while the daemon is quiesced (spec
// §15's grey-out-with-refusal for mutating actions). Refusing starts under
// open chrome also makes chrome-over-modal unreachable in both directions:
// a flow modal can now open only while chrome is closed, and while one is
// open handleKey's modal priority consumes the very keys that would open
// chrome. It deliberately guards only flow STARTS — a landing edit is never
// blocked, because refusing a finish would discard content the user already
// wrote; a quiesce that begins mid-edit merely defers the capture, which the
// pendingCapture deadline toast names explicitly.
func (m *Model) refuseFlowStart() bool {
	if m.editing != nil {
		m.toastEditorBusy()
		return true
	}
	if m.flowModal != nil {
		m.pushToast("a prompt is already open — finish or esc it first")
		return true
	}
	if m.searchOverlay != nil {
		m.pushToast("search is open — esc it first")
		return true
	}
	if m.paletteOpen {
		m.pushToast("the palette is open — esc it first")
		return true
	}
	if m.quiesced() {
		m.toastQuiesceRefusal()
		return true
	}
	return false
}

// toastEditorBusy is the one active-session refusal wording, shared by
// refuseFlowStart (request gating) and beginEdit (the session guard), so
// the two paths can never drift.
func (m *Model) toastEditorBusy() {
	m.pushToast(fmt.Sprintf("editor already open (since %s) — finish that edit first", m.editing.startedAt.Format("15:04:05")))
}

// refuseNonFact toasts and refuses a mutation over anything but a fact-class
// memory — the ONE place the derived-class gate lives, shared by the e/r/d
// request handlers, so a MEMORY.md-style derived index (or a regenerated
// file) can never be hand-edited, renamed, or deleted from the hub: the
// provider rebuilds those from the fact files, so the fact files are where
// an edit belongs.
func (m *Model) refuseNonFact(memory memoryfs.Memory) bool {
	if memory.Class == provider.ClassFact {
		return false
	}
	m.pushToast("derived index — regenerated by the provider; edit the memory files instead")
	return true
}

// beginEdit resolves the editor, stages the scratch copy, records the
// session, and returns the launch Cmd — or toasts the refusal/failure and
// returns nil with no session recorded. ErrNoEditor's own message is
// exactly the spec's footer wording, so it is toasted verbatim.
func (m *Model) beginEdit(session editSession) tea.Cmd {
	if m.editing != nil {
		// The at-most-one-session invariant, enforced where sessions are
		// created. Request handlers refuse via refuseFlowStart well before
		// this line; the guard protects the callers that DON'T pass through
		// it — a modal submit whose session appeared after the modal opened,
		// and any future caller — from clobbering the live session (which
		// would leak its scratch and mis-adjudicate its editor's exit
		// against the wrong session).
		m.toastEditorBusy()
		return nil
	}
	editor, err := editorx.Resolve(m.settings.Editor, m.getenv)
	if err != nil {
		m.pushToast(err.Error())
		return nil
	}
	scratchDir, cleanup, err := editorx.NewScratchDir(m.cacheRoot)
	if err != nil {
		m.pushToast(err.Error())
		return nil
	}
	scratchPath, err := editorx.Stage(scratchDir, path.Base(session.targetRel), session.original)
	if err != nil {
		cleanup()
		m.pushToast(err.Error())
		return nil
	}
	session.scratchPath = scratchPath
	session.cleanup = cleanup
	session.startedAt = m.now
	m.editing = &session
	return launchEditorCmd(editor, scratchPath)
}

// launchEditorCmd builds the Cmd that runs the resolved editor over the
// staged scratch — the launch decision, in one inspectable seam.
//
// InTerminal editors go through tea.ExecProcess, which suspends the TUI
// around the child. No guard frame or repaint workaround is needed: in the
// pinned bubbletea v2.0.8, Program.exec releases the terminal before the
// child runs — releaseTerminal stops the renderer, whose close exits the
// alternate screen (cursed_renderer.go close → enableAltScreen(false)) —
// and RestoreTerminal afterwards restarts the renderer with starting=true,
// so the next flush bypasses its unchanged-view short-circuit while
// enterAltScreen erases the screen for a full repaint (exec.go
// Program.exec; tea.go releaseTerminal/RestoreTerminal; cursed_renderer.go
// start/flush/enterAltScreen — verified against the module source). The hub
// always runs the altscreen (dashboard.go View sets view.AltScreen = true),
// so resuming lands back on a fully repainted alternate screen.
//
// InTerminal=false editors (GUI, configured with their wait flag — ADR 20
// D2) run from a plain Cmd goroutine instead: the TUI keeps the terminal
// and stays live, and the editor's exit arrives as an ordinary message.
func launchEditorCmd(editor editorx.Editor, scratchPath string) tea.Cmd {
	command := editorx.Command(editor, scratchPath)
	if editor.InTerminal {
		return tea.ExecProcess(command, func(err error) tea.Msg { return editorFinishedMsg{err: err} })
	}
	return func() tea.Msg { return editorFinishedMsg{err: command.Run()} }
}

// finishEdit handles editorFinishedMsg — the scratch verdict and, when the
// bytes changed, the one atomic land (kubectl's rule: byte-equal means
// cancelled). Scratch cleanup is deliberately per-path:
//
//   - editor error, cancelled, landed → cleaned up (nothing left to lose);
//   - scratch unreadable, land failed → PRESERVED, with the toast naming
//     the path — on those paths the scratch is (whatever remains of) the
//     only copy of the user's work, and content is never discarded to tidy
//     a temp dir.
func (m Model) finishEdit(finished editorFinishedMsg) Model {
	session := m.editing
	if session == nil {
		return m // a stray finish with no session (defensive; nothing to do)
	}
	m.editing = nil
	if finished.err != nil {
		session.cleanup()
		m.pushToast("editor failed: " + finished.err.Error())
		return m
	}
	changed, edited, err := editorx.Changed(session.original, session.scratchPath)
	if err != nil {
		m.pushToast("edit not landed: " + err.Error() + " — scratch kept at " + session.scratchPath)
		return m
	}
	if !changed {
		session.cleanup()
		m.pushToast("edit cancelled, no changes made")
		return m
	}
	if err := memoryfs.WriteFileAtomic(session.targetDir, session.targetRel, edited); err != nil {
		m.pushToast("save failed: " + err.Error() + " — your edit is kept at " + session.scratchPath)
		return m
	}
	session.cleanup()
	m.pendingCapture = &pendingCapture{folder: session.folder, since: m.now}
	if session.kind == editNew && session.provider == "claude" {
		// Spec §5: a new claude memory is only discoverable once its
		// MEMORY.md index line exists, and that index is a derived file
		// this flow refuses to edit — so remind, don't rewrite.
		m.pushToast("saved — remember the MEMORY.md index line")
	} else {
		m.pushToast("saved")
	}
	return m
}

// updateFlowModal owns the keyboard while a flow modal is open: esc always
// aborts (consumed here — it must never also pop the screen under the
// modal), the delete confirm answers only to y/Y/n/N, and the name inputs
// take enter as submit with every other key forwarded to the text input.
func (m Model) updateFlowModal(msg tea.KeyPressMsg) (Model, tea.Cmd) {
	if keybinding.Matches(msg, views.DashboardKeys.Cancel) {
		m.flowModal = nil
		return m, nil
	}
	if m.flowModal.kind == flowModalDeleteConfirm {
		if keybinding.Matches(msg, views.DashboardKeys.ConfirmDecision) {
			switch msg.String() {
			case "y", "Y":
				return m.performDelete(), nil
			default:
				m.flowModal = nil
			}
		}
		return m, nil // default No: everything but y/Y/n/N/esc is inert
	}
	if keybinding.Matches(msg, views.DashboardKeys.Accept) {
		return m.submitFlowModal()
	}
	modal := *m.flowModal // copy-on-write: see flowModal's doc
	var cmd tea.Cmd
	modal.input, cmd = modal.input.Update(msg)
	m.flowModal = &modal
	return m, cmd
}

// performDelete is the delete confirm's y: remove the provider file (plain
// os.Remove inside memoryfs.Delete — deletion IS the mutation the watcher
// captures; recoverable via history restore, spec §6) and await the capture.
func (m Model) performDelete() Model {
	memory := m.flowModal.memory
	m.flowModal = nil
	if err := memoryfs.Delete(memory); err != nil {
		m.pushToast(err.Error())
		return m
	}
	m.pendingCapture = &pendingCapture{folder: memory.Folder, since: m.now}
	m.pushToast("deleted " + memory.RelPath)
	return m
}

// submitFlowModal is enter on a name-input modal. A refused name toasts the
// reason and keeps the modal open for a correction; only a valid submission
// (or esc) closes it.
func (m Model) submitFlowModal() (Model, tea.Cmd) {
	modal := m.flowModal
	switch modal.kind {
	case flowModalNewName:
		return m.submitNewName()
	case flowModalRenameName:
		return m.submitRename(), nil
	default:
		return m, nil // the delete confirm has no enter path (default No)
	}
}

// submitNewName validates the typed name, refuses an already-taken one, and
// hands the provider skeleton to the same beginEdit the e flow uses — with
// original = the skeleton bytes, so saving it untouched is a cancel and no
// file ever lands (the kubectl rule, applied to creation).
func (m Model) submitNewName() (Model, tea.Cmd) {
	modal := m.flowModal
	rel, displayName, err := validateNewMemoryName(modal.unit.Provider, modal.input.Value())
	if err != nil {
		m.pushToast(err.Error())
		return m, nil
	}
	targetPath := filepath.Join(modal.unit.LocalDir, filepath.FromSlash(rel))
	// Lstat, not Stat: a symlink squatting on the name still counts as
	// taken (memoryfs's symlink-averse posture). Check-then-stage has an
	// inherent race against a concurrent agent write, tolerated for the
	// same reason concurrent edits are: the land is one atomic replace and
	// capture history keeps every version (spec §6).
	if _, statErr := os.Lstat(targetPath); statErr == nil {
		m.pushToast(rel + " already exists — edit it instead")
		return m, nil
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		m.pushToast(statErr.Error())
		return m, nil
	}
	unit := modal.unit
	m.flowModal = nil
	cmd := m.beginEdit(editSession{
		kind:      editNew,
		folder:    unit.Folder,
		provider:  unit.Provider,
		targetDir: unit.LocalDir,
		targetRel: rel,
		original:  []byte(memoryfs.Skeleton(unit.Provider, displayName)),
	})
	return m, cmd
}

// submitRename validates and performs the rename. A value equal to the
// current name is a quiet cancel (nothing to do — the rename analog of the
// unchanged-edit rule); memoryfs.Rename's own guards (ValidateRelPath,
// extension pinning, no-clobber) are surfaced verbatim with the modal kept
// open, since each names exactly what to correct.
func (m Model) submitRename() Model {
	modal := m.flowModal
	newRel := strings.TrimSpace(modal.input.Value())
	if newRel == "" {
		m.pushToast("name must not be empty")
		return m
	}
	if newRel == modal.memory.RelPath {
		m.flowModal = nil
		return m
	}
	if err := memoryfs.Rename(modal.memory, newRel); err != nil {
		m.pushToast(err.Error())
		return m
	}
	m.flowModal = nil
	m.pushToast("renamed to " + newRel)
	return m
}

// validateNewMemoryName applies spec §5's n rules: non-empty, no "/" (a new
// memory is a file in the unit dir, never a nested path), .md forced for
// claude (append-if-missing, so "api-notes.md" is not doubled), and the
// shared repo.ValidateRelPath guard — the same one WriteFileAtomic enforces
// at land time — so a traversal-shaped name (".." and friends) is refused
// here with a message instead of failing later. displayName is the
// extension-less stem the skeleton templates interpolate, matching how
// memoryfs.List derives a display name from a filename.
func validateNewMemoryName(providerName, typed string) (rel, displayName string, err error) {
	name := strings.TrimSpace(typed)
	if name == "" {
		return "", "", errors.New("name must not be empty")
	}
	if strings.Contains(name, "/") {
		return "", "", errors.New("name must not contain /")
	}
	// Validate the TYPED name, before the .md forcing: appending a suffix
	// to ".." yields the technically-clean "...md", which would smuggle a
	// traversal-shaped input past a post-forcing check — while a name that
	// validates here stays valid after the append (one slash-free segment
	// gaining a suffix cannot become absolute, backslashed, or unclean).
	if err := repo.ValidateRelPath(name); err != nil {
		return "", "", err
	}
	if providerName == "claude" && !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	return name, strings.TrimSuffix(name, path.Ext(name)), nil
}

// flowModalFooterLine renders the open modal in the footer's single
// reserved line — the quitPrompt precedent, so a modal never costs the
// pushed screen a row of its height budget. The delete confirm's wording is
// spec §5's, naming the file and defaulting to No.
func (m Model) flowModalFooterLine() string {
	modal := m.flowModal
	switch modal.kind {
	case flowModalNewName:
		return m.styles.Warn.Render("new memory name: ") + modal.input.View() +
			m.styles.Dim.Render("  · enter create · esc cancel")
	case flowModalRenameName:
		return m.styles.Warn.Render("rename to: ") + modal.input.View() +
			m.styles.Dim.Render("  · enter rename · esc cancel")
	case flowModalDeleteConfirm:
		return m.styles.Warn.Render(fmt.Sprintf("delete %s? it stays recoverable from history (y/N)", modal.memory.RelPath))
	default:
		return ""
	}
}

// checkPendingCapture runs on every statusMsg: it resolves the awaited
// capture against the daemon's last cycle, or expires the wait. Order
// matters and every branch is gated on LastSync.At being AFTER the
// mutation landed — a cycle from before it can neither confirm nor fail
// this capture:
//
//  1. a capture commit naming the folder → confirmed, worded by push state
//     (Pushed beats PushQueued; a commit that is neither pushed nor queued
//     means the cycle broke after committing, which the error branch
//     reports);
//  2. a cycle error → the capture attempt failed; surface it;
//  3. otherwise keep waiting (the cycle may simply not have swept this
//     folder yet) until the 90s deadline, whose toast names the two honest
//     causes (quiesced / offline) and where to look.
//
// The deadline check reads m.now — tick-driven, never time.Now — so tests
// drive expiry with the same fake clock as everything else.
func (m *Model) checkPendingCapture() {
	pending := m.pendingCapture
	if pending == nil {
		return
	}
	if lastSync := m.status.LastSync; lastSync != nil && lastSync.At.After(pending.since) {
		if capturedFolder(lastSync.Commits, pending.folder) && (lastSync.Pushed || lastSync.PushQueued) {
			if lastSync.Pushed {
				m.pushToast("✓ captured — pushed")
			} else {
				m.pushToast("✓ captured — push queued")
			}
			m.pendingCapture = nil
			return
		}
		if lastSync.Error != "" {
			m.pushToast("capture failed: " + lastSync.Error)
			m.pendingCapture = nil
			return
		}
	}
	if !m.now.Before(pending.since.Add(pendingCaptureDeadline)) {
		m.pushToast("capture not yet confirmed — daemon may be quiesced or offline (see Activity)")
		m.pendingCapture = nil
	}
}

// capturedFolder reports whether any commit subject is a capture of
// folder, matched by field POSITION against the engine's own convention —
// a space-delimited substring is not enough, because a folder named like
// the host would sit space-delimited in every subject from that host.
func capturedFolder(commitSubjects []string, folder string) bool {
	for _, subject := range commitSubjects {
		if capturedFolderName, ok := captureSubjectFolder(subject); ok && capturedFolderName == folder {
			return true
		}
	}
	return false
}

// captureSubjectFolder extracts the folder field from one capture-commit
// subject, ok=false for anything that is not a folder capture. The shape
// mirrors the engine's own subject convention and parser
// (internal/engine/commit.go, history.go): `memory: <host> <folder>
// <timestamp>` — exactly four space-separated fields, the first literally
// "memory:", the last parsing as RFC3339.
//
// The folder-field value "manifest" is RESERVED by the engine's meta
// convention (`memory: <host> manifest <stamp>` — the identical shape), so
// a subject carrying it is registry/manifest bookkeeping, never a folder
// capture. The engine's own parser escapes that ambiguity because its
// history queries pathspec-filter meta commits out upstream; the hub reads
// the daemon's unfiltered LastSync subjects, so the reservation must be
// applied here. A real folder literally named "manifest" therefore never
// confirms by subject and degrades to the honest deadline toast.
func captureSubjectFolder(subject string) (string, bool) {
	fields := strings.Split(subject, " ")
	if len(fields) != 4 || fields[0] != "memory:" {
		return "", false
	}
	if _, err := time.Parse(time.RFC3339, fields[3]); err != nil {
		return "", false
	}
	if fields[2] == "manifest" {
		return "", false
	}
	return fields[2], true
}

// toastQuiesceRefusal is the one quiesce-refusal wording, shared by
// refuseIfQuiesced (registry-dispatched mutations) and refuseFlowStart
// (flow requests), so the two paths can never drift.
func (m *Model) toastQuiesceRefusal() {
	m.pushToast(fmt.Sprintf("daemon quiesced until %s — retry after", m.status.QuiescedUntil.Format("15:04:05")))
}

// editorResolves reports whether an editor would resolve right now — the
// availability half of the ErrNoEditor contract (the row is visibly
// disabled; pressing it toasts the exact wording).
func (m *Model) editorResolves() bool {
	_, err := editorx.Resolve(m.settings.Editor, m.getenv)
	return err == nil
}

// browserHasUnits reports whether the navigation stack's top is a Browser
// whose folder has at least one enrolled unit — the receiving end a new
// memory needs (pickUnit's ok=false case, surfaced as availability so the
// n row reads struck instead of lit-but-refusing).
func (m *Model) browserHasUnits() bool {
	top, ok := m.stackTop()
	if !ok {
		return false
	}
	browser, ok := top.(*views.Browser)
	return ok && len(browser.Units()) > 0
}

// flowTarget resolves the memory the flow keys would act on from the top of
// the navigation stack: the browser's cursor row, or the open reading
// view's memory. ok=false with no stack, no selection, or a stack top that
// is neither (the availability gates then read "nothing to act on").
func (m *Model) flowTarget() (memoryfs.Memory, bool) {
	top, ok := m.stackTop()
	if !ok {
		return memoryfs.Memory{}, false
	}
	switch screen := top.(type) {
	case *views.Browser:
		return screen.Selected()
	case *views.Reading:
		return screen.Memory(), true
	default:
		return memoryfs.Memory{}, false
	}
}

// flowAvailable answers available(id) for the flow rows: the brief's
// editor-resolves ∧ fact-class ∧ no-active-session, with each conjunct
// applied where it is meaningful — new has no existing target (its class
// conjunct is replaced by "the folder has a unit to receive the file"),
// rename/delete never touch the editor (no editor conjunct), and the
// one-session gate binds them all. The footer renders a false answer as a
// visibly struck row (stackFooterRows), and the request handlers enforce
// the same gates with a toast, so availability is honest advertising,
// never the only line of defense.
func (m *Model) flowAvailable(id string) bool {
	if m.editing != nil {
		return false
	}
	switch id {
	case "browser-new":
		return m.editorResolves() && m.browserHasUnits()
	case "browser-edit", "reading-edit":
		target, ok := m.flowTarget()
		return ok && target.Class == provider.ClassFact && m.editorResolves()
	case "browser-rename", "browser-delete":
		target, ok := m.flowTarget()
		return ok && target.Class == provider.ClassFact
	default:
		return false
	}
}
