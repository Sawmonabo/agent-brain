package views

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard/theme"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// ActivityView renders the daemon status snapshot (spec §7): uptime, state
// detail, quiesced-until (Task 2), the fleet watch-trigger total, and the
// last SyncSummary. It holds no state of its own beyond its styles — the
// daemon status and the unit list are owned by the root model and passed in
// at render.
//
// The fleet's watch-trigger count (spec §7's "watch trigger counts") is the
// MAX of the per-unit WatchTriggers the Projects payload now carries (Task
// 6.5) — the raw trigger count, since triggers are fleet-global (the WHY is
// at the call site) — so the units are passed in at render, still zero new
// daemon endpoints.
type ActivityView struct {
	styles theme.Styles

	// pane bounds the snapshot to the tab's height budget and scrolls it in
	// place (spec §7): a long sync summary (many commits, scrubbed/degraded
	// lists) can run past a short terminal, so it renders through a viewport
	// rather than growing the frame and letting the root clamp silently cut the
	// tail. The daemon status and unit list stay root-owned and passed in at
	// render; the pane holds only the scroll offset and its change key.
	pane scrollPane
}

// NewActivityView builds an Activity tab with its scroll pane initialized. The
// model constructs it here rather than zero-valuing it because the pane's
// viewport needs its scroll keymap before ctrl+d/G ever reach it.
func NewActivityView() ActivityView {
	return ActivityView{pane: newScrollPane()}
}

// SetStyles installs the palette-derived style set this view renders
// through. Root calls it once on construction and again on every
// tea.BackgroundColorMsg — never per render.
func (v *ActivityView) SetStyles(styles theme.Styles) {
	v.styles = styles
}

// View renders the Activity tab: a fixed section title over a height-bounded,
// scrollable body. The daemon status and unit list are still owned by the root
// and passed in fresh at render (this view stores none of that data); width/
// height come fresh too, so a resize is handled by construction. now feeds the
// live uptime/quiesce-remaining, which is why the body is re-installed every
// render even without new data. Value receiver, like DoctorView.View — the
// scroll offset is advanced only by Scroll on the root's copy.
func (v ActivityView) View(status api.StatusResponse, statusErr error, units []api.UnitInfo, now time.Time, width, height int) string {
	v.syncPane(status, statusErr, units, now)
	return sectionTitle(v.styles, "Activity") + "\n\n" +
		v.pane.render(v.styles, width, max(height-sectionChromeLines, 1))
}

// Scroll routes a scroll key to the snapshot pane, sizing it to the same budget
// View uses so the page math matches what is drawn. It reports whether the key
// was a scroll key it consumed; Activity has no other keys, so the root ignores
// a miss.
func (v *ActivityView) Scroll(msg tea.KeyPressMsg, status api.StatusResponse, statusErr error, units []api.UnitInfo, now time.Time, width, height int) bool {
	v.syncPane(status, statusErr, units, now)
	return v.pane.scroll(msg, width, max(height-sectionChromeLines, 1))
}

// OnData refreshes the scroll pane's change tracking when new daemon data
// arrives (a status poll or a projects refresh), resetting to the top only on a
// materially changed status — never on the 2s cadence itself, which would yank a
// reader mid-scroll. Called from the root's message handlers so the reset
// PERSISTS (View runs on a value copy); the ticking uptime is excluded from the
// change key, so only real status changes reset the scroll.
func (v *ActivityView) OnData(status api.StatusResponse, statusErr error, units []api.UnitInfo, now time.Time) {
	v.syncPane(status, statusErr, units, now)
}

// syncPane installs the rendered body in the scroll pane. The change key is the
// body rendered at a ZERO reference time: uptimeSuffix returns "" for it and the
// quiesce-remaining collapses to a now-invariant value, so the once-a-second
// uptime tick is not read as a new document (which would GotoTop every frame and
// make the tab unscrollable) while a real change — a new sync, a state flip, a
// fresh quiesce deadline — still resets the scroll to the top. The huge clamped
// remaining in the change key is never shown; it only has to be stable.
func (v *ActivityView) syncPane(status api.StatusResponse, statusErr error, units []api.UnitInfo, now time.Time) {
	v.pane.refresh(v.body(status, statusErr, units, now), v.body(status, statusErr, units, time.Time{}))
}

