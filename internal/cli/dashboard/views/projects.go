// Package views holds the dashboard's four tab views (Projects, Conflicts,
// Activity, Doctor) split out of the root package (spec §15) so each new
// screen in the dashboard-hub wave lands in its final home from the start.
// The root package (internal/cli/dashboard) is the tab-switching reducer: it
// owns the shared status poll, the daemon-down/service-start flow, and the
// tea.Model plumbing, and forwards view-specific messages and keys down to
// the types here. Views hold their own render state and mutate it through
// exported Set*/On* methods the root calls; they never import the root
// package back (that would cycle) and never perform I/O outside a returned
// tea.Cmd (model purity, enforced by the Q3 gate).
package views

import (
	"context"
	"fmt"
	"strings"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// ProjectsView is the enrolled-fleet table (spec §7). Its columns are
// genuinely per-unit — provider · folder · health · watch · last-cycle —
// with an optional local-dir column on a roomy terminal. Watch state and
// last-cycle are the per-unit telemetry Task 6.5 added to api.UnitInfo; they
// are real per-row signals now. Fleet-level posture the API cannot break
// down per unit (daemon state, quiesce) stays once in the root's persistent
// status header, never fabricated down every identical row.
//
// Keys: `s` syncs the selected folder (client.Sync), `t` toggles tracking
// with an inline y/N confirm. Because the list holds only already-enrolled
// units, the toggle direction is always untrack (client.Untrack, never
// --purge — a purge is a destructive typed-confirm operation reserved for
// the CLI).
type ProjectsView struct {
	styles theme.Styles
	table  table.Model
	Units  []api.UnitInfo
	wide   bool // terminal is roomy enough for the LOCAL DIR column
	// loaded latches true on the first Units delivery (success or error) so
	// the view can tell "not fetched yet" from "genuinely empty" and never
	// flash the empty-state guidance on open.
	loaded bool

	Confirming bool
	// confirmUnit is the unit captured when the confirm opened. `y` untracks
	// exactly this identity — it is never re-resolved through the cursor, which
	// the 2s poll can move onto a different unit while the confirm sits open.
	confirmUnit api.UnitInfo

	// Add flow (track.go): a modal state machine over discovery → picker →
	// path confirm → identity → optional naming → Track. The picker is a
	// multi-select (spec §13): addSelected holds the space-toggled row set,
	// and enter confirms the whole set into addQueue, which the per-candidate
	// stages then work one at a time — addQueuePos names the candidate in
	// flight and addEnrolled accumulates every folder enrolled across the
	// queue, so an esc mid-queue can name exactly what already landed.
	Adding        AddStage
	addCandidates []TrackCandidate
	addCursor     int
	addSelected   map[int]bool
	addQueue      []TrackCandidate
	addQueuePos   int
	addEnrolled   []string
	addChoice     TrackCandidate
	addInput      textinput.Model

	// Migrate flow (migrate.go): the spec §10 bash-era importer driven
	// through the daemon, a modal state machine mirroring the add flow —
	// preflight → discover → single-select pick → path confirm → identity →
	// optional naming → Migrate. migratePreflighted latches the once-per-
	// session chezmoi gate; migrateProjectPath/migrateLiveDir carry the
	// confirmed path and its resolved live provider dir into the request.
	Migrating          MigrateStage
	migrateCandidates  []MigrateCandidate
	migrateCursor      int
	migrateChoice      MigrateCandidate
	migrateInput       textinput.Model
	migrateProjectPath string
	migrateLiveDir     string
	migratePreflighted bool

	notice  string // transient result of the last s/t action
	loadErr error
}

// NewProjectsView builds a ready-to-use ProjectsView.
func NewProjectsView() ProjectsView {
	view := ProjectsView{}
	view.table = table.New(table.WithFocused(true), table.WithHeight(10))
	view.setColumns(false)
	view.addInput = textinput.New()
	view.migrateInput = textinput.New()
	return view
}

// SetStyles installs the palette-derived style set this view renders
// through. Root calls it once on construction and again on every
// tea.BackgroundColorMsg — never per render — so a palette swap recolors
// this view from one call, not threaded styling on every keystroke.
func (v *ProjectsView) SetStyles(styles theme.Styles) {
	v.styles = styles
}

// setColumns installs the per-unit column set. wide adds LOCAL DIR, shown only
// when the terminal can carry the full path without crowding the essentials.
//
// Rows and columns must change atomically. bubbles renders inside SetColumns
// (SetColumns -> UpdateViewport -> renderRow indexes m.cols by each ROW's cell
// count), so installing a narrower column set while the table still holds the
// wider rows indexes past the fresh column slice and panics. Clearing the rows
// first renders the empty table safely under any column set; the closing
// rebuild then re-seeds rows that match the new width, so no caller can ever
// leave columns and rows disagreeing — and SetSize installs columns through
// here without a second rebuild of its own.
func (v *ProjectsView) setColumns(wide bool) {
	columns := []table.Column{
		{Title: "PROVIDER", Width: 9},
		{Title: "FOLDER", Width: 20},
		{Title: "HEALTH", Width: 9},
		{Title: "WATCH", Width: 9},
		{Title: "LAST CYCLE", Width: 11},
	}
	if wide {
		columns = append(columns, table.Column{Title: "LOCAL DIR", Width: 48})
	}
	// SetRows(nil) drives the bubbles cursor to -1 and rebuild re-seats it to
	// row 0, so capture the selection first and restore it afterward: a mere
	// resize must not move the highlight off the unit the user had selected.
	cursor := v.table.Cursor()
	v.table.SetRows(nil)
	v.table.SetColumns(columns)
	v.wide = wide
	v.rebuild()
	if cursor >= 0 && cursor < len(v.Units) {
		v.table.SetCursor(cursor)
	}
}

// SetUnits installs a freshly-fetched unit list.
func (v *ProjectsView) SetUnits(units []api.UnitInfo) {
	v.Units = units
	v.loaded = true
	v.loadErr = nil
	v.rebuild()
}

// SetLoadErr records a failed fetch.
func (v *ProjectsView) SetLoadErr(err error) {
	v.loaded = true
	v.loadErr = err
}

// SetSize adjusts the table to the terminal's current dimensions.
func (v *ProjectsView) SetSize(width, height int) {
	if width > 0 {
		v.table.SetWidth(width)
		// The five essential columns need ~58 cols and always fit; LOCAL DIR (48)
		// pushes the full set past ~115, so it is added only on a genuinely wide
		// terminal where the path renders in full rather than truncating the
		// essentials it sits beside.
		if wide := width >= 120; wide != v.wide {
			// setColumns rebuilds the rows itself (it must, to keep columns and
			// rows in step) — no separate rebuild here.
			v.setColumns(wide)
		}
	}
	// Reserve every non-table line the composed Projects frame carries at full
	// chrome, so the space-filled table never pushes a real row off the bottom
	// (spec §9): 8 content lines — the status header, both toast slots (sticky +
	// info), the tab bar, this tab's section title, the fleet header (one row
	// above the table), the action notice, and the footer — plus the 6 blank
	// separators between those blocks. 14 total; the fleet header is the row
	// that took it from 13 to 14. This reservation is fixed at the two-toast
	// maximum regardless of actual toast occupancy — unlike tabBodyHeight's
	// frameChromeLines (dashboard.go), which measures the header's real height
	// every frame. Fewer toasts than the maximum leave the TABLE short of the
	// room actually available, not the composed frame: the root's
	// fitAndFillHeight pads any such gap out to the terminal's exact height
	// regardless (dashboard.go's own doc), so the footer still lands on the
	// last row — the table itself just does not grow to use the freed space.
	if bodyHeight := height - 14; bodyHeight > 3 {
		v.table.SetHeight(bodyHeight)
	}
}

func (v *ProjectsView) rebuild() {
	v.table.SetRows(projectRows(v.Units, v.wide))
	if len(v.Units) == 0 {
		return
	}
	// bubbles/v2 tables start with cursor -1 (no selection) and SetRows does
	// not advance it; seat it on a valid row so `s`/`t` always act on the
	// highlighted unit, and re-seat it if a shrunk fleet stranded it past the
	// last row.
	if cursor := v.table.Cursor(); cursor < 0 {
		v.table.SetCursor(0)
	} else if cursor >= len(v.Units) {
		v.table.SetCursor(len(v.Units) - 1)
	}
}

// Update handles the Projects view's own keys. It mutates v in place and
// returns any Cmd the action produced (all I/O stays in the returned Cmd —
// never inline — so Update stays pure). The root routes keys here only when
// Projects is the active view.
func (v *ProjectsView) Update(msg tea.KeyPressMsg, data DataSource, actions TrackActions, migrate MigrateActions) tea.Cmd {
	if handled, cmd := v.updateAdd(msg, data, actions); handled {
		return cmd
	}
	if handled, cmd := v.updateMigrate(msg, data, migrate); handled {
		return cmd
	}
	if v.Confirming {
		switch {
		case keybinding.Matches(msg, DashboardKeys.ConfirmDecision):
			// Membership gate, then the concrete key decides — the TabSwitch
			// idiom. y/Y confirms; n/N (the only other members) cancels, so the
			// default arm is exact, not a catch-all.
			switch msg.String() {
			case "y", "Y":
				v.Confirming = false
				// Untrack the unit captured when the confirm opened, not whatever the
				// cursor points at now — a poll may have rebuilt the rows underneath it.
				unit := v.confirmUnit
				v.notice = fmt.Sprintf("untracking %s…", unit.Folder)
				return untrackCmd(data, unit)
			default:
				v.Confirming = false
				v.notice = "untrack cancelled"
				return nil
			}
		case keybinding.Matches(msg, DashboardKeys.Cancel):
			v.Confirming = false
			v.notice = "untrack cancelled"
			return nil
		}
		return nil // swallow everything else while the confirm is open
	}

	switch {
	case keybinding.Matches(msg, DashboardKeys.Sync):
		unit, ok := v.SelectedUnit()
		if !ok {
			return nil
		}
		v.notice = fmt.Sprintf("syncing %s…", unit.Folder)
		return SyncCmd(data, unit.Folder)
	case keybinding.Matches(msg, DashboardKeys.Untrack):
		unit, ok := v.SelectedUnit()
		if !ok {
			return nil
		}
		v.Confirming = true
		v.confirmUnit = unit
		v.notice = ""
		return nil
	case keybinding.Matches(msg, DashboardKeys.Add):
		if !actions.AddAvailable() {
			v.notice = "add is unavailable in this build"
			return nil
		}
		v.Adding = AddDiscovering
		v.notice = ""
		return discoverCmd(actions)
	case keybinding.Matches(msg, DashboardKeys.Migrate):
		if !migrate.MigrateAvailable() {
			v.notice = "migrate is unavailable in this build"
			return nil
		}
		v.notice = ""
		// The chezmoi pre-flight gate (spec §10) runs once per hub session; a
		// later m goes straight to discovery — migrateOne's own resume idiom is
		// the daemon-side Skipped marker, not a repeated gate.
		if v.migratePreflighted {
			v.Migrating = MigrateDiscovering
			return migrateDiscoverCmd(migrate)
		}
		v.Migrating = MigratePreflighting
		return migratePreflightCmd(migrate)
	case keybinding.Matches(msg, DashboardKeys.Open):
		unit, ok := v.SelectedUnit()
		if !ok {
			return nil
		}
		return openFolderCmd(unit.Folder, v.Units)
	}

	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

// SelectedUnit reports the unit under the table's cursor, if any.
func (v ProjectsView) SelectedUnit() (api.UnitInfo, bool) {
	cursor := v.table.Cursor()
	if cursor < 0 || cursor >= len(v.Units) {
		return api.UnitInfo{}, false
	}
	return v.Units[cursor], true
}

type (
	// SyncResultMsg is produced by SyncCmd — this view's own sync-key
	// action — but exported because the root's Update switches on it
	// directly (spec §15's views split) to re-fetch the fleet state after
	// the sync completes.
	SyncResultMsg struct {
		folder string
		resp   api.SyncResponse
		err    error
	}
	// UntrackResultMsg is produced by untrackCmd — this view's own untrack
	// confirm — but exported for the same reason as SyncResultMsg: the
	// root's Update switches on it directly to re-fetch the fleet state
	// after the untrack completes.
	UntrackResultMsg struct {
		folder string
		resp   api.UntrackResponse
		err    error
	}
	// OpenFolderMsg is produced by openFolderCmd — this view's own
	// enter-to-browse action — and exported so the root's Update switches on
	// it directly to push a memory browser Screen (spec §3). Units carries
	// every fleet row that shares Folder, not just the row the cursor was on:
	// a project tracked by more than one provider shows one table row per
	// provider, but the browser groups every provider's memories under that
	// one project, so BrowserDeps needs the whole matching subset.
	OpenFolderMsg struct {
		Folder string
		Units  []api.UnitInfo
	}
)

// OnSyncResult records a sync's outcome for display.
func (v *ProjectsView) OnSyncResult(msg SyncResultMsg) {
	label := msg.folder
	if label == "" {
		label = "fleet"
	}
	switch {
	case msg.err != nil:
		v.notice = fmt.Sprintf("sync %s failed: %v", label, msg.err)
	case msg.resp.Status == "running":
		v.notice = fmt.Sprintf("sync %s still running — check Activity", label)
	default:
		v.notice = fmt.Sprintf("synced %s", label)
	}
}

// OnUntrackResult records an untrack's outcome for display.
func (v *ProjectsView) OnUntrackResult(msg UntrackResultMsg) {
	switch {
	case msg.err != nil:
		v.notice = fmt.Sprintf("untrack %s failed: %v", msg.folder, msg.err)
	case msg.resp.Removed:
		v.notice = fmt.Sprintf("untracked %s (repo history retained)", msg.folder)
	default:
		v.notice = fmt.Sprintf("%s was not enrolled — nothing to remove", msg.folder)
	}
}

// ModalOpen reports whether a Projects-view modal (untrack confirm or the
// add flow) owns the keyboard: while true, the root must route keys here
// BEFORE its own tab/quit globals, so typing a path containing "1" or "q"
// edits the input instead of switching tabs or quitting.
func (v ProjectsView) ModalOpen() bool {
	return v.Confirming || v.Adding != AddNone || v.Migrating != MigrateNone
}

// View renders the Projects tab: the add flow (if active), else the fleet
// header above the table or an empty/loading/error state, followed by the
// confirm prompt or the last action's notice. fleetHeader is the root-composed
// one-line fleet summary (spec §9) — rendered only above a populated table
// (the empty/loading/error/add states carry their own guidance and would only
// be crowded by a "0 units" line above them); "" suppresses it (the row the
// header would occupy is always reserved in SetSize, so a suppressed header
// leaves the frame short, never overflowing).
func (v ProjectsView) View(fleetHeader string) string {
	var b strings.Builder
	b.WriteString(sectionTitle(v.styles, "Projects"))
	b.WriteString("\n\n")

	switch {
	case v.Adding != AddNone:
		b.WriteString(v.addView())
		return strings.TrimRight(b.String(), "\n")
	case v.Migrating != MigrateNone:
		b.WriteString(v.migrateView())
		return strings.TrimRight(b.String(), "\n")
	case v.loadErr != nil:
		fmt.Fprintf(&b, "projects unavailable: %v", v.loadErr)
	case !v.loaded:
		b.WriteString(v.styles.Dim.Render("loading projects…"))
	case len(v.Units) == 0:
		b.WriteString(v.styles.Dim.Render("no projects enrolled — run `agent-brain track` or press a"))
	default:
		if fleetHeader != "" {
			b.WriteString(v.styles.Dim.Render(fleetHeader))
			b.WriteString("\n")
		}
		b.WriteString(v.table.View())
	}
	b.WriteString("\n")

	switch {
	case v.Confirming:
		b.WriteString(v.styles.Warn.Render(fmt.Sprintf("untrack %s? (y/N)", v.confirmUnit.Folder)))
	case v.notice != "":
		b.WriteString(v.styles.Dim.Render(v.notice))
	}
	return strings.TrimRight(b.String(), "\n")
}

// sectionTitle renders a view's header in the shared Title style — every
// view (Projects, Conflicts, Activity, Doctor) starts its render with it.
func sectionTitle(styles theme.Styles, title string) string {
	return styles.Title.Render(title)
}

func projectRows(units []api.UnitInfo, wide bool) []table.Row {
	rows := make([]table.Row, len(units))
	for i, unit := range units {
		health := "ok"
		if unit.Degraded {
			health = "degraded"
		}
		row := table.Row{unit.Provider, unit.Folder, health, watchCell(unit.WatchState), lastCycleCell(unit.LastCycle)}
		if wide {
			row = append(row, unit.LocalDir)
		}
		rows[i] = row
	}
	return rows
}

// watchCell compresses a unit's WatchState into a table token: "watching", the
// bare "failed" (the full reason is too long for a column — it rides
// `projects --json`), or "—" before the daemon's first watcher build records it.
func watchCell(state string) string {
	switch {
	case state == "":
		return "—"
	case strings.HasPrefix(state, "failed"):
		return "failed"
	default:
		return state
	}
}

// lastCycleCell renders a unit's last-cycle outcome, or "—" when its folder has
// not cycled yet. The outcome carries the one signal HEALTH's degraded-bool
// cannot: a whole-cycle "error" that left nothing degraded.
func lastCycleCell(cycle *api.UnitCycleResult) string {
	if cycle == nil {
		return "—"
	}
	return cycle.Outcome
}

// SyncCmd fires a sync for folder ("" syncs the whole fleet). It is exported
// because the root's Update calls it directly after a successful
// TrackResultMsg, to make the enrollment's first mirror-in visible right
// away rather than waiting on the next poll.
func SyncCmd(data DataSource, folder string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.Sync(ctx, folder)
		return SyncResultMsg{folder: folder, resp: resp, err: err}
	}
}

func untrackCmd(data DataSource, unit api.UnitInfo) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), RequestTimeout)
		defer cancel()
		resp, err := data.Untrack(ctx, api.UntrackRequest{Provider: unit.Provider, LocalDir: unit.LocalDir})
		return UntrackResultMsg{folder: unit.Folder, resp: resp, err: err}
	}
}

// openFolderCmd emits OpenFolderMsg for folder, pre-filtering fleetUnits down
// to the rows that share it. Wrapped in a Cmd — even though building the
// message involves no I/O — so Update keeps its "returned Cmd, never inline"
// rule with no special case: the root's Update switches on the resulting
// message the same way it already does for SyncResultMsg/UntrackResultMsg,
// so pushing the browser screen stays entirely the root's decision.
func openFolderCmd(folder string, fleetUnits []api.UnitInfo) tea.Cmd {
	matching := make([]api.UnitInfo, 0, len(fleetUnits))
	for _, unit := range fleetUnits {
		if unit.Folder == folder {
			matching = append(matching, unit)
		}
	}
	return func() tea.Msg {
		return OpenFolderMsg{Folder: folder, Units: matching}
	}
}
