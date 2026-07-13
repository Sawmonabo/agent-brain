package views

import (
	"fmt"
	"path"
	"strings"
	"time"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/links"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/memoryfs"
	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// conflictDetailMetaLines is the fixed height of the metadata block View draws
// above the content viewport: time, project, path, and mode, one line each.
// Fixed regardless of the record's field lengths so the viewport's height
// budget — and with it the exact-fill contract — never shifts with content.
const conflictDetailMetaLines = 4

// Notices the detail renders in place of content when the record's path no
// longer resolves to a live file. Untracked (no enrolled unit carries the
// path) and file-gone (the unit is enrolled but the specific file was deleted
// since the conflict) are distinct facts, told apart so the notice is honest
// about which one happened; both leave nothing to read or edit.
const (
	conflictUntrackedNotice = "this memory is no longer tracked on this machine"
	conflictMissingNotice   = "this memory's file is not present on this machine"
)

// ConflictDetailDeps is everything the conflict detail screen needs, injected
// once by the root (buildConflictDetailDeps). Units is the whole fleet
// snapshot; the screen resolves the record's repo-relative path down to the
// one enrolled unit that still carries it. Registry/ReadBody/Render/Styles are
// the same seams every browsing screen shares, so a detail and the reading
// view it pushes always agree about how bodies are read and markdown rendered.
type ConflictDetailDeps struct {
	Record   config.ConflictRecord // Path is repo-relative: <folder>/<provider>[/<repo_subdir>]/<rel>
	Units    []api.UnitInfo
	Registry *provider.Registry
	ReadBody func(memoryfs.Memory) (string, error)
	Render   func(string, int) string
	Styles   theme.Styles
	// Data and Now feed the history affordance (h). Data is the read-only
	// version surface every history-capable screen shares — threaded straight to
	// the History this screen builds; Now seeds that pushed History's clock so
	// its relative timestamps agree with the rest of the dashboard. A nil Data is
	// reachable only in tests, where History degrades to its loading notice
	// rather than fetching (history.go's Data doc).
	Data HistoryDataSource
	Now  time.Time
}

// ConflictDetail is the pushed detail screen for one retain-both conflict
// event (spec §10): the event's metadata over the memory's CURRENT
// union-merged content, with e handing the live file to the root's edit flow
// (cleaning up a merge IS an edit) and enter drilling into the full reading
// view. Pointer-receiver Screen, mutating in place, for the same reason as
// Reading: a viewport and a render cache are naturally mutable state.
//
// The record's Path is the exact git pathname the merge driver logged
// (internal/cli/merge.go): repo-relative <folder>/<provider>[/<repo_subdir>]/
// <rel>. resolve maps it back to a live memoryfs.Memory; when no enrolled unit
// still carries it — the project was untracked, or the file deleted, since the
// conflict — the screen renders the metadata over an honest notice and offers
// no read or edit (availability, gated on mapped, strikes those footer rows).
type ConflictDetail struct {
	deps ConflictDetailDeps
	// now is the clock the h affordance seeds into a pushed History, advanced by
	// RefreshMsg's Now — a stored value, never a live source (the tick-clock
	// contract shared with the browser's b.now).
	now time.Time

	// folder and repoPath are the record's path split into memoryfs's two
	// coordinates once by resolve — the leading <folder> segment and the
	// <provider>[/<repo_subdir>]/<rel> repo path — so the metadata block and the
	// history affordance both read the same split rather than re-cutting it.
	folder   string
	repoPath string

	// memory is the resolved target; mapped is false when the recorded path no
	// longer maps to a live file, in which case notice explains why and
	// body/loadErr are unused.
	memory memoryfs.Memory
	mapped bool
	notice string

	// historyAvailable gates the h affordance: true exactly when the path
	// resolves to an enrolled unit — the mapped case and the enrolled-but-deleted
	// case — so history is offered wherever a version chain can exist and struck
	// where the project is untracked or its memories cannot be listed.
	historyAvailable bool

	// folderMemories is every memory in the record's folder, captured once by
	// resolve — the source both for finding the target and for the link index
	// the pushed reading view resolves against, so enter never re-walks the
	// tree.
	folderMemories []memoryfs.Memory

	body     string
	loadErr  error
	viewport viewport.Model

	rendered conflictRenderState
}

// conflictRenderState keys the content viewport's cache. Only the successful
// glamour render is cached — the one expensive output — so width is the whole
// key: a mapped body changes only on an explicit refresh, which clears valid,
// and a theme swap (SetRender) clears it too. The notice and read-error paths
// are cheap strings set fresh every View, never cached, so a healed read can
// never leave a stale error on screen.
type conflictRenderState struct {
	valid bool
	width int
}

// NewConflictDetail builds a ready detail screen and resolves the record's
// path to its live memory — construction I/O under the same documented
// local-read exception as NewReading/NewBrowser (screen.go's Screen.Update
// doc): a walk of the record's folder buys a populated first frame instead of
// a guaranteed-blank one until the first tick.
func NewConflictDetail(deps ConflictDetailDeps) *ConflictDetail {
	contentViewport := viewport.New()
	contentViewport.KeyMap = readingViewportKeyMap()
	detail := &ConflictDetail{deps: deps, now: deps.Now, viewport: contentViewport}
	detail.resolve()
	return detail
}

// resolve maps the recorded repo-relative path back to a live memory. The path
// splits into <folder> and the repo path memoryfs speaks (<provider>[/
// <repo_subdir>]/<rel>); LocalTarget answers whether an enrolled unit still
// carries that (folder, path) — the untracked check — and a folder listing
// then locates the on-disk file by (LocalDir, RelPath), the pair LocalTarget
// returned. Listing the whole folder rather than a single guessed unit both
// stays correct for a folder tracked by more than one unit (the codex
// memories+chronicle shape, spec §3) and captures folderMemories for the link
// index enter builds — one walk serving both.
func (d *ConflictDetail) resolve() {
	d.folder, d.repoPath = splitConflictPath(d.deps.Record.Path)
	if d.folder == "" {
		d.notice = conflictUntrackedNotice
		return
	}
	dir, rel, ok := memoryfs.LocalTarget(d.deps.Units, d.folder, d.repoPath)
	if !ok {
		d.notice = conflictUntrackedNotice
		return
	}
	memories, err := memoryfs.List(d.deps.Registry, unitsForFolder(d.deps.Units, d.folder))
	if err != nil {
		d.notice = fmt.Sprintf("cannot read this project's memories: %v", err)
		return
	}
	d.folderMemories = memories
	for _, memory := range memories {
		if memory.LocalDir == dir && memory.RelPath == rel {
			d.memory = memory
			d.mapped = true
			d.historyAvailable = true
			d.readBody()
			return
		}
	}
	// Enrolled-but-deleted: an enrolled unit still carries this path, but the
	// file itself is gone. A version chain can still exist for it, so h stays
	// live (history over on-disk-now content) and the notice points at it;
	// missingNotice words whether an earlier version can be restored, which turns
	// on the path's provider class.
	d.historyAvailable = true
	d.notice = d.missingNotice()
}

// missingNotice words the enrolled-but-deleted notice. The base fact — the file
// is not present on this machine — is constant; the affordance clause always
// names h, and mentions restoring an earlier version only for a fact-class
// path, since a derived index or regenerated artifact is rebuilt from its
// sources rather than restored from history (History's own R-gate enforces
// that; this notice only tells the truth about it). A path memoryfs cannot
// classify (provider segment missing or provider unregistered) falls to the
// conservative no-restore wording — though such a path cannot in fact reach
// here, its folder listing having failed first.
func (d *ConflictDetail) missingNotice() string {
	if class, ok := memoryfs.ClassifyRepoPath(d.deps.Registry, d.repoPath); ok && class == provider.ClassFact {
		return conflictMissingNotice + " — press h to view its history and restore an earlier version"
	}
	return conflictMissingNotice + " — press h to view its history"
}

// splitConflictPath separates the leading <folder> segment from the repo path
// memoryfs speaks. Folder names are a single path segment by contract
// (repo.ValidateFolderName forbids '/'), so the first segment IS the folder; a
// path with no separator has no folder at all and cannot map to a unit.
func splitConflictPath(recordPath string) (folder, repoPath string) {
	folder, repoPath, found := strings.Cut(recordPath, "/")
	if !found {
		return "", recordPath
	}
	return folder, repoPath
}

// unitsForFolder filters the fleet snapshot to folder's own units — the subset
// the record's memory and its link index resolve within. A four-line filter
// rather than a shared export across the views boundary, matching how
// openFolderCmd (projects.go) computes the same subset inline.
func unitsForFolder(units []api.UnitInfo, folder string) []api.UnitInfo {
	matching := make([]api.UnitInfo, 0, len(units))
	for _, unit := range units {
		if unit.Folder == folder {
			matching = append(matching, unit)
		}
	}
	return matching
}

// readBody reads the resolved memory's body, surfacing the first failure so
// the content area shows it immediately rather than a blank frame.
func (d *ConflictDetail) readBody() {
	body, err := d.deps.ReadBody(d.memory)
	if err != nil {
		d.loadErr = err
		return
	}
	d.loadErr = nil
	d.body = body
	d.rendered.valid = false
}

// refreshBody re-reads the memory and adopts a changed body, keeping the last
// good content on a transient read error — cleaning up a merge writes to this
// exact file, so the detail stays live against that write the same way the
// reading view does (screen.go's RefreshMsg contract), and a mid-write read
// must degrade to "stale but readable", never blank an open document.
func (d *ConflictDetail) refreshBody() {
	body, err := d.deps.ReadBody(d.memory)
	if err != nil {
		return
	}
	if d.loadErr != nil || body != d.body {
		d.loadErr = nil
		d.body = body
		d.rendered.valid = false
	}
}

// Update handles one message. RefreshMsg (the root's tick forward) re-reads the
// body and advances the clock the h affordance seeds into a pushed History, so
// that screen's relative timestamps start from the same instant as the rest of
// the dashboard. The metadata block itself renders the recorded event instant,
// absolute by design, so the clock never changes what this screen shows.
func (d *ConflictDetail) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case RefreshMsg:
		d.now = msg.Now
		if d.mapped {
			d.refreshBody()
		}
		return d, nil
	case tea.KeyPressMsg:
		return d.updateKey(msg)
	}
	return d, nil
}