// body renders everything BELOW the section title — the daemon line, optional
// detail/quiesce/trigger lines, and the last-sync summary — or the read-error
// placeholder. It is the scroll pane's content (the title is fixed chrome above
// it). now feeds the relative uptime and quiesce-remaining; the caller passes a
// zero now to derive a change key free of those live values (syncPane).
func (v ActivityView) body(status api.StatusResponse, statusErr error, units []api.UnitInfo, now time.Time) string {
	if statusErr != nil {
		// Daemon-down is handled by the root before any view renders; a
		// non-down error here is some other read failure worth showing plainly.
		return fmt.Sprintf("status unavailable: %v", statusErr)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "daemon: %s (version %s, pid %d%s)\n",
		valueOrDash(status.State), valueOrDash(status.Version), status.PID, uptimeSuffix(status.StartedAt, now))
	if status.StateDetail != "" {
		fmt.Fprintf(&b, "  detail: %s\n", status.StateDetail)
	}
	if status.QuiescedUntil != nil && status.QuiescedUntil.After(now) {
		fmt.Fprintf(&b, "quiesced until %s (%s remaining)\n",
			status.QuiescedUntil.Format("2006-01-02 15:04:05 MST"),
			status.QuiescedUntil.Sub(now).Round(time.Second))
	}
	if len(units) > 0 {
		// Triggers are fleet-global (ADR 07: the watcher never routes per-unit; a
		// trigger drives one whole-fleet cycle), so each unit's WatchTriggers is a
		// participation count — the triggers that fired while it was under watch
		// coverage. The truthful fleet number is the MAX, not the sum: a root
		// watched since daemon start caught every trigger, so max is the raw
		// trigger count since the longest-watched root. Summing would amplify that
		// raw count by fleet size.
		var triggers uint64
		for _, unit := range units {
			if unit.WatchTriggers > triggers {
				triggers = unit.WatchTriggers
			}
		}
		fmt.Fprintf(&b, "watch triggers: %d\n", triggers)
	}

	b.WriteString("\n")
	if status.LastSync == nil {
		b.WriteString("last sync: never")
		return b.String()
	}
	fmt.Fprintf(&b, "last sync: %s\n", status.LastSync.At.Format("2006-01-02 15:04:05 MST"))
	writeSyncSummary(&b, status.LastSync)
	return strings.TrimRight(b.String(), "\n")
}

// writeSyncSummary renders one cycle's outcome, mirroring the CLI `status`
// surface (internal/cli/client_commands.go's printSummary). Scrubbed is the
// loudest signal a cycle can carry (a push tried to unscope the encryption
// filter, spec §5), so it is always shown when present.
func writeSyncSummary(b *strings.Builder, summary *api.SyncSummary) {
	if summary.Error != "" {
		fmt.Fprintf(b, "  error: %s\n", summary.Error)
	}
	if summary.Offline {
		b.WriteString("  offline: remote unreachable this cycle — local commits queued\n")
	}
	for _, subject := range summary.Commits {
		fmt.Fprintf(b, "  commit: %s\n", subject)
	}
	fmt.Fprintf(b, "  in:  %d copied / %d deleted / %d skipped\n",
		summary.MirrorIn.Copied, summary.MirrorIn.Deleted, summary.MirrorIn.Skipped)
	fmt.Fprintf(b, "  out: %d copied / %d deleted / %d skipped\n",
		summary.MirrorOut.Copied, summary.MirrorOut.Deleted, summary.MirrorOut.Skipped)
	fmt.Fprintf(b, "  pushed: %v  queued: %v\n", summary.Pushed, summary.PushQueued)
	if len(summary.Scrubbed) > 0 {
		fmt.Fprintf(b, "  scrubbed: %s\n", strings.Join(summary.Scrubbed, ", "))
		b.WriteString("    ^ git-meta removed/healed — a push tried to unscope the encryption filter\n")
	}
	if len(summary.Degraded) > 0 {
		fmt.Fprintf(b, "  degraded: %s\n", strings.Join(summary.Degraded, ", "))
	}
}

// uptimeSuffix renders ", up 3h12m0s" for a daemon that reported a start time,
// mirroring the CLI's uptimeSuffix. A zero or future StartedAt (older daemon,
// clock skew) renders nothing rather than a nonsense duration.
func uptimeSuffix(startedAt, now time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	uptime := now.Sub(startedAt)
	if uptime < 0 {
		return ""
	}
	return ", up " + uptime.Round(time.Second).String()
}

func valueOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
