package views

import (
	"fmt"
	"strings"

	keybinding "charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/config"
)

// maxConflictRows bounds how many records the Conflicts view renders at once —
// the log is retained indefinitely and there is no scroll component here, so a
// pathological log must not blow up one frame. Records arrive newest-first (the
// data adapter already reversed them), so the cap keeps the most recent events.
const maxConflictRows = 200

// ConflictsView lists retained retain-both conflict records (spec §7), loaded
// from the read-only conflict log via DataSource.Conflicts. It owns its
// snapshot because no other view or root-level decision needs it.
type ConflictsView struct {
	styles  theme.Styles
	records []config.ConflictRecord
	err     error
	loaded  bool
	// cursor selects one record for enter-to-detail (spec §10). It indexes the
	// RENDERED window — the newest maxConflictRows records — so it can never
	// point past what View draws, and Set re-clamps it when a refreshed
	// snapshot shrinks the log underneath it.
	cursor int
}

// OpenConflictMsg asks the root to push the selected record's detail screen
// (spec §10). ConflictsView cannot build the *ConflictDetail itself — it holds
// none of Registry/Units/memoryfs/glamour — so it emits this and the root, the
// only place with all of those, constructs and pushes it, exactly as
// OpenFolderMsg drives the Projects tab's enter-to-browse.
type OpenConflictMsg struct{ Record config.ConflictRecord }

// SetStyles installs the palette-derived style set this view renders
// through. Root calls it once on construction and again on every
// tea.BackgroundColorMsg — never per render.
func (v *ConflictsView) SetStyles(styles theme.Styles) {
	v.styles = styles
}

// Set installs a freshly-fetched conflict log snapshot, re-clamping the cursor
// so a refreshed log that shrank underneath it (records rotated out, or a read
// that now errors) never leaves the selection pointing past the last row.
func (v *ConflictsView) Set(records []config.ConflictRecord, err error) {
	v.records = records
	v.err = err
	v.loaded = true
	if v.cursor >= v.selectableCount() {
		v.cursor = max(v.selectableCount()-1, 0)
	}
}

// Update handles the Conflicts tab's own keys: ↑/↓/k/j move the selection and
// enter drills into the selected record's detail screen. The root routes keys
// here only while the Conflicts tab is active, exactly as it routes to
// ProjectsView; all I/O stays in the returned Cmd, so Update stays pure.
func (v *ConflictsView) Update(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case keybinding.Matches(msg, DashboardKeys.ConflictsSelect):
		v.moveCursor(msg.String())
		return nil
	case keybinding.Matches(msg, DashboardKeys.ConflictsOpen):
		return v.open()
	}
	return nil
}

// selectableCount is how many records the cursor can land on: the rendered
// window, capped at maxConflictRows so the cursor never selects a record View
// does not draw.
func (v ConflictsView) selectableCount() int {
	return min(len(v.records), maxConflictRows)
}

// moveCursor steps the selection within the rendered window, clamped at both
// ends (no wrap — a bounded list matches the Projects table's own behavior).
func (v *ConflictsView) moveCursor(key string) {
	count := v.selectableCount()
	if count == 0 {
		v.cursor = 0
		return
	}
	switch key {
	case "up", "k":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < count-1 {
			v.cursor++
		}
	}
}

// open emits the selected record for the root to resolve into a detail screen,
// or nothing when the log is empty. Wrapped in a Cmd — though building the
// message needs no I/O — so the root switches on OpenConflictMsg exactly as it
// does OpenFolderMsg, keeping the push entirely the root's decision.
func (v ConflictsView) open() tea.Cmd {
	if v.selectableCount() == 0 {
		return nil
	}
	record := v.records[v.cursor]
	return func() tea.Msg { return OpenConflictMsg{Record: record} }
}

// conflictsChrome is the fixed vertical overhead View renders above the record
// rows: the title line, its trailing blank, and the TIME/PATH/MODE column
// header. The height window subtracts it (and the truncation notice's own line
// when shown) so the rows plus their chrome never exceed the tab budget.
const conflictsChrome = 3

// View renders the Conflicts tab from the last Set snapshot, windowed to the
// height the root's tab budget hands it (tabBodyHeight). height comes fresh on
// every call, so a resize is handled by construction — no cached dimension to
// invalidate — exactly as the pushed screens size themselves.
func (v ConflictsView) View(height int) string {
	var b strings.Builder
	b.WriteString(sectionTitle(v.styles, "Conflicts"))
	b.WriteString("\n\n")

	switch {
	case v.err != nil:
		fmt.Fprintf(&b, "conflict log unavailable: %v", v.err)
		return b.String()
	case !v.loaded:
		b.WriteString(v.styles.Dim.Render("loading…"))
		return b.String()
	case len(v.records) == 0:
		b.WriteString(v.styles.Dim.Render("no retained conflicts — nothing to reconcile"))
		return b.String()
	}

	records := v.records
	truncated := 0
	if len(records) > maxConflictRows {
		truncated = len(records) - maxConflictRows
		records = records[:maxConflictRows]
	}

	b.WriteString(v.styles.Header.Render(fmt.Sprintf("  %-25s %-44s %s", "TIME", "PATH", "MODE")))
	b.WriteString("\n")

	// Window the (already 200-capped) records to the height budget around the
	// cursor, the same visibleWindow the browser list uses: a log longer than
	// the tab is tall must keep the selected row on screen and never overflow
	// the budget, rather than render every row from the top and let the cursor
	// walk off the bottom (where the root's fitAndFillHeight backstop would clip it).
	budget := max(height-conflictsChrome, 1)
	if truncated > 0 {
		budget = max(budget-1, 1) // the "… and N older" disclosure row
	}
	start, end := visibleWindow(v.cursor, len(records), budget)
	for i := start; i < end; i++ {
		record := records[i]
		marker := "  "
		row := fmt.Sprintf("%-25s %-44s %s", record.Time, record.Path, record.Mode)
		if i == v.cursor {
			marker = "> "
			row = v.styles.Selected.Render(row)
		}
		fmt.Fprintf(&b, "%s%s\n", marker, row)
	}
	if truncated > 0 {
		b.WriteString(v.styles.Dim.Render(fmt.Sprintf("… and %d older record(s) — see `agent-brain conflicts list`", truncated)))
	}
	return strings.TrimRight(b.String(), "\n")
}
