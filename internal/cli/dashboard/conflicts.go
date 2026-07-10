package dashboard

import (
	"fmt"
	"strings"
)

// maxConflictRows bounds how many records the Conflicts view renders at once —
// the log is retained indefinitely and there is no scroll component here, so a
// pathological log must not blow up one frame. Newest-first (parseConflictLog
// already reversed), so the cap keeps the most recent events.
const maxConflictRows = 200

// conflictsView lists retained retain-both conflict records (spec §7), loaded
// from the read-only conflict log via dashboardData.Conflicts. It owns its
// snapshot because no other view or root-level decision needs it.
type conflictsView struct {
	records []ConflictRecord
	err     error
	loaded  bool
}

func (v *conflictsView) set(records []ConflictRecord, err error) {
	v.records = records
	v.err = err
	v.loaded = true
}

func (v conflictsView) view() string {
	var b strings.Builder
	b.WriteString(sectionTitle("Conflicts"))
	b.WriteString("\n\n")

	switch {
	case v.err != nil:
		fmt.Fprintf(&b, "conflict log unavailable: %v", v.err)
		return b.String()
	case !v.loaded:
		b.WriteString(dimStyle.Render("loading…"))
		return b.String()
	case len(v.records) == 0:
		b.WriteString(dimStyle.Render("no retained conflicts — nothing to reconcile"))
		return b.String()
	}

	records := v.records
	truncated := 0
	if len(records) > maxConflictRows {
		truncated = len(records) - maxConflictRows
		records = records[:maxConflictRows]
	}

	b.WriteString(headerStyle.Render(fmt.Sprintf("%-25s %-44s %s", "TIME", "PATH", "MODE")))
	b.WriteString("\n")
	for _, record := range records {
		fmt.Fprintf(&b, "%-25s %-44s %s\n", record.Time, record.Path, record.Mode)
	}
	if truncated > 0 {
		b.WriteString(dimStyle.Render(fmt.Sprintf("… and %d older record(s) — see `agent-brain conflicts list`", truncated)))
	}
	return strings.TrimRight(b.String(), "\n")
}
