package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/daemon/api"
)

// newAPIClient dials the daemon, translating a dead socket into guidance
// instead of a raw dial error. It validates the socket path BEFORE dialing
// (handoff item 4): an oversized AGENT_BRAIN_RUNTIME_DIR would otherwise
// surface as a bare EINVAL from the unix dialer.
func newAPIClient() (*api.Client, error) {
	socketPath, err := daemon.SocketPathForClient()
	if err != nil {
		return nil, err
	}
	if err := config.ValidateSocketPath(socketPath); err != nil {
		return nil, err
	}
	return api.NewClient(socketPath), nil
}

// printJSON marshals v (a daemon/api response struct, verbatim) as indented
// JSON — the --json surface on the read commands.
func printJSON(cmd *cobra.Command, v any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
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
	var jsonOutput bool
	cmd := &cobra.Command{
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
			if jsonOutput {
				return printJSON(cmd, status)
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the raw daemon/api.StatusResponse as indented JSON")
	return cmd
}

func newSyncCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Trigger a sync cycle now and report the outcome",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			response, err := client.Sync(cmd.Context(), project)
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
	cmd.Flags().StringVar(&project, "project", "",
		"limit the cycle to one enrolled folder (see `agent-brain projects`); default is the whole fleet")
	return cmd
}

func newProjectsCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
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
			if jsonOutput {
				return printJSON(cmd, projects)
			}
			report := &reportWriter{w: cmd.OutOrStdout()}
			if len(projects.Units) == 0 {
				report.println("no projects enrolled — run `agent-brain track`")
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print the raw daemon/api.ProjectsResponse as indented JSON")
	return cmd
}