func (d *ConflictDetail) updateKey(msg tea.KeyPressMsg) (Screen, tea.Cmd) {
	switch {
	case keybinding.Matches(msg, DashboardKeys.ConflictDetailBack):
		// The detail has no internal open state to consume first, so esc always
		// signals the root to pop (esc-ordering lesson).
		return d, func() tea.Msg { return PopScreenMsg{} }
	case keybinding.Matches(msg, DashboardKeys.ConflictDetailEdit):
		if !d.mapped {
			return d, nil
		}
		// Emit-only, exactly like the browser's and reading view's e: the root
		// owns the fact-class/editor/one-session gates and the handoff itself
		// (screen.go's EditRequestMsg doc), so the detail never gates or toasts.
		memory := d.memory
		return d, func() tea.Msg { return EditRequestMsg{Memory: memory} }
	case keybinding.Matches(msg, DashboardKeys.ConflictDetailRead):
		if !d.mapped {
			return d, nil
		}
		return d, d.openReading()
	case keybinding.Matches(msg, DashboardKeys.ConflictDetailHistory):
		// openHistory self-gates on historyAvailable (returning nil where the
		// footer strikes the row), so h is inert on an untracked record without a
		// second guard here.
		return d, d.openHistory()
	case msg.String() == "g":
		d.viewport.GotoTop()
		return d, nil
	case msg.String() == "G":
		d.viewport.GotoBottom()
		return d, nil
	}
	var cmd tea.Cmd
	d.viewport, cmd = d.viewport.Update(msg)
	return d, cmd
}

