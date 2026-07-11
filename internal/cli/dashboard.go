package cli

import (
	"context"
	"errors"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Sawmonabo/agent-brain/internal/cli/dashboard"
	"github.com/Sawmonabo/agent-brain/internal/doctor"
	"github.com/Sawmonabo/agent-brain/internal/service"
)

// newDashboardCmd wires `agent-brain dashboard` (spec §7): the bubbletea v2 TUI
// in internal/cli/dashboard. This command and that package are the only two
// places in the repo permitted to import bubbletea/lipgloss directly (ADR 05
// amendment); everything else stays huh/fang.
func newDashboardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
		Short: "Live TUI over the running daemon: projects, conflicts, activity, doctor",
		Long: "Live terminal dashboard over the running daemon — enrolled projects, " +
			"retained conflicts, daemon activity, and doctor checks, refreshed every " +
			"couple of seconds.\n\n" +
			"It requires an interactive terminal. For scripted or non-interactive use " +
			"the equivalents are `agent-brain status --json` and `agent-brain projects --json`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isInteractiveTTY(cmd) {
				return errors.New("dashboard requires an interactive terminal (for scripting use `agent-brain status --json` or `agent-brain projects --json`)")
			}
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			binaryPath, err := resolveBinary()
			if err != nil {
				return err
			}
			controller, err := service.NewController(binaryPath)
			if err != nil {
				return err
			}

			model := dashboard.New(dashboard.Config{
				Data: dashboard.NewData(client, offlineDoctorRunner()),
				// The start offer only appears on the daemon-down screen. A
				// service that probes as already running there means a daemon
				// that is up-but-unresponsive or crash-looping — starting
				// cannot help, so name the next move instead of relaying the
				// sentinel as a bare "start failed: service already running".
				StartService: func() error {
					err := controller.Start()
					if errors.Is(err, service.ErrAlreadyRunning) {
						return errors.New("service already running but the daemon is not responding — check `agent-brain service logs`")
					}
					return err
				},
			})
			program := tea.NewProgram(
				model,
				tea.WithContext(cmd.Context()),
				tea.WithInput(cmd.InOrStdin()),
				tea.WithOutput(cmd.OutOrStdout()),
			)
			if _, err := program.Run(); err != nil {
				// A context-cancelled run (external signal) is a clean user exit,
				// not a CLI failure to report as exit code 1.
				if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			return nil
		},
	}
}

// offlineDoctorRunner assembles the full doctor.Deps (the same registry/gh
// composition `doctor` itself uses) and runs the battery read-only with
// --offline semantics — the 2s dashboard poll must never make a network
// ls-remote. Deps is built here, in package cli, because the provider/ghx/
// registry packages it needs sit outside the dashboard package's import
// allowlist (ADR 05 amendment); the dashboard only ever sees the doctor.Report.
func offlineDoctorRunner() func(context.Context) (doctor.Report, error) {
	return func(ctx context.Context) (doctor.Report, error) {
		deps, err := buildDoctorDeps(true, os.Getenv(testBinaryPathEnv))
		if err != nil {
			return doctor.Report{}, err
		}
		return doctor.Run(ctx, deps), nil
	}
}

// isInteractiveTTY reports whether the command's stdin AND stdout are real
// terminals. A bubbletea program needs both: stdin to read keys, stdout to
// render the alternate screen. In tests (and any piped invocation) the cobra
// in/out are byte buffers, not *os.File, so this returns false and the command
// refuses cleanly instead of hanging on a non-tty (task brief EOF/TTY contract).
func isInteractiveTTY(cmd *cobra.Command) bool {
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return false
	}
	out, ok := cmd.OutOrStdout().(*os.File)
	if !ok || !term.IsTerminal(int(out.Fd())) {
		return false
	}
	return true
}
