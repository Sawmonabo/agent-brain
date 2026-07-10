package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// activityView renders the daemon status snapshot (spec §7): uptime, state
// detail, quiesced-until (Task 2), and the last SyncSummary. It holds no state
// of its own — the daemon status is owned by the root model (it also drives the
// daemon-down screen and the Projects fleet header) and passed in at render.
//
// The brief also lists "watch trigger counts", but no field in
// api.StatusResponse or api.SyncSummary carries them; surfacing them would need
// a new daemon endpoint, which this task explicitly must not add (spec §7 /
// task brief: "If a view seems to need a new daemon endpoint, STOP"). They are
// therefore omitted rather than invented.
type activityView struct{}

func (activityView) view(status api.StatusResponse, statusErr error, now time.Time) string {
	if statusErr != nil {
		// Daemon-down is handled by the root before any view renders; a
		// non-down error here is some other read failure worth showing plainly.
		return sectionTitle("Activity") + "\n\n" + fmt.Sprintf("status unavailable: %v", statusErr)
	}

	var b strings.Builder
	b.WriteString(sectionTitle("Activity"))
	b.WriteString("\n\n")

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
