package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// newAPIClient dials the daemon, translating a dead socket into
// guidance instead of a raw dial error.
func newAPIClient() (*api.Client, error) {
	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		return nil, err
	}
	return api.NewClient(socketPath), nil
}

func explainDown(err error) error {
	if errors.Is(err, api.ErrDaemonNotRunning) {
		return fmt.Errorf("%w\nStart it with `agent-brain service install` (login service) or `agent-brain daemon run` (foreground)", err)
	}
	return err
}

// reportWriter records the first write failure and skips the rest, so a
// command that prints many lines checks the error once at the end
// rather than at every call site (the "errors are values" idiom).
type reportWriter struct {
	w   io.Writer
	err error
}

func (r *reportWriter) printf(format string, args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintf(r.w, format, args...)
}

func (r *reportWriter) println(args ...any) {
	if r.err != nil {
		return
	}
	_, r.err = fmt.Fprintln(r.w, args...)
}

func printSummary(report *reportWriter, summary *api.SyncSummary) {
	if summary == nil {
		return
	}
	if summary.Error != "" {
		report.printf("  error: %s\n", summary.Error)
	}
	for _, subject := range summary.Commits {
		report.printf("  commit: %s\n", subject)
	}
	report.printf("  in: %d copied / %d deleted / %d skipped\n",
		summary.MirrorIn.Copied, summary.MirrorIn.Deleted, summary.MirrorIn.Skipped)
	report.printf("  out: %d copied / %d deleted / %d skipped\n",
		summary.MirrorOut.Copied, summary.MirrorOut.Deleted, summary.MirrorOut.Skipped)
	report.printf("  pushed: %v  queued: %v\n", summary.Pushed, summary.PushQueued)
	if len(summary.Degraded) > 0 {
		report.printf("  degraded: %v\n", summary.Degraded)
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon state and the last sync cycle",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			status, err := client.Status(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			report := &reportWriter{w: cmd.OutOrStdout()}
			report.printf("daemon: %s (version %s, pid %d)\n", status.State, status.Version, status.PID)
			if status.LastSync == nil {
				report.println("last sync: never")
				return report.err
			}
			report.printf("last sync: %s\n", status.LastSync.At.Format("2006-01-02 15:04:05 MST"))
			printSummary(report, status.LastSync)
			return report.err
		},
	}
}

func newSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Trigger a sync cycle now and report the outcome",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			response, err := client.Sync(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			report := &reportWriter{w: cmd.OutOrStdout()}
			if response.Status == "running" {
				report.println("sync still running — check `agent-brain status`")
				return report.err
			}
			report.println("sync completed")
			printSummary(report, response.Summary)
			return report.err
		},
	}
}

func newProjectsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "projects",
		Short: "List enrolled projects and their health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			projects, err := client.Projects(cmd.Context())
			if err != nil {
				return explainDown(err)
			}
			report := &reportWriter{w: cmd.OutOrStdout()}
			if len(projects.Units) == 0 {
				report.println("no projects enrolled (enrollment arrives with Phase 3's init/track)")
				return report.err
			}
			for _, unit := range projects.Units {
				health := "ok"
				if unit.Degraded {
					health = "degraded"
				}
				report.printf("%-8s %-24s %-9s %s\n", unit.Provider, unit.Folder, health, unit.LocalDir)
			}
			return report.err
		},
	}
}