// openReading pushes the resolved memory's full reading view (spec §10's
// enter). The link index resolves within the record's own project (spec §4),
// built from the folder listing resolve already captured so enter never
// re-walks the tree. Constructing the Reading here — its synchronous body read
// included — mirrors how the browser opens one (browser.go's openReading); the
// Cmd merely delivers the finished screen to the root's stack.
func (d *ConflictDetail) openReading() tea.Cmd {
	index := links.BuildIndex(d.folderMemories, d.deps.ReadBody)
	reading := NewReading(ReadingDeps{
		Memory:   d.memory,
		Index:    index,
		ReadBody: d.deps.ReadBody,
		Render:   d.deps.Render,
		Styles:   d.deps.Styles,
	})
	return func() tea.Msg { return PushScreenMsg{Screen: reading} }
}

// openHistory pushes the version-history screen for the record's memory (spec
// §10's file-gone notice made live: h). It returns nil when the path did not
// resolve to a unit — untracked, or its folder unlistable — so h is inert
// exactly where historyAvailable strikes its footer row. The pushed History is
// built the same two ways the browser builds its own (browser.go's openHistory
// / openDeletedHistory): a mapped conflict hands History the resolved Memory and
// a Live that re-reads that file; an enrolled-but-deleted conflict hands it a
// zero Memory and a Live reading whatever is on disk now (memoryfs.LiveContent),
// so a restore performed inside History shows through immediately rather than
// diffing against a frozen empty snapshot.
func (d *ConflictDetail) openHistory() tea.Cmd {
	if !d.historyAvailable {
		return nil
	}
	deps := HistoryDeps{
		Folder:   d.folder,
		RepoPath: d.repoPath,
		Data:     d.deps.Data,
		Render:   d.deps.Render,
		Styles:   d.deps.Styles,
		Now:      d.now,
	}
	if d.mapped {
		memory := d.memory
		deps.Memory = memory
		deps.RepoPath = memory.RepoPath
		deps.Live = func() (string, error) { return d.deps.ReadBody(memory) }
	} else {
		folder, repoPath, units := d.folder, d.repoPath, d.deps.Units
		deps.Live = func() (string, error) { return memoryfs.LiveContent(units, folder, repoPath) }
	}
	history := NewHistory(deps)
	return func() tea.Msg { return PushScreenMsg{Screen: history} }
}

