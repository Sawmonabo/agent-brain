package dashboard

import (
	"context"
	"fmt"
	"strings"

	keybinding "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// projectsView is the enrolled-fleet table (spec §7). Its columns are genuinely
// per-unit — provider · folder · health · watch · last-cycle — with an optional
// local-dir column on a roomy terminal. Watch state and last-cycle are the
// per-unit telemetry Task 6.5 added to api.UnitInfo; they are real per-row
// signals now. Fleet-level posture the API cannot break down per unit (daemon
// state, quiesce) stays once in the persistent status header, never fabricated
// down every identical row.
//
// Keys: `s` syncs the selected folder (client.Sync), `t` toggles tracking with
// an inline y/N confirm. Because the list holds only already-enrolled units,
// the toggle direction is always untrack (client.Untrack, never --purge — a
// purge is a destructive typed-confirm operation reserved for the CLI).
type projectsView struct {
	table table.Model
	units []api.UnitInfo
	wide  bool // terminal is roomy enough for the LOCAL DIR column
	// loaded latches true on the first projectsMsg (success or error) so the
	// view can tell "not fetched yet" from "genuinely empty" and never flash the
	// empty-state guidance on open.
	loaded bool

	confirming bool
	// confirmUnit is the unit captured when the confirm opened. `y` untracks
	// exactly this identity — it is never re-resolved through the cursor, which
	// the 2s poll can move onto a different unit while the confirm sits open.
	confirmUnit api.UnitInfo

	notice  string // transient result of the last s/t action
	loadErr error
}

func newProjectsView() projectsView {
	view := projectsView{}
	view.table = table.New(table.WithFocused(true), table.WithHeight(10))
	view.setColumns(false)
	return view
}

// setColumns installs the per-unit column set. wide adds LOCAL DIR, shown only
// when the terminal can carry the full path without crowding the essentials.
func (v *projectsView) setColumns(wide bool) {
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
	v.wide = wide
	v.table.SetColumns(columns)
}

func (v *projectsView) setUnits(units []api.UnitInfo) {
	v.units = units
	v.loaded = true
	v.loadErr = nil
	v.rebuild()
}

func (v *projectsView) setLoadErr(err error) {
	v.loaded = true
	v.loadErr = err
}

func (v *projectsView) setSize(width, height int) {
	if width > 0 {
		v.table.SetWidth(width)
		// The five essential columns need ~58 cols and always fit; LOCAL DIR (48)
		// pushes the full set past ~115, so it is added only on a genuinely wide
		// terminal where the path renders in full rather than truncating the
		// essentials it sits beside.
		if wide := width >= 120; wide != v.wide {
			v.setColumns(wide)
			v.rebuild()
		}
	}
	// Leave room for the status header, tab bar, section title, footer, notice.
	if bodyHeight := height - 13; bodyHeight > 3 {
		v.table.SetHeight(bodyHeight)
	}
}

func (v *projectsView) rebuild() {
	v.table.SetRows(projectRows(v.units, v.wide))
	if len(v.units) == 0 {
		return
	}
	// bubbles/v2 tables start with cursor -1 (no selection) and SetRows does
	// not advance it; seat it on a valid row so `s`/`t` always act on the
	// highlighted unit, and re-seat it if a shrunk fleet stranded it past the
	// last row.
	if cursor := v.table.Cursor(); cursor < 0 {
		v.table.SetCursor(0)
	} else if cursor >= len(v.units) {
		v.table.SetCursor(len(v.units) - 1)
	}
}

// update handles the Projects view's own keys. It mutates v in place and
// returns any Cmd the action produced (all I/O stays in the returned Cmd —
// never inline — so Update stays pure). The root routes keys here only when
// Projects is the active view.
func (v *projectsView) update(msg tea.KeyPressMsg, data dashboardData) tea.Cmd {
	if v.confirming {
		switch msg.String() {
		case "y", "Y":
			v.confirming = false
			// Untrack the unit captured when the confirm opened, not whatever the
			// cursor points at now — a poll may have rebuilt the rows underneath it.
			unit := v.confirmUnit
			v.notice = fmt.Sprintf("untracking %s…", unit.Folder)
			return untrackCmd(data, unit)
		case "n", "N", "esc":
			v.confirming = false
			v.notice = "untrack cancelled"
			return nil
		}
		return nil // swallow everything else while the confirm is open
	}

	switch {
	case keybinding.Matches(msg, dashboardKeys.Sync):
		unit, ok := v.selectedUnit()
		if !ok {
			return nil
		}
		v.notice = fmt.Sprintf("syncing %s…", unit.Folder)
		return syncCmd(data, unit.Folder)
	case keybinding.Matches(msg, dashboardKeys.Untrack):
		unit, ok := v.selectedUnit()
		if !ok {
			return nil
		}
		v.confirming = true
		v.confirmUnit = unit
		v.notice = ""
		return nil
	}

	var cmd tea.Cmd
	v.table, cmd = v.table.Update(msg)
	return cmd
}

func (v projectsView) selectedUnit() (api.UnitInfo, bool) {
	cursor := v.table.Cursor()
	if cursor < 0 || cursor >= len(v.units) {
		return api.UnitInfo{}, false
	}
	return v.units[cursor], true
}

func (v *projectsView) onSyncResult(msg syncResultMsg) {
	switch {
	case msg.err != nil:
		v.notice = fmt.Sprintf("sync %s failed: %v", msg.folder, msg.err)
	case msg.resp.Status == "running":
		v.notice = fmt.Sprintf("sync %s still running — check Activity", msg.folder)
	default:
		v.notice = fmt.Sprintf("synced %s", msg.folder)
	}
}

func (v *projectsView) onUntrackResult(msg untrackResultMsg) {
	switch {
	case msg.err != nil:
		v.notice = fmt.Sprintf("untrack %s failed: %v", msg.folder, msg.err)
	case msg.resp.Removed:
		v.notice = fmt.Sprintf("untracked %s (repo history retained)", msg.folder)
	default:
		v.notice = fmt.Sprintf("%s was not enrolled — nothing to remove", msg.folder)
	}
}

func (v projectsView) view() string {
	var b strings.Builder
	b.WriteString(sectionTitle("Projects"))
	b.WriteString("\n\n")

	if v.loadErr != nil {
		fmt.Fprintf(&b, "projects unavailable: %v", v.loadErr)
		return b.String()
	}
	if !v.loaded {
		b.WriteString(dimStyle.Render("loading projects…"))
		return b.String()
	}
	if len(v.units) == 0 {
		b.WriteString(dimStyle.Render("no projects enrolled — run `agent-brain track`"))
		return b.String()
	}

	b.WriteString(v.table.View())
	b.WriteString("\n")

	switch {
	case v.confirming:
		b.WriteString(warnStyle.Render(fmt.Sprintf("untrack %s? (y/N)", v.confirmUnit.Folder)))
	case v.notice != "":
		b.WriteString(dimStyle.Render(v.notice))
	}
	return strings.TrimRight(b.String(), "\n")
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

func syncCmd(data dashboardData, folder string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		resp, err := data.Sync(ctx, folder)
		return syncResultMsg{folder: folder, resp: resp, err: err}
	}
}

func untrackCmd(data dashboardData, unit api.UnitInfo) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()
		resp, err := data.Untrack(ctx, api.UntrackRequest{Provider: unit.Provider, LocalDir: unit.LocalDir})
		return untrackResultMsg{folder: unit.Folder, resp: resp, err: err}
	}
}
