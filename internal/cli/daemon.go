package cli

import (
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Sawmonabo/agent-brain/internal/config"
	"github.com/Sawmonabo/agent-brain/internal/daemon"
	"github.com/Sawmonabo/agent-brain/internal/provider"
)

// buildRegistry is THE provider composition site. Phase 2 ships no
// adapters; Phase 3 registers claude and codex here and nowhere else.
func buildRegistry() (*provider.Registry, error) {
	return provider.NewRegistry()
}

func newDaemonCmd() *cobra.Command {
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Daemon process control",
	}
	daemonCmd.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run the sync daemon in the foreground (the service manager invokes this)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.DefaultPaths()
			if err != nil {
				return err
			}
			settings, err := config.LoadSettings(paths.SettingsFile())
			if err != nil {
				return err
			}
			registry, err := buildRegistry()
			if err != nil {
				return err
			}
			d, err := daemon.New(daemon.Config{
				Paths:    paths,
				Settings: settings,
				Registry: registry,
				Version:  Version,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), "agent-brain daemon starting (Ctrl-C to stop)"); err != nil {
				return err
			}
			return d.Run(ctx)
		},
	})
	return daemonCmd
}