// Title names the breadcrumb segment: the memory's display name when mapped,
// else the record's own filename — an honest segment even when the file is
// gone.
func (d *ConflictDetail) Title() string {
	if d.mapped {
		return d.memory.Name
	}
	return path.Base(d.deps.Record.Path)
}

// Memory reports the resolved target and whether the record still maps to one.
// Exported for the root's flow-availability gates (fact-class ∧ …) and its
// read-availability check, outside the Screen interface for the same reason as
// Browser.Selected/Reading.Memory: the root reaches the concrete type, the
// stack contract stays Update/View/Title.
func (d *ConflictDetail) Memory() (memoryfs.Memory, bool) {
	return d.memory, d.mapped
}

// HistoryAvailable reports whether h does anything — true exactly when the
// record resolved to an enrolled unit (mapped or enrolled-but-deleted).
// Exported for the root's footer/dispatch availability gate, outside the Screen
// interface for the same reason as Memory: the root reaches the concrete type
// while the stack contract stays Update/View/Title.
func (d *ConflictDetail) HistoryAvailable() bool {
	return d.historyAvailable
}

// SetStyles installs a new theme — root-propagated via applyStackTheme on a
// background-color swap. Styles feed the metadata chrome (re-rendered every
// View) directly; only the cached glamour content needs explicit
// invalidation, which SetRender handles.
func (d *ConflictDetail) SetStyles(styles theme.Styles) {
	d.deps.Styles = styles
}

// SetRender installs a new markdown-render seam, invalidating the content
// cache unconditionally: a func value has no equality check, so clearing here
// is the only way a theme swap reliably forces the next View through the new
// renderer instead of a cached string rendered under the old one.
func (d *ConflictDetail) SetRender(render func(md string, width int) string) {
	d.deps.Render = render
	d.rendered.valid = false
}

// View renders the fixed metadata block over the content viewport, which fills
// the remaining height exactly (the viewport space-fills its content area, so
// short content and an unmapped notice pad to the full budget rather than
// leaving the footer floating). The chrome floor is conflictDetailMetaLines +
// 1 (the metadata block plus its trailing blank); at and above it View renders
// exactly height lines, and one viewport row survives below the floor rather
// than collapsing the content to nothing.
func (d *ConflictDetail) View(width, height int) string {
	var view strings.Builder
	view.WriteString(d.metaBlock())
	view.WriteString("\n\n")
	chrome := conflictDetailMetaLines + 1

	d.viewport.SetWidth(width)
	d.viewport.SetHeight(max(height-chrome, 1))
	d.ensureRendered(width)
	view.WriteString(d.viewport.View())
	return strings.TrimRight(view.String(), "\n")
}

// metaBlock renders the event's facts, one per line: the recorded instant, the
// project folder, the repo path within it, and the resolution mode. Exactly
// conflictDetailMetaLines lines regardless of value lengths.
func (d *ConflictDetail) metaBlock() string {
	rows := [conflictDetailMetaLines]struct{ label, value string }{
		{"time", orDash(d.deps.Record.Time)},
		{"project", orDash(d.folder)},
		{"path", orDash(d.repoPath)},
		{"mode", orDash(d.deps.Record.Mode)},
	}
	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = d.deps.Styles.Header.Render(fmt.Sprintf("%-8s", row.label)) + row.value
	}
	return strings.Join(lines, "\n")
}

// ensureRendered brings the viewport's content up to date. The successful
// glamour render is cached on width alone (see conflictRenderState); the
// notice and read-error renders are set fresh every call and never cached, so
// a healed read never shows a stale error.
func (d *ConflictDetail) ensureRendered(width int) {
	cacheable := d.mapped && d.loadErr == nil
	if cacheable && d.rendered.valid && d.rendered.width == width {
		return
	}
	d.viewport.SetContent(d.renderContent(width))
	if cacheable {
		d.rendered = conflictRenderState{valid: true, width: width}
	} else {
		d.rendered.valid = false
	}
}

// renderContent is the viewport body: the glamour-rendered union-merged
// content when mapped, a read-error notice when the file could not be read, or
// the honest "no longer tracked / not present" notice when the path did not
// resolve.
func (d *ConflictDetail) renderContent(width int) string {
	switch {
	case !d.mapped:
		return d.deps.Styles.Dim.Render(d.notice)
	case d.loadErr != nil:
		return d.deps.Styles.Fail.Render(fmt.Sprintf("content unavailable: %v", d.loadErr))
	case d.deps.Render != nil:
		return d.deps.Render(d.body, width)
	default:
		return d.body
	}
}

// orDash renders "—" for an empty field so a blank value reads as "absent"
// rather than a stray gap in the metadata block.
func orDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}
